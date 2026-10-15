package maintenance

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type coreRunnerFunc func(context.Context, Command) error

func (fn coreRunnerFunc) Run(ctx context.Context, command Command) error { return fn(ctx, command) }

func coreMetadata(version, release, epoch string) string {
	data := "pkgbase = fixture-bin\n\tpkgver = " + version + "\n\tpkgrel = " + release + "\n"
	if epoch != "" {
		data += "\tepoch = " + epoch + "\n"
	}
	return data + "\tarch = any\npkgname = fixture-bin\n"
}

func TestMetadataVersions(t *testing.T) {
	for _, tc := range []struct{ version, release, epoch, want string }{
		{"1.10.0", "1", "", "1.10.0-1"}, {"01.002", "2.1", "0", "01.002-2.1"}, {"1", "2", "0000", "1-2"}, {"1", "2", "00002", "00002:1-2"}, {"1", "2", "99999999999999999999999999999999999999999999999999999999999999999999", "99999999999999999999999999999999999999999999999999999999999999999999:1-2"},
	} {
		got, err := parseMetadata([]byte(coreMetadata(tc.version, tc.release, tc.epoch)), "fixture-bin")
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != tc.want || got.Pkgver != tc.version {
			t.Fatalf("metadata = %#v, want %q", got, tc.want)
		}
	}
	base := coreMetadata("1.2", "3", "")
	for _, key := range []string{"pkgbase", "pkgname", "pkgver", "pkgrel", "epoch"} {
		var value string
		switch key {
		case "pkgbase", "pkgname":
			value = "fixture-bin"
		case "pkgver":
			value = "1.2"
		case "pkgrel":
			value = "3"
		case "epoch":
			value = "0"
		}
		data := base + key + " = " + value + "\n"
		if key == "epoch" {
			data += "epoch = 0\n"
		}
		if _, err := parseMetadata([]byte(data), "fixture-bin"); err == nil {
			t.Fatalf("accepted duplicate %s", key)
		}
	}
	for _, key := range []string{"pkgbase", "pkgname", "pkgver", "pkgrel"} {
		lines := strings.Split(base, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), key+" = ") {
				lines[i] = "# omitted"
			}
		}
		if _, err := parseMetadata([]byte(strings.Join(lines, "\n")), "fixture-bin"); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
	}
	for _, data := range []string{
		coreMetadata("v1", "1", ""), coreMetadata("1-2", "1", ""), coreMetadata("1..2", "1", ""), coreMetadata("1", "0-1", ""), coreMetadata("1", "1", "-1"), coreMetadata("1", "1", "1.2"), coreMetadata("1", "1", "+1"), coreMetadata("1", "1", " ") + "", strings.ReplaceAll(base, "fixture-bin", "other-bin"), strings.Replace(base, "pkgver = 1.2", "pkgver = 1.2 ", 1), base + "pkgname = split-bin\n",
	} {
		if _, err := parseMetadata([]byte(data), "fixture-bin"); err == nil {
			t.Fatalf("accepted invalid metadata %q", data)
		}
	}
	if _, err := parseMetadata([]byte(strings.TrimSuffix(base, "\n")), "fixture-bin"); err != nil {
		t.Fatalf("final line without newline: %v", err)
	}
}

func TestMetadataDependencies(t *testing.T) {
	data := []byte("depends = common>=1\nmakedepends = build=2\ncheckdepends = common<3\ndepends_x86_64 = arch-specific\nmakedepends_aarch64 = other-arch\ncheckdepends_x86_64 = check>0\noptdepends = ignored: description\ncheckdepends = last\n")
	got, err := dependencyNames(data, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"common", "build", "arch-specific", "check", "last"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deps = %q, want %q", got, want)
	}
	got, err = dependencyNames(data, "aarch64")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"common", "build", "other-arch", "last"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deps = %q, want %q", got, want)
	}
	for _, dependency := range []string{"bad name", "../package", "", "=1", "name:why", "name >=1"} {
		if _, err := dependencyNames([]byte("depends = "+dependency+"\n"), "x86_64"); err == nil {
			t.Fatalf("accepted dependency %q", dependency)
		}
	}
	if got, err := dependencyNames([]byte("depends_aarch64 = invalid dependency\n"), "x86_64"); err != nil || len(got) != 0 {
		t.Fatalf("unselected architecture: %q, %v", got, err)
	}
}

func TestMetadataStaticVersionUpdatesPreserveOtherBytes(t *testing.T) {
	input := "# comment\npkgname=fixture-bin\npkgver='1.9' # old\npkgrel=23\n  pkgver=unchanged\npackage() { printf '%s' \"$pkgver\"; }"
	want := "# comment\npkgname=fixture-bin\npkgver=1.10\npkgrel=1\n  pkgver=unchanged\npackage() { printf '%s' \"$pkgver\"; }"
	got, err := updatedPKGBUILD([]byte(input), "1.10")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("updated bytes = %q, want %q", got, want)
	}
	for _, raw := range []string{"pkgver=1\n", "pkgver=1\npkgver=2\npkgrel=1\n", " pkgver=1\npkgrel=1\n", "pkgver=1\npkgrel=1\npkgrel=2\n"} {
		if _, err := updatedPKGBUILD([]byte(raw), "2"); err == nil {
			t.Fatalf("accepted ambiguous assignments %q", raw)
		}
	}
}
