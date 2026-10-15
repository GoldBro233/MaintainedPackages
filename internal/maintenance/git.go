package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// git keeps checkout access compatible with the root-owned CI checkout.
func (s *Service) git(ctx context.Context, repo string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	argv := s.gitArgs(repo, args...)
	err := s.runner.Run(ctx, Command{Name: "git", Args: argv, Dir: repo, Stdout: &stdout, Stderr: s.stderr})
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", firstGitArg(args), err)
	}
	return stdout.Bytes(), nil
}

func (s *Service) gitArgs(repo string, args ...string) []string {
	argv := make([]string, 0, len(args)+5)
	if repo == s.root {
		argv = append(argv, "-c", "safe.directory="+s.root)
	}
	argv = append(argv, "--literal-pathspecs", "-C", repo)
	return append(argv, args...)
}

func firstGitArg(args []string) string {
	if len(args) == 0 {
		return "command"
	}
	return args[0]
}

func gitExitCode(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

type gitEntry struct {
	mode, kind, object, path string
}

func parseGitEntries(data []byte) ([]gitEntry, error) {
	entries := make([]gitEntry, 0)
	for len(data) > 0 {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			return nil, fmt.Errorf("unterminated Git tree entry")
		}
		record := string(data[:end])
		data = data[end+1:]
		header, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if !ok || path == "" || len(fields) != 3 {
			return nil, fmt.Errorf("invalid Git tree entry")
		}
		entries = append(entries, gitEntry{mode: fields[0], kind: fields[1], object: fields[2], path: path})
	}
	return entries, nil
}

func (e gitEntry) regular() bool {
	return e.kind == "blob" && (e.mode == "100644" || e.mode == "100755")
}

// gitBlob never follows Git symlinks or treats a tree or submodule as data.
func (s *Service) gitBlob(ctx context.Context, repo, ref, path string) ([]byte, error) {
	data, err := s.git(ctx, repo, "ls-tree", "-z", ref, "--", path)
	if err != nil {
		return nil, err
	}
	entries, err := parseGitEntries(data)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) != 1 || entries[0].path != path || !entries[0].regular() {
		return nil, fmt.Errorf("non-regular Git entry: %s", path)
	}
	data, err = s.git(ctx, repo, "cat-file", "blob", entries[0].object)
	if err != nil {
		return nil, fmt.Errorf("cannot read Git blob %s: %w", path, err)
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

func (s *Service) blobHash(ctx context.Context, repo, ref, path string) (*string, error) {
	data, err := s.gitBlob(ctx, repo, ref, path)
	if err != nil || data == nil {
		return nil, err
	}
	hash := hashBytes(data)
	return &hash, nil
}

func (s *Service) gitHashes(ctx context.Context, repo string, ref *string, prefix string, files []string) (map[string]*string, error) {
	hashes := make(map[string]*string, len(files))
	for _, file := range files {
		var hash *string
		if ref != nil {
			var err error
			hash, err = s.blobHash(ctx, repo, *ref, prefix+file)
			if err != nil {
				return nil, err
			}
		}
		hashes[file] = hash
	}
	return hashes, nil
}

type aurState struct {
	Dir    string
	Commit *string
}

func (s *Service) aurSnapshot(ctx context.Context, name, dest string) (aurState, error) {
	if _, err := s.git(ctx, s.root, "clone", "-q", "https://aur.archlinux.org/"+name+".git", dest); err != nil {
		return aurState{}, fmt.Errorf("cannot read AUR repository: %w", err)
	}
	state := aurState{Dir: dest}
	commit, err := s.git(ctx, dest, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/master")
	if err != nil {
		if !gitExitCode(err, 1) {
			return aurState{}, fmt.Errorf("cannot inspect AUR master: %w", err)
		}
		refs, err := s.git(ctx, dest, "ls-remote", "origin")
		if err != nil {
			return aurState{}, fmt.Errorf("cannot read AUR refs: %w", err)
		}
		if len(bytes.TrimSpace(refs)) != 0 {
			return aurState{}, fmt.Errorf("AUR master is missing")
		}
		return state, nil
	}
	ref := strings.TrimSpace(string(commit))
	if ref == "" {
		return aurState{}, fmt.Errorf("AUR master is missing")
	}
	state.Commit = &ref
	if _, err := s.git(ctx, dest, "checkout", "-q", "-B", "master", ref); err != nil {
		return aurState{}, fmt.Errorf("cannot checkout AUR master: %w", err)
	}
	metadata, err := s.gitBlob(ctx, dest, ref, ".SRCINFO")
	if err != nil {
		return aurState{}, err
	}
	if metadata == nil {
		return aurState{}, fmt.Errorf("AUR .SRCINFO is missing")
	}
	return state, nil
}
