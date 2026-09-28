// Package plan orchestrates one plan lifecycle — init, plan, show, summary — for a single
// workspace. It is the M3 boundary the M6 worker reaches instead of shelling out: the CLI
// and the future service run the same Planner, so a plan means one thing everywhere.
package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/tf"
)

// WorkspaceResult is one workspace's plan outcome, ready to render — the CLI prints it,
// M6's worker serializes it into a check run.
type WorkspaceResult struct {
	Workspace  string
	HasChanges bool
	ExitCode   int
	PlanFile   string
	Summary    string
	PlanRaw    string

	// PlanJSON is the parsed plan, for callers that need addresses (staging, checks).
	PlanJSON *tfjson.Plan
}

// Stage writes the workspace's artifacts to dir/ as the §5 staging skeleton: tfplan.txt
// (human) and plan.json (machine). M4 replaces the layout with the GCS key and KMS
// signature; the local write stays for the CLI's --stage path.
func (r WorkspaceResult) Stage(dir string) (string, error) {
	dst := filepath.Join(dir, r.Workspace)
	if err := os.MkdirAll(dst, 0755); err != nil {
		return "", fmt.Errorf("creating stage directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "tfplan.txt"), []byte(r.PlanRaw), 0644); err != nil {
		return "", fmt.Errorf("writing plan raw: %w", err)
	}
	raw, err := json.MarshalIndent(r.PlanJSON, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding plan json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "plan.json"), raw, 0644); err != nil {
		return "", fmt.Errorf("writing plan json: %w", err)
	}
	return dst, nil
}

// Error distinguishes the two ways a plan goes wrong: a timeout is a retry with a bigger
// budget; anything else is a repo problem. Exit codes 0 and 2 are both success (§4.3).
func (r WorkspaceResult) Failed() bool {
	return r.ExitCode != 0 && r.ExitCode != 2
}

// TimeoutError is the error Run returns when plan_timeout fires; it wraps
// tf.ErrPlanTimeout so callers can errors.Is the verdict.
func TimeoutError(timeout time.Duration) error {
	return fmt.Errorf("%w after %v; raise plan_timeout and retry", tf.ErrPlanTimeout, timeout)
}

// Run plans one workspace end to end: init (GCS backend), plan (-lock=false -out=tfplan,
// -detailed-exitcode semantics live in the executor), then show -json and show. A timeout
// surfaces as a distinct error rather than a plan failure.
func Run(ctx context.Context, executor tf.Executor, ws config.Workspace, root string) (WorkspaceResult, error) {
	workDir := root
	if ws.Dir != "" {
		workDir = filepath.Join(root, ws.Dir)
	}

	backendConfig := map[string]string{
		"bucket": ws.Backend.Bucket,
		"prefix": ws.Backend.Prefix,
	}

	if err := executor.Init(ctx, workDir, backendConfig); err != nil {
		return WorkspaceResult{}, fmt.Errorf("terraform init failed: %w", err)
	}

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
		if result.TimedOut {
			return WorkspaceResult{Workspace: ws.Name, ExitCode: 1}, TimeoutError(planOpts.PlanTimeout)
		}
		return WorkspaceResult{Workspace: ws.Name, ExitCode: 1}, fmt.Errorf("terraform plan failed: %w", err)
	}

	planJSON, err := executor.ShowPlanJSON(ctx, result.PlanFile)
	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("terraform show -json failed: %w", err)
	}

	planRaw, err := executor.ShowPlanRaw(ctx, result.PlanFile)
	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("terraform show failed: %w", err)
	}

	return WorkspaceResult{
		Workspace:  ws.Name,
		HasChanges: result.HasChanges,
		ExitCode:   result.ExitCode,
		PlanFile:   result.PlanFile,
		Summary:    tf.BuildPlanSummary(planJSON, ws.SummaryDetail),
		PlanRaw:    planRaw,
		PlanJSON:   planJSON,
	}, nil
}