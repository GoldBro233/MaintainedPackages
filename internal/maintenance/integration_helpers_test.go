//go:build integration

package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

const fixtureName = "fixture-bin"
const fixtureToken = "test-only-local-transport"
const fixturePayload = "A small real maintenance package payload.\n"
const fixtureInstall = "post_install() { printf '%s\\n' 'Enable the fixture service'; }\n"

// Every command receives a fresh environment. In particular none of the caller's
// Git, SSH, makepkg, credential, output-directory or configuration overrides leak
// into a fixture. Canonical remote URLs are rewritten to local bare repositories;
// GIT_ALLOW_PROTOCOL=file makes an omitted mapping fail closed.
type integrationFixture struct {
	t                                     *testing.T
	dir, repo, github, source, key, goBin string
	env                                   []string
	packages                              []Package
	tag, response                         string
	status                                int
	requests                              atomic.Int64
	server                                *httptest.Server
	client                                *http.Client
	stdout, stderr                        bytes.Buffer
	runner                                Runner
}

type fixtureTransport struct {
	base      *url.URL
	transport http.RoundTripper
}

func (t fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "api.github.com" || !strings.HasSuffix(r.URL.Path, "/releases/latest") {
		return nil, fmt.Errorf("unexpected upstream request: %s", r.URL)
	}
	clone := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = t.base.Scheme
	u.Host = t.base.Host
	clone.URL = &u
	clone.Host = t.base.Host
	return t.transport.RoundTrip(clone)
}

func integrationPrerequisites(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Fatal("integration tests require a non-root user for real makepkg")
	}
	for _, name := range []string{"git", "makepkg", "updpkgsums", "vercmp", "pacman", "ssh-keygen", "bsdtar", "zstd", "gzip", "xz", "bzip2", "fakeroot", "strip", "find", "patch"} {
		found := false
		for _, dir := range []string{"/usr/bin", "/bin"} {
			st, err := os.Stat(filepath.Join(dir, name))
			if err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing integration prerequisite: %s (PATH /usr/bin:/bin)", name)
		}
	}
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if st, err := os.Stat(goBin); err != nil || st.IsDir() || st.Mode().Perm()&0111 == 0 {
		t.Fatalf("missing Go compiler at %s", goBin)
	}
	return goBin
}

func newIntegrationFixture(t *testing.T, count int) *integrationFixture {
	t.Helper()
	f := &integrationFixture{t: t, dir: filepath.Join(t.TempDir(), "fixture with spaces"), goBin: integrationPrerequisites(t), tag: "v1.9.0", status: http.StatusOK}
	f.repo = filepath.Join(f.dir, "repo")
	f.github = filepath.Join(f.dir, "github.git")
	for _, name := range []string{"repo", "home", "xdg", "cache", "sources", "builds", "logs", "artifacts", "go-cache", "go-mod-cache", "tmp"} {
		mustMkdir(t, filepath.Join(f.dir, name))
	}
	f.env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(f.dir, "home"), "XDG_CONFIG_HOME=" + filepath.Join(f.dir, "xdg"), "XDG_CACHE_HOME=" + filepath.Join(f.dir, "cache"), "TMPDIR=" + filepath.Join(f.dir, "tmp"), "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(f.dir, "gitconfig"), "GIT_ALLOW_PROTOCOL=file", "GIT_TERMINAL_PROMPT=0", "GOTOOLCHAIN=local", "GOCACHE=" + filepath.Join(f.dir, "go-cache"), "GOMODCACHE=" + filepath.Join(f.dir, "go-mod-cache")}
	f.runner = ExecRunner{Env: f.env}
	f.git(f.dir, "config", "--file", filepath.Join(f.dir, "gitconfig"), "commit.gpgsign", "false")
	config, err := os.ReadFile("/etc/makepkg.conf")
	if err != nil {
		t.Fatalf("read default makepkg config: %v", err)
	}
	// Retain the complete system compiler/compressor configuration, replacing only
	// fixture paths and signing behavior. Package creation still uses real tools.
	config = append(config, []byte("\nBUILDENV=(!distcc !color !ccache check !sign)\nPKGEXT='.pkg.tar.zst'\n")...)
	for _, pair := range [][2]string{{"PKGDEST", "artifacts"}, {"SRCDEST", "sources"}, {"BUILDDIR", "builds"}, {"LOGDEST", "logs"}} {
		config = append(config, []byte(pair[0]+"="+shellQuote(filepath.Join(f.dir, pair[1]))+"\n")...)
	}
	mustWrite(t, filepath.Join(f.dir, "home/.makepkg.conf"), config, 0600)
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate integration fixture source")
	}
	sourceRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	for _, name := range []string{"go.mod", "cmd", "internal", ".github/aur_known_hosts", ".github/workflows/maintain-packages.yml", ".github/workflows/release-package.yml"} {
		copyFixtureTree(t, filepath.Join(sourceRoot, name), filepath.Join(f.repo, name))
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "go.sum")); err == nil {
		copyFixtureTree(t, filepath.Join(sourceRoot, "go.sum"), filepath.Join(f.repo, "go.sum"))
	}
	names := []string{fixtureName}
	if count == 2 {
		names = append(names, "other-bin")
	}
	for _, name := range names {
		f.packages = append(f.packages, Package{Name: name, Enabled: true, UpstreamRepo: "fixture/" + name, TagPrefix: "v", AURExtraFiles: []string{"fixture-source"}})
		f.writeRecipe(name, "1.9.0", "2", "")
	}
	f.writeConfig()
	f.git(f.repo, "init", "-q", "-b", "main")
	f.identity(f.repo)
	f.git(f.repo, "add", ".")
	f.git(f.repo, "commit", "-q", "-m", "Trusted fixture inputs")
	f.git(f.dir, "init", "--bare", "-q", f.github)
	f.git(f.dir, "--git-dir="+f.github, "symbolic-ref", "HEAD", "refs/heads/main")
	f.git(f.repo, "remote", "add", "origin", "https://github.com/fixture/maintained.git")
	f.mapping(f.github, "https://github.com/fixture/maintained.git")
	f.git(f.repo, "push", "-q", "origin", "HEAD:main")
	f.source = strings.TrimSpace(f.git(f.repo, "rev-parse", "HEAD"))
	for _, name := range names {
		f.createAUR(name)
	}
	keyPath := filepath.Join(f.dir, "fixture-key")
	f.command(f.dir, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath)
	f.key = string(mustRead(t, keyPath))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/repos/fixture/") || !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Error(w, "unexpected endpoint", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		if f.response != "" {
			_, _ = io.WriteString(w, f.response)
		} else {
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": f.tag})
		}
	}))
	t.Cleanup(f.server.Close)
	base, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.client = &http.Client{Transport: fixtureTransport{base: base, transport: f.server.Client().Transport}}
	return f
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
}
func mustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
func copyFixtureTree(t *testing.T, src, dst string) {
	t.Helper()
	st, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if st.IsDir() {
		mustMkdir(t, dst)
		entries, err := os.ReadDir(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			copyFixtureTree(t, filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()))
		}
		return
	}
	if !st.Mode().IsRegular() {
		t.Fatalf("fixture input is not a regular file: %s", src)
	}
	mustWrite(t, dst, mustRead(t, src), st.Mode().Perm())
}
func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func (f *integrationFixture) command(dir, name string, args ...string) string {
	f.t.Helper()
	var out, diagnostics bytes.Buffer
	err := (ExecRunner{Env: f.env}).Run(context.Background(), Command{Name: name, Args: args, Dir: dir, Stdout: &out, Stderr: &diagnostics})
	if err != nil {
		f.t.Fatalf("fixture command %s %q: %v\n%s", name, args, err, diagnostics.String())
	}
	return out.String()
}
func (f *integrationFixture) git(dir string, args ...string) string {
	f.t.Helper()
	return f.command(dir, "git", args...)
}
func (f *integrationFixture) identity(dir string) {
	f.git(dir, "config", "user.name", "Maintenance Test")
	f.git(dir, "config", "user.email", "maintenance@example.invalid")
	f.git(dir, "config", "commit.gpgsign", "false")
}
func (f *integrationFixture) mapping(remote, canonical string) {
	f.git(f.dir, "config", "--file", filepath.Join(f.dir, "gitconfig"), "--add", "url."+remote+".insteadOf", canonical)
}
func (f *integrationFixture) writeConfig() {
	b, err := json.Marshal(Config{Packages: f.packages})
	if err != nil {
		f.t.Fatal(err)
	}
	mustWrite(f.t, filepath.Join(f.repo, "packages.json"), b, 0644)
}
func (f *integrationFixture) recipeDir(name string) string {
	return filepath.Join(f.repo, "packages", name)
}
func (f *integrationFixture) writeRecipe(name, version, release, extra string) {
	f.t.Helper()
	dir := f.recipeDir(name)
	mustMkdir(f.t, dir)
	payload := []byte(fixturePayload)
	mustWrite(f.t, filepath.Join(dir, "fixture-source"), payload, 0644)
	recipe := fmt.Sprintf("pkgname=%s\npkgver=%s\npkgrel=%s\npkgdesc='Local package maintenance fixture'\narch=('any')\nlicense=('MIT')\nsource=('fixture-source')\nsha256sums=('%s')\npackage() { install -Dm644 \"$srcdir/fixture-source\" \"$pkgdir/usr/share/%s/payload\"; }\n%s", name, version, release, digest(payload), name, extra)
	mustWrite(f.t, filepath.Join(dir, "PKGBUILD"), []byte(recipe), 0644)
	f.srcinfo(dir)
}
func (f *integrationFixture) srcinfo(dir string) {
	output := f.command(dir, "makepkg", "--printsrcinfo")
	mustWrite(f.t, filepath.Join(dir, ".SRCINFO"), []byte(output), 0644)
}
func (f *integrationFixture) editRecipe(dir string, edit func(string) string) {
	mustWrite(f.t, filepath.Join(dir, "PKGBUILD"), []byte(edit(string(mustRead(f.t, filepath.Join(dir, "PKGBUILD"))))), 0644)
	f.srcinfo(dir)
}
func (f *integrationFixture) commitSource() {
	f.git(f.repo, "add", ".")
	f.git(f.repo, "commit", "-q", "-m", "Fixture source change")
	f.git(f.repo, "push", "-q", "origin", "HEAD:main")
	f.source = strings.TrimSpace(f.git(f.repo, "rev-parse", "HEAD"))
}
func (f *integrationFixture) aur(name string) string  { return filepath.Join(f.dir, name+".git") }
func (f *integrationFixture) seed(name string) string { return filepath.Join(f.dir, "seed-"+name) }
func (f *integrationFixture) createAUR(name string) {
	remote, seed := f.aur(name), f.seed(name)
	f.git(f.dir, "init", "--bare", "-q", remote)
	f.git(f.dir, "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/master")
	f.git(f.dir, "init", "-q", "-b", "master", seed)
	f.identity(seed)
	for _, p := range f.packages {
		if p.Name == name {
			for _, file := range p.ManagedFiles() {
				copyFixtureTree(f.t, filepath.Join(f.recipeDir(name), file), filepath.Join(seed, file))
			}
		}
	}
	f.git(seed, "add", ".")
	f.git(seed, "commit", "-q", "-m", "AUR baseline")
	f.git(seed, "remote", "add", "origin", remote)
	f.git(seed, "push", "-q", "origin", "HEAD:master")
	f.mapping(remote, "https://aur.archlinux.org/"+name+".git")
	f.mapping(remote, "ssh://aur@aur.archlinux.org/"+name+".git")
}
func (f *integrationFixture) commitAUR(name string) {
	seed := f.seed(name)
	f.git(seed, "add", "-A")
	f.git(seed, "commit", "-q", "-m", "AUR fixture update")
	f.git(seed, "push", "-q", "origin", "HEAD:master")
}
func (f *integrationFixture) service() *Service {
	f.t.Helper()
	s, err := New(f.repo, f.runner, f.client, &f.stdout, &f.stderr)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *integrationFixture) candidate(name string) string {
	return filepath.Join(f.dir, "candidate-"+name)
}
func (f *integrationFixture) prepare(name string) PrepareResult {
	f.t.Helper()
	result, err := f.service().Prepare(context.Background(), name, f.candidate(name), PrepareOptions{SourceCommit: f.source})
	if err != nil {
		f.t.Fatalf("prepare: %v\n%s", err, f.stderr.String())
	}
	return result
}
func (f *integrationFixture) options() PublishOptions {
	return PublishOptions{SourceCommit: f.source, TargetBranch: "main", AURSSHPrivateKey: f.key, GitHubToken: fixtureToken}
}
func (f *integrationFixture) publish(name string) {
	f.t.Helper()
	if err := f.service().Publish(context.Background(), name, f.candidate(name), f.options()); err != nil {
		f.t.Fatalf("publish: %v\n%s", err, f.stderr.String())
	}
}
func (f *integrationFixture) head(remote, branch string) string {
	return strings.TrimSpace(f.git(f.dir, "--git-dir="+remote, "rev-parse", branch))
}
func (f *integrationFixture) remoteFile(remote, ref, path string) string {
	return f.git(f.dir, "--git-dir="+remote, "show", ref+":"+path)
}
func (f *integrationFixture) assertRemoteFile(remote, ref, path, want string) {
	f.t.Helper()
	if got := f.remoteFile(remote, ref, path); got != want {
		f.t.Fatalf("%s %s:%s = %q, want %q", remote, ref, path, got, want)
	}
}
func (f *integrationFixture) assertVersion(remote, ref, prefix, version string) {
	f.t.Helper()
	if got := f.remoteFile(remote, ref, prefix+"PKGBUILD"); !strings.Contains(got, "pkgver="+version+"\n") {
		f.t.Fatalf("remote recipe lacks version %s: %s", version, got)
	}
}
func (f *integrationFixture) clone(remote, label string) string {
	path := filepath.Join(f.dir, label)
	f.git(f.dir, "clone", "-q", remote, path)
	f.identity(path)
	return path
}
func (f *integrationFixture) commitPush(dir, branch string) {
	f.git(dir, "add", "-A")
	f.git(dir, "commit", "-q", "-m", "Concurrent fixture update")
	f.git(dir, "push", "-q", "origin", "HEAD:"+branch)
}
func (f *integrationFixture) build(name, label string) error {
	return f.service().Build(context.Background(), name, f.candidate(name), filepath.Join(f.dir, label), f.source)
}
func (f *integrationFixture) archives() []string {
	entries, err := os.ReadDir(filepath.Join(f.dir, "artifacts"))
	if err != nil {
		f.t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".pkg.tar.") {
			paths = append(paths, filepath.Join(f.dir, "artifacts", e.Name()))
		}
	}
	return paths
}
func (f *integrationFixture) installRejectHook(remote string) {
	f.t.Helper()
	_, file, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(file), "testdata/hook/main.go")
	f.command(f.dir, f.goBin, "build", "-trimpath", "-buildvcs=false", "-o", filepath.Join(remote, "hooks/pre-receive"), src)
}
func (f *integrationFixture) assertNoSecrets(err error) {
	f.t.Helper()
	text := f.stdout.String() + f.stderr.String()
	if err != nil {
		text += err.Error()
	}
	if strings.Contains(text, fixtureToken) || strings.Contains(text, f.key) || strings.Contains(text, "BEGIN OPENSSH PRIVATE KEY") {
		f.t.Fatal("operation leaked fixture credentials")
	}
}

type runnerFunc func(context.Context, Command) error

func (r runnerFunc) Run(ctx context.Context, c Command) error { return r(ctx, c) }
func isPush(c Command) bool {
	if filepath.Base(c.Name) != "git" {
		return false
	}
	for _, a := range c.Args {
		if a == "push" {
			return true
		}
	}
	return false
}
func commandDirectory(c Command) string {
	dir := c.Dir
	for i, a := range c.Args {
		if a == "-C" && i+1 < len(c.Args) {
			dir = c.Args[i+1]
		}
	}
	return dir
}
func (f *integrationFixture) pushRemote(c Command) string {
	if !isPush(c) {
		return ""
	}
	for _, a := range c.Args {
		if strings.HasPrefix(a, "ssh://") || strings.HasPrefix(a, "https://") || a == f.github || a == f.aur(fixtureName) {
			return a
		}
	}
	return strings.TrimSpace(f.git(commandDirectory(c), "remote", "get-url", "origin"))
}
func (f *integrationFixture) aurPush(c Command) bool {
	remote := f.pushRemote(c)
	return remote == f.aur(fixtureName) || strings.Contains(remote, "aur.archlinux.org/"+fixtureName+".git")
}
func (f *integrationFixture) githubPush(c Command) bool {
	remote := f.pushRemote(c)
	return remote == f.github || remote == "https://github.com/fixture/maintained.git"
}
func candidateDigests(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type().IsRegular() {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			result[rel] = digest(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func mutateManifest(t *testing.T, dir string, fn func(map[string]json.RawMessage)) {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(mustRead(t, path), &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, b, 0644)
}

func newAbortServer(t *testing.T, requests *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); panic(http.ErrAbortHandler) }))
	t.Cleanup(server.Close)
	return server
}
func parseFixtureURL(raw string) (*url.URL, error) { return url.Parse(raw) }
