package maintenance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Config struct {
	Packages []Package `json:"packages"`
}

type Package struct {
	Name          string   `json:"name"`
	Enabled       bool     `json:"enabled"`
	UpstreamRepo  string   `json:"upstream_repo"`
	TagPrefix     string   `json:"tag_prefix"`
	AURExtraFiles []string `json:"aur_extra_files"`
}

var packageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9@._+\-]*$`)
var upstreamRepoPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.\-]{1,100}$`)
var tagPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9._+\-]*$`)

func jsonObject(data []byte) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	return object, nil
}

func jsonString(data json.RawMessage) (string, error) {
	var value *string
	if err := json.Unmarshal(data, &value); err != nil {
		return "", err
	}
	if value == nil {
		return "", fmt.Errorf("expected string")
	}
	return *value, nil
}

func parseConfig(data []byte) (Config, error) {
	invalid := func(err error) (Config, error) { return Config{}, fmt.Errorf("Invalid packages.json: %w", err) }
	object, err := jsonObject(data)
	if err != nil {
		return invalid(err)
	}
	if len(object) != 1 || object["packages"] == nil {
		return invalid(fmt.Errorf("expected only packages"))
	}
	var records []json.RawMessage
	if err := json.Unmarshal(object["packages"], &records); err != nil {
		return invalid(err)
	}
	if records == nil {
		return invalid(fmt.Errorf("packages must be an array"))
	}
	config := Config{Packages: make([]Package, 0, len(records))}
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		fields, err := jsonObject(record)
		if err != nil {
			return invalid(err)
		}
		for key := range fields {
			switch key {
			case "name", "enabled", "upstream_repo", "tag_prefix", "aur_extra_files":
			default:
				return invalid(fmt.Errorf("unknown package field %q", key))
			}
		}
		p := Package{AURExtraFiles: []string{}}
		p.Name, err = jsonString(fields["name"])
		if err != nil || !packageNamePattern.MatchString(p.Name) {
			return invalid(fmt.Errorf("invalid package name"))
		}
		p.UpstreamRepo, err = jsonString(fields["upstream_repo"])
		if err != nil || !upstreamRepoPattern.MatchString(p.UpstreamRepo) {
			return invalid(fmt.Errorf("invalid upstream_repo for %s", p.Name))
		}
		repoName := strings.SplitN(p.UpstreamRepo, "/", 2)[1]
		if repoName == "." || repoName == ".." {
			return invalid(fmt.Errorf("invalid upstream_repo for %s", p.Name))
		}
		p.TagPrefix, err = jsonString(fields["tag_prefix"])
		if err != nil || !tagPrefixPattern.MatchString(p.TagPrefix) {
			return invalid(fmt.Errorf("invalid tag_prefix for %s", p.Name))
		}
		var enabled *bool
		if err := json.Unmarshal(fields["enabled"], &enabled); err != nil || enabled == nil {
			return invalid(fmt.Errorf("enabled must be a boolean for %s", p.Name))
		}
		p.Enabled = *enabled
		if raw := fields["aur_extra_files"]; raw != nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
			var extras []json.RawMessage
			if err := json.Unmarshal(raw, &extras); err != nil || extras == nil {
				return invalid(fmt.Errorf("aur_extra_files must be an array for %s", p.Name))
			}
			unique := make(map[string]bool, len(extras))
			for _, extra := range extras {
				file, err := jsonString(extra)
				if err != nil || !validExtraPath(file) || unique[file] {
					return invalid(fmt.Errorf("invalid or duplicate aur_extra_files for %s", p.Name))
				}
				unique[file] = true
				p.AURExtraFiles = append(p.AURExtraFiles, file)
			}
		}
		if seen[p.Name] {
			return invalid(fmt.Errorf("duplicate package %s", p.Name))
		}
		seen[p.Name] = true
		config.Packages = append(config.Packages, p)
	}
	return config, nil
}

func validExtraPath(path string) bool {
	if path == "" || strings.Contains(path, "\\") || path == "PKGBUILD" || path == ".SRCINFO" {
		return false
	}
	for _, c := range path {
		if c < 32 || c == 127 {
			return false
		}
	}
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".", "..", ".git", "src", "pkg":
			return false
		}
	}
	return true
}

func loadConfig(root string) (Config, error) {
	data, err := readRegularFile(filepath.Join(root, "packages.json"))
	if err != nil {
		return Config{}, err
	}
	config, err := parseConfig(data)
	if err != nil {
		return Config{}, err
	}
	for _, p := range config.Packages {
		for _, file := range []string{"PKGBUILD", ".SRCINFO"} {
			if err := regularFile(filepath.Join(root, "packages", p.Name, file)); err != nil {
				return Config{}, err
			}
		}
		for _, extra := range p.AURExtraFiles {
			path, err := safePath(filepath.Join(root, "packages", p.Name, extra))
			if err != nil {
				return Config{}, err
			}
			if _, err := os.Lstat(path); err == nil {
				if err := regularFile(path); err != nil {
					return Config{}, err
				}
			} else if !os.IsNotExist(err) {
				return Config{}, err
			}
		}
	}
	return config, nil
}

func (c Config) selectPackage(name string) (Package, error) {
	for _, p := range c.Packages {
		if p.Name == name {
			if !p.Enabled {
				return Package{}, fmt.Errorf("Disabled package: %s", name)
			}
			return p, nil
		}
	}
	return Package{}, fmt.Errorf("Unknown package: %s", name)
}

func (p Package) ManagedFiles() []string {
	files := make([]string, 0, len(p.AURExtraFiles)+2)
	files = append(files, "PKGBUILD", ".SRCINFO")
	return append(files, p.AURExtraFiles...)
}
