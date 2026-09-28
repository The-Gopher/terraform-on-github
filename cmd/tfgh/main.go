// Command tfgh runs the terraform-on-github steps by hand, one verb per step, against a
// repository's .terraform-on-github.yaml. See docs/ROADMAP.md §3.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/ghapp"
	"github.com/the-gopher/terraform-on-github/internal/scope"
	"github.com/the-gopher/terraform-on-github/internal/tf"
)

const usage = `tfgh — terraform-on-github

Usage:
  tfgh <command> [flags]

Commands:
  config   Fetch, validate and print a repository's normalized workspace set
  scope    Given a PR, print the workspaces it affects and why
  plan     Run terraform plan for the workspaces a PR affects

Run "tfgh <command> -h" for a command's flags.

Environment:
  GITHUB_TOKEN   required by every command
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	args := os.Args[2:]
	switch os.Args[1] {
	case "config":
		runConfig(args)
	case "scope":
		runScope(args)
	case "plan":
		runPlan(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

// runConfig implements M1: resolve the config ref, fetch the file at *that* ref, parse,
// validate, and print the normalized workspaces.
func runConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	repo := fs.String("repo", "", "Repository (owner/repo)")
	configRef := fs.String("config-ref", "", "Config ref override (default refs/heads/main)")
	configFile := fs.String("config", "", "Path to local config file")
	jsonOut := fs.Bool("json", false, "Output as JSON")
	_ = fs.Parse(args)

	if *repo == "" {
		fatalf("--repo is required")
	}

	ctx := context.Background()
	client, owner, repoName := setup(*repo)

	cfg := loadConfig(ctx, client, owner, repoName, *configRef, *configFile, validate)

	if *jsonOut {
		printJSON(cfg)
		return
	}

	printConfig(os.Stdout, cfg)
}

// printConfig renders the normalized workspace set — the "what did the defaults actually
// resolve to" view that makes the command worth running over reading the YAML.
// nolint:errcheck
func printConfig(w io.Writer, cfg *config.Config) {
	fmt.Fprintf(w, "version: %d\n", cfg.Version)
	if len(cfg.ModuleSources) > 0 {
		fmt.Fprintf(w, "module_sources: %s\n", strings.Join(cfg.ModuleSources, ", "))
	}
	if len(cfg.Workspaces) == 0 {
		fmt.Fprintln(w, "No workspaces defined.")
		return
	}
	for _, ws := range cfg.Workspaces {
		fmt.Fprintf(w, "\n- %s\n", ws.Name)
		fmt.Fprintf(w, "    branch:      %s\n", ws.Branch)
		fmt.Fprintf(w, "    dir:         %s\n", ws.Dir)
		if len(ws.Watch) > 0 {
			fmt.Fprintf(w, "    watch:       %s\n", strings.Join(ws.Watch, ", "))
		}
		if ws.TerraformWorkspace != "" {
			fmt.Fprintf(w, "    tf_workspace: %s\n", ws.TerraformWorkspace)
		}
		fmt.Fprintf(w, "    terraform:   %s\n", ws.TerraformVersion)
		fmt.Fprintf(w, "    backend:     gs://%s/%s\n", ws.Backend.Bucket, ws.Backend.Prefix)
		fmt.Fprintf(w, "    impersonate: plan=%s apply=%s\n", ws.Impersonate.Plan, ws.Impersonate.Apply)
		fmt.Fprintf(w, "    apply:       enabled, environment=%s on_stale=%s\n", ws.Environment(), ws.Apply.OnStale)
	}
}

// runScope implements M2: filter the config's workspaces by the PR's base branch and changed
// paths, printing the path that pulled each one in.
func runScope(args []string) {
	fs := flag.NewFlagSet("scope", flag.ExitOnError)
	repo := fs.String("repo", "", "Repository (owner/repo)")
	pr := fs.String("pr", "", "PR number")
	configRef := fs.String("config-ref", "", "Config ref override (default refs/heads/main)")
	configFile := fs.String("config", "", "Path to local config file")
	jsonOut := fs.Bool("json", false, "Output as JSON")
	_ = fs.Parse(args)

	if *repo == "" || *pr == "" {
		fatalf("--repo and --pr are required")
	}

	ctx := context.Background()
	client, owner, repoName := setup(*repo)

	// Normalize only. Validate here would be the stricter, better behavior, but scope did not
	// validate before subcommands existed and turning it on now breaks every repo that uses the
	// `terraform_workspace` escape hatch — see validateNoOverlap and DESIGN.md §13.1.
	cfg := loadConfig(ctx, client, owner, repoName, *configRef, *configFile, normalizeOnly)

	scoper := scope.NewScoper(client, cfg)
	res, err := scoper.Scope(ctx, owner, repoName, parsePR(*pr))
	if err != nil {
		fatalf("scoping PR: %v", err)
	}

	if *jsonOut {
		printJSON(res)
		return
	}

	if res.ConfigChanged {
		fmt.Printf("Advisory: %s has changed in this PR. Changes take effect after merge to trusted ref.\n", config.Filename)
	}

	if len(res.Workspaces) == 0 {
		fmt.Println("No workspaces in scope.")
		return
	}

	for _, w := range res.Workspaces {
		fmt.Printf("- %s [%s]: %s\n", w.Workspace.Name, w.Status, w.Reason)
	}
}

// runPlan implements M3: run terraform plan for the workspaces a PR affects. With
// --workspace it plans that one workspace (verified in scope); without it, every
// workspace the PR touches.
func runPlan(args []string) {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	repo := fs.String("repo", "", "Repository (owner/repo)")
	pr := fs.String("pr", "", "PR number")
	workspace := fs.String("workspace", "", "Workspace name (optional, plans all affected if omitted)")
	configRef := fs.String("config-ref", "", "Config ref override (default refs/heads/main)")
	configFile := fs.String("config", "", "Path to local config file")
	root := fs.String("root", ".", "Path to the repository root")
	stage := fs.String("stage", "", "Directory to write plan artifacts (optional, for local testing)")
	jsonOut := fs.Bool("json", false, "Output plan summaries as JSON")
	_ = fs.Parse(args)

	if *repo == "" || *pr == "" {
		fs.Usage()
		os.Exit(1)
	}

	ctx := context.Background()
	client, owner, repoName := setup(*repo)

	cfg := loadConfig(ctx, client, owner, repoName, *configRef, *configFile, validate)

	scoper := scope.NewScoper(client, cfg)
	res, err := scoper.Scope(ctx, owner, repoName, parsePR(*pr))
	if err != nil {
		fatalf("scoping PR: %v", err)
	}

	var targets []string
	if *workspace != "" {
		ws, ok := cfg.Workspace(*workspace)
		if !ok {
			fatalf("workspace %q not found in config", *workspace)
		}
		inScope := false
		for _, w := range res.Workspaces {
			if w.Workspace.Name == ws.Name {
				inScope = true
				break
			}
		}
		if !inScope {
			fatalf("workspace %q is not in scope for this PR", *workspace)
		}
		targets = []string{*workspace}
	} else {
		for _, w := range res.Workspaces {
			targets = append(targets, w.Workspace.Name)
		}
	}
	if len(targets) == 0 {
		fmt.Println("No workspaces in scope to plan.")
		return
	}

	terraformPath, err := tf.FindTerraformBinary()
	if err != nil {
		fatalf("%v", err)
	}
	executor := tf.NewExecutor(terraformPath)

	var summaries []map[string]any
	failed := false

	for _, name := range targets {
		ws, ok := cfg.Workspace(name)
		if !ok {
			fatalf("workspace %q not found in config", name)
		}

		workDir := *root
		if ws.Dir != "" {
			workDir = filepath.Join(*root, ws.Dir)
		}

		backendConfig := map[string]string{
			"bucket": ws.Backend.Bucket,
			"prefix": ws.Backend.Prefix,
		}

		fmt.Fprintf(os.Stderr, "Running terraform init in %s...\n", workDir)
		if err := executor.Init(ctx, workDir, backendConfig); err != nil {
			fatalf("terraform init failed: %v", err)
		}

		fmt.Fprintf(os.Stderr, "Running terraform plan in %s...\n", workDir)
		planOpts := tf.PlanOptions{
			TerraformVersion:   ws.TerraformVersion,
			PlanTimeout:        ws.PlanTimeout.Duration(),
			VarFiles:           ws.VarFiles,
			PlanArgs:           ws.PlanArgs,
			TerraformWorkspace: ws.TerraformWorkspace,
			Lock:               false,
			BackendConfig:      backendConfig,
		}

		result, err := executor.Plan(ctx, workDir, planOpts)
		if err != nil {
			fatalf("terraform plan failed: %v", err)
		}

		planJSON, err := executor.ShowPlanJSON(ctx, result.PlanFile)
		if err != nil {
			fatalf("terraform show -json failed: %v", err)
		}

		planRaw, err := executor.ShowPlanRaw(ctx, result.PlanFile)
		if err != nil {
			fatalf("terraform show failed: %v", err)
		}

		summary := buildPlanSummary(planJSON, ws.SummaryDetail)

		if *jsonOut {
			summaries = append(summaries, map[string]any{
				"workspace":   ws.Name,
				"has_changes": result.HasChanges,
				"exit_code":   result.ExitCode,
				"plan_file":   result.PlanFile,
				"summary":     summary,
				"plan_raw":    planRaw,
			})
		} else {
			fmt.Printf("Workspace: %s\n", ws.Name)
			fmt.Printf("Has Changes: %v\n", result.HasChanges)
			fmt.Printf("Exit Code: %d\n", result.ExitCode)
			fmt.Printf("Plan File: %s\n", result.PlanFile)
			fmt.Println()
			fmt.Println(summary)
		}

		if result.ExitCode != 0 && result.ExitCode != 2 {
			failed = true
		}

		if *stage != "" {
			stageOne(ws.Name, planJSON, planRaw, *stage)
		}
	}

	if *jsonOut {
		printJSON(summaries)
	}
	if failed {
		os.Exit(1)
	}
}

// stageOne writes one workspace's plan artifacts into the stage directory, namespaced by
// workspace name so a multi-workspace run does not overwrite itself.
func stageOne(name string, plan *tfjson.Plan, planRaw, stage string) {
	dir := filepath.Join(stage, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		fatalf("creating stage directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tfplan.txt"), []byte(planRaw), 0644); err != nil {
		fatalf("writing plan raw: %v", err)
	}
	if planJSONBytes, err := json.MarshalIndent(plan, "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "plan.json"), planJSONBytes, 0644); err != nil {
			fatalf("writing plan json: %v", err)
		}
	}
	fmt.Fprintf(os.Stderr, "Artifacts written to %s\n", dir)
}

// buildPlanSummary renders the plan summary via internal/tf, shared with the M6 worker.
func buildPlanSummary(plan *tfjson.Plan, detail config.SummaryDetail) string {
	return tf.BuildPlanSummary(plan, detail)
}

// setup validates the shared flags every command takes and builds an authenticated client.
func setup(repo string) (*ghapp.Client, string, string) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fatalf("GITHUB_TOKEN environment variable is required")
	}

	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		fatalf("--repo must be in owner/repo format")
	}

	return ghapp.NewClient(context.Background(), token), parts[0], parts[1]
}

// Whether loadConfig runs Config.Validate after normalizing.
const (
	validate      = true
	normalizeOnly = false
)

// loadConfig fetches the config at ref, always normalizing it — an un-normalized config reads
// back defaults as unset — and validating it when the caller asks.
func loadConfig(ctx context.Context, client *ghapp.Client, owner, repo, ref, localFile string, check bool) *config.Config {
	var raw []byte
	var err error

	if localFile != "" {
		raw, err = os.ReadFile(localFile)
		if err != nil {
			fatalf("reading local config: %v", err)
		}
	} else {
		if ref == "" {
			ref = "refs/heads/main"
		}
		raw, err = client.GetContents(ctx, owner, repo, config.Filename, ref)
		if err != nil {
			fatalf("loading config from GitHub: %v", err)
		}
	}

	cfg, err := config.Parse(raw)
	if err != nil {
		fatalf("parsing config: %v", err)
	}
	cfg.Normalize()

	if check {
		if err := cfg.Validate(); err != nil {
			fatalf("invalid config: %v", err)
		}
	}
	return cfg
}

func printJSON(v any) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatalf("encoding JSON: %v", err)
	}
	fmt.Println(string(out))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

// parsePR accepts a PR number as a string flag value.
func parsePR(s string) int {
	var pr int
	_, err := fmt.Sscanf(s, "%d", &pr)
	if err != nil {
		return 0
	}
	return pr
}