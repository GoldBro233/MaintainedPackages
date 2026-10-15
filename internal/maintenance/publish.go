package maintenance

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

// PublishError records partial publication without obscuring its original cause.
type PublishError struct {
	AURSynced bool
	Stage     string
	Err       error
}

func (e *PublishError) Error() string {
	if e.AURSynced {
		return fmt.Sprintf("AUR synchronized; GitHub synchronization failed: %s: %v", e.Stage, e.Err)
	}
	return fmt.Sprintf("publication failed: %s: %v", e.Stage, e.Err)
}

func (e *PublishError) Unwrap() error { return e.Err }

func githubFailure(stage string, err error) error {
	return &PublishError{AURSynced: true, Stage: stage, Err: err}
}

// trustedTree includes both revisions' entire maintenance source trees. Comparing
// their unions catches additions and deletions as well as changed blobs.
func (s *Service) trustedTree(ctx context.Context, repo, ref string) (map[string]string, error) {
	data, err := s.git(ctx, repo, "ls-tree", "-r", "-z", ref)
	if err != nil {
		return nil, err
	}
	entries, err := parseGitEntries(data)
	if err != nil {
		return nil, err
	}
	required := []string{
		"packages.json", "go.mod", ".github/aur_known_hosts",
		".github/workflows/maintain-packages.yml", ".github/workflows/release-package.yml",
	}
	wanted := make(map[string]bool, len(required)+1)
	for _, path := range required {
		wanted[path] = true
	}
	wanted["go.sum"] = true
	result := make(map[string]string)
	var hasCmd, hasInternal bool
	for _, entry := range entries {
		cmd := strings.HasPrefix(entry.path, "cmd/")
		internal := strings.HasPrefix(entry.path, "internal/")
		if !wanted[entry.path] && !cmd && !internal && entry.path != "cmd" && entry.path != "internal" {
			continue
		}
		if !entry.regular() {
			return nil, fmt.Errorf("non-regular trusted Git entry: %s", entry.path)
		}
		if entry.path == "cmd" || entry.path == "internal" {
			return nil, fmt.Errorf("trusted source tree is missing: %s", entry.path)
		}
		result[entry.path] = entry.object
		hasCmd = hasCmd || cmd
		hasInternal = hasInternal || internal
	}
	for _, path := range required {
		if _, ok := result[path]; !ok {
			return nil, fmt.Errorf("trusted maintenance input is missing: %s", path)
		}
	}
	if !hasCmd || !hasInternal {
		return nil, fmt.Errorf("trusted cmd/ or internal/ source tree is missing")
	}
	return result, nil
}

func (s *Service) checkGitHub(ctx context.Context, repo, ref string, c *candidate) error {
	source := c.Manifest.SourceCommit
	if _, err := s.git(ctx, repo, "merge-base", "--is-ancestor", source, ref); err != nil {
		return fmt.Errorf("source commit is not an ancestor of GitHub target: %w", err)
	}
	initialTree, err := s.trustedTree(ctx, s.root, source)
	if err != nil {
		return err
	}
	currentTree, err := s.trustedTree(ctx, repo, ref)
	if err != nil {
		return err
	}
	for path, initial := range initialTree {
		if currentTree[path] != initial {
			return fmt.Errorf("trusted maintenance configuration changed: %s", path)
		}
	}
	for path := range currentTree {
		if _, ok := initialTree[path]; !ok {
			return fmt.Errorf("trusted maintenance configuration changed: %s", path)
		}
	}
	initialData, err := s.gitBlob(ctx, s.root, source, "packages.json")
	if err != nil {
		return err
	}
	currentData, err := s.gitBlob(ctx, repo, ref, "packages.json")
	if err != nil {
		return err
	}
	initialConfig, err := parseConfig(initialData)
	if err != nil {
		return fmt.Errorf("source configuration: %w", err)
	}
	currentConfig, err := parseConfig(currentData)
	if err != nil {
		return fmt.Errorf("target configuration: %w", err)
	}
	initialPackage, err := initialConfig.selectPackage(c.Package.Name)
	if err != nil {
		return fmt.Errorf("source package configuration: %w", err)
	}
	currentPackage, err := currentConfig.selectPackage(c.Package.Name)
	if err != nil {
		return fmt.Errorf("target package configuration: %w", err)
	}
	if !reflect.DeepEqual(initialPackage, currentPackage) || !reflect.DeepEqual(initialPackage, c.Package) {
		return fmt.Errorf("package configuration changed or disabled on GitHub")
	}
	for _, file := range c.Package.ManagedFiles() {
		path := "packages/" + c.Package.Name + "/" + file
		initial, err := s.blobHash(ctx, s.root, source, path)
		if err != nil {
			return err
		}
		current, err := s.blobHash(ctx, repo, ref, path)
		if err != nil {
			return err
		}
		wanted := c.Manifest.Files[file]
		if !hashEqual(current, initial) && !hashEqual(current, &wanted) {
			return fmt.Errorf("GitHub package changed since prepare: %s", path)
		}
	}
	return nil
}

func (s *Service) checkAUR(ctx context.Context, state aurState, c *candidate) (bool, error) {
	current, err := s.gitHashes(ctx, state.Dir, state.Commit, "", c.Package.ManagedFiles())
	if err != nil {
		return false, err
	}
	synced := true
	for _, file := range c.Package.ManagedFiles() {
		wanted := c.Manifest.Files[file]
		if !hashEqual(current[file], &wanted) {
			synced = false
		}
	}
	if synced {
		return true, nil
	}
	for _, file := range c.Package.ManagedFiles() {
		if !hashEqual(current[file], c.Manifest.AURFiles[file]) {
			return false, fmt.Errorf("AUR managed files changed since prepare: %s", file)
		}
	}
	if state.Commit == nil {
		return false, nil
	}
	data, err := s.gitBlob(ctx, state.Dir, *state.Commit, ".SRCINFO")
	if err != nil {
		return false, err
	}
	if data == nil {
		return false, fmt.Errorf("AUR .SRCINFO is missing")
	}
	metadata, err := parseMetadata(data, c.Package.Name)
	if err != nil {
		return false, fmt.Errorf("AUR metadata: %w", err)
	}
	order, err := s.compareVersions(ctx, metadata.Version, c.Manifest.Version)
	if err != nil {
		return false, err
	}
	if order > 0 {
		return false, fmt.Errorf("AUR version is newer than candidate")
	}
	if metadata.Version == c.Manifest.Version {
		wanted := c.Manifest.Files["PKGBUILD"]
		if !hashEqual(current["PKGBUILD"], &wanted) {
			return false, fmt.Errorf("Same version has different PKGBUILD; increase pkgrel")
		}
	}
	return false, nil
}

var aurHostRecord = regexp.MustCompile(`^aur\.archlinux\.org[\t ]ssh-ed25519[\t ][A-Za-z0-9+/]+={0,2}$`)

func sshQuotedPath(path string) (string, error) {
	if strings.ContainsAny(path, "\r\n\x00") {
		return "", fmt.Errorf("invalid SSH configuration path")
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`, nil
}

func shellQuotedPath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func (s *Service) configureAURSSH(ctx context.Context, temp, privateKey string) (string, error) {
	if privateKey == "" {
		return "", fmt.Errorf("AUR_SSH_PRIVATE_KEY is required")
	}
	known := filepath.Join(s.root, ".github", "aur_known_hosts")
	// readRegularFile checks every existing path component, including parents.
	data, err := readRegularFile(known)
	if err != nil {
		return "", fmt.Errorf("AUR host key file: %w", err)
	}
	if bytes.Count(data, []byte{'\n'}) != 1 || len(data) == 0 || data[len(data)-1] != '\n' || !aurHostRecord.Match(data[:len(data)-1]) {
		return "", fmt.Errorf("invalid AUR host key record")
	}
	if err := s.runner.Run(ctx, Command{Name: "ssh-keygen", Args: []string{"-lf", known, "-E", "sha256"}, Dir: s.root, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		return "", fmt.Errorf("invalid AUR ed25519 key: %w", err)
	}
	dir := filepath.Join(temp, "ssh")
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	keyPath, knownPath := filepath.Join(dir, "key"), filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(keyPath, []byte(strings.TrimRight(privateKey, "\n")+"\n"), 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(knownPath, data, 0600); err != nil {
		return "", err
	}
	quotedKey, err := sshQuotedPath(keyPath)
	if err != nil {
		return "", err
	}
	quotedKnown, err := sshQuotedPath(knownPath)
	if err != nil {
		return "", err
	}
	config := "Host aur.archlinux.org\n  BatchMode yes\n  StrictHostKeyChecking yes\n  IdentitiesOnly yes\n  IdentityAgent none\n  UserKnownHostsFile " + quotedKnown + "\n  GlobalKnownHostsFile /dev/null\n  IdentityFile " + quotedKey + "\n"
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		return "", err
	}
	return "ssh -F " + shellQuotedPath(configPath), nil
}

// Retry only a genuine porcelain rejection, never an authentication, transport,
// hook or unrelated rejection. Do not print captured push output or credentials.
func retryablePush(output []byte, err error) bool {
	if !gitExitCode(err, 1) {
		return false
	}
	rejected := false
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, "!\t") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || (fields[2] != "[rejected] (non-fast-forward)" && fields[2] != "[rejected] (fetch first)") {
			return false
		}
		rejected = true
	}
	return rejected
}

func (s *Service) push(ctx context.Context, repo, branch string, env []string) (bool, error) {
	var output bytes.Buffer
	env = append(env, "LC_ALL=C")
	// Porcelain is stdout only; stderr diagnostics must not interleave with it.
	err := s.runner.Run(ctx, Command{Name: "git", Args: s.gitArgs(repo, "push", "--porcelain", "origin", "HEAD:"+branch), Dir: repo, Env: env, Stdout: &output, Stderr: io.Discard})
	if err == nil {
		return false, nil
	}
	return ctx.Err() == nil && retryablePush(output.Bytes(), err), fmt.Errorf("git push failed: %w", err)
}

func (s *Service) setPublisherIdentity(ctx context.Context, repo string) error {
	if _, err := s.git(ctx, repo, "config", "user.name", "github-actions[bot]"); err != nil {
		return err
	}
	_, err := s.git(ctx, repo, "config", "user.email", "41898282+github-actions[bot]@users.noreply.github.com")
	return err
}

func (s *Service) githubTarget(ctx context.Context, repo, branch string) (string, string, error) {
	if _, err := s.git(ctx, repo, "fetch", "-q", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return "", "fetch", err
	}
	data, err := s.git(ctx, repo, "rev-parse", "--verify", "refs/remotes/origin/"+branch)
	if err != nil {
		return "", "missing branch", err
	}
	return strings.TrimSpace(string(data)), "", nil
}

func (s *Service) Publish(ctx context.Context, name, candidateDir string, opts PublishOptions) error {
	if opts.SourceCommit == "" {
		return fmt.Errorf("SOURCE_COMMIT is required")
	}
	if opts.TargetBranch != "main" {
		return fmt.Errorf("publication must target main")
	}
	c, err := s.validateCandidate(ctx, name, candidateDir, opts.SourceCommit)
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "maintpkg-publish-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	url, err := s.git(ctx, s.root, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("cannot determine trusted GitHub origin: %w", err)
	}
	githubRepo := filepath.Join(temp, "github")
	if _, err := s.git(ctx, s.root, "clone", "-q", strings.TrimSpace(string(url)), githubRepo); err != nil {
		return fmt.Errorf("cannot read GitHub origin: %w", err)
	}
	ref, _, err := s.githubTarget(ctx, githubRepo, opts.TargetBranch)
	if err != nil {
		return err
	}
	if err := s.checkGitHub(ctx, githubRepo, ref, c); err != nil {
		return err
	}

	var sshCommand string
	var lastPush error
	aurSynced := false
	for attempt := 1; attempt <= 3; attempt++ {
		state, err := s.aurSnapshot(ctx, name, filepath.Join(temp, fmt.Sprintf("aur-%d", attempt)))
		if err != nil {
			return err
		}
		synced, err := s.checkAUR(ctx, state, c)
		if err != nil {
			return err
		}
		if synced {
			aurSynced = true
			break
		}
		if state.Commit == nil {
			if _, err := s.git(ctx, state.Dir, "checkout", "-q", "-b", "master"); err != nil {
				return fmt.Errorf("cannot create AUR master: %w", err)
			}
		}
		if sshCommand == "" {
			sshCommand, err = s.configureAURSSH(ctx, temp, opts.AURSSHPrivateKey)
			if err != nil {
				return err
			}
		}
		for _, file := range c.Package.ManagedFiles() {
			// Check the complete destination before creating any parent or copying.
			destination := filepath.Join(state.Dir, filepath.FromSlash(file))
			if _, err := safePath(destination); err != nil {
				return fmt.Errorf("AUR destination %s: %w", file, err)
			}
			if err := copyFile(filepath.Join(c.Dir, "package", filepath.FromSlash(file)), destination); err != nil {
				return fmt.Errorf("copy AUR file %s: %w", file, err)
			}
		}
		if err := s.setPublisherIdentity(ctx, state.Dir); err != nil {
			return err
		}
		args := append([]string{"add", "--"}, c.Package.ManagedFiles()...)
		if _, err := s.git(ctx, state.Dir, args...); err != nil {
			return err
		}
		if _, err := s.git(ctx, state.Dir, "commit", "-qm", "chore("+name+"): update to "+c.Manifest.Version); err != nil {
			return fmt.Errorf("cannot commit AUR metadata: %w", err)
		}
		if _, err := s.git(ctx, state.Dir, "remote", "set-url", "--push", "origin", "ssh://aur@aur.archlinux.org/"+name+".git"); err != nil {
			return err
		}
		retry, err := s.push(ctx, state.Dir, "master", []string{"GIT_SSH_COMMAND=" + sshCommand})
		if err == nil {
			aurSynced = true
			break
		}
		if !retry {
			return fmt.Errorf("AUR push failed: %w", err)
		}
		lastPush = err
		if attempt < 3 {
			fmt.Fprintf(s.stderr, "AUR changed concurrently, retrying (%d/3)\n", attempt)
		}
	}
	if !aurSynced {
		return fmt.Errorf("AUR push rejected after 3 attempts: %w", lastPush)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ref, stage, err := s.githubTarget(ctx, githubRepo, opts.TargetBranch)
		if err != nil {
			return githubFailure(stage, err)
		}
		if err := s.checkGitHub(ctx, githubRepo, ref, c); err != nil {
			return githubFailure("target changed", err)
		}
		if _, err := s.git(ctx, githubRepo, "checkout", "-q", "-B", "maintenance", ref); err != nil {
			return githubFailure("checkout", err)
		}
		paths := []string{"packages/" + name + "/PKGBUILD", "packages/" + name + "/.SRCINFO"}
		for i, file := range []string{"PKGBUILD", ".SRCINFO"} {
			if err := copyFile(filepath.Join(c.Dir, "package", file), filepath.Join(githubRepo, filepath.FromSlash(paths[i]))); err != nil {
				return githubFailure("copy", err)
			}
		}
		if _, err := s.git(ctx, githubRepo, append([]string{"add", "--"}, paths...)...); err != nil {
			return githubFailure("stage", err)
		}
		if _, err := s.git(ctx, githubRepo, "diff", "--cached", "--quiet"); err == nil {
			return nil
		} else if !gitExitCode(err, 1) {
			return githubFailure("diff", err)
		}
		if err := s.setPublisherIdentity(ctx, githubRepo); err != nil {
			return githubFailure("commit", err)
		}
		if _, err := s.git(ctx, githubRepo, "commit", "-qm", "chore("+name+"): update to "+c.Manifest.Version); err != nil {
			return githubFailure("commit", err)
		}
		if opts.GitHubToken == "" {
			return githubFailure("credentials", fmt.Errorf("GITHUB_TOKEN is required"))
		}
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + opts.GitHubToken))
		retry, err := s.push(ctx, githubRepo, opts.TargetBranch, []string{
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + auth,
		})
		if err == nil {
			return nil
		}
		if !retry {
			return githubFailure("push", err)
		}
		lastPush = err
		if attempt < 3 {
			fmt.Fprintf(s.stderr, "GitHub changed concurrently, retrying (%d/3)\n", attempt)
		}
	}
	return githubFailure("concurrent pushes", lastPush)
}
