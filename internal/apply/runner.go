package apply

import (
	"context"
	"time"

	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghapp"
	"github.com/sampleserve/terraform-on-github/internal/store"
)

// Task is the Cloud Tasks payload for one apply. Every field is a hint; see Verify.
type Task struct {
	InstallationID int64
	Key            store.PlanKey
	PR             int

	// Attempt distinguishes apply-log objects and bounds the approval poll.
	Attempt int
}

// Worker handles POST /tasks/apply and POST /reconcile.
type Worker struct {
	Auth     *ghapp.AppAuth
	Loader   *config.Loader
	Index    store.Index
	Lease    store.Lease
	Plans    store.PlanReader
	Logs     store.ApplyLogWriter
	Verifier store.Verifier

	TerraformBinaries map[string]string
	MirrorURL         string
}

// Run executes one apply task.
//
//  1. Verify. Nothing else happens first — no clone, no init, no credential mint. A forged
//     task should cost one GitHub read.
//
//  2. Acquire the workspace lease with a fencing token. Terraform's state lock is the
//     correctness backstop, but losing the race here costs milliseconds instead of the minutes
//     an init-then-fail would, and gives the loser something coherent to put in the check
//     ("waiting on apply of PR #410").
//
//  3. Create a Deployment to the workspace's GitHub Environment. If protection rules hold it
//     in `queued`, return a retryable status so Cloud Tasks backs off, bounded by
//     ApprovalTimeout — after which the run is Abandoned, never applied. Reusing GitHub's
//     approval UI keeps the audit trail next to the code that changed.
//
//  4. Checkout Meta.MergeCommitSHA and assert the checkout's TreeSHA equals
//     Meta.PlannedTreeSHA a second time. Verify already checked it via the API; this checks
//     what actually landed on disk, which is the thing Terraform is about to read.
//
//  5. Init with the apply SA impersonated, then assert ProviderLockDigest matches
//     Meta.ProviderLockSHA256. Different provider code applying a plan built against other
//     provider code is not the reviewed change.
//
//  6. Apply the saved plan. tf.ErrStalePlan → onStale.
//
//  7. Upload the apply log, set the deployment status, update the plan check run with the
//     outcome so the PR timeline reads as one story, release the lease.
//
// A failure between 6 and 7 may have partially mutated infrastructure. Record StateApplyFailed
// and stop: the next plan will show the remainder, and a human reads the log before deciding.
// Automatic retry of a half-applied change is how a bad afternoon becomes an outage.
func (w *Worker) Run(ctx context.Context, t Task) error { return errNotImplemented }

// onStale handles Terraform refusing the saved plan because state moved — the expected outcome
// when two PRs merge into one workspace in quick succession.
//
//	StaleFail                → fail the check and the deployment, require a human re-request.
//	StaleReplanIfEquivalent  → re-plan at the merge commit, compare projections against the
//	                           reviewed plan.json, apply the fresh plan only on equivalence and
//	                           say so explicitly in the check. On any difference, render the
//	                           diff and fall back to failing.
//
// The re-plan runs with the *apply* identity, because this service holds no plan identity. So
// the re-plan is not read-only in the IAM sense even though `plan` does not mutate — a real
// asymmetry, and a reason to prefer GitHub's merge queue, which prevents the situation instead
// of recovering from it.
func (w *Worker) onStale(ctx context.Context, v Verified, ws config.Workspace) error {
	return errNotImplemented
}

// Reconcile handles POST /reconcile, on a Cloud Scheduler tick.
//
// Sweeps runs in StatePlanned whose PR has since merged and enqueues applies for them. Two jobs:
//
//   - Backstop. A dropped webhook or a lost task otherwise means a merged PR whose plan silently
//     never applies — the worst failure mode here, because nothing is red.
//   - Optional hardening. If you drop cloudtasks.enqueuer on the apply queue from tf-plan@ and
//     let this be the only path to an apply, the plan service loses even the ability to *ask*
//     for one. Costs ~30s of latency. See DESIGN.md §7.4.
//
// A merged PR with no StatePlanned run gets a check and a comment saying no plan was applied —
// never a silent skip and never an unreviewed plan-and-apply.
func (w *Worker) Reconcile(ctx context.Context, olderThan time.Duration) error {
	return errNotImplemented
}

// Retryable reports whether Cloud Tasks should retry.
//
// Retry: pending environment approval, lease contention, tf.ErrLockHeld, transport and 5xx.
// Do not retry: anything from Verify (all terminal), store.ErrBadSignature, a failed apply.
func Retryable(err error) bool { return false }
