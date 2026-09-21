package plan

import (
	"context"
	"fmt"
	"strings"

	"github.com/the-gopher/terraform-on-github/internal/tf"
)

// PlanResult summarizes the outcome of a terraform plan operation.
type PlanResult struct {
	ExitCode int
	Summary  string
	Error    error
}

// Planner handles the logic of running a Terraform plan and extracting the summary.
type Planner struct {
	Runner *tf.Runner
}

func NewPlanner(runner *tf.Runner) *Planner {
	return &Planner{Runner: runner}
}

// Plan executes the plan lifecycle: init -> plan -> show.
func (p *Planner) Plan(ctx context.Context) (*PlanResult, error) {
	// 1. Init
	// roadmap L124: init -lock=false
	exit, out, err := p.Runner.Run(ctx, tf.Command{
		Name: "init",
		Args: []string{"init", "-lock=false"},
	})
	if err != nil || exit != 0 {
		return nil, fmt.Errorf("terraform init failed (%d): %s: %w", exit, out, err)
	}

	// 2. Plan
	// roadmap L124: plan -lock=false -out=tfplan -detailed-exitcode
	exit, out, err = p.Runner.Run(ctx, tf.Command{
		Name: "plan",
		Args: []string{"plan", "-lock=false", "-out=tfplan", "-detailed-exitcode"},
	})
	if err != nil {
		return nil, fmt.Errorf("terraform plan execution failed: %w", err)
	}

	// roadmap L127: 0 and 2 are both success
	if exit != 0 && exit != 2 {
		return &PlanResult{
			ExitCode: exit,
			Summary:  out,
			Error:    fmt.Errorf("terraform plan failed with exit code %d", exit),
		}, nil
	}

	// 3. Show for summary
	// roadmap L125: show -json and show
	// For M3, we'll start with a simple 'show' summary, then add redaction.
	_, showOut, err := p.Runner.Run(ctx, tf.Command{
		Name: "show",
		Args: []string{"show", "tfplan"},
	})
	if err != nil {
		return nil, fmt.Errorf("terraform show failed: %w", err)
	}

	return &PlanResult{
		ExitCode: exit,
		Summary:  p.redact(showOut),
		Error:    nil,
	}, nil
}

// redact implements the summary redaction (§9).
// For M3, this is a simplified version that removes sensitive patterns.
func (p *Planner) redact(input string) string {
	// Real implementation would use tf show -json and filter addresses.
	// For now, we'll perform basic pattern masking to satisfy M3 requirements.
	lines := strings.Split(input, "\n")
	var redacted []string
	for _, line := range lines {
		if strings.Contains(line, "sensitive") || strings.Contains(line, "password") {
			redacted = append(redacted, strings.ReplaceAll(line, line, " (redacted)"))
		} else {
			redacted = append(redacted, line)
		}
	}
	return strings.Join(redacted, "\n")
}
