package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GoldBro233/MaintainedPackages/internal/maintenance"
)

const usage = `Usage:
  maintpkg [--repo <path>] matrix [name]
  maintpkg [--repo <path>] prepare <name> <candidate-dir>
  maintpkg [--repo <path>] install-build-deps <name> <candidate-dir>
  maintpkg [--repo <path>] build <name> <candidate-dir> <build-dir>
  maintpkg [--repo <path>] publish <name> <candidate-dir>

--repo defaults to the current directory and must precede the command.
prepare: SOURCE_COMMIT (optional), FORCE_BUILD (true/false, default false).
install-build-deps/build/publish: SOURCE_COMMIT is required.
install-build-deps: CARCH defaults to uname -m.
publish: GITHUB_TARGET_BRANCH must be main; credentials are required only for writes.
`

// Run accepts arguments without argv[0]; environment and streams belong to the caller.
func Run(ctx context.Context, args []string, env []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("maintpkg", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	repo := flags.String("repo", ".", "repository path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if _, err := io.WriteString(stdout, usage); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return 0
		}
		fmt.Fprintf(stderr, "%v\n%s", err, usage)
		return 2
	}
	args = flags.Args()
	badArgs := func() int { fmt.Fprint(stderr, usage); return 2 }
	if len(args) == 0 {
		return badArgs()
	}
	command := args[0]
	switch command {
	case "matrix":
		if len(args) > 2 || (len(args) == 2 && args[1] == "") {
			return badArgs()
		}
	case "prepare", "install-build-deps", "publish":
		if len(args) != 3 || args[1] == "" {
			return badArgs()
		}
	case "build":
		if len(args) != 4 || args[1] == "" {
			return badArgs()
		}
	default:
		fmt.Fprintf(stderr, "Unknown command: %s\n", command)
		return badArgs()
	}
	values := make(map[string]string, len(env))
	childEnv := make([]string, 0, len(env))
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
			if key != "AUR_SSH_PRIVATE_KEY" && key != "GITHUB_TOKEN" {
				childEnv = append(childEnv, entry)
			}
		}
	}
	fail := func(err error) int { fmt.Fprintln(stderr, err); return 1 }
	source := values["SOURCE_COMMIT"]
	if command == "install-build-deps" || command == "build" || command == "publish" {
		if source == "" {
			return fail(errors.New("Environment variable SOURCE_COMMIT is required"))
		}
	}
	force := false
	if command == "prepare" {
		switch values["FORCE_BUILD"] {
		case "", "false":
		case "true":
			force = true
		default:
			return fail(errors.New("FORCE_BUILD must be true or false"))
		}
	}
	s, err := maintenance.New(*repo, maintenance.ExecRunner{Env: childEnv}, &http.Client{}, stdout, stderr)
	if err != nil {
		return fail(err)
	}
	switch command {
	case "matrix":
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		matrix, err := s.Matrix(name)
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(stdout).Encode(matrix); err != nil {
			return fail(err)
		}
	case "prepare":
		result, err := s.Prepare(ctx, args[1], args[2], maintenance.PrepareOptions{SourceCommit: source, ForceBuild: force})
		if err != nil {
			return fail(err)
		}
		if _, err := fmt.Fprintf(stdout, "needs_build=%t\nneeds_publish=%t\ncandidate_version=%s\n", result.NeedsBuild, result.NeedsPublish, result.CandidateVersion); err != nil {
			return fail(err)
		}
	case "install-build-deps":
		err = s.InstallBuildDeps(ctx, args[1], args[2], source, values["CARCH"])
	case "build":
		err = s.Build(ctx, args[1], args[2], args[3], source)
	case "publish":
		err = s.Publish(ctx, args[1], args[2], maintenance.PublishOptions{SourceCommit: source, TargetBranch: values["GITHUB_TARGET_BRANCH"], AURSSHPrivateKey: values["AUR_SSH_PRIVATE_KEY"], GitHubToken: values["GITHUB_TOKEN"]})
	}
	if err != nil {
		return fail(err)
	}
	return 0
}
