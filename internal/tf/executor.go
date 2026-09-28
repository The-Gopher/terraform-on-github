package tf

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/hashicorp/terraform-json"
)

// Executor runs Terraform commands for a workspace.
type Executor interface {
	Init(ctx context.Context, workDir string, backendConfig map[string]string) error
	Plan(ctx context.Context, workDir string, opts PlanOptions) (PlanResult, error)
	ShowPlanJSON(ctx context.Context, planFile string) (*tfjson.Plan, error)
	ShowPlanRaw(ctx context.Context, planFile string) (string, error)
}

type tfexecExecutor struct {
	terraformPath string
}

// NewExecutor creates a new tfexec-based executor.
func NewExecutor(terraformPath string) Executor {
	if terraformPath == "" {
		terraformPath = "terraform"
	}
	return &tfexecExecutor{terraformPath: terraformPath}
}

func (e *tfexecExecutor) Init(ctx context.Context, workDir string, backendConfig map[string]string) error {
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

func (e *tfexecExecutor) Plan(ctx context.Context, workDir string, opts PlanOptions) (PlanResult, error) {
	if opts.PlanTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.PlanTimeout)
		defer cancel()
	}

	tf, err := tfexec.NewTerraform(workDir, e.terraformPath)
	if err != nil {
		return PlanResult{}, fmt.Errorf("create tfexec: %w", err)
	}

	// Select workspace if specified
	if opts.TerraformWorkspace != "" {
		if err := tf.WorkspaceSelect(ctx, opts.TerraformWorkspace); err != nil {
			return PlanResult{}, fmt.Errorf("select workspace %q: %w", opts.TerraformWorkspace, err)
		}
	}

	// Capture stdout/stderr
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
	stdout := stdoutBuf.String()
	stderr := stderrBuf.String()

	result := PlanResult{
		PlanFile:   planFile,
		Stdout:     stdout,
		Stderr:     stderr,
		HasChanges: hasChanges,
	}

	if err != nil {
		return result, fmt.Errorf("terraform plan: %w", err)
	}

	return result, nil
}

func (e *tfexecExecutor) ShowPlanJSON(ctx context.Context, planFile string) (*tfjson.Plan, error) {
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

func (e *tfexecExecutor) ShowPlanRaw(ctx context.Context, planFile string) (string, error) {
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
		"-lock":        true,
		"-out":         true,
		"-input":       true,
		"-state":       true,
		"-var-file":    true,
		"-chdir":       true,
		"--lock":       true,
		"--out":        true,
		"--input":      true,
		"--state":      true,
		"--var-file":   true,
		"--chdir":      true,
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