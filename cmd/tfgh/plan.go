package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/storage"

	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/plan"
	"github.com/the-gopher/terraform-on-github/internal/scope"
	"github.com/the-gopher/terraform-on-github/internal/store"
	"github.com/the-gopher/terraform-on-github/internal/tf"
)

// runPlan implements M3: run terraform plan for the workspaces a PR affects. With
// --workspace it plans that one workspace (verified in scope); without it, every
// workspace the PR touches.
func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f sharedFlags
	fs := newFlagSet("plan", stderr)
	f.registerRepo(fs)
	f.registerPR(fs)
	f.registerConfigRef(fs)
	f.registerConfigFile(fs)
	f.registerJSON(fs, "Output plan summaries as JSON")
	workspace := fs.String("workspace", "", "Workspace name (optional, plans all affected if omitted)")
	root := fs.String("root", ".", "Path to the repository root")
	stageDir := fs.String("stage", "", "Directory to write plan artifacts (optional, for local testing)")
	stageGCS := fs.Bool("stage-gcs", false, "Stage artifacts to the §5 GCS buckets and sign meta.json with KMS")
	plansBucket := fs.String("plans-bucket", "", "Plan-artifact bucket (required with --stage-gcs)")
	runsBucket := fs.String("runs-bucket", "", "Coordination bucket (required with --stage-gcs)")
	kmsKey := fs.String("kms-key", "", "KMS cryptoKeyVersion for signing meta.json (required with --stage-gcs)")
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
	if *stageGCS && (*plansBucket == "" || *runsBucket == "" || *kmsKey == "") {
		return usageError("--stage-gcs requires --plans-bucket, --runs-bucket and --kms-key")
	}
	if *stageGCS && *stageDir != "" {
		return usageError("--stage and --stage-gcs are mutually exclusive")
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

	scopeRes, err := scope.NewScoper(client, cfg).Scope(ctx, owner, repo, f.pr)
	if err != nil {
		return fmt.Errorf("scoping PR: %w", err)
	}

	// §4.1: plan only against an up-to-date head. A base that has moved means the plan would
	// describe a merge that is not the one under review. behind/diverged must rebase first.
	for _, m := range scopeRes.Workspaces {
		if m.Status == scope.StatusBehind || m.Status == scope.StatusDiverged {
			return fmt.Errorf("PR is %s against the base for workspace %q; update the branch and re-plan", m.Status, m.Workspace.Name)
		}
	}

	targets, err := planTargets(cfg, scopeRes, *workspace)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintln(stdout, "No workspaces in scope to plan.")
		return nil
	}

	terraformPath, err := tf.FindTerraformBinary()
	if err != nil {
		return err
	}
	executor := tf.NewExecutor(terraformPath)

	var stager *gcsStager
	if *stageGCS {
		// The planned_tree_sha is the head's tree (§4.1): with up-to-dateness enforced, every
		// merge strategy produces the head's tree. Resolving it here (not from the plan) keeps
		// §6.2's comparison well-defined. One head, so one lookup for every workspace.
		headTree, err := client.GetCommitTree(ctx, owner, repo, scopeRes.HeadSHA)
		if err != nil {
			return fmt.Errorf("resolving head tree: %w", err)
		}
		stager, err = newGCSStager(ctx, *plansBucket, *runsBucket, *kmsKey, store.StageInput{
			Owner:          owner,
			Repo:           repo,
			PR:             f.pr,
			BaseSHA:        scopeRes.BaseSHA,
			HeadSHA:        scopeRes.HeadSHA,
			PlannedTreeSHA: headTree,
		})
		if err != nil {
			return err
		}
		defer func() { _ = stager.Close() }()
	}

	var results []plan.WorkspaceResult
	var failed []string
	for _, ws := range targets {
		fmt.Fprintf(stderr, "Planning workspace %q...\n", ws.Name)
		res, err := plan.Run(ctx, executor, ws, *root)
		if err != nil {
			// A timeout and a plan failure both stop the run here; the error says which.
			return fmt.Errorf("%w (workspace %q)", err, ws.Name)
		}

		if f.json {
			results = append(results, res)
		} else {
			fmt.Fprintf(stdout, "Workspace: %s\n", res.Workspace)
			fmt.Fprintf(stdout, "Has Changes: %v\n", res.HasChanges)
			fmt.Fprintf(stdout, "Exit Code: %d\n", res.ExitCode)
			fmt.Fprintf(stdout, "Plan File: %s\n", res.PlanFile)
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, res.Summary)
		}

		if res.Failed() {
			failed = append(failed, ws.Name)
		}

		if *stageDir != "" {
			dst, err := res.Stage(*stageDir)
			if err != nil {
				return err
			}
			fmt.Fprintf(stderr, "Artifacts written to %s\n", dst)
		}

		if stager != nil {
			staged, err := stager.Stage(ctx, ws, res)
			if err != nil {
				return fmt.Errorf("staging %q: %w", ws.Name, err)
			}
			if staged.CacheHit {
				fmt.Fprintf(stderr, "Plan for this (base, head) already staged — reusing artifact (§5.2)\n")
			} else {
				fmt.Fprintf(stderr, "Staged %s · run %s\n", staged.MetaKey, staged.RunKey)
			}
		}
	}

	if f.json {
		if err := printJSON(stdout, results); err != nil {
			return err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("plan failed for %s", strings.Join(failed, ", "))
	}
	return nil
}

// planTargets returns the workspaces to plan: the named one, which must be in scope, or every
// workspace in scope when name is empty.
func planTargets(cfg *config.Config, res *scope.Results, name string) ([]config.Workspace, error) {
	if name == "" {
		targets := make([]config.Workspace, 0, len(res.Workspaces))
		for _, m := range res.Workspaces {
			targets = append(targets, m.Workspace)
		}
		return targets, nil
	}

	ws, ok := cfg.Workspace(name)
	if !ok {
		return nil, fmt.Errorf("workspace %q not found in config", name)
	}
	inScope := slices.ContainsFunc(res.Workspaces, func(m scope.Match) bool {
		return m.Workspace.Name == ws.Name
	})
	if !inScope {
		return nil, fmt.Errorf("workspace %q is not in scope for this PR", name)
	}
	return []config.Workspace{ws}, nil
}

// gcsStager stages plan results to the two §5 buckets, signing meta.json with KMS. It holds
// one GCS and one KMS client for every workspace in the run.
type gcsStager struct {
	gcs  *storage.Client
	kms  *kms.KeyManagementClient
	base store.StageInput // the per-PR fields; Stage fills in the per-workspace ones

	artifacts, coordination store.Bucket
	signer                  store.Signer
}

func newGCSStager(ctx context.Context, plansBucket, runsBucket, kmsKey string, base store.StageInput) (*gcsStager, error) {
	gcs, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs client: %w", err)
	}
	kmsClient, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		_ = gcs.Close()
		return nil, fmt.Errorf("kms client: %w", err)
	}
	return &gcsStager{
		gcs:          gcs,
		kms:          kmsClient,
		base:         base,
		artifacts:    store.NewGCSBucket(gcs.Bucket(plansBucket)),
		coordination: store.NewGCSBucket(gcs.Bucket(runsBucket)),
		signer:       store.NewKMSSigner(kmsClient, kmsKey),
	}, nil
}

// Stage runs store.Stage for one workspace's result.
func (s *gcsStager) Stage(ctx context.Context, ws config.Workspace, res plan.WorkspaceResult) (*store.Staged, error) {
	in := s.base
	in.Workspace = ws.Name
	in.TerraformVersion = ws.TerraformVersion
	in.Now = time.Now()
	return store.Stage(ctx, s.artifacts, s.coordination, s.signer, res, in)
}

// Close releases both clients.
func (s *gcsStager) Close() error {
	return errors.Join(s.gcs.Close(), s.kms.Close())
}
