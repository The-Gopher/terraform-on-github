package plan

import (
	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/store"
)

// MaxRenderedAddresses caps how many resource addresses the summary lists. Check fields cap at
// 65535 bytes and GitHub rejects rather than truncates oversize payloads, so bound the list
// before formatting and say how many were omitted.
const MaxRenderedAddresses = 200

// Summary is the rendered check-run output.
type Summary struct {
	Title      string
	Body       string
	Conclusion string
}

// Render turns plan.json into check-run output.
//
//	prod-networking · envs/prod/networking · 3 to add, 1 to change, 0 to destroy
//
//	  + google_compute_subnetwork.app["c"]
//	  ~ google_compute_firewall.allow_health_checks
//	  + google_service_account.runner
//
//	plan a1b2c3d…c3d4e5f · terraform 1.9.8 · 42s
//	[view full diff] (expires in 15m)
//
// Both "no changes" and "has changes" are success conclusions — the counts carry the
// difference. Making "has changes" fail would paint every substantive PR red and train people
// to ignore the check.
func Render(w config.Workspace, m store.Meta, planJSON []byte, fullDiffURL string) (Summary, error) {
	return Summary{}, errNotImplemented
}

// redact strips values from a rendered diff.
//
// Two reasons the default is addresses-only. First, a plan file embeds a snapshot of prior
// state, so every secret in state is in the plan. Second, Terraform's `sensitive` marking is
// not a security boundary — it covers what the provider schema and the config declare
// sensitive, and not a token someone pasted into a startup script or a JSON blob in a
// metadata attribute.
//
// So `addresses` renders address + action + counts and nothing else, and `full` is an explicit
// per-workspace opt-in for workspaces whose diffs are known to be dull. Even under `full`,
// anything Terraform *does* mark sensitive is masked — that part is cheap and strictly
// additive.
func redact(planJSON []byte, detail config.SummaryDetail) (string, error) {
	return "", errNotImplemented
}

// Counts tallies resource_changes from plan.json.
//
// A "replace" is an action pair — ["delete","create"] or ["create","delete"] for
// create_before_destroy — not its own action string, and must be counted as one replace rather
// than one create plus one delete. Getting this wrong understates destructive changes in the
// headline, which is the one number reviewers actually read.
func Counts(planJSON []byte) (store.ResourceCounts, error) {
	return store.ResourceCounts{}, errNotImplemented
}

// NoOpConclusion returns the conclusion for a workspace with no changes.
func NoOpConclusion() string { return "success" }
