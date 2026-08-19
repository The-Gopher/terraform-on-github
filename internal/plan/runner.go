// Package plan orchestrates one workspace's plan: claim, checkout, plan, store, publish.
package plan

import (
	"context"
	"errors"

	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghapp"
	"github.com/sampleserve/terraform-on-github/internal/store"
)

var errNotImplemented = errors.New("not implemented")

// Task is the Cloud Tasks payload for one plan.
//
// Carries the base SHA the scoping decision was made against, so the worker plans the same
// pair the receiver scoped even if the base branch has moved in between. A moved base produces
// a fresh event and a fresh task with a new key, rather than silently retargeting this one.
type Task struct {
	InstallationID int64
	Key            store.PlanKey
	PR             int
	CheckRunID     int64
}

// Worker handles POST /tasks/plan.
type Worker struct {
	Auth     *ghapp.AppAuth
	Loader   *config.Loader
	Index    store.Index
	Artifact store.PlanWriter
	Signer   store.Signer

	TerraformBinaries map[string]string // version → path, baked into the image
	MirrorURL         string
	CloneURLTemplate  string
}

// Run executes one plan task.
//
// Sequence, and why each step is where it is:
//
//  1. Claim the run with a create-if-absent write. At-least-once delivery meets a minutes-long
//     credentials, so idempotency comes first. An existing StatePlanned run short-circuits to
//     step 8 and just refreshes the check — that is the (base_sha, head_sha) cache.
//
//  2. Reload config from the trusted ref. The receiver already read it, but the worker re-reads
//     rather than accepting a config through the task payload — config decides which credentials
//     this process is about to assume. Note the ref is the app-side trusted one, never the PR's
//     base: in a promotion flow the base may be a branch developers can merge to freely, which
//     makes its config attacker-controlled. Record the resolved SHA in Meta.ConfigRefSHA.
//     See DESIGN.md §3.1.
//
//  3. Mint a down-scoped installation token (ghapp.PlanScope): contents:read, checks:write.
//
//  4. Check run → in_progress, so a long plan does not look stuck.
//
//  5. Compare base...head. Anything but `ahead`/`identical` means the head does not contain its
//     base, so its tree is not what a merge would produce: fail the check with "update your
//     branch" and stop. Not retryable — it needs a push. Otherwise check out the head directly
//     and record its tree as Meta.PlannedTreeSHA. See DESIGN.md §4.1.
//
//  6. tf.CheckModuleSources against the base-branch allowlist. Before init, because init is
//     what fetches and runs module code.
//
//  7. Init and Plan, with -lock=false and the plan SA impersonated. Read-only throughout.
//
//  8. Upload tfplan, plan.json, plan.txt, then meta.json last — meta's presence is the marker
//     that the plan is complete. Sign in PutMeta.
//
//  9. Render the summary (redacted per SummaryDetail) and complete the check run.
//
//  10. Index → StatePlanned, and Supersede older runs for this (workspace, PR) so /reconcile
//     cannot later resurrect a plan for a head SHA nobody reviewed.
//
// Failure at any step after 1 sets StatePlanFailed and completes the check with a failure
// conclusion. Never leave the check in in_progress: a check that never resolves blocks merges
// with no explanation.
func (w *Worker) Run(ctx context.Context, t Task) error { return errNotImplemented }

// Retryable reports whether a failed task should be retried by Cloud Tasks.
//
// Retry: transport errors, 5xx from GitHub, KMS/GCS unavailability, lock contention.
// Do not retry: tf.ErrBranchBehindBase, invalid config, denied module source, plan errors in the
// Terraform config itself. These need a push, so retrying burns credentials and quota for a
// result that cannot change.
func Retryable(err error) bool { return false }
