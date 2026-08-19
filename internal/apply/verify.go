// Package apply orchestrates applying a reviewed plan after merge.
package apply

import (
	"context"
	"errors"

	"github.com/sampleserve/terraform-on-github/internal/ghapp"
	"github.com/sampleserve/terraform-on-github/internal/store"
)

var errNotImplemented = errors.New("not implemented")

// Verification failures. All terminal — none is retryable, and none is ever resolved by
// generating a fresh plan and applying it.
var (
	ErrNotMerged      = errors.New("apply: pull request is not merged")
	ErrBaseMoved      = errors.New("apply: base branch moved between plan and merge")
	ErrTreeMismatch   = errors.New("apply: merge commit tree does not match the planned tree")
	ErrWorkspaceScope = errors.New("apply: workspace is not bound to this pull request's base branch")
	ErrApplyDisabled  = errors.New("apply: workspace has apply disabled")
	ErrSuperseded     = errors.New("apply: plan was superseded by a later push")
)

// Verified is the authorization to apply, produced only by Verify.
//
// Its fields come from GitHub and from a signature-checked Meta, never from a task payload.
// Constructing one by hand defeats the point; the apply Worker takes a Verified rather than a
// Task so the type system makes the ordering explicit.
type Verified struct {
	Meta           store.Meta
	MergeCommitSHA string
	Environment    string
}

// Verify re-derives, from scratch, the authorization to apply.
//
// The trigger is a hint, not a permission. This matters concretely: because the webhook
// receiver lives in the plan service, tf-plan@ holds cloudtasks.enqueuer on the apply queue and
// can therefore ask the apply service to do something (DESIGN.md §7.4). What makes that
// acceptable is that a forged or tampered task cannot survive the checks below.
//
// The checks, in order — each one closing a specific way a wrong plan could reach production:
//
//  1. PR is merged, and pr.MergeCommitSHA matches what the task claims. Closes "apply a plan
//     for a PR that was closed unmerged, or is still open".
//
//  2. Config loaded from the repo's **trusted ref**, at its current tip, binds this workspace to
//     pr.BaseRef and has apply enabled.
//
//     Not from the merge commit, which contains the PR's own changes and would let a PR grant
//     itself a workspace binding and an apply identity in the same commit that gets applied. And
//     not from the pre-merge base either, which an earlier draft did: in a promotion flow the base
//     may be a branch developers merge to freely, so its config is attacker-controlled — a
//     workspace entry naming a production apply identity for `branch: integration` would look
//     exactly as reviewed as a real one. See DESIGN.md §3.1.1.
//
//     At the *current* tip, deliberately, rather than at Meta.ConfigRefSHA. Config is fail-closed
//     on change: a workspace deauthorized, or repointed at a narrower identity, between plan and
//     merge must not apply. Meta.ConfigRefSHA is recorded for audit and for rendering what
//     changed — never used to authorize.
//
//  3. Meta signature verifies (store.GetMeta does this or fails), and Meta's
//     (owner, repo, workspace, base_sha, head_sha) equal the requested key. Closes "point the
//     apply at a plan built for a different workspace".
//
//  4. The tree of pr.MergeCommitSHA equals Meta.PlannedTreeSHA.
//
//     The one check that subsumes several others. Equal trees mean the working tree about to be
//     applied is byte-identical to the tree that was planned and reviewed — which also proves
//     the base did not move, since a moved base yields a different tree. And it holds under
//     squash and rebase merges, where the merge commit SHA cannot match by construction but the
//     content is preserved. See DESIGN.md §6.2.
//
//  5. Index state is StatePlanned, not StateSuperseded. Closes the /reconcile race where a
//     later push invalidated this plan.
//
// Returns ErrBaseMoved rather than ErrTreeMismatch when the ancestry check can attribute the
// mismatch to a moved base, purely so the failing check can tell the author to rebase instead
// of filing a bug.
func Verify(ctx context.Context, c *ghapp.Client, idx store.Index, reader store.PlanReader, k store.PlanKey, prNumber int) (Verified, error) {
	return Verified{}, errNotImplemented
}
