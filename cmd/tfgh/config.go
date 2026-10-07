package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/ghapp"
)

// runConfig implements M1: resolve the config ref, fetch the file at *that* ref, parse,
// validate, and print the normalized workspaces.
func runConfig(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f sharedFlags
	fs := newFlagSet("config", stderr)
	f.registerRepo(fs)
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
	client, err := newGitHubClient(ctx)
	if err != nil {
		return err
	}

	cfg, err := loadConfig(ctx, client, owner, repo, f.configRef, f.configFile)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	if f.json {
		return printJSON(stdout, cfg)
	}
	return printConfig(stdout, cfg)
}

// printConfig renders the normalized workspace set — the "what did the defaults actually
// resolve to" view that makes the command worth running over reading the YAML.
func printConfig(w io.Writer, cfg *config.Config) error {
	var b strings.Builder
	fmt.Fprintf(&b, "version: %d\n", cfg.Version)
	if len(cfg.ModuleSources) > 0 {
		fmt.Fprintf(&b, "module_sources: %s\n", strings.Join(cfg.ModuleSources, ", "))
	}
	if len(cfg.Workspaces) == 0 {
		b.WriteString("No workspaces defined.\n")
	}
	for _, ws := range cfg.Workspaces {
		fmt.Fprintf(&b, "\n- %s\n", ws.Name)
		fmt.Fprintf(&b, "    branch:      %s\n", ws.Branch)
		fmt.Fprintf(&b, "    dir:         %s\n", ws.Dir)
		if len(ws.Watch) > 0 {
			fmt.Fprintf(&b, "    watch:       %s\n", strings.Join(ws.Watch, ", "))
		}
		if ws.TerraformWorkspace != "" {
			fmt.Fprintf(&b, "    tf_workspace: %s\n", ws.TerraformWorkspace)
		}
		fmt.Fprintf(&b, "    terraform:   %s\n", ws.TerraformVersion)
		fmt.Fprintf(&b, "    backend:     gs://%s/%s\n", ws.Backend.Bucket, ws.Backend.Prefix)
		fmt.Fprintf(&b, "    impersonate: plan=%s apply=%s\n", ws.Impersonate.Plan, ws.Impersonate.Apply)
		fmt.Fprintf(&b, "    apply:       enabled, environment=%s on_stale=%s\n", ws.Environment(), ws.Apply.OnStale)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// loadConfig reads the config from localFile when set, otherwise from GitHub at ref, and
// returns it parsed and normalized — an un-normalized config reads back defaults as unset.
// Validation is the caller's call: scope deliberately skips it (see runScope).
func loadConfig(ctx context.Context, client *ghapp.Client, owner, repo, ref, localFile string) (*config.Config, error) {
	var raw []byte
	var err error
	if localFile != "" {
		raw, err = os.ReadFile(localFile) //nolint:gosec // G304: the operator names the file
		if err != nil {
			return nil, fmt.Errorf("reading local config: %w", err)
		}
	} else {
		if ref == "" {
			ref = "refs/heads/main"
		}
		raw, err = client.GetContents(ctx, owner, repo, config.Filename, ref)
		if err != nil {
			return nil, fmt.Errorf("loading config from GitHub: %w", err)
		}
	}

	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.Normalize()
	return cfg, nil
}
