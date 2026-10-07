// Command tfgh runs the terraform-on-github steps by hand, one verb per step, against a
// repository's .terraform-on-github.yaml. See docs/ROADMAP.md §3.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/the-gopher/terraform-on-github/internal/ghapp"
)

const usage = `tfgh — terraform-on-github

Usage:
  tfgh <command> [flags]

Commands:
  config   Fetch, validate and print a repository's normalized workspace set
  scope    Given a PR, print the workspaces it affects and why
  plan     Run terraform plan for the workspaces a PR affects (--stage-gcs to stage §5 artifacts)
  apply    Verify that a merged PR's plan may be applied ("tfgh apply verify")

Run "tfgh <command> -h" for a command's flags.

Environment:
  GITHUB_TOKEN   required by every command
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()

	if err != nil && !errors.Is(err, flag.ErrHelp) && !errors.Is(err, errBadFlags) {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	os.Exit(exitCode(err))
}

// run dispatches to a subcommand. It never exits the process: every failure comes back as an
// error, so deferred cleanups run and main owns the exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return usageError("no command given")
	}

	cmd, args := args[0], args[1:]
	switch cmd {
	case "config":
		return runConfig(ctx, args, stdout, stderr)
	case "scope":
		return runScope(ctx, args, stdout, stderr)
	case "plan":
		return runPlan(ctx, args, stdout, stderr)
	case "apply":
		return runApply(ctx, args, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return usageError(fmt.Sprintf("unknown command %q", cmd))
	}
}

// usageError is a bad invocation. main exits 2 for it, the flag package's convention.
type usageError string

func (e usageError) Error() string { return string(e) }

// errBadFlags is a flag-parsing failure the flag package has already reported, with the
// command's usage. main exits 2 without printing it again.
var errBadFlags = errors.New("invalid flags")

func exitCode(err error) int {
	var ue usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ue), errors.Is(err, errBadFlags):
		return 2
	default:
		return 1
	}
}

// newFlagSet returns a FlagSet that reports parse errors instead of exiting.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	return errBadFlags
}

// sharedFlags are the flags several subcommands take. Each subcommand registers the subset
// it accepts.
type sharedFlags struct {
	repo       string
	pr         int
	configRef  string
	configFile string
	json       bool
}

func (f *sharedFlags) registerRepo(fs *flag.FlagSet) {
	fs.StringVar(&f.repo, "repo", "", "Repository (owner/repo)")
}

func (f *sharedFlags) registerPR(fs *flag.FlagSet) {
	fs.IntVar(&f.pr, "pr", 0, "PR number")
}

func (f *sharedFlags) registerConfigRef(fs *flag.FlagSet) {
	fs.StringVar(&f.configRef, "config-ref", "", "Config ref override (default refs/heads/main)")
}

func (f *sharedFlags) registerConfigFile(fs *flag.FlagSet) {
	fs.StringVar(&f.configFile, "config", "", "Path to local config file")
}

func (f *sharedFlags) registerJSON(fs *flag.FlagSet, usage string) {
	fs.BoolVar(&f.json, "json", false, usage)
}

// ownerRepo splits --repo into its owner and repository name.
func (f *sharedFlags) ownerRepo() (owner, name string, err error) {
	owner, name, ok := strings.Cut(f.repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", usageError("--repo must be in owner/repo format")
	}
	return owner, name, nil
}

// requirePR rejects a missing or non-positive --pr.
func (f *sharedFlags) requirePR() error {
	if f.pr <= 0 {
		return usageError("--pr must be a positive PR number")
	}
	return nil
}

// newGitHubClient builds a client authenticated with GITHUB_TOKEN.
func newGitHubClient(ctx context.Context) (*ghapp.Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN environment variable is required")
	}
	return ghapp.NewClient(ctx, token), nil
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	return nil
}
