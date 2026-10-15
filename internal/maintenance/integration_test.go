//go:build integration

package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func TestIntegrationProcessEnvironmentAndPATH(t *testing.T) {
	integrationPrerequisites(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin with spaces")
	mustMkdir(t, bin)
	copyFixtureTree(t, "/usr/bin/printenv", filepath.Join(bin, "fixture-tool"))
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	originalEnv := os.Environ()
	runner := ExecRunner{Env: []string{"PATH=/does-not-exist", "FIXTURE_VALUE=base"}}
	var out, diagnostics bytes.Buffer
	err = runner.Run(context.Background(), Command{Name: "fixture-tool", Args: []string{"FIXTURE_VALUE"}, Dir: dir, Env: []string{"PATH=bin with spaces", "FIXTURE_VALUE=effective"}, Stdout: &out, Stderr: &diagnostics})
	if err != nil || out.String() != "effective\n" {
		t.Fatalf("effective PATH/environment: output=%q error=%v diagnostics=%s", out.String(), err, diagnostics.String())
	}
	out.Reset()
	err = runner.Run(context.Background(), Command{Name: "bin with spaces/fixture-tool", Args: []string{"FIXTURE_VALUE"}, Dir: dir, Stdout: &out, Stderr: &diagnostics})
	if err != nil || out.String() != "base\n" {
		t.Fatalf("relative executable: %q %v", out.String(), err)
	}
	mustWrite(t, filepath.Join(bin, "not-executable"), []byte("data"), 0644)
	mustMkdir(t, filepath.Join(bin, "directory"))
	for _, name := range []string{"absent", "not-executable", "directory"} {
		if err := runner.Run(context.Background(), Command{Name: name, Dir: dir, Env: []string{"PATH=" + bin}, Stdout: &out, Stderr: &diagnostics}); err == nil {
			t.Fatalf("executed %s", name)
		}
	}
	err = runner.Run(context.Background(), Command{Name: "/usr/bin/false", Dir: dir, Stdout: &out, Stderr: &diagnostics})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("lost real exit status: %v", err)
	}
	out.Reset()
	err = runner.Run(context.Background(), Command{Name: "/usr/bin/seq", Args: []string{"1", "20000"}, Dir: dir, Stdout: &out, Stderr: &diagnostics})
	if err != nil || !strings.HasPrefix(out.String(), "1\n2\n") || !strings.HasSuffix(out.String(), "20000\n") || out.Len() < 100000 {
		t.Fatalf("long output was not streamed intact: bytes=%d error=%v", out.Len(), err)
	}
	now, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if now != cwd || !reflect.DeepEqual(originalEnv, os.Environ()) {
		t.Fatal("runner modified process cwd or environment")
	}
}

func TestIntegrationPrepareVersions(t *testing.T) {
	for _, tc := range []struct {
		name, tag, extra, want string
		build, publish, force  bool
	}{
		{name: "synchronized", tag: "v1.9.0", want: "1.9.0-2"},
		{name: "numeric upgrade", tag: "v1.10.0", want: "1.10.0-1", build: true, publish: true},
		{name: "older tag", tag: "v1.8.0", want: "1.9.0-2"},
		{name: "epoch text preserved", tag: "v1.10.0", extra: "epoch=01\n", want: "01:1.10.0-1", build: true, publish: true},
		{name: "forced synchronized", tag: "v1.9.0", want: "1.9.0-2", build: true, force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			f.tag = tc.tag
			if tc.extra != "" {
				f.editRecipe(f.recipeDir(fixtureName), func(s string) string { return s + tc.extra })
				f.commitSource()
			}
			result, err := f.service().Prepare(context.Background(), fixtureName, f.candidate(fixtureName), PrepareOptions{SourceCommit: f.source, ForceBuild: tc.force})
			if err != nil {
				t.Fatalf("prepare: %v\n%s", err, f.stderr.String())
			}
			if result.CandidateVersion != tc.want || result.NeedsBuild != tc.build || result.NeedsPublish != tc.publish {
				t.Fatalf("result=%+v, want version=%s build=%v publish=%v", result, tc.want, tc.build, tc.publish)
			}
			if f.requests.Load() != 1 {
				t.Fatalf("upstream requests=%d", f.requests.Load())
			}
			var manifest Manifest
			if err := json.Unmarshal(mustRead(t, filepath.Join(f.candidate(fixtureName), "manifest.json")), &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Version != tc.want || manifest.SourceCommit != f.source {
				t.Fatalf("manifest=%+v", manifest)
			}
		})
	}
}

func TestIntegrationPrepareUsesCommittedRecipe(t *testing.T) {
	f := newIntegrationFixture(t, 1)
	f.tag = "v1.10.0"
	f.editRecipe(f.recipeDir(fixtureName), func(s string) string {
		return strings.ReplaceAll(s, "Local package maintenance fixture", "UNCOMMITTED UNTRUSTED RECIPE")
	})
	f.prepare(fixtureName)
	got := string(mustRead(t, filepath.Join(f.candidate(fixtureName), "package/PKGBUILD")))
	if strings.Contains(got, "UNCOMMITTED") || !strings.Contains(got, "Local package maintenance fixture") {
		t.Fatalf("candidate used worktree changes: %s", got)
	}
	_, err := f.service().Prepare(context.Background(), fixtureName, filepath.Join(f.dir, "wrong-source"), PrepareOptions{SourceCommit: strings.Repeat("0", 40)})
	if err == nil {
		t.Fatal("accepted mismatched SOURCE_COMMIT")
	}
}

func TestIntegrationUpstreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name, tag, response string
		status              int
	}{
		{name: "wrong prefix", tag: "release-1.10.0", status: 200},
		{name: "non numeric", tag: "v1.10beta", status: 200},
		{name: "missing tag", response: "{}", status: 200},
		{name: "null tag", response: `{"tag_name":null}`, status: 200},
		{name: "numeric tag", response: `{"tag_name":110}`, status: 200},
		{name: "malformed JSON", response: "{", status: 200},
		{name: "not found", status: 404},
		{name: "server error", status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			f.tag = tc.tag
			f.response = tc.response
			f.status = tc.status
			_, err := f.service().Prepare(context.Background(), fixtureName, f.candidate(fixtureName), PrepareOptions{SourceCommit: f.source})
			if err == nil {
				t.Fatal("invalid upstream response succeeded")
			}
			if f.requests.Load() == 0 {
				t.Fatalf("failure occurred before HTTP request: %v", err)
			}
			if f.head(f.github, "main") != f.source {
				t.Fatal("failed prepare changed GitHub")
			}
		})
	}
	t.Run("connection interrupted after request", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		server := newAbortServer(t, &f.requests)
		base, err := parseFixtureURL(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		f.client = &http.Client{Transport: fixtureTransport{base: base, transport: server.Client().Transport}}
		_, err = f.service().Prepare(context.Background(), fixtureName, f.candidate(fixtureName), PrepareOptions{SourceCommit: f.source})
		if err == nil || f.requests.Load() == 0 {
			t.Fatalf("network failure was not exercised: requests=%d error=%v", f.requests.Load(), err)
		}
	})
}

func TestIntegrationAURBaseline(t *testing.T) {
	t.Run("empty first publish", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.git(f.dir, "--git-dir="+f.aur(fixtureName), "update-ref", "-d", "refs/heads/master")
		result := f.prepare(fixtureName)
		if !result.NeedsBuild || !result.NeedsPublish {
			t.Fatalf("empty AUR result=%+v", result)
		}
		f.publish(fixtureName)
		f.assertRemoteFile(f.aur(fixtureName), "master", "fixture-source", fixturePayload)
		if f.head(f.github, "main") != f.source {
			t.Fatal("first AUR publish changed GitHub")
		}
	})
	for _, name := range []string{"missing remote", "unreadable remote", "other refs without master", "missing metadata", "AUR ahead", "same version changed recipe", "nonregular blob"} {
		t.Run(name, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			seed := f.seed(fixtureName)
			switch name {
			case "missing remote":
				if err := os.Rename(f.aur(fixtureName), f.aur(fixtureName)+"-absent"); err != nil {
					t.Fatal(err)
				}
			case "unreadable remote":
				if err := os.Chmod(f.aur(fixtureName), 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(f.aur(fixtureName), 0755) })
			case "other refs without master":
				f.git(f.dir, "--git-dir="+f.aur(fixtureName), "update-ref", "refs/heads/other", f.head(f.aur(fixtureName), "master"))
				f.git(f.dir, "--git-dir="+f.aur(fixtureName), "update-ref", "-d", "refs/heads/master")
			case "missing metadata":
				mustRemove(t, filepath.Join(seed, ".SRCINFO"))
				f.commitAUR(fixtureName)
			case "AUR ahead":
				f.editRecipe(seed, func(s string) string { return strings.Replace(s, "pkgrel=2", "pkgrel=9", 1) })
				f.commitAUR(fixtureName)
			case "same version changed recipe":
				f.editRecipe(seed, func(s string) string {
					return strings.Replace(s, "Local package maintenance fixture", "Changed recipe at identical version", 1)
				})
				f.commitAUR(fixtureName)
			case "nonregular blob":
				mustRemove(t, filepath.Join(seed, ".SRCINFO"))
				if err := os.Symlink("PKGBUILD", filepath.Join(seed, ".SRCINFO")); err != nil {
					t.Fatal(err)
				}
				f.commitAUR(fixtureName)
			}
			_, err := f.service().Prepare(context.Background(), fixtureName, f.candidate(fixtureName), PrepareOptions{SourceCommit: f.source})
			if err == nil {
				t.Fatal("invalid AUR baseline accepted")
			}
			if name == "same version changed recipe" && !strings.Contains(err.Error(), "pkgrel") {
				t.Fatalf("missing pkgrel diagnostic: %v", err)
			}
		})
	}
	t.Run("stale metadata only", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		p := filepath.Join(f.seed(fixtureName), ".SRCINFO")
		mustWrite(t, p, append(mustRead(t, p), []byte("\toptdepends = optional-fixture-package\n")...), 0644)
		f.commitAUR(fixtureName)
		result := f.prepare(fixtureName)
		if !result.NeedsBuild || !result.NeedsPublish {
			t.Fatalf("stale metadata result=%+v", result)
		}
		f.publish(fixtureName)
		f.assertRemoteFile(f.aur(fixtureName), "master", ".SRCINFO", string(mustRead(t, filepath.Join(f.recipeDir(fixtureName), ".SRCINFO"))))
		if f.head(f.github, "main") != f.source {
			t.Fatal("metadata-only AUR repair changed GitHub")
		}
	})
}

func TestIntegrationCandidateRejectsBeforeSideEffects(t *testing.T) {
	f := newIntegrationFixture(t, 1)
	f.tag = "v1.10.0"
	f.prepare(fixtureName)
	aurBefore := f.head(f.aur(fixtureName), "master")
	githubBefore := f.head(f.github, "main")
	tests := []struct {
		name   string
		mutate func(string)
	}{
		{"tampered bytes", func(d string) {
			p := filepath.Join(d, "package/PKGBUILD")
			mustWrite(t, p, append(mustRead(t, p), '\n'), 0644)
		}},
		{"missing file", func(d string) { mustRemove(t, filepath.Join(d, "package/.SRCINFO")) }},
		{"extra file", func(d string) { mustWrite(t, filepath.Join(d, "package/unlisted"), []byte("extra"), 0644) }},
		{"extra directory", func(d string) { mustMkdir(t, filepath.Join(d, "package/unused")) }},
		{"outside package directory", func(d string) { mustMkdir(t, filepath.Join(d, "surprise")) }},
		{"symlink", func(d string) {
			p := filepath.Join(d, "package/.SRCINFO")
			mustRemove(t, p)
			if err := os.Symlink(filepath.Join(f.recipeDir(fixtureName), ".SRCINFO"), p); err != nil {
				t.Fatal(err)
			}
		}},
		{"FIFO", func(d string) {
			if err := syscall.Mkfifo(filepath.Join(d, "package/fifo"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra manifest key", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) { m["unexpected"] = json.RawMessage(`true`) })
		}},
		{"invalid file digest", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) { m["files"] = json.RawMessage(`{"PKGBUILD":"bad"}`) })
		}},
		{"missing file key", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) {
				var files map[string]string
				_ = json.Unmarshal(m["files"], &files)
				delete(files, ".SRCINFO")
				m["files"], _ = json.Marshal(files)
			})
		}},
		{"extra AUR key", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) {
				var files map[string]*string
				_ = json.Unmarshal(m["aur_files"], &files)
				files["surprise"] = nil
				m["aur_files"], _ = json.Marshal(files)
			})
		}},
		{"invalid AUR digest", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) {
				var files map[string]*string
				_ = json.Unmarshal(m["aur_files"], &files)
				bad := "bad"
				files["PKGBUILD"] = &bad
				m["aur_files"], _ = json.Marshal(files)
			})
		}},
		{"wrong package", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) { m["package"] = json.RawMessage(`"other-bin"`) })
		}},
		{"wrong source", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) { m["source_commit"], _ = json.Marshal(strings.Repeat("0", 40)) })
		}},
		{"invalid source", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) { m["source_commit"] = json.RawMessage(`"UPPERCASE"`) })
		}},
		{"wrong version", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) { m["version"] = json.RawMessage(`"9.0-1"`) })
		}},
		{"invalid AUR commit", func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) { m["aur_commit"] = json.RawMessage(`"not-a-commit"`) })
		}},
	}
	for _, key := range []string{"package", "source_commit", "version", "aur_commit", "files", "aur_files"} {
		key := key
		tests = append(tests, struct {
			name   string
			mutate func(string)
		}{"missing " + key, func(d string) { mutateManifest(t, d, func(m map[string]json.RawMessage) { delete(m, key) }) }})
	}
	for _, key := range []string{"package", "source_commit", "version", "files", "aur_files"} {
		key := key
		tests = append(tests, struct {
			name   string
			mutate func(string)
		}{"null " + key, func(d string) {
			mutateManifest(t, d, func(m map[string]json.RawMessage) { m[key] = json.RawMessage(`null`) })
		}})
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(f.dir, fmt.Sprintf("mutation-%d", i))
			copyFixtureTree(t, f.candidate(fixtureName), dir)
			tc.mutate(dir)
			calls := 0
			base := ExecRunner{Env: f.env}
			f.runner = runnerFunc(func(ctx context.Context, c Command) error {
				if isPush(c) || filepath.Base(c.Name) == "makepkg" || filepath.Base(c.Name) == "pacman" {
					calls++
					return fmt.Errorf("unexpected side effect: %s", c.Name)
				}
				return base.Run(ctx, c)
			})
			s := f.service()
			for _, op := range []struct {
				name string
				run  func() error
			}{
				{"install", func() error { return s.InstallBuildDeps(context.Background(), fixtureName, dir, f.source, "x86_64") }},
				{"build", func() error {
					return s.Build(context.Background(), fixtureName, dir, filepath.Join(f.dir, fmt.Sprintf("bad-build-%d", i)), f.source)
				}},
				{"publish", func() error { return s.Publish(context.Background(), fixtureName, dir, f.options()) }},
			} {
				if err := op.run(); err == nil {
					t.Fatalf("%s accepted mutation", op.name)
				}
			}
			if calls != 0 {
				t.Fatalf("invalid candidate reached %d side effects", calls)
			}
			if f.head(f.aur(fixtureName), "master") != aurBefore || f.head(f.github, "main") != githubBefore {
				t.Fatal("invalid candidate changed remote refs")
			}
		})
	}
	f.runner = ExecRunner{Env: f.env}
	t.Run("candidate root symlink", func(t *testing.T) {
		path := filepath.Join(f.dir, "candidate-link")
		if err := os.Symlink(f.candidate(fixtureName), path); err != nil {
			t.Fatal(err)
		}
		if err := f.service().Publish(context.Background(), fixtureName, path, f.options()); err == nil {
			t.Fatal("accepted candidate root symlink")
		}
	})
	t.Run("required source and checkout", func(t *testing.T) {
		s := f.service()
		dir := f.candidate(fixtureName)
		if err := s.InstallBuildDeps(context.Background(), fixtureName, dir, "", "x86_64"); err == nil {
			t.Fatal("install accepted empty source")
		}
		if err := s.Build(context.Background(), fixtureName, dir, filepath.Join(f.dir, "empty-source-build"), ""); err == nil {
			t.Fatal("build accepted empty source")
		}
		opts := f.options()
		opts.SourceCommit = ""
		if err := s.Publish(context.Background(), fixtureName, dir, opts); err == nil {
			t.Fatal("publish accepted empty source")
		}
		f.git(f.repo, "commit", "--allow-empty", "-q", "-m", "Different checkout HEAD")
		if err := s.Publish(context.Background(), fixtureName, dir, f.options()); err == nil {
			t.Fatal("accepted candidate from a different checkout HEAD")
		}
	})
}

func TestIntegrationDependencies(t *testing.T) {
	t.Run("common and current arch", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.editRecipe(f.recipeDir(fixtureName), func(s string) string {
			return strings.Replace(strings.Replace(s, "pkgrel=2", "pkgrel=3", 1), "arch=('any')", "arch=('x86_64' 'aarch64')", 1) + "\ndepends=('common-one>=2' 'duplicate=3')\nmakedepends=('make-one<9' 'duplicate')\ncheckdepends=('check-one>1')\ndepends_x86_64=('arch-one<=2' 'common-one')\nmakedepends_x86_64=('arch-make=1')\ncheckdepends_x86_64=('arch-check>=1')\ndepends_aarch64=('wrong-arch')\n"
		})
		f.commitSource()
		f.prepare(fixtureName)
		// Capture only the privileged installation boundary; all metadata and Git
		// commands, and makepkg's dependency checks in other tests, remain real.
		var calls [][]string
		installErr := errors.New("fixture pacman installation failed")
		fail := false
		base := ExecRunner{Env: f.env}
		f.runner = runnerFunc(func(ctx context.Context, c Command) error {
			if filepath.Base(c.Name) == "pacman" {
				calls = append(calls, append([]string(nil), c.Args...))
				if fail {
					return installErr
				}
				return nil
			}
			return base.Run(ctx, c)
		})
		s := f.service()
		if err := s.InstallBuildDeps(context.Background(), fixtureName, f.candidate(fixtureName), f.source, "x86_64"); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || len(calls[0]) < 3 || !reflect.DeepEqual(calls[0][:3], []string{"-S", "--noconfirm", "--needed"}) {
			t.Fatalf("unexpected installation command: %q", calls)
		}
		// Ordering is covered against explicit metadata in TestMetadataDependencies;
		// this real makepkg case checks the selected names without reimplementing parsing.
		got := append([]string(nil), calls[0][3:]...)
		sort.Strings(got)
		want := []string{"arch-check", "arch-make", "arch-one", "check-one", "common-one", "duplicate", "make-one"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("installed dependencies=%q want=%q", got, want)
		}
		fail = true
		if err := s.InstallBuildDeps(context.Background(), fixtureName, f.candidate(fixtureName), f.source, "x86_64"); !errors.Is(err, installErr) {
			t.Fatalf("installation failure did not propagate: %v", err)
		}
	})
	t.Run("empty avoids installation", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.prepare(fixtureName)
		calls := 0
		base := ExecRunner{Env: f.env}
		f.runner = runnerFunc(func(ctx context.Context, c Command) error {
			if filepath.Base(c.Name) == "pacman" {
				calls++
				return errors.New("must not install")
			}
			return base.Run(ctx, c)
		})
		if err := f.service().InstallBuildDeps(context.Background(), fixtureName, f.candidate(fixtureName), f.source, ""); err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatal("empty dependencies called pacman")
		}
	})
	t.Run("invalid dependency rejected", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.prepare(fixtureName)
		d := f.candidate(fixtureName)
		p := filepath.Join(d, "package/.SRCINFO")
		mustWrite(t, p, append(mustRead(t, p), []byte("\tdepends = invalid/dependency\n")...), 0644)
		mutateManifest(t, d, func(m map[string]json.RawMessage) {
			var files map[string]string
			_ = json.Unmarshal(m["files"], &files)
			files[".SRCINFO"] = digest(mustRead(t, p))
			m["files"], _ = json.Marshal(files)
		})
		calls := 0
		base := ExecRunner{Env: f.env}
		f.runner = runnerFunc(func(ctx context.Context, c Command) error {
			if filepath.Base(c.Name) == "pacman" {
				calls++
				return nil
			}
			return base.Run(ctx, c)
		})
		if err := f.service().InstallBuildDeps(context.Background(), fixtureName, d, f.source, "x86_64"); err == nil {
			t.Fatal("accepted unsafe dependency")
		}
		if calls != 0 {
			t.Fatal("unsafe dependency reached pacman")
		}
	})
}

func TestMakepkgSmoke(t *testing.T) {
	f := newIntegrationFixture(t, 1)
	f.tag = "v1.10.0"
	dir := f.recipeDir(fixtureName)
	mustWrite(t, filepath.Join(dir, "fixture.install"), []byte(fixtureInstall), 0644)
	f.packages[0].AURExtraFiles = append(f.packages[0].AURExtraFiles, "fixture.install")
	f.writeConfig()
	f.editRecipe(dir, func(s string) string { return s + "\ninstall=fixture.install\n" })
	f.commitSource()
	result := f.prepare(fixtureName)
	if result.CandidateVersion != "1.10.0-1" {
		t.Fatalf("upgrade result=%+v", result)
	}
	before := candidateDigests(t, f.candidate(fixtureName))
	if err := f.build(fixtureName, "build with spaces"); err != nil {
		t.Fatalf("real makepkg build: %v\n%s", err, f.stderr.String())
	}
	after := candidateDigests(t, f.candidate(fixtureName))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("build modified immutable candidate")
	}
	archives := f.archives()
	if len(archives) != 1 {
		t.Fatalf("expected one real package, got %v", archives)
	}
	if got := f.command(f.dir, "bsdtar", "-xOf", archives[0], "usr/share/"+fixtureName+"/payload"); got != fixturePayload {
		t.Fatalf("archive payload=%q", got)
	}
	if got := f.command(f.dir, "bsdtar", "-xOf", archives[0], ".INSTALL"); got != fixtureInstall {
		t.Fatalf("archive install instructions=%q", got)
	}
	f.publish(fixtureName)
	f.assertRemoteFile(f.aur(fixtureName), "master", "fixture.install", fixtureInstall)
}

func TestMakepkgMissingDependency(t *testing.T) {
	f := newIntegrationFixture(t, 1)
	f.editRecipe(f.recipeDir(fixtureName), func(s string) string {
		return strings.Replace(s, "pkgrel=2", "pkgrel=3", 1) + "\ndepends=('nonexistent-maintenance-fixture>=999')\n"
	})
	f.commitSource()
	f.prepare(fixtureName)
	if err := f.build(fixtureName, "missing-dependency-build"); err == nil {
		t.Fatal("real makepkg accepted unavailable dependency")
	}
	if got := f.archives(); len(got) != 0 {
		t.Fatalf("missing dependency produced package: %v", got)
	}
	if !strings.Contains(f.stderr.String()+f.stdout.String(), "nonexistent-maintenance-fixture") {
		t.Fatal("missing dependency did not reach real makepkg/pacman check")
	}
}
