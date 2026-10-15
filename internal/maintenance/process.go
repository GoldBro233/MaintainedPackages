package maintenance

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Command struct {
	Name           string
	Args           []string
	Dir            string
	Env            []string
	Stdout, Stderr io.Writer
}

type Runner interface {
	Run(context.Context, Command) error
}

type ExecRunner struct{ Env []string }

func mergeEnv(base, overrides []string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	indices := make(map[string]int, len(base)+len(overrides))
	add := func(entry string) {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return
		}
		if i, exists := indices[key]; exists {
			result[i] = entry
		} else {
			indices[key] = len(result)
			result = append(result, entry)
		}
	}
	for _, entry := range base {
		add(entry)
	}
	for _, entry := range overrides {
		add(entry)
	}
	return result
}

// exec.Command resolves names against the parent PATH before Cmd.Env is set.
// Resolve against the effective child environment instead, without global mutation.
func executable(name, dir string, env []string) (string, error) {
	base, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	absolute := func(path string) string {
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Join(base, path)
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return absolute(name), nil
	}
	var search string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			search = value
			break
		}
	}
	for _, path := range filepath.SplitList(search) {
		candidate := absolute(filepath.Join(path, name))
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func (r ExecRunner) Run(ctx context.Context, command Command) error {
	env := mergeEnv(r.Env, command.Env)
	path, err := executable(command.Name, command.Dir, env)
	if err != nil {
		return fmt.Errorf("%s: %w", command.Name, err)
	}
	cmd := exec.CommandContext(ctx, path, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = env
	cmd.Stdout = command.Stdout
	cmd.Stderr = command.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", command.Name, err)
	}
	return nil
}
