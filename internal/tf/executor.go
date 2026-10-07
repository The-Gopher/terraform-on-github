// Package tf runs Terraform for one workspace — init, plan and show — and renders plan
// summaries. Summary rendering lives here rather than in cmd/ so the M6 worker and the CLI
// render identically from the same code.
package tf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
)

// Executor runs Terraform commands for a workspace.
type Executor interface {
	Init(ctx context.Context, workDir string, backendConfig map[string]string) error
	Plan(ctx context.Context, workDir string, opts PlanOptions) (PlanResult, error)
	ShowPlanJSON(ctx context.Context, planFile string) (*tfjson.Plan, error)
	ShowPlanRaw(ctx context.Context, planFile string) (string, error)
}

// CLIExecutor is the Executor that drives a terraform binary through terraform-exec.
type CLIExecutor struct {
	terraformPath string
}

// NewExecutor returns a CLIExecutor for the terraform binary at terraformPath, or for
// "terraform" on PATH when terraformPath is empty.
func NewExecutor(terraformPath string) *CLIExecutor {
	if terraformPath == "" {
		terraformPath = "terraform"
	}
	return &CLIExecutor{terraformPath: terraformPath}
}

// Init runs terraform init with the given -backend-config values and -lock=false.
func (e *CLIExecutor) Init(ctx context.Context, workDir string, backendConfig map[string]string) error {
	tf, err := tfexec.NewTerraform(workDir, e.terraformPath)
	if err != nil {
		return fmt.Errorf("create tfexec: %w", err)
	}

	opts := []tfexec.InitOption{
		tfexec.Lock(false),
	}
	for key, value := range backendConfig {
		opts = append(opts, tfexec.BackendConfig(key+"="+value))
	}

	if err := tf.Init(ctx, opts...); err != nil {
		return fmt.Errorf("terraform init: %w", err)
	}
	return nil
}

// Plan runs terraform plan -out=tfplan in workDir, bounded by opts.PlanTimeout when it is set.
// A plan killed by the timeout returns an error wrapping ErrPlanTimeout.
func (e *CLIExecutor) Plan(ctx context.Context, workDir string, opts PlanOptions) (PlanResult, error) {
	if opts.PlanTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.PlanTimeout)
		defer cancel()
	}

	tf, err := tfexec.NewTerraform(workDir, e.terraformPath)
	if err != nil {
		return PlanResult{}, fmt.Errorf("create tfexec: %w", err)
	}

	if opts.TerraformWorkspace != "" {
		if err := tf.WorkspaceSelect(ctx, opts.TerraformWorkspace); err != nil {
			return PlanResult{}, fmt.Errorf("select workspace %q: %w", opts.TerraformWorkspace, err)
		}
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	tf.SetStdout(&stdoutBuf)
	tf.SetStderr(&stderrBuf)

	planFile := filepath.Join(workDir, "tfplan")
	planOpts := []tfexec.PlanOption{
		tfexec.Lock(opts.Lock),
		tfexec.Out(planFile),
	}

	for _, vf := range opts.VarFiles {
		planOpts = append(planOpts, tfexec.VarFile(vf))
	}

	for _, arg := range opts.PlanArgs {
		planOpts = append(planOpts, tfexec.Var(arg))
	}

	hasChanges, err := tf.Plan(ctx, planOpts...)

	result := PlanResult{
		PlanFile:   planFile,
		Stdout:     stdoutBuf.String(),
		Stderr:     stderrBuf.String(),
		HasChanges: hasChanges,
	}

	// §4.3: 0 = no changes, 2 = changes present, both are success. tfexec folds both into
	// (bool, nil); anything else is a real failure. Populate ExitCode so callers can report
	// the distinction instead of a bare bool.
	result.ExitCode = 1
	if hasChanges {
		result.ExitCode = 2
	} else if err == nil {
		result.ExitCode = 0
	}

	if err != nil {
		// A plan killed by the context deadline is a timeout, not a Terraform failure.
		// tfexec's cmdErr answers errors.Is for context.DeadlineExceeded; check the
		// error itself, not just ctx.Err(), so callers can errors.Is the same way.
		if errors.Is(err, context.DeadlineExceeded) {
			return result, fmt.Errorf("%w after %v: %w", ErrPlanTimeout, opts.PlanTimeout, err)
		}
		return result, fmt.Errorf("terraform plan: %w", err)
	}

	return result, nil
}

// ShowPlanJSON runs terraform show -json on planFile and returns the parsed plan.
func (e *CLIExecutor) ShowPlanJSON(ctx context.Context, planFile string) (*tfjson.Plan, error) {
	workDir := filepath.Dir(planFile)
	tf, err := tfexec.NewTerraform(workDir, e.terraformPath)
	if err != nil {
		return nil, fmt.Errorf("create tfexec: %w", err)
	}

	plan, err := tf.ShowPlanFile(ctx, planFile)
	if err != nil {
		return nil, fmt.Errorf("terraform show -json: %w", err)
	}

	return plan, nil
}

// ShowPlanRaw runs terraform show on planFile and returns the human-readable plan.
func (e *CLIExecutor) ShowPlanRaw(ctx context.Context, planFile string) (string, error) {
	workDir := filepath.Dir(planFile)
	tf, err := tfexec.NewTerraform(workDir, e.terraformPath)
	if err != nil {
		return "", fmt.Errorf("create tfexec: %w", err)
	}

	output, err := tf.ShowPlanFileRaw(ctx, planFile)
	if err != nil {
		return "", fmt.Errorf("terraform show: %w", err)
	}

	return strings.TrimSpace(output), nil
}

// FindTerraformBinary attempts to locate terraform binary on PATH.
func FindTerraformBinary() (string, error) {
	path, err := exec.LookPath("terraform")
	if err != nil {
		return "", fmt.Errorf("terraform not found in PATH: %w", err)
	}
	return path, nil
}

// ValidatePlanArgs checks that plan args don't contain reserved flags.
func ValidatePlanArgs(args []string) error {
	reserved := map[string]bool{
		"-lock":      true,
		"-out":       true,
		"-input":     true,
		"-state":     true,
		"-var-file":  true,
		"-chdir":     true,
		"--lock":     true,
		"--out":      true,
		"--input":    true,
		"--state":    true,
		"--var-file": true,
		"--chdir":    true,
	}

	for _, arg := range args {
		// Check for flags with or without equals
		parts := strings.SplitN(arg, "=", 2)
		flag := parts[0]
		if reserved[flag] {
			return fmt.Errorf("reserved plan arg %q is not allowed", arg)
		}
	}
	return nil
}
