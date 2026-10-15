package maintenance

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *Service) InstallBuildDeps(ctx context.Context, name, candidateDir, sourceCommit, arch string) error {
	if sourceCommit == "" {
		return fmt.Errorf("Environment variable SOURCE_COMMIT is required")
	}
	candidate, err := s.validateCandidate(ctx, name, candidateDir, sourceCommit)
	if err != nil {
		return err
	}
	if arch == "" {
		var output bytes.Buffer
		if err := s.runner.Run(ctx, Command{Name: "uname", Args: []string{"-m"}, Stdout: &output, Stderr: s.stderr}); err != nil {
			return fmt.Errorf("Cannot determine build architecture: %w", err)
		}
		arch = strings.TrimRight(output.String(), "\n")
		if arch == "" {
			return fmt.Errorf("Empty build architecture")
		}
	}
	data, err := readRegularFile(filepath.Join(candidate.Dir, "package", ".SRCINFO"))
	if err != nil {
		return err
	}
	deps, err := dependencyNames(data, arch)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		return nil
	}
	args := append([]string{"-S", "--noconfirm", "--needed"}, deps...)
	if err := s.runner.Run(ctx, Command{Name: "pacman", Args: args, Stdout: s.stdout, Stderr: s.stderr}); err != nil {
		return fmt.Errorf("pacman failed: %w", err)
	}
	return nil
}

func (s *Service) Build(ctx context.Context, name, candidateDir, buildDir, sourceCommit string) error {
	if sourceCommit == "" {
		return fmt.Errorf("Environment variable SOURCE_COMMIT is required")
	}
	candidate, err := s.validateCandidate(ctx, name, candidateDir, sourceCommit)
	if err != nil {
		return err
	}
	buildDir, err = outsideDirectory(s.root, buildDir, candidate.Dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		return err
	}
	for _, file := range candidate.Package.ManagedFiles() {
		if err := copyFile(filepath.Join(candidate.Dir, "package", file), filepath.Join(buildDir, file)); err != nil {
			return err
		}
	}
	if err := s.runner.Run(ctx, Command{Name: "makepkg", Args: []string{"-f", "--noconfirm"}, Dir: buildDir, Stdout: s.stdout, Stderr: s.stderr}); err != nil {
		return fmt.Errorf("Build failed: %w", err)
	}
	return nil
}
