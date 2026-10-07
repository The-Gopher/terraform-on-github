package main

import (
	"context"
	"fmt"
	"io"

	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/scope"
)

// runScope implements M2: filter the config's workspaces by the PR's base branch and changed
// paths, printing the path that pulled each one in.
func runScope(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f sharedFlags
	fs := newFlagSet("scope", stderr)
	f.registerRepo(fs)
	f.registerPR(fs)
	f.registerConfigRef(fs)
	f.registerConfigFile(fs)
	f.registerJSON(fs, "Output as JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	owner, repo, err := f.ownerRepo()
	if err != nil {
		return err
	}
	if err := f.requirePR(); err != nil {
		return err
	}
	client, err := newGitHubClient(ctx)
	if err != nil {
		return err
	}

	// Normalize only. Validate here would be the stricter, better behavior, but scope did not
	// validate before subcommands existed and turning it on now breaks every repo that uses the
	// `terraform_workspace` escape hatch — see validateNoOverlap and DESIGN.md §13.1.
	cfg, err := loadConfig(ctx, client, owner, repo, f.configRef, f.configFile)
	if err != nil {
		return err
	}

	res, err := scope.NewScoper(client, cfg).Scope(ctx, owner, repo, f.pr)
	if err != nil {
		return fmt.Errorf("scoping PR: %w", err)
	}

	if f.json {
		return printJSON(stdout, res)
	}

	if res.ConfigChanged {
		fmt.Fprintf(stdout, "Advisory: %s has changed in this PR. Changes take effect after merge to trusted ref.\n", config.Filename)
	}
	if len(res.Workspaces) == 0 {
		fmt.Fprintln(stdout, "No workspaces in scope.")
		return nil
	}
	for _, m := range res.Workspaces {
		fmt.Fprintf(stdout, "- %s [%s]: %s\n", m.Workspace.Name, m.Status, m.Reason)
	}
	return nil
}
