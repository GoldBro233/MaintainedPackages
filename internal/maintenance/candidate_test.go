package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func coreManifest() Manifest {
	return Manifest{Package: "fixture-bin", SourceCommit: strings.Repeat("a", 40), Version: "1.2-3", Files: map[string]string{"PKGBUILD": strings.Repeat("b", 64), ".SRCINFO": strings.Repeat("c", 64)}, AURFiles: map[string]*string{"PKGBUILD": nil, ".SRCINFO": nil}}
}

func TestCandidateManifestFields(t *testing.T) {
	data := coreJSON(t, coreManifest())
	manifest, err := parseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.AURCommit != nil || manifest.AURFiles["PKGBUILD"] != nil {
		t.Fatal("null baseline not preserved")
	}
	for _, field := range []string{"package", "source_commit", "version", "aur_commit", "files", "aur_files"} {
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		delete(object, field)
		if _, err := parseManifest(coreJSON(t, object)); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
	cases := []struct {
		key   string
		value any
	}{
		{"package", nil}, {"package", 1}, {"source_commit", nil}, {"source_commit", strings.Repeat("A", 40)}, {"source_commit", strings.Repeat("a", 39)},
		{"version", nil}, {"version", "v1-2"}, {"version", "1"}, {"version", "1-2-3"}, {"aur_commit", false}, {"aur_commit", strings.Repeat("a", 41)},
		{"files", nil}, {"files", []any{}}, {"files", map[string]any{"PKGBUILD": nil}}, {"files", map[string]any{"PKGBUILD": strings.Repeat("A", 64)}}, {"files", map[string]any{"PKGBUILD": strings.Repeat("a", 63)}},
		{"aur_files", nil}, {"aur_files", []any{}}, {"aur_files", map[string]any{"PKGBUILD": false}}, {"aur_files", map[string]any{"PKGBUILD": strings.Repeat("a", 65)}}, {"unknown", false},
	}
	for _, tc := range cases {
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		object[tc.key] = tc.value
		if _, err := parseManifest(coreJSON(t, object)); err == nil {
			t.Fatalf("accepted %s = %#v", tc.key, tc.value)
		}
	}
	for _, raw := range []string{"null", "[]", "{}", string(data) + "{}"} {
		if _, err := parseManifest([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	valid := coreManifest()
	commit := strings.Repeat("d", 40)
	hash := strings.Repeat("e", 64)
	valid.AURCommit = &commit
	valid.AURFiles["PKGBUILD"] = &hash
	valid.Version = strings.Repeat("9", 100) + ":1.2-3.4"
	if _, err := parseManifest(coreJSON(t, valid)); err != nil {
		t.Fatal(err)
	}
}

func coreCandidateFixture(t *testing.T) (*Service, string, Manifest) {
	t.Helper()
	root, dir := t.TempDir(), t.TempDir()
	p := Package{Name: "fixture-bin", Enabled: true, UpstreamRepo: "sample/fixture", TagPrefix: "v", AURExtraFiles: []string{"hooks/install", "LICENSE"}}
	manifest := coreManifest()
	for file, data := range map[string]string{"PKGBUILD": "pkgver=1.2\npkgrel=3\n", ".SRCINFO": coreMetadata("1.2", "3", ""), "hooks/install": "post_install() { printf 'installed'; }\n", "LICENSE": "fixture license\n"} {
		coreWriteFile(t, filepath.Join(dir, "package", file), data)
		manifest.Files[file] = hashBytes([]byte(data))
		manifest.AURFiles[file] = nil
	}
	coreWriteFile(t, filepath.Join(dir, "manifest.json"), string(coreJSON(t, manifest)))
	service := &Service{root: root, config: Config{Packages: []Package{p}}, stdout: io.Discard, stderr: io.Discard}
	service.runner = coreRunnerFunc(func(_ context.Context, c Command) error {
		if c.Name != "git" {
			return fmt.Errorf("unexpected external effect: %s", c.Name)
		}
		_, err := io.WriteString(c.Stdout, strings.Repeat("a", 40)+"\n")
		return err
	})
	return service, dir, manifest
}

func TestCandidateValidation(t *testing.T) {
	service, dir, manifest := coreCandidateFixture(t)
	candidate, err := service.validateCandidate(context.Background(), "fixture-bin", dir+"///", manifest.SourceCommit)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Dir != dir || candidate.Metadata.Version != manifest.Version || candidate.Package.Name != manifest.Package {
		t.Fatalf("candidate = %#v", candidate)
	}
	if _, err := service.validateCandidate(context.Background(), "fixture-bin", dir, ""); err != nil {
		t.Fatalf("prepare internal validation: %v", err)
	}
	if _, err := service.validateCandidate(context.Background(), "fixture-bin", dir, strings.Repeat("f", 40)); err == nil {
		t.Fatal("accepted expected source mismatch")
	}
}

func TestCandidateRejectsTransportTampering(t *testing.T) {
	for _, scenario := range []string{"bytes", "missing", "extra-file", "extra-directory", "symlink-file", "symlink-directory", "fifo", "wrong-package", "wrong-source", "wrong-version", "file-set", "aur-file-set", "extra-file-key", "bad-digest", "missing-manifest-key", "manifest-null", "package-file", "manifest-directory", "inside-checkout"} {
		t.Run(scenario, func(t *testing.T) {
			service, dir, manifest := coreCandidateFixture(t)
			writeManifest := false
			switch scenario {
			case "bytes":
				coreWriteFile(t, filepath.Join(dir, "package", "PKGBUILD"), "tampered")
			case "missing":
				if err := os.Remove(filepath.Join(dir, "package", "LICENSE")); err != nil {
					t.Fatal(err)
				}
			case "extra-file":
				coreWriteFile(t, filepath.Join(dir, "package", "unmanaged"), "extra")
			case "extra-directory":
				if err := os.Mkdir(filepath.Join(dir, "unused"), 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink-file":
				path := filepath.Join(dir, "package", "LICENSE")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "package", "PKGBUILD"), path); err != nil {
					t.Fatal(err)
				}
			case "symlink-directory":
				path := filepath.Join(dir, "package", "hooks")
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				path := filepath.Join(dir, "package", "LICENSE")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-package":
				manifest.Package = "other"
				writeManifest = true
			case "wrong-source":
				manifest.SourceCommit = strings.Repeat("b", 40)
				writeManifest = true
			case "wrong-version":
				manifest.Version = "1.2-4"
				writeManifest = true
			case "file-set":
				delete(manifest.Files, "LICENSE")
				writeManifest = true
			case "aur-file-set":
				delete(manifest.AURFiles, "LICENSE")
				writeManifest = true
			case "extra-file-key":
				manifest.Files["extra"] = strings.Repeat("d", 64)
				writeManifest = true
			case "bad-digest":
				manifest.Files["LICENSE"] = strings.Repeat("d", 64)
				writeManifest = true
			case "missing-manifest-key":
				var object map[string]any
				if err := json.Unmarshal(coreJSON(t, manifest), &object); err != nil {
					t.Fatal(err)
				}
				delete(object, "aur_commit")
				coreWriteFile(t, filepath.Join(dir, "manifest.json"), string(coreJSON(t, object)))
			case "manifest-null":
				coreWriteFile(t, filepath.Join(dir, "manifest.json"), "null")
			case "package-file":
				if err := os.RemoveAll(filepath.Join(dir, "package")); err != nil {
					t.Fatal(err)
				}
				coreWriteFile(t, filepath.Join(dir, "package"), "file")
			case "manifest-directory":
				if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, "manifest.json"), 0755); err != nil {
					t.Fatal(err)
				}
			case "inside-checkout":
				service.root = filepath.Dir(dir)
			}
			if writeManifest {
				coreWriteFile(t, filepath.Join(dir, "manifest.json"), string(coreJSON(t, manifest)))
			}
			if _, err := service.validateCandidate(context.Background(), "fixture-bin", dir, ""); err == nil {
				t.Fatalf("accepted %s", scenario)
			}
		})
	}
}

func TestCandidateInstallAndBuildRejectBeforeEffects(t *testing.T) {
	service, dir, manifest := coreCandidateFixture(t)
	calls := 0
	service.runner = coreRunnerFunc(func(context.Context, Command) error { calls++; return fmt.Errorf("unexpected command") })
	if err := service.InstallBuildDeps(context.Background(), "fixture-bin", dir, "", "x86_64"); err == nil {
		t.Fatal("install accepted missing source")
	}
	build := filepath.Join(t.TempDir(), "build")
	if err := service.Build(context.Background(), "fixture-bin", dir, build, ""); err == nil {
		t.Fatal("build accepted missing source")
	}
	if calls != 0 {
		t.Fatal("missing-source invocation ran a command")
	}
	coreWriteFile(t, filepath.Join(dir, "manifest.json"), "invalid")
	if err := service.InstallBuildDeps(context.Background(), "fixture-bin", dir, manifest.SourceCommit, ""); err == nil {
		t.Fatal("install accepted invalid candidate")
	}
	if err := service.Build(context.Background(), "fixture-bin", dir, build, manifest.SourceCommit); err == nil {
		t.Fatal("build accepted invalid candidate")
	}
	if calls != 0 {
		t.Fatal("invalid candidate reached tools")
	}
	if _, err := os.Stat(build); !os.IsNotExist(err) {
		t.Fatalf("invalid candidate created build directory: %v", err)
	}
}
