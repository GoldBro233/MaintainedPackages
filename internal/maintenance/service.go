package maintenance

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Service holds immutable operation dependencies. Each operation owns its state.
type Service struct {
	root           string
	config         Config
	runner         Runner
	client         *http.Client
	stdout, stderr io.Writer
}

type PrepareOptions struct {
	SourceCommit string
	ForceBuild   bool
}

type PublishOptions struct {
	SourceCommit, TargetBranch, AURSSHPrivateKey, GitHubToken string
}

type PrepareResult struct {
	NeedsBuild, NeedsPublish bool
	CandidateVersion         string
}

type Matrix struct {
	Include []MatrixEntry `json:"include"`
}

type MatrixEntry struct {
	Package string `json:"package"`
}

func New(root string, runner Runner, client *http.Client, stdout, stderr io.Writer) (*Service, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("repository path: %w", err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("repository path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("repository: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("repository is not a directory: %s", abs)
	}
	config, err := loadConfig(abs)
	if err != nil {
		return nil, err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &Service{root: abs, config: config, runner: runner, client: client, stdout: stdout, stderr: stderr}, nil
}

func (s *Service) Matrix(name string) (Matrix, error) {
	matrix := Matrix{Include: make([]MatrixEntry, 0)}
	if name != "" {
		p, err := s.config.selectPackage(name)
		if err != nil {
			return Matrix{}, err
		}
		matrix.Include = append(matrix.Include, MatrixEntry{Package: p.Name})
		return matrix, nil
	}
	for _, p := range s.config.Packages {
		if p.Enabled {
			matrix.Include = append(matrix.Include, MatrixEntry{Package: p.Name})
		}
	}
	return matrix, nil
}
