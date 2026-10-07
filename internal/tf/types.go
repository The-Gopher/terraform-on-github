package tf

import (
	"errors"
	"time"
)

// PlanOptions carries workspace config into the executor.
type PlanOptions struct {
	TerraformVersion   string
	PlanTimeout        time.Duration
	VarFiles           []string
	PlanArgs           []string
	TerraformWorkspace string
	Lock               bool
	BackendConfig      map[string]string
}

// ErrPlanTimeout is what a plan that outlived its plan_timeout returns; callers
// errors.Is against it to distinguish "bigger budget" from "repo is broken" (§4.3).
var ErrPlanTimeout = errors.New("terraform plan timeout")

// PlanResult captures the plan outcome.
type PlanResult struct {
	HasChanges bool
	ExitCode   int
	PlanFile   string
	Stdout     string
	Stderr     string
}
