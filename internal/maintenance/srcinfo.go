package maintenance

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
)

type Metadata struct{ Pkgver, Version string }

var decimalVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
var decimalPattern = regexp.MustCompile(`^[0-9]+$`)
var metadataLinePattern = regexp.MustCompile(`^[[:space:]]*(pkgbase|pkgname|pkgver|pkgrel|epoch)[[:space:]]*=[[:space:]]*(.*)$`)
var dependencyLinePattern = regexp.MustCompile(`^[[:space:]]*([^[:space:]=]+)[[:space:]]*=[[:space:]]*(.*)$`)
var dependencyNamePattern = regexp.MustCompile(`^[a-zA-Z0-9@._+\-]+$`)
var versionOrderPattern = regexp.MustCompile(`^-?[0-9]+$`)

func parseMetadata(data []byte, name string) (Metadata, error) {
	values := make(map[string]string, 5)
	for _, line := range strings.Split(string(data), "\n") {
		match := metadataLinePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if _, found := values[match[1]]; found {
			return Metadata{}, fmt.Errorf("Invalid single-package .SRCINFO: duplicate %s", match[1])
		}
		values[match[1]] = match[2]
	}
	base, hasBase := values["pkgbase"]
	pkg, hasName := values["pkgname"]
	ver, hasVer := values["pkgver"]
	rel, hasRel := values["pkgrel"]
	epoch, hasEpoch := values["epoch"]
	if !hasBase || !hasName || !hasVer || !hasRel || base != name || pkg != name || !decimalVersionPattern.MatchString(ver) || !decimalVersionPattern.MatchString(rel) || (hasEpoch && !decimalPattern.MatchString(epoch)) {
		return Metadata{}, fmt.Errorf("Invalid single-package .SRCINFO for %s", name)
	}
	version := ver + "-" + rel
	// Epoch is an arbitrary-size decimal, not a machine integer. Preserve its
	// original spelling when nonzero, as required by the manifest contract.
	if hasEpoch && strings.TrimLeft(epoch, "0") != "" {
		version = epoch + ":" + version
	}
	return Metadata{Pkgver: ver, Version: version}, nil
}

func (s *Service) compareVersions(ctx context.Context, a, b string) (int, error) {
	var out bytes.Buffer
	if err := s.runner.Run(ctx, Command{Name: "vercmp", Args: []string{a, b}, Stdout: &out, Stderr: s.stderr}); err != nil {
		return 0, fmt.Errorf("Cannot compare versions %s and %s: %w", a, b, err)
	}
	value := strings.TrimRight(out.String(), "\n")
	if !versionOrderPattern.MatchString(value) {
		return 0, fmt.Errorf("Invalid vercmp result")
	}
	if strings.TrimLeft(strings.TrimPrefix(value, "-"), "0") == "" {
		return 0, nil
	}
	if strings.HasPrefix(value, "-") {
		return -1, nil
	}
	return 1, nil
}

func dependencyNames(data []byte, arch string) ([]string, error) {
	deps := []string{}
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		match := dependencyLinePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		selected := false
		for _, kind := range []string{"depends", "makedepends", "checkdepends"} {
			if match[1] == kind || match[1] == kind+"_"+arch {
				selected = true
				break
			}
		}
		if !selected {
			continue
		}
		name := match[2]
		if i := strings.IndexAny(name, "<>="); i >= 0 {
			name = name[:i]
		}
		if !dependencyNamePattern.MatchString(name) {
			return nil, fmt.Errorf("Invalid dependency: %s", match[2])
		}
		if !seen[name] {
			seen[name] = true
			deps = append(deps, name)
		}
	}
	return deps, nil
}
