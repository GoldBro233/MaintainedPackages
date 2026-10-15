package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cliRepo(t *testing.T, records string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"alpha-bin", "beta-bin", "disabled-bin"} {
		dir := filepath.Join(root, "packages", name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"PKGBUILD", ".SRCINFO"} {
			if err := os.WriteFile(filepath.Join(dir, file), nil, 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, "packages.json"), []byte(`{"packages":`+records+`}`), 0644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCLIHelpAndArgumentErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"help without repository", []string{"--repo", missing, "--help"}, 0},
		{"missing command", nil, 2},
		{"unknown command", []string{"unknown"}, 2},
		{"unknown flag", []string{"--unknown"}, 2},
		{"missing repo value", []string{"--repo"}, 2},
		{"empty matrix selection", []string{"matrix", ""}, 2},
		{"extra matrix selection", []string{"matrix", "a", "b"}, 2},
		{"missing prepare destination", []string{"prepare", "alpha-bin"}, 2},
		{"missing build destination", []string{"build", "alpha-bin", "/candidate"}, 2},
		{"global flag after command", []string{"matrix", "--repo", missing}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			if got := Run(context.Background(), tc.args, []string{"PATH="}, &out, &diagnostic); got != tc.code {
				t.Fatalf("exit=%d, want %d; stderr=%s", got, tc.code, &diagnostic)
			}
			if tc.code == 0 {
				if !strings.Contains(out.String(), "maintpkg [--repo <path>] matrix") || diagnostic.Len() != 0 {
					t.Fatalf("help streams: stdout=%q stderr=%q", &out, &diagnostic)
				}
			} else if out.Len() != 0 || diagnostic.Len() == 0 {
				t.Fatalf("error streams: stdout=%q stderr=%q", &out, &diagnostic)
			}
		})
	}
}

func TestCLIMatrixOfflineSelection(t *testing.T) {
	root := cliRepo(t, `[
		{"name":"beta-bin","enabled":true,"upstream_repo":"sample/beta","tag_prefix":"v"},
		{"name":"disabled-bin","enabled":false,"upstream_repo":"sample/disabled","tag_prefix":""},
		{"name":"alpha-bin","enabled":true,"upstream_repo":"sample/alpha","tag_prefix":"v"}
	]`)
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
		code int
	}{
		{"all in configured order", nil, `{"include":[{"package":"beta-bin"},{"package":"alpha-bin"}]}` + "\n", 0},
		{"selected", []string{"alpha-bin"}, `{"include":[{"package":"alpha-bin"}]}` + "\n", 0},
		{"disabled", []string{"disabled-bin"}, "", 1},
		{"unknown", []string{"missing-bin"}, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			args := append([]string{"--repo", root, "matrix"}, tc.args...)
			if code := Run(context.Background(), args, []string{"PATH="}, &out, &diagnostic); code != tc.code {
				t.Fatalf("exit=%d stderr=%s", code, &diagnostic)
			}
			if out.String() != tc.want {
				t.Fatalf("stdout=%q, want %q", &out, tc.want)
			}
			if tc.code == 0 && diagnostic.Len() != 0 {
				t.Fatal(diagnostic.String())
			}
		})
	}
	after, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("working directory changed: %s -> %s", before, after)
	}
}

func TestCLIEmptyMatrixIsArray(t *testing.T) {
	for _, records := range []string{`[]`, `[{"name":"disabled-bin","enabled":false,"upstream_repo":"sample/disabled","tag_prefix":"v"}]`} {
		root := cliRepo(t, records)
		var out, diagnostic bytes.Buffer
		if code := Run(context.Background(), []string{"--repo", root, "matrix"}, nil, &out, &diagnostic); code != 0 {
			t.Fatalf("exit=%d: %s", code, &diagnostic)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if string(decoded["include"]) != "[]" {
			t.Fatalf("include=%s, want []", decoded["include"])
		}
	}
}

func TestCLIRequiredEnvironment(t *testing.T) {
	root := cliRepo(t, `[]`)
	for _, tc := range []struct {
		args, env []string
		message   string
	}{
		{[]string{"prepare", "alpha-bin", "/candidate"}, []string{"FORCE_BUILD=yes"}, "FORCE_BUILD must be true or false"},
		{[]string{"build", "alpha-bin", "/candidate", "/build"}, nil, "SOURCE_COMMIT is required"},
		{[]string{"install-build-deps", "alpha-bin", "/candidate"}, nil, "SOURCE_COMMIT is required"},
		{[]string{"publish", "alpha-bin", "/candidate"}, nil, "SOURCE_COMMIT is required"},
	} {
		var out, diagnostic bytes.Buffer
		args := append([]string{"--repo", root}, tc.args...)
		if code := Run(context.Background(), args, tc.env, &out, &diagnostic); code != 1 {
			t.Fatalf("%v: exit=%d, stderr=%s", tc.args, code, &diagnostic)
		}
		if out.Len() != 0 || !strings.Contains(diagnostic.String(), tc.message) {
			t.Fatalf("%v: stdout=%q stderr=%q", tc.args, &out, &diagnostic)
		}
	}
}
