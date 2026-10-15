package maintenance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func coreSetCandidateMetadata(t *testing.T, dir string, manifest Manifest, metadata string) {
	t.Helper()
	coreWriteFile(t, filepath.Join(dir, "package", ".SRCINFO"), metadata)
	manifest.Files[".SRCINFO"] = hashBytes([]byte(metadata))
	coreWriteFile(t, filepath.Join(dir, "manifest.json"), string(coreJSON(t, manifest)))
}

func TestBuildDependencyInstallationBoundary(t *testing.T) {
	for _, scenario := range []string{"current-arch", "detected-arch", "no-deps", "failure", "invalid-dep"} {
		t.Run(scenario, func(t *testing.T) {
			service, dir, manifest := coreCandidateFixture(t)
			metadata := coreMetadata("1.2", "3", "")
			if scenario != "no-deps" {
				metadata += "depends = common>=2\nmakedepends_x86_64 = compiler=1\ncheckdepends = common\ncheckdepends_x86_64 = checks\ndepends_aarch64 = excluded\n"
			}
			if scenario == "invalid-dep" {
				metadata += "depends = invalid dependency\n"
			}
			coreSetCandidateMetadata(t, dir, manifest, metadata)
			failure := errors.New("pacman installation failed")
			pacmanCalls, unameCalls := 0, 0
			var stdout, stderr bytes.Buffer
			service.stdout = &stdout
			service.stderr = &stderr
			service.runner = coreRunnerFunc(func(_ context.Context, c Command) error {
				switch c.Name {
				case "git":
					_, err := io.WriteString(c.Stdout, manifest.SourceCommit+"\n")
					return err
				case "uname":
					unameCalls++
					if !reflect.DeepEqual(c.Args, []string{"-m"}) {
						t.Fatalf("uname args = %q", c.Args)
					}
					_, err := io.WriteString(c.Stdout, "x86_64\n")
					return err
				case "pacman":
					pacmanCalls++
					want := []string{"-S", "--noconfirm", "--needed", "common", "compiler", "checks"}
					if !reflect.DeepEqual(c.Args, want) {
						t.Fatalf("pacman args = %q, want %q", c.Args, want)
					}
					if c.Stdout != &stdout || c.Stderr != &stderr {
						t.Fatal("installation streams not forwarded")
					}
					if scenario == "failure" {
						return failure
					}
					return nil
				default:
					t.Fatalf("unexpected command %s", c.Name)
					return nil
				}
			})
			arch := "x86_64"
			if scenario == "detected-arch" {
				arch = ""
			}
			err := service.InstallBuildDeps(context.Background(), "fixture-bin", dir, manifest.SourceCommit, arch)
			switch scenario {
			case "failure":
				if !errors.Is(err, failure) {
					t.Fatalf("lost pacman cause: %v", err)
				}
			case "invalid-dep":
				if err == nil {
					t.Fatal("accepted invalid dependency")
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			wantCalls := 1
			if scenario == "no-deps" || scenario == "invalid-dep" {
				wantCalls = 0
			}
			if pacmanCalls != wantCalls {
				t.Fatalf("pacman calls = %d, want %d", pacmanCalls, wantCalls)
			}
			wantUname := 0
			if scenario == "detected-arch" {
				wantUname = 1
			}
			if unameCalls != wantUname {
				t.Fatalf("uname calls = %d, want %d", unameCalls, wantUname)
			}
		})
	}
}

func TestBuildRejectsOverlappingDestination(t *testing.T) {
	for _, where := range []string{"candidate", "inside-candidate", "checkout", "nonempty"} {
		t.Run(where, func(t *testing.T) {
			service, dir, manifest := coreCandidateFixture(t)
			build := dir
			switch where {
			case "inside-candidate":
				build = filepath.Join(dir, "build")
			case "checkout":
				build = filepath.Join(service.root, "build")
			case "nonempty":
				build = t.TempDir()
				coreWriteFile(t, filepath.Join(build, "keep"), "keep")
			}
			calls := 0
			original := service.runner
			service.runner = coreRunnerFunc(func(ctx context.Context, c Command) error {
				if c.Name != "git" {
					calls++
				}
				return original.Run(ctx, c)
			})
			if err := service.Build(context.Background(), "fixture-bin", dir, build, manifest.SourceCommit); err == nil {
				t.Fatal("accepted invalid build destination")
			}
			if calls != 0 {
				t.Fatal("invalid destination reached build tool")
			}
			if where == "nonempty" {
				data, err := os.ReadFile(filepath.Join(build, "keep"))
				if err != nil || strings.TrimSpace(string(data)) != "keep" {
					t.Fatal("existing output changed")
				}
			}
		})
	}
}
