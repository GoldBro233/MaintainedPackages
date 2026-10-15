//go:build integration

package maintenance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIntegrationExtrasLocalAndUnmanagedFiles(t *testing.T) {
	f := newIntegrationFixture(t, 1)
	f.tag = "v1.10.0"
	dir := f.recipeDir(fixtureName)
	license := "Verified local fixture license.\n"
	mustWrite(t, filepath.Join(dir, "fixture-LICENSE"), []byte(license), 0644)
	mustWrite(t, filepath.Join(dir, "fixture.install"), []byte(fixtureInstall), 0644)
	f.packages[0].AURExtraFiles = append(f.packages[0].AURExtraFiles, "fixture-LICENSE", "fixture.install")
	f.writeConfig()
	f.editRecipe(dir, func(s string) string {
		return s + "\ninstall=fixture.install\nsource+=('fixture-LICENSE')\nsha256sums+=('" + digest([]byte(license)) + "')\n"
	})
	for _, name := range []string{"src/archive", "pkg/archive", "fixture-bin-1.pkg.tar.zst"} {
		mustWrite(t, filepath.Join(dir, name), []byte("Unrelated tracked build output\n"), 0644)
	}
	f.commitSource()
	mustWrite(t, filepath.Join(f.seed(fixtureName), "LICENSE"), []byte("AUR-owned license\n"), 0644)
	f.commitAUR(fixtureName)
	f.prepare(fixtureName)
	f.publish(fixtureName)
	for name, want := range map[string]string{"fixture-LICENSE": license, "fixture.install": fixtureInstall, "fixture-source": fixturePayload, "LICENSE": "AUR-owned license\n"} {
		f.assertRemoteFile(f.aur(fixtureName), "master", name, want)
	}
	f.assertVersion(f.github, "main", "packages/"+fixtureName+"/", "1.10.0")
	assertChangedPaths(t, f, f.github, "main", []string{"packages/" + fixtureName + "/.SRCINFO", "packages/" + fixtureName + "/PKGBUILD"})
	assertChangedPaths(t, f, f.aur(fixtureName), "master", []string{".SRCINFO", "PKGBUILD", "fixture-LICENSE", "fixture.install"})
	listing := f.git(f.dir, "--git-dir="+f.aur(fixtureName), "ls-tree", "-r", "--name-only", "master")
	for _, bad := range []string{"src/archive", "pkg/archive", ".pkg.tar."} {
		if strings.Contains(listing, bad) {
			t.Fatalf("AUR contains unrelated artifacts: %s", listing)
		}
	}
	// Source extras are deliberately not part of GitHub's release writeback.
	f.assertRemoteFile(f.github, "main", "packages/"+fixtureName+"/fixture-LICENSE", license)
}

func TestIntegrationExtrasDownloadedAndChecksums(t *testing.T) {
	for _, bad := range []bool{false, true} {
		name := "verified download"
		if bad {
			name = "download checksum mismatch"
		}
		t.Run(name, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			if !bad {
				f.tag = "v1.10.0"
			}
			expected := "A verified downloaded license.\n"
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/license" {
					http.Error(w, "bad path", 404)
					return
				}
				if bad {
					_, _ = w.Write([]byte("Replaced source with invalid bytes.\n"))
				} else {
					_, _ = w.Write([]byte(expected))
				}
			}))
			t.Cleanup(server.Close)
			f.packages[0].AURExtraFiles = append(f.packages[0].AURExtraFiles, "download-LICENSE")
			f.writeConfig()
			f.editRecipe(f.recipeDir(fixtureName), func(s string) string {
				return strings.Replace(s, "pkgrel=2", "pkgrel=3", 1) + "\nsource+=('download-LICENSE::" + server.URL + "/license')\nsha256sums+=('" + digest([]byte(expected)) + "')\n"
			})
			f.commitSource()
			_, err := f.service().Prepare(context.Background(), fixtureName, f.candidate(fixtureName), PrepareOptions{SourceCommit: f.source})
			if requests.Load() == 0 {
				t.Fatalf("download did not reach loopback HTTP server: %v", err)
			}
			if bad {
				if err == nil {
					t.Fatal("bad downloaded checksum was accepted")
				}
				if f.head(f.github, "main") != f.source {
					t.Fatal("checksum failure wrote GitHub")
				}
				return
			}
			if err != nil {
				t.Fatalf("download prepare: %v\n%s", err, f.stderr.String())
			}
			if got := string(mustRead(t, filepath.Join(f.candidate(fixtureName), "package/download-LICENSE"))); got != expected {
				t.Fatalf("candidate downloaded bytes=%q", got)
			}
			f.publish(fixtureName)
			f.assertRemoteFile(f.aur(fixtureName), "master", "download-LICENSE", expected)
			assertChangedPaths(t, f, f.github, "main", []string{"packages/" + fixtureName + "/.SRCINFO", "packages/" + fixtureName + "/PKGBUILD"})
			listing := f.git(f.dir, "--git-dir="+f.github, "ls-tree", "-r", "--name-only", "main")
			if strings.Contains(listing, "download-LICENSE") {
				t.Fatal("downloaded extra leaked into GitHub writeback")
			}
		})
	}
	t.Run("local source checksum mismatch", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		mustWrite(t, filepath.Join(f.recipeDir(fixtureName), "fixture-source"), []byte("Changed source without updating checksum\n"), 0644)
		f.commitSource()
		before := f.head(f.aur(fixtureName), "master")
		if _, err := f.service().Prepare(context.Background(), fixtureName, f.candidate(fixtureName), PrepareOptions{SourceCommit: f.source}); err == nil {
			t.Fatal("bad local source checksum accepted")
		}
		if f.head(f.github, "main") != f.source || f.head(f.aur(fixtureName), "master") != before {
			t.Fatal("checksum failure changed remote refs")
		}
	})
}

func assertChangedPaths(t *testing.T, f *integrationFixture, remote, ref string, want []string) {
	t.Helper()
	out := f.git(f.dir, "--git-dir="+remote, "diff-tree", "--no-commit-id", "--name-only", "-r", ref)
	got := strings.Fields(out)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("release changed unexpected paths: got %q want %q", got, want)
	}
}

func TestIntegrationTrustInputsAndPackageConflicts(t *testing.T) {
	cases := []struct{ name, path, operation string }{
		{"add command file", "cmd/new-resource", "add"}, {"delete command file", "cmd/maintpkg/main.go", "delete"}, {"modify command file", "cmd/maintpkg/main.go", "modify"},
		{"add internal file", "internal/new-resource", "add"}, {"delete internal file", "internal/maintenance/service.go", "delete"}, {"modify internal file", "internal/maintenance/service.go", "modify"},
		{"module", "go.mod", "modify"}, {"optional sum added", "go.sum", "add"},
		{"package registry", "packages.json", "modify"}, {"host key", ".github/aur_known_hosts", "modify"},
		{"maintenance workflow", ".github/workflows/maintain-packages.yml", "modify"}, {"release workflow", ".github/workflows/release-package.yml", "modify"},
		{"same package recipe", "packages/" + fixtureName + "/PKGBUILD", "recipe"}, {"disabled package", "packages.json", "disable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			f.tag = "v1.10.0"
			f.prepare(fixtureName)
			before := f.head(f.aur(fixtureName), "master")
			clone := f.clone(f.github, "trust-change")
			path := filepath.Join(clone, tc.path)
			switch tc.operation {
			case "add":
				mustWrite(t, path, []byte("New tracked trusted input\n"), 0644)
			case "delete":
				mustRemove(t, path)
			case "modify":
				mustWrite(t, path, append(mustRead(t, path), []byte("\nChanged trusted input\n")...), 0644)
			case "recipe":
				f.editRecipe(filepath.Dir(path), func(s string) string { return strings.Replace(s, "pkgrel=2", "pkgrel=9", 1) })
			case "disable":
				var config Config
				if err := json.Unmarshal(mustRead(t, path), &config); err != nil {
					t.Fatal(err)
				}
				config.Packages[0].Enabled = false
				data, err := json.Marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				mustWrite(t, path, data, 0644)
			}
			f.commitPush(clone, "main")
			target := f.head(f.github, "main")
			err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options())
			if err == nil {
				t.Fatal("stale candidate accepted changed trusted input")
			}
			if f.head(f.aur(fixtureName), "master") != before {
				t.Fatal("trust check failed after writing AUR")
			}
			if f.head(f.github, "main") != target {
				t.Fatal("trust check overwrote changed GitHub target")
			}
		})
	}
	t.Run("required trusted input absent at source", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		mustRemove(t, filepath.Join(f.repo, "go.mod"))
		f.commitSource()
		f.tag = "v1.10.0"
		f.prepare(fixtureName)
		before := f.head(f.aur(fixtureName), "master")
		if err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options()); err == nil {
			t.Fatal("accepted missing required trust input")
		}
		if f.head(f.aur(fixtureName), "master") != before {
			t.Fatal("missing trust input wrote AUR")
		}
	})
	t.Run("nonregular trusted input", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.tag = "v1.10.0"
		f.prepare(fixtureName)
		before := f.head(f.aur(fixtureName), "master")
		clone := f.clone(f.github, "trust-symlink")
		path := filepath.Join(clone, "internal/linked-resource")
		if err := os.Symlink("maintenance/service.go", path); err != nil {
			t.Fatal(err)
		}
		f.commitPush(clone, "main")
		if err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options()); err == nil {
			t.Fatal("accepted nonregular trusted input")
		}
		if f.head(f.aur(fixtureName), "master") != before {
			t.Fatal("nonregular trust input wrote AUR")
		}
	})
	t.Run("source no longer ancestor", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.tag = "v1.10.0"
		f.prepare(fixtureName)
		before := f.head(f.aur(fixtureName), "master")
		tree := strings.TrimSpace(f.git(f.repo, "rev-parse", "HEAD^{tree}"))
		unrelated := strings.TrimSpace(f.git(f.repo, "commit-tree", tree, "-m", "Unrelated root commit"))
		f.git(f.dir, "--git-dir="+f.github, "fetch", "-q", f.repo, unrelated)
		f.git(f.dir, "--git-dir="+f.github, "update-ref", "refs/heads/main", unrelated)
		if err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options()); err == nil {
			t.Fatal("accepted non-ancestor source")
		}
		if f.head(f.aur(fixtureName), "master") != before {
			t.Fatal("ancestry failure wrote AUR")
		}
	})
	t.Run("target must be main", func(t *testing.T) {
		f := newIntegrationFixture(t, 1)
		f.prepare(fixtureName)
		opts := f.options()
		opts.TargetBranch = "other"
		if err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), opts); err == nil {
			t.Fatal("accepted non-main target")
		}
	})
}

func TestIntegrationPrepareNoExtrasVerifiesSources(t *testing.T) {
	for _, bad := range []bool{false, true} {
		name := "verified"
		if bad {
			name = "invalid checksum"
		}
		t.Run(name, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body := fixturePayload
				if bad {
					body = "Changed unverified bytes\n"
				}
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			f.packages[0].AURExtraFiles = []string{}
			f.writeConfig()
			f.editRecipe(f.recipeDir(fixtureName), func(s string) string {
				s = strings.Replace(s, "pkgrel=2", "pkgrel=3", 1)
				return strings.Replace(s, "source=('fixture-source')", "source=('fixture-source::"+server.URL+"/payload')", 1)
			})
			f.commitSource()
			result, err := f.service().Prepare(context.Background(), fixtureName, f.candidate(fixtureName), PrepareOptions{SourceCommit: f.source})
			if requests.Load() == 0 {
				t.Fatalf("source verification was omitted for candidate without extras: %v", err)
			}
			if bad {
				if err == nil {
					t.Fatal("unverified source accepted without extras")
				}
				return
			}
			if err != nil || !result.NeedsBuild || !result.NeedsPublish {
				t.Fatalf("no-extras result=%+v error=%v", result, err)
			}
			var manifest Manifest
			if err := json.Unmarshal(mustRead(t, filepath.Join(f.candidate(fixtureName), "manifest.json")), &manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest.Files) != 2 {
				t.Fatalf("non-managed source included in candidate: %+v", manifest.Files)
			}
		})
	}
}

func TestIntegrationTrustExistingOptionalSum(t *testing.T) {
	for _, operation := range []string{"delete", "modify"} {
		t.Run(operation, func(t *testing.T) {
			f := newIntegrationFixture(t, 1)
			mustWrite(t, filepath.Join(f.repo, "go.sum"), []byte{}, 0644)
			f.commitSource()
			f.tag = "v1.10.0"
			f.prepare(fixtureName)
			before := f.head(f.aur(fixtureName), "master")
			clone := f.clone(f.github, "sum-change")
			path := filepath.Join(clone, "go.sum")
			if operation == "delete" {
				mustRemove(t, path)
			} else {
				mustWrite(t, path, []byte("\n"), 0644)
			}
			f.commitPush(clone, "main")
			if err := f.service().Publish(context.Background(), fixtureName, f.candidate(fixtureName), f.options()); err == nil {
				t.Fatal("changed optional go.sum accepted")
			}
			if f.head(f.aur(fixtureName), "master") != before {
				t.Fatal("optional sum trust failure wrote AUR")
			}
		})
	}
}
