//go:build integration

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationConcurrentTwoPackages(t *testing.T) {
	f := newIntegrationFixture(t, 2)
	f.tag = "v1.10.0"
	f.prepare(fixtureName)
	f.prepare("other-bin")
	f.publish(fixtureName)
	f.publish("other-bin")
	for _, name := range []string{fixtureName, "other-bin"} {
		f.assertVersion(f.github, "main", "packages/"+name+"/", "1.10.0")
		f.assertVersion(f.aur(name), "master", "", "1.10.0")
	}
}

// Each injected update is an actual competing commit and push. The decorated
// Runner then executes the unmodified maintenance push and records the real
// receive-pack rejection; a successful final tree alone would miss fake retries.
func TestIntegrationConcurrentPushes(t *testing.T) {
	for _, target := range []string{"github", "aur"} {
		t.Run(target, func(t *testing.T) {
			f := newIntegrationFixture(t, 2)
			f.tag = "v1.10.0"
			f.prepare(fixtureName)
			remote, branch := f.github, "main"
			if target == "aur" {
				remote, branch = f.aur(fixtureName), "master"
			}
			competitor := f.clone(remote, "competitor")
			injected, rejected, pushes := 0, 0, 0
			base := ExecRunner{Env: f.env}
			f.runner = runnerFunc(func(ctx context.Context, c Command) error {
				match := false
				if isPush(c) {
					if target == "aur" {
						match = f.aurPush(c)
					} else {
						match = f.githubPush(c)
					}
				}
				if !match {
					return base.Run(ctx, c)
				}
				pushes++
				if injected == 0 {
					injected++
					if target == "aur" {
						mustWrite(t, filepath.Join(competitor, "LICENSE"), []byte("Concurrent AUR license\n"), 0644)
					} else {
						f.editRecipe(filepath.Join(competitor, "packages/other-bin"), func(s string) string { return strings.Replace(s, "pkgrel=2", "pkgrel=3", 1) })
					}
					f.commitPush(competitor, branch)
				}
				var porcelain bytes.Buffer
				c.Stdout = io.MultiWriter(nonNilWriter(c.Stdout), &porcelain)
				err := base.Run(ctx, c)
				if err != nil && strings.Contains(porcelain.String(), "[rejected]") {
					rejected++
				}
				return err
			})
			f.publish(fixtureName)
			if injected != 1 || rejected != 1 || pushes != 2 {
				t.Fatalf("race was not exercised: injected=%d rejections=%d pushes=%d\n%s", injected, rejected, pushes, f.stderr.String())
			}
			f.assertVersion(f.github, "main", "packages/"+fixtureName+"/", "1.10.0")
			f.assertVersion(f.aur(fixtureName), "master", "", "1.10.0")
			if target == "aur" {
				f.assertRemoteFile(remote, branch, "LICENSE", "Concurrent AUR license\n")
			} else {
				if got := f.remoteFile(remote, branch, "packages/other-bin/PKGBUILD"); !strings.Contains(got, "pkgrel=3\n") {
					t.Fatalf("retry lost other package update: %s", got)
				}
			}
		})
	}
}

func TestIntegrationConcurrentManagedConflict(t *testing.T) {
	f := newIntegrationFixture(t, 1)
	f.tag = "v1.10.0"
	f.prepare(fixtureName)
	competitor := f.clone(f.aur(fixtureName), "managed-conflict")
	base := ExecRunner{Env: f.env}
	pushes, rejected := 0, 0
	f.runner = runnerFunc(func(ctx context.Context, c Command) error {
		if !isPush(c) || !f.aurPush(c) {
			return base.Run(ctx, c)
		}
		pushes++
		if pushes == 1 {
			f.editRecipe(competitor, func(s string) string { return strings.Replace(s, "pkgrel=2", "pkgrel=9", 1) })
			f.commitPush(competitor, "master")
		}
		var out bytes.Buffer
		c.Stdout = io.MultiWriter(nonNilWriter(c.Stdout), &out)
		err := base.Run(ctx, c)
		if err != nil && strings.Contains(out.String(), "[rejected]") {
			rejected++
		}
		return err
	})
	err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options())
	if err == nil {
		t.Fatal("concurrent managed conflict was overwritten")
	}
	if pushes != 1 || rejected != 1 {
		t.Fatalf("conflict race was not exercised: pushes=%d rejections=%d error=%v", pushes, rejected, err)
	}
	if got := f.remoteFile(f.aur(fixtureName), "master", "PKGBUILD"); !strings.Contains(got, "pkgrel=9\n") {
		t.Fatalf("competitor recipe lost: %s", got)
	}
	if f.head(f.github, "main") != f.source {
		t.Fatal("AUR conflict changed GitHub")
	}
}

func TestIntegrationConcurrentRetryLimit(t *testing.T) {
	for _, target := range []string{"aur", "github"} {
		t.Run(target, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			f.tag = "v1.10.0"
			f.prepare(fixtureName)
			remote, branch := f.github, "main"
			if target == "aur" {
				remote, branch = f.aur(fixtureName), "master"
			}
			competitor := f.clone(remote, "continuous-competitor")
			base := ExecRunner{Env: f.env}
			pushes, rejected := 0, 0
			f.runner = runnerFunc(func(ctx context.Context, c Command) error {
				match := false
				if isPush(c) {
					if target == "aur" {
						match = f.aurPush(c)
					} else {
						match = f.githubPush(c)
					}
				}
				if !match {
					return base.Run(ctx, c)
				}
				pushes++
				mustWrite(t, filepath.Join(competitor, fmt.Sprintf("unmanaged-race-%d", pushes)), []byte("Concurrent commit\n"), 0644)
				f.commitPush(competitor, branch)
				var out bytes.Buffer
				c.Stdout = io.MultiWriter(nonNilWriter(c.Stdout), &out)
				err := base.Run(ctx, c)
				if err != nil && strings.Contains(out.String(), "[rejected]") {
					rejected++
				}
				return err
			})
			err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options())
			if err == nil {
				t.Fatal("continuous non-fast-forward updates succeeded")
			}
			if pushes != 3 || rejected != 3 {
				t.Fatalf("expected three real refused pushes: pushes=%d rejected=%d error=%v", pushes, rejected, err)
			}
			for i := 1; i <= 3; i++ {
				f.assertRemoteFile(remote, branch, fmt.Sprintf("unmanaged-race-%d", i), "Concurrent commit\n")
			}
			f.assertVersion(f.github, "main", "packages/"+fixtureName+"/", "1.9.0")
			var partial *PublishError
			if target == "github" {
				if !errors.As(err, &partial) || !partial.AURSynced || partial.Stage != "concurrent pushes" {
					t.Fatalf("lost partial state: %v", err)
				}
				f.assertVersion(f.aur(fixtureName), "master", "", "1.10.0")
			} else {
				f.assertVersion(f.aur(fixtureName), "master", "", "1.9.0")
			}
		})
	}
}

func TestIntegrationConcurrentNonRetryablePushes(t *testing.T) {
	for _, target := range []string{"aur", "github"} {
		for _, kind := range []string{"receive hook rejection", "transport failure"} {
			t.Run(target+" "+kind, func(t *testing.T) {
				f := newIntegrationFixture(t, 1)
				f.tag = "v1.10.0"
				f.prepare(fixtureName)
				remote := f.github
				if target == "aur" {
					remote = f.aur(fixtureName)
				}
				if kind == "receive hook rejection" {
					f.installRejectHook(remote)
				}
				base := ExecRunner{Env: f.env}
				pushes := 0
				f.runner = runnerFunc(func(ctx context.Context, c Command) error {
					match := false
					if isPush(c) {
						if target == "aur" {
							match = f.aurPush(c)
						} else {
							match = f.githubPush(c)
						}
					}
					if !match {
						return base.Run(ctx, c)
					}
					pushes++
					if kind == "transport failure" {
						f.git(commandDirectory(c), "remote", "set-url", "--push", "origin", filepath.Join(f.dir, "missing-transport.git"))
					}
					return base.Run(ctx, c)
				})
				err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options())
				if err == nil {
					t.Fatal("refused push succeeded")
				}
				if pushes != 1 {
					t.Fatalf("ordinary failure retried %d pushes: %v", pushes, err)
				}
				if f.head(f.github, "main") != f.source {
					t.Fatal("refused GitHub push changed target")
				}
				f.assertVersion(f.aur(fixtureName), "master", "", map[bool]string{true: "1.9.0", false: "1.10.0"}[target == "aur"])
				f.assertNoSecrets(err)
			})
		}
	}
}

func TestIntegrationPublishRecovery(t *testing.T) {
	f := newIntegrationFixture(t, 1)
	f.tag = "v1.10.0"
	f.prepare(fixtureName)
	f.installRejectHook(f.github)
	err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options())
	var partial *PublishError
	if !errors.As(err, &partial) || !partial.AURSynced || partial.Stage != "push" || !strings.Contains(err.Error(), "AUR synchronized; GitHub synchronization failed: push") {
		t.Fatalf("missing explicit partial publication state: %v", err)
	}
	aurHead := f.head(f.aur(fixtureName), "master")
	f.assertVersion(f.aur(fixtureName), "master", "", "1.10.0")
	if f.head(f.github, "main") != f.source {
		t.Fatal("rejected GitHub write changed HEAD")
	}
	f.assertNoSecrets(err)
	mustRemove(t, filepath.Join(f.github, "hooks/pre-receive"))
	// Recovery uses a freshly prepared candidate and a real build, just as the
	// operational recovery procedure requires, rather than reusing stale state.
	if err := os.Rename(f.candidate(fixtureName), filepath.Join(f.dir, "previous-candidate")); err != nil {
		t.Fatal(err)
	}
	result := f.prepare(fixtureName)
	if !result.NeedsBuild || !result.NeedsPublish {
		t.Fatalf("recovery prepare=%+v", result)
	}
	if err := f.build(fixtureName, "recovery-build"); err != nil {
		t.Fatalf("recovery real build: %v\n%s", err, f.stderr.String())
	}
	f.publish(fixtureName)
	f.assertVersion(f.github, "main", "packages/"+fixtureName+"/", "1.10.0")
	if f.head(f.aur(fixtureName), "master") != aurHead {
		t.Fatal("recovery created an unnecessary AUR commit")
	}
}

func TestIntegrationPublishIsolationCandidateCode(t *testing.T) {
	f := newIntegrationFixture(t, 1)
	f.tag = "v1.10.0"
	marker := filepath.Join(f.dir, "recipe-executed")
	f.editRecipe(f.recipeDir(fixtureName), func(s string) string { return s + "\nprintf 'ran\\n' > " + shellQuote(marker) + "\n" })
	f.commitSource()
	mustRemove(t, marker)
	f.prepare(fixtureName)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("prepare did not execute trusted recipe: %v", err)
	}
	mustRemove(t, marker)
	f.publish(fixtureName)
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publish executed candidate code: %v", err)
	}
	f.assertNoSecrets(nil)
}

func TestIntegrationPublishIsolationSymlinkTargets(t *testing.T) {
	for _, kind := range []string{"managed parent", "managed target"} {
		t.Run(kind, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			f.tag = "v1.10.0"
			extra := "hooks/fixture.install"
			if kind == "managed target" {
				extra = "fixture.install"
			}
			mustWrite(t, filepath.Join(f.recipeDir(fixtureName), extra), []byte(fixtureInstall), 0644)
			f.packages[0].AURExtraFiles = append(f.packages[0].AURExtraFiles, extra)
			f.writeConfig()
			f.commitSource()
			// This symlink is untracked by the managed-file registry and remains in the
			// AUR tree. Blob validation cannot prove that copying through it is safe.
			outside := filepath.Join(f.dir, "outside target")
			mustMkdir(t, outside)
			sentinel := filepath.Join(outside, "fixture.install")
			mustWrite(t, sentinel, []byte("Do not overwrite\n"), 0644)
			link := filepath.Join(f.seed(fixtureName), "hooks")
			linkTo := outside
			if kind == "managed target" {
				link = filepath.Join(f.seed(fixtureName), extra)
				linkTo = sentinel
			}
			if err := os.Symlink(linkTo, link); err != nil {
				t.Fatal(err)
			}
			f.commitAUR(fixtureName)
			aurBefore := f.head(f.aur(fixtureName), "master")
			// A managed target that is itself a symlink is rejected during baseline
			// reading; a symlink parent reaches the copy boundary after preparation.
			result, err := f.service().Prepare(context.Background(), fixtureName, f.candidate(fixtureName), PrepareOptions{SourceCommit: f.source})
			if kind == "managed target" {
				if err == nil {
					t.Fatalf("symlink managed blob accepted: %+v", result)
				}
			} else {
				if err != nil {
					t.Fatalf("prepare with unmanaged symlink parent: %v", err)
				}
				if err = f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options()); err == nil {
					t.Fatal("AUR copy followed unmanaged symlink parent")
				}
			}
			if got := string(mustRead(t, sentinel)); got != "Do not overwrite\n" {
				t.Fatalf("copy escaped clone: %q", got)
			}
			if f.head(f.aur(fixtureName), "master") != aurBefore || f.head(f.github, "main") != f.source {
				t.Fatal("symlink rejection changed remote refs")
			}
		})
	}
}

func TestIntegrationPublishIsolationKnownHosts(t *testing.T) {
	for _, kind := range []string{"file symlink", "parent symlink", "wrong hostname", "multiple records", "wrong key type"} {
		t.Run(kind, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			f.tag = "v1.10.0"
			f.prepare(fixtureName)
			aurBefore := f.head(f.aur(fixtureName), "master")
			path := filepath.Join(f.repo, ".github/aur_known_hosts")
			original := mustRead(t, path)
			switch kind {
			case "file symlink":
				target := filepath.Join(f.dir, "known hosts")
				mustWrite(t, target, original, 0600)
				mustRemove(t, path)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				target := filepath.Join(f.dir, "github config")
				if err := os.Rename(filepath.Join(f.repo, ".github"), target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(f.repo, ".github")); err != nil {
					t.Fatal(err)
				}
			case "wrong hostname":
				mustWrite(t, path, []byte(strings.Replace(string(original), "aur.archlinux.org", "example.invalid", 1)), 0600)
			case "multiple records":
				mustWrite(t, path, append(append([]byte(nil), original...), original...), 0600)
			case "wrong key type":
				mustWrite(t, path, []byte(strings.Replace(string(original), "ssh-ed25519", "ssh-rsa", 1)), 0600)
			}
			err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options())
			if err == nil {
				t.Fatal("unsafe known_hosts accepted")
			}
			if f.head(f.aur(fixtureName), "master") != aurBefore || f.head(f.github, "main") != f.source {
				t.Fatal("known_hosts rejection changed remote refs")
			}
			f.assertNoSecrets(err)
		})
	}
}

func TestIntegrationPublishIsolationCredentials(t *testing.T) {
	t.Run("synchronized requires no credentials", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.prepare(fixtureName)
		aur := f.head(f.aur(fixtureName), "master")
		opts := f.options()
		opts.AURSSHPrivateKey = ""
		opts.GitHubToken = ""
		if err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), opts); err != nil {
			t.Fatalf("no-op required credentials: %v", err)
		}
		if f.head(f.aur(fixtureName), "master") != aur || f.head(f.github, "main") != f.source {
			t.Fatal("no-op changed refs")
		}
	})
	t.Run("missing AUR key before writes", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.tag = "v1.10.0"
		f.prepare(fixtureName)
		aur := f.head(f.aur(fixtureName), "master")
		opts := f.options()
		opts.AURSSHPrivateKey = ""
		if err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), opts); err == nil {
			t.Fatal("AUR write accepted missing key")
		}
		if f.head(f.aur(fixtureName), "master") != aur || f.head(f.github, "main") != f.source {
			t.Fatal("missing key changed refs")
		}
	})
	t.Run("missing GitHub token retains partial state", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.tag = "v1.10.0"
		f.prepare(fixtureName)
		opts := f.options()
		opts.GitHubToken = ""
		err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), opts)
		var partial *PublishError
		if !errors.As(err, &partial) || !partial.AURSynced || partial.Stage != "credentials" || !strings.Contains(err.Error(), "AUR synchronized; GitHub synchronization failed: credentials") {
			t.Fatalf("lost credential failure partial state: %v", err)
		}
		f.assertVersion(f.aur(fixtureName), "master", "", "1.10.0")
		if f.head(f.github, "main") != f.source {
			t.Fatal("missing token changed GitHub")
		}
		f.assertNoSecrets(err)
		// Once AUR is synchronized, recovery needs only the GitHub credential.
		opts = f.options()
		opts.AURSSHPrivateKey = ""
		if err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), opts); err != nil {
			t.Fatalf("AUR no-op demanded a key during GitHub recovery: %v", err)
		}
		f.assertVersion(f.github, "main", "packages/"+fixtureName+"/", "1.10.0")
	})
}

func nonNilWriter(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
