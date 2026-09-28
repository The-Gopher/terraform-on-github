package tf

import (
	"time"
)

// PlanOptions carries workspace config into the executor.
type PlanOptions struct {
	TerraformVersion     string
	PlanTimeout          time.Duration
	VarFiles             []string
	PlanArgs             []string
	TerraformWorkspace   string
	Lock                 bool
	BackendConfig        map[string]string
}

// PlanResult captures the plan outcome.
type PlanResult struct {
	HasChanges bool
	ExitCode   int
	PlanFile   string
	Stdout     string
	Stderr     string

	// TimedOut distinguishes "plan ran out of time" from "plan failed". Both are errors, but
	// a timeout is retried with a bigger budget, while a plan failure is a repo problem.
	TimedOut bool
}