package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Service) latestVersion(ctx context.Context, p Package) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+p.UpstreamRepo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("Cannot fetch upstream release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Cannot fetch upstream release: HTTP %d", response.StatusCode)
	}
	var release struct {
		TagName *string `json:"tag_name"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&release); err != nil {
		return "", fmt.Errorf("Invalid upstream release: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", fmt.Errorf("Invalid upstream release JSON")
	}
	if release.TagName == nil {
		return "", fmt.Errorf("Missing upstream tag")
	}
	tag := *release.TagName
	if !strings.HasPrefix(tag, p.TagPrefix) {
		return "", fmt.Errorf("Unexpected upstream tag prefix: %s", tag)
	}
	version := strings.TrimPrefix(tag, p.TagPrefix)
	if !decimalVersionPattern.MatchString(version) {
		return "", fmt.Errorf("Unexpected upstream tag format: %s", tag)
	}
	return version, nil
}

// Keep verified downloads beside the trusted recipe even when makepkg.conf
// configures a shared SRCDEST. Other build and compiler settings stay inherited.
func (s *Service) prepareTool(ctx context.Context, dir, name string, stdout io.Writer, args ...string) error {
	return s.runner.Run(ctx, Command{Name: name, Args: args, Dir: dir, Env: []string{"SRCDEST=" + dir}, Stdout: stdout, Stderr: s.stderr})
}

func (s *Service) metadata(ctx context.Context, dir, name string) ([]byte, Metadata, error) {
	var output bytes.Buffer
	if err := s.prepareTool(ctx, dir, "makepkg", &output, "--printsrcinfo"); err != nil {
		return nil, Metadata{}, fmt.Errorf("Cannot read trusted package metadata: %w", err)
	}
	data := output.Bytes()
	parsed, err := parseMetadata(data, name)
	return data, parsed, err
}

// Only replace whole static assignment lines, leaving every other byte intact.
func updatedPKGBUILD(data []byte, version string) ([]byte, error) {
	lines := bytes.SplitAfter(data, []byte("\n"))
	for _, key := range []string{"pkgver", "pkgrel"} {
		count := 0
		for _, line := range lines {
			if bytes.HasPrefix(line, []byte(key+"=")) {
				count++
			}
		}
		if count != 1 {
			return nil, fmt.Errorf("Expected one static %s assignment", key)
		}
	}
	var output bytes.Buffer
	for _, line := range lines {
		replacement := ""
		if bytes.HasPrefix(line, []byte("pkgver=")) {
			replacement = "pkgver=" + version
		}
		if bytes.HasPrefix(line, []byte("pkgrel=")) {
			replacement = "pkgrel=1"
		}
		if replacement == "" {
			output.Write(line)
			continue
		}
		output.WriteString(replacement)
		if bytes.HasSuffix(line, []byte("\n")) {
			output.WriteByte('\n')
		}
	}
	return output.Bytes(), nil
}

func (s *Service) updatePackage(ctx context.Context, dir, version string) error {
	path := filepath.Join(dir, "PKGBUILD")
	data, err := readRegularFile(path)
	if err != nil {
		return err
	}
	data, err = updatedPKGBUILD(data, version)
	if err != nil {
		return err
	}
	if err := writeRegularFile(path, data); err != nil {
		return err
	}
	if err := s.prepareTool(ctx, dir, "updpkgsums", s.stderr); err != nil {
		return fmt.Errorf("Cannot update source checksums: %w", err)
	}
	return nil
}

func (s *Service) Prepare(ctx context.Context, name, dest string, opts PrepareOptions) (PrepareResult, error) {
	p, err := s.config.selectPackage(name)
	if err != nil {
		return PrepareResult{}, err
	}
	dest, err = outsideDirectory(s.root, dest, "")
	if err != nil {
		return PrepareResult{}, err
	}
	head, err := s.git(ctx, s.root, "rev-parse", "HEAD")
	if err != nil {
		return PrepareResult{}, fmt.Errorf("Cannot read checkout HEAD: %w", err)
	}
	source := strings.TrimRight(string(head), "\n")
	if !commitPattern.MatchString(source) {
		return PrepareResult{}, fmt.Errorf("Invalid checkout HEAD")
	}
	if opts.SourceCommit != "" && opts.SourceCommit != source {
		return PrepareResult{}, fmt.Errorf("SOURCE_COMMIT is not checkout HEAD")
	}
	temp, err := os.MkdirTemp("", "maintpkg-prepare-")
	if err != nil {
		return PrepareResult{}, err
	}
	defer os.RemoveAll(temp)
	trusted := filepath.Join(temp, "trusted")
	if err := os.MkdirAll(trusted, 0755); err != nil {
		return PrepareResult{}, err
	}
	if err := os.MkdirAll(filepath.Join(dest, "package"), 0755); err != nil {
		return PrepareResult{}, err
	}
	prefix := "packages/" + name + "/"
	var originalSRCINFO []byte
	for _, file := range []string{"PKGBUILD", ".SRCINFO"} {
		data, err := s.gitBlob(ctx, s.root, source, prefix+file)
		if err != nil {
			return PrepareResult{}, err
		}
		if data == nil {
			return PrepareResult{}, fmt.Errorf("Missing tracked source: %s", file)
		}
		if err := writeRegularFile(filepath.Join(trusted, file), data); err != nil {
			return PrepareResult{}, err
		}
		if file == ".SRCINFO" {
			originalSRCINFO = data
		}
	}
	// A local install hook must exist even for makepkg --printsrcinfo. A
	// downloaded alias instead has to pass makepkg's source verification first.
	for _, file := range p.AURExtraFiles {
		data, err := s.gitBlob(ctx, s.root, source, prefix+file)
		if err != nil {
			return PrepareResult{}, err
		}
		if data != nil && !bytes.Contains(originalSRCINFO, []byte("source = "+file+"::")) {
			if err := writeRegularFile(filepath.Join(trusted, file), data); err != nil {
				return PrepareResult{}, err
			}
		}
	}
	_, current, err := s.metadata(ctx, trusted, name)
	if err != nil {
		return PrepareResult{}, err
	}
	latest, err := s.latestVersion(ctx, p)
	if err != nil {
		return PrepareResult{}, err
	}
	order, err := s.compareVersions(ctx, latest, current.Pkgver)
	if err != nil {
		return PrepareResult{}, err
	}
	needVerify := order > 0
	if needVerify {
		if err := s.updatePackage(ctx, trusted, latest); err != nil {
			return PrepareResult{}, err
		}
	}
	data, metadata, err := s.metadata(ctx, trusted, name)
	if err != nil {
		return PrepareResult{}, err
	}
	if err := writeRegularFile(filepath.Join(dest, "package", ".SRCINFO"), data); err != nil {
		return PrepareResult{}, err
	}
	if err := copyFile(filepath.Join(trusted, "PKGBUILD"), filepath.Join(dest, "package", "PKGBUILD")); err != nil {
		return PrepareResult{}, err
	}
	aur, err := s.aurSnapshot(ctx, name, filepath.Join(temp, "aur"))
	if err != nil {
		return PrepareResult{}, err
	}
	managed := p.ManagedFiles()
	aurFiles, err := s.gitHashes(ctx, aur.Dir, aur.Commit, "", managed)
	if err != nil {
		return PrepareResult{}, err
	}
	if aur.Commit != nil {
		data, err := s.gitBlob(ctx, aur.Dir, *aur.Commit, ".SRCINFO")
		if err != nil {
			return PrepareResult{}, err
		}
		aurMetadata, err := parseMetadata(data, name)
		if err != nil {
			return PrepareResult{}, err
		}
		order, err := s.compareVersions(ctx, aurMetadata.Version, metadata.Version)
		if err != nil {
			return PrepareResult{}, err
		}
		if order > 0 {
			return PrepareResult{}, fmt.Errorf("AUR version is newer than candidate")
		}
		hash, err := fileHash(filepath.Join(dest, "package", "PKGBUILD"))
		if err != nil {
			return PrepareResult{}, err
		}
		if aurMetadata.Version == metadata.Version && !hashEqual(aurFiles["PKGBUILD"], hash) {
			return PrepareResult{}, fmt.Errorf("Same version has different PKGBUILD; increase pkgrel")
		}
	}
	for _, file := range p.AURExtraFiles {
		hash, err := fileHash(filepath.Join(trusted, file))
		if err != nil {
			return PrepareResult{}, err
		}
		if hash == nil {
			needVerify = true
			continue
		}
		if err := copyFile(filepath.Join(trusted, file), filepath.Join(dest, "package", file)); err != nil {
			return PrepareResult{}, err
		}
	}
	verified := needVerify || len(p.AURExtraFiles) > 0
	if verified {
		if err := s.prepareTool(ctx, trusted, "makepkg", s.stderr, "--verifysource", "--noconfirm"); err != nil {
			return PrepareResult{}, fmt.Errorf("Source verification failed: %w", err)
		}
	}
	for _, file := range p.AURExtraFiles {
		hash, err := fileHash(filepath.Join(dest, "package", file))
		if err != nil {
			return PrepareResult{}, err
		}
		if hash == nil {
			if err := copyFile(filepath.Join(trusted, file), filepath.Join(dest, "package", file)); err != nil {
				return PrepareResult{}, err
			}
		}
	}
	files := make(map[string]string, len(managed))
	repoDiff, aurDiff := false, false
	for _, file := range managed {
		hash, err := fileHash(filepath.Join(dest, "package", file))
		if err != nil {
			return PrepareResult{}, err
		}
		if hash == nil {
			return PrepareResult{}, fmt.Errorf("Missing candidate file: %s", file)
		}
		files[file] = *hash
		if !hashEqual(hash, aurFiles[file]) {
			aurDiff = true
		}
		if file == "PKGBUILD" || file == ".SRCINFO" {
			original, err := s.blobHash(ctx, s.root, source, prefix+file)
			if err != nil {
				return PrepareResult{}, err
			}
			if !hashEqual(original, hash) {
				repoDiff = true
			}
		}
	}
	manifest := Manifest{Package: name, SourceCommit: source, Version: metadata.Version, AURCommit: aur.Commit, Files: files, AURFiles: aurFiles}
	data, err = json.Marshal(manifest)
	if err != nil {
		return PrepareResult{}, err
	}
	if err := writeRegularFile(filepath.Join(dest, "manifest.json"), append(data, '\n')); err != nil {
		return PrepareResult{}, err
	}
	if _, err := s.validateCandidate(ctx, name, dest, opts.SourceCommit); err != nil {
		return PrepareResult{}, err
	}
	needsBuild := repoDiff || aurDiff || opts.ForceBuild
	if needsBuild && !verified {
		if err := s.prepareTool(ctx, trusted, "makepkg", s.stderr, "--verifysource", "--noconfirm"); err != nil {
			return PrepareResult{}, fmt.Errorf("Source verification failed: %w", err)
		}
	}
	return PrepareResult{NeedsBuild: needsBuild, NeedsPublish: repoDiff || aurDiff, CandidateVersion: metadata.Version}, nil
}
