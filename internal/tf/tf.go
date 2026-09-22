package tf

import (
	"context"
	"fmt"
	"time"
	"os/exec"
)

// Command represents a Terraform CLI command to be executed.
type Command struct {
	Name string
	Args []string
	Dir  string // Optional override for the working directory
}
// Runner handles the execution of Terraform commands within a specific working directory.
type Runner struct {
	Dir    string
	Timeout time.Duration
}

// NewRunner creates a new Terraform runner.
func NewRunner(dir string, timeout time.Duration) *Runner {
	return &Runner{
		Dir:     dir,
		Timeout: timeout,
	}
}

// Run executes a Terraform command and returns the exit code and stdout/stderr.
func (r *Runner) Run(ctx context.Context, cmd Command) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	exe := exec.CommandContext(ctx, "terraform", cmd.Args...)
	dir := r.Dir
	if cmd.Dir != "" {
		dir = cmd.Dir
	}
	exe.Dir = dir
	
	// We capture combined output for simplicity in the summary, 
	// though internal/plan might need separation for specific errors.
	out, err := exe.CombinedOutput()
	
	if ctx.Err() == context.DeadlineExceeded {
		return -1, string(out), fmt.Errorf("terraform command %q timed out after %v", cmd.Name, r.Timeout)
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), string(out), nil
		}
		return -1, string(out), err
	}

	return 0, string(out), nil
}
