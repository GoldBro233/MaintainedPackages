package maintenance

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathsRejectRawComponentsAndLinks(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "absent"), filepath.Join(base, "dangling")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", "", base + "/./output", base + "/child/../output", base + "/link", base + "/link/file", base + "/dangling", base + "/dangling/file", base + "/link/../output"} {
		if _, err := safePath(path); err == nil {
			t.Fatalf("accepted unsafe path %q", path)
		}
	}
	path := base + "//ordinary space///child/"
	got, err := safePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "ordinary space", "child"); got != want {
		t.Fatalf("normalized %q to %q, want %q", path, got, want)
	}
}

func TestPathsOutputBoundaries(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	candidate := filepath.Join(base, "candidate")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(candidate, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, root + "/child", root + "//child", root + "///"} {
		if _, err := outsideDirectory(root, path, ""); err == nil {
			t.Fatalf("accepted checkout output %q", path)
		}
	}
	for _, path := range []string{candidate, candidate + "/build", base} {
		if _, err := outsideDirectory(root, path, candidate); err == nil {
			t.Fatalf("accepted overlap %q", path)
		}
	}
	for _, path := range []string{base + "/repo-sibling", base + "//empty space///"} {
		got, err := outsideDirectory(root, path, candidate)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Clean(path) {
			t.Fatalf("unexpected normalized path %q", got)
		}
	}
	coreWriteFile(t, filepath.Join(base, "not-dir"), "x")
	if _, err := outsideDirectory(root, filepath.Join(base, "not-dir"), ""); err == nil {
		t.Fatal("accepted file output")
	}
	coreWriteFile(t, filepath.Join(base, "not-empty", "data"), "x")
	if _, err := outsideDirectory(root, filepath.Join(base, "not-empty"), ""); err == nil {
		t.Fatal("accepted nonempty directory")
	}
	if _, err := outsideDirectory(root, candidate, ""); err != nil {
		t.Fatal(err)
	}
}

func TestPathsCopyRejectsDestinationLinks(t *testing.T) {
	for _, scenario := range []string{"parent", "target", "dangling", "source", "directory"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			external := t.TempDir()
			src := filepath.Join(base, "source")
			dst := filepath.Join(base, "destination", "file")
			coreWriteFile(t, src, "new")
			coreWriteFile(t, filepath.Join(external, "file"), "untouched")
			switch scenario {
			case "parent":
				if err := os.Symlink(external, filepath.Join(base, "destination")); err != nil {
					t.Fatal(err)
				}
			case "target", "dangling":
				if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(external, "file")
				if scenario == "dangling" {
					target = filepath.Join(external, "missing")
				}
				if err := os.Symlink(target, dst); err != nil {
					t.Fatal(err)
				}
			case "source":
				if err := os.Remove(src); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(external, "file"), src); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.MkdirAll(dst, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := copyFile(src, dst); err == nil {
				t.Fatal("accepted unsafe copy")
			}
			data, err := os.ReadFile(filepath.Join(external, "file"))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "untouched" {
				t.Fatal("external file changed")
			}
		})
	}
}
