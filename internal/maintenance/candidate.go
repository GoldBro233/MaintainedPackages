package maintenance

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Manifest struct {
	Package      string             `json:"package"`
	SourceCommit string             `json:"source_commit"`
	Version      string             `json:"version"`
	AURCommit    *string            `json:"aur_commit"`
	Files        map[string]string  `json:"files"`
	AURFiles     map[string]*string `json:"aur_files"`
}

type candidate struct {
	Dir      string
	Package  Package
	Manifest Manifest
	Metadata Metadata
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var fullVersionPattern = regexp.MustCompile(`^([0-9]+:)?[0-9]+(\.[0-9]+)*-[0-9]+(\.[0-9]+)*$`)

func parseManifest(data []byte) (Manifest, error) {
	fields, err := jsonObject(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("Invalid candidate manifest: %w", err)
	}
	keys := []string{"package", "source_commit", "version", "aur_commit", "files", "aur_files"}
	if len(fields) != len(keys) {
		return Manifest{}, fmt.Errorf("Invalid candidate manifest fields")
	}
	for _, key := range keys {
		if fields[key] == nil {
			return Manifest{}, fmt.Errorf("Missing candidate manifest field: %s", key)
		}
	}
	var manifest Manifest
	manifest.Package, err = jsonString(fields["package"])
	if err != nil {
		return Manifest{}, fmt.Errorf("Invalid candidate package: %w", err)
	}
	manifest.SourceCommit, err = jsonString(fields["source_commit"])
	if err != nil || !commitPattern.MatchString(manifest.SourceCommit) {
		return Manifest{}, fmt.Errorf("Invalid candidate source commit")
	}
	manifest.Version, err = jsonString(fields["version"])
	if err != nil || !fullVersionPattern.MatchString(manifest.Version) {
		return Manifest{}, fmt.Errorf("Invalid candidate version")
	}
	if !bytes.Equal(bytes.TrimSpace(fields["aur_commit"]), []byte("null")) {
		value, err := jsonString(fields["aur_commit"])
		if err != nil || !commitPattern.MatchString(value) {
			return Manifest{}, fmt.Errorf("Invalid candidate AUR commit")
		}
		manifest.AURCommit = &value
	}
	fileFields, err := jsonObject(fields["files"])
	if err != nil {
		return Manifest{}, fmt.Errorf("Invalid candidate files: %w", err)
	}
	manifest.Files = make(map[string]string, len(fileFields))
	for name, raw := range fileFields {
		value, err := jsonString(raw)
		if err != nil || !digestPattern.MatchString(value) {
			return Manifest{}, fmt.Errorf("Invalid candidate file digest: %s", name)
		}
		manifest.Files[name] = value
	}
	aurFields, err := jsonObject(fields["aur_files"])
	if err != nil {
		return Manifest{}, fmt.Errorf("Invalid candidate aur_files: %w", err)
	}
	manifest.AURFiles = make(map[string]*string, len(aurFields))
	for name, raw := range aurFields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			manifest.AURFiles[name] = nil
			continue
		}
		value, err := jsonString(raw)
		if err != nil || !digestPattern.MatchString(value) {
			return Manifest{}, fmt.Errorf("Invalid candidate AUR digest: %s", name)
		}
		manifest.AURFiles[name] = &value
	}
	return manifest, nil
}

// Match both files and directories; a whitelist of files alone would allow
// extra empty directories and special entries to pass transport validation.
func validateCandidateEntries(dir string, files []string) error {
	expected := map[string]bool{"manifest.json": false, "package": true}
	for _, file := range files {
		path := filepath.Join("package", file)
		expected[path] = false
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			expected[parent] = true
		}
	}
	seen := make(map[string]bool, len(expected))
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		wantDir, exists := expected[rel]
		if !exists {
			return fmt.Errorf("Unexpected candidate entry: %s", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if wantDir {
			if !info.IsDir() {
				return fmt.Errorf("Missing candidate directory: %s", rel)
			}
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("Non-regular candidate entry: %s", rel)
		}
		seen[rel] = true
		return nil
	})
	if err != nil {
		return err
	}
	for name := range expected {
		if !seen[name] {
			return fmt.Errorf("Missing candidate entry: %s", name)
		}
	}
	return nil
}

func (s *Service) validateCandidate(ctx context.Context, name, dir, sourceCommit string) (*candidate, error) {
	p, err := s.config.selectPackage(name)
	if err != nil {
		return nil, err
	}
	dir, err = safePath(dir)
	if err != nil {
		return nil, err
	}
	if containsPath(s.root, dir) {
		return nil, fmt.Errorf("Unsafe candidate directory: %s", dir)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("Missing candidate directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("Missing candidate directory: %s", dir)
	}
	files := p.ManagedFiles()
	if err := validateCandidateEntries(dir, files); err != nil {
		return nil, err
	}
	data, err := readRegularFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	manifest, err := parseManifest(data)
	if err != nil {
		return nil, err
	}
	if manifest.Package != name {
		return nil, fmt.Errorf("Candidate package mismatch")
	}
	if sourceCommit != "" && sourceCommit != manifest.SourceCommit {
		return nil, fmt.Errorf("Candidate source commit mismatch")
	}
	head, err := s.git(ctx, s.root, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("Cannot read checkout HEAD: %w", err)
	}
	if strings.TrimRight(string(head), "\n") != manifest.SourceCommit {
		return nil, fmt.Errorf("Candidate does not match trusted checkout")
	}
	if len(manifest.Files) != len(files) || len(manifest.AURFiles) != len(files) {
		return nil, fmt.Errorf("Candidate file set differs")
	}
	for _, file := range files {
		expected, ok := manifest.Files[file]
		if !ok {
			return nil, fmt.Errorf("Candidate file set differs")
		}
		if _, ok := manifest.AURFiles[file]; !ok {
			return nil, fmt.Errorf("Candidate AUR file set differs")
		}
		hash, err := fileHash(filepath.Join(dir, "package", file))
		if err != nil {
			return nil, err
		}
		if hash == nil || *hash != expected {
			return nil, fmt.Errorf("Candidate file hashes differ: %s", file)
		}
	}
	data, err = readRegularFile(filepath.Join(dir, "package", ".SRCINFO"))
	if err != nil {
		return nil, err
	}
	metadata, err := parseMetadata(data, name)
	if err != nil {
		return nil, err
	}
	if metadata.Version != manifest.Version {
		return nil, fmt.Errorf("Candidate version differs from .SRCINFO")
	}
	return &candidate{Dir: dir, Package: p, Manifest: manifest, Metadata: metadata}, nil
}
