package maintenance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func coreWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func coreJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func coreRecord() map[string]any {
	return map[string]any{"name": "fixture-bin", "enabled": true, "upstream_repo": "sample/fixture", "tag_prefix": "v"}
}

func TestConfigStrictFields(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{}`, `{"packages":null}`, `{"packages":false}`, `{"packages":{}}`, `{"packages":[],"other":1}`, `{"packages":[null]}`, `{"packages":[[]]}`, `{"packages":[]}{}`, `{"packages":[`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseConfig([]byte(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
	for _, key := range []string{"name", "enabled", "upstream_repo", "tag_prefix"} {
		for _, missing := range []bool{false, true} {
			t.Run(key+"/"+map[bool]string{false: "null", true: "missing"}[missing], func(t *testing.T) {
				record := coreRecord()
				if missing {
					delete(record, key)
				} else {
					record[key] = nil
				}
				if _, err := parseConfig(coreJSON(t, map[string]any{"packages": []any{record}})); err == nil {
					t.Fatal("accepted missing or null required field")
				}
			})
		}
	}
	cases := []struct {
		key   string
		value any
	}{
		{"enabled", "true"}, {"enabled", 1}, {"enabled", []any{}},
		{"name", ""}, {"name", "Upper"}, {"name", "../escape"}, {"name", 1},
		{"upstream_repo", "owner/."}, {"upstream_repo", "owner/.."}, {"upstream_repo", "-owner/repo"}, {"upstream_repo", "owner-/repo"}, {"upstream_repo", "owner/a/b"}, {"upstream_repo", strings.Repeat("a", 40) + "/repo"}, {"upstream_repo", "owner/" + strings.Repeat("r", 101)},
		{"tag_prefix", "v/"}, {"tag_prefix", true}, {"mystery", 1},
	}
	for _, tc := range cases {
		t.Run(tc.key+"/"+string(coreJSON(t, tc.value)), func(t *testing.T) {
			record := coreRecord()
			record[tc.key] = tc.value
			if _, err := parseConfig(coreJSON(t, map[string]any{"packages": []any{record}})); err == nil {
				t.Fatal("accepted invalid package record")
			}
		})
	}
	first := coreRecord()
	if _, err := parseConfig(coreJSON(t, map[string]any{"packages": []any{first, first}})); err == nil {
		t.Fatal("accepted duplicate package name")
	}
}

func TestConfigExtraFilesCompatibility(t *testing.T) {
	for _, value := range []any{nil, false, []any{}} {
		record := coreRecord()
		record["aur_extra_files"] = value
		config, err := parseConfig(coreJSON(t, map[string]any{"packages": []any{record}}))
		if err != nil {
			t.Fatal(err)
		}
		if len(config.Packages[0].AURExtraFiles) != 0 {
			t.Fatalf("unexpected extras: %#v", config)
		}
	}
	for _, value := range []any{true, "LICENSE", 0, map[string]any{}, []any{nil}, []any{false}, []any{"LICENSE", "LICENSE"}} {
		record := coreRecord()
		record["aur_extra_files"] = value
		if _, err := parseConfig(coreJSON(t, map[string]any{"packages": []any{record}})); err == nil {
			t.Fatalf("accepted extras %#v", value)
		}
	}
	for _, file := range []string{"", "../escape", "a/../b", "a/./b", "a//b", "/LICENSE", "LICENSE/", "a\\b", "line\nbreak", "a\x7fb", ".git/config", "a/.git/config", "src/a", "a/pkg/b", "PKGBUILD", ".SRCINFO"} {
		record := coreRecord()
		record["aur_extra_files"] = []any{file}
		if _, err := parseConfig(coreJSON(t, map[string]any{"packages": []any{record}})); err == nil {
			t.Fatalf("accepted extra %q", file)
		}
	}
	record := coreRecord()
	record["tag_prefix"] = ""
	record["aur_extra_files"] = []any{"LICENSE", "hooks/install hook", "a\u0085b"}
	config, err := parseConfig(coreJSON(t, map[string]any{"packages": []any{record}}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PKGBUILD", ".SRCINFO", "LICENSE", "hooks/install hook", "a\u0085b"}
	if got := config.Packages[0].ManagedFiles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("managed files = %q, want %q", got, want)
	}
}

func TestConfigSelectionAndMatrixOrder(t *testing.T) {
	config, err := parseConfig([]byte(`{"packages":[{"name":"z-bin","enabled":true,"upstream_repo":"a/b","tag_prefix":""},{"name":"old-bin","enabled":false,"upstream_repo":"a/c","tag_prefix":"v"},{"name":"a-bin","enabled":true,"upstream_repo":"a/d","tag_prefix":"v"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{config: config}
	matrix, err := service.Matrix("")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(coreJSON(t, matrix)); got != `{"include":[{"package":"z-bin"},{"package":"a-bin"}]}` {
		t.Fatalf("matrix = %s", got)
	}
	for _, name := range []string{"old-bin", "unknown", ""} {
		if _, err := config.selectPackage(name); err == nil {
			t.Fatalf("selected %q", name)
		}
	}
	if _, err := config.selectPackage("z-bin"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"packages":[]}`, `{"packages":[{"name":"a","enabled":false,"upstream_repo":"a/b","tag_prefix":""}]}`} {
		config, err := parseConfig([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		matrix, err := (&Service{config: config}).Matrix("")
		if err != nil {
			t.Fatal(err)
		}
		if got := string(coreJSON(t, matrix)); got != `{"include":[]}` {
			t.Fatalf("empty matrix = %s", got)
		}
	}
}

func TestConfigChecksDisabledRecipesAndExtras(t *testing.T) {
	for _, scenario := range []string{"valid-missing-extra", "missing-PKGBUILD", "missing-SRCINFO", "linked-PKGBUILD", "linked-extra", "linked-parent", "directory-extra", "linked-config"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			record := coreRecord()
			record["enabled"] = false
			record["aur_extra_files"] = []string{"hooks/install"}
			coreWriteFile(t, filepath.Join(root, "packages.json"), string(coreJSON(t, map[string]any{"packages": []any{record}})))
			pkg := filepath.Join(root, "packages", "fixture-bin")
			coreWriteFile(t, filepath.Join(pkg, "PKGBUILD"), "recipe")
			coreWriteFile(t, filepath.Join(pkg, ".SRCINFO"), "metadata")
			switch scenario {
			case "missing-PKGBUILD":
				if err := os.Remove(filepath.Join(pkg, "PKGBUILD")); err != nil {
					t.Fatal(err)
				}
			case "missing-SRCINFO":
				if err := os.Remove(filepath.Join(pkg, ".SRCINFO")); err != nil {
					t.Fatal(err)
				}
			case "linked-PKGBUILD":
				if err := os.Remove(filepath.Join(pkg, "PKGBUILD")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(pkg, ".SRCINFO"), filepath.Join(pkg, "PKGBUILD")); err != nil {
					t.Fatal(err)
				}
			case "linked-extra":
				if err := os.MkdirAll(filepath.Join(pkg, "hooks"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(pkg, ".SRCINFO"), filepath.Join(pkg, "hooks", "install")); err != nil {
					t.Fatal(err)
				}
			case "linked-parent":
				if err := os.Symlink(t.TempDir(), filepath.Join(pkg, "hooks")); err != nil {
					t.Fatal(err)
				}
			case "directory-extra":
				if err := os.MkdirAll(filepath.Join(pkg, "hooks", "install"), 0755); err != nil {
					t.Fatal(err)
				}
			case "linked-config":
				if err := os.Rename(filepath.Join(root, "packages.json"), filepath.Join(root, "config-real")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "config-real"), filepath.Join(root, "packages.json")); err != nil {
					t.Fatal(err)
				}
			}
			_, err := loadConfig(root)
			if scenario == "valid-missing-extra" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s", scenario)
			}
		})
	}
}
