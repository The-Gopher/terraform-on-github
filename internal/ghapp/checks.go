package ghapp

import (
	"context"
	"fmt"
	"time"
)

// GitHub caps check-run `summary` and `text` at 65535 bytes each and rejects oversize
// payloads rather than truncating them, so callers must truncate first.
const MaxCheckFieldBytes = 65535

// Check conclusions used by this app.
const (
	ConclusionSuccess        = "success"
	ConclusionFailure        = "failure"
	ConclusionNeutral        = "neutral"
	ConclusionSkipped        = "skipped"
	ConclusionActionRequired = "action_required" // fork PR awaiting a maintainer's /plan
	ConclusionTimedOut       = "timed_out"
)

// CheckName is the check-run name for a workspace, e.g. "terraform/plan (prod-networking)".
//
// The name is the idempotency key GitHub gives us for free: re-running an event updates the
// existing check for that (head_sha, name) pair instead of stacking duplicates.
func CheckName(stage, workspace string) string {
	return fmt.Sprintf("terraform/%s (%s)", stage, workspace)
}

// CheckRun is the payload for creating or updating a check run.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Status     string // queued | in_progress | completed
	Conclusion string // set only when Status == completed
	Title      string
	Summary    string
	Text       string
	StartedAt  *time.Time
	DetailsURL string // signed URL to the full plan text, short TTL
}

// UpsertCheckRun creates the check run or updates the existing one for (HeadSHA, Name).
//
// Called at least three times per plan: queued at webhook time (so the PR shows work is
// pending before a worker picks it up), in_progress when the worker claims it, completed with
// the summary. On merge the *plan* check is updated with the apply outcome as well, so the PR
// timeline reads as one story rather than two unrelated check families.
func (c *Client) UpsertCheckRun(ctx context.Context, cr CheckRun) (int64, error) {
	return 0, errNotImplemented
}

// Deployment tracks an apply against a GitHub Environment.
type Deployment struct {
	ID          int64
	Environment string
	Ref         string // the merge commit SHA
	State       string // queued | in_progress | success | failure | error
}

// CreateDeployment opens a deployment to a workspace's Environment.
//
// This is the approval gate. If the Environment has protection rules (required reviewers, wait
// timer), GitHub holds the deployment in `queued` and the apply worker polls until it is
// approved — reusing GitHub's own approval UI and audit trail instead of building one, and
// keeping the record next to the code it changed. See DESIGN.md §6.4.
func (c *Client) CreateDeployment(ctx context.Context, environment, ref, description string) (Deployment, error) {
	return Deployment{}, errNotImplemented
}

// DeploymentApproved reports whether a queued deployment has cleared its protection rules.
//
// The worker cannot block for hours inside one request, so a false result becomes a retryable
// response and Cloud Tasks backs off — bounded by ApplyPolicy.ApprovalTimeout, after which the
// run is abandoned rather than applied.
func (c *Client) DeploymentApproved(ctx context.Context, deploymentID int64) (bool, error) {
	return false, errNotImplemented
}

// SetDeploymentStatus records progress and the final outcome.
func (c *Client) SetDeploymentStatus(ctx context.Context, deploymentID int64, state, description, logURL string) error {
	return errNotImplemented
}

// Truncate cuts s to fit MaxCheckFieldBytes, on a UTF-8 boundary, appending a marker. Plans
// against large workspaces routinely exceed the cap.
func Truncate(s string) string { return s }
