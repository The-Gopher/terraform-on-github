// Command plan-service is the read-only tier: it receives GitHub webhooks and runs Terraform
// plans.
//
// Runs as tf-plan@, which holds:
//   - iam.serviceAccountTokenCreator on each per-workspace *plan* service account, individually
//   - storage.objectCreator on the plan bucket — create only, no read, no overwrite
//   - cloudkms.signer on the plan-signing key
//   - objectAdmin on the coordination bucket (run index, leases, dedupe — CAS needs overwrite)
//   - cloudtasks.enqueuer, secretmanager.secretAccessor
//
// And explicitly not: any role on any target project, any read of the plan bucket, any write to
// a state bucket, run.invoker on the apply service. See DESIGN.md §7.1.
package main

import (
	"net/http"
)

func main() {
	mux := http.NewServeMux()

	// Public ingress — GitHub posts here. HMAC is the only authentication.
	mux.HandleFunc("POST /webhook", handleWebhook)

	// Cloud Tasks posts here. Cloud Run has no per-path ingress control and /webhook needs
	// public ingress, so this path is internet-reachable and must authenticate in-process:
	// verify the Google-signed OIDC token (issuer, audience, invoker SA email) before doing
	// anything. See DESIGN.md §7.5.
	mux.HandleFunc("POST /tasks/plan", handlePlanTask)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// TODO: wire config.Loader, ghapp.AppAuth, store.Coordination, store.GCS, store.KMSSigner,
	// Cloud Tasks client; then http.ListenAndServe on $PORT.
	_ = mux
}

// handleWebhook verifies, scopes and enqueues. It never plans inline.
//
//  1. Read the raw body once and HMAC it exactly as received. Any decode-and-re-encode in front
//     of the verification makes the signature meaningless.
//  2. Dedupe on X-GitHub-Delivery. GitHub retries on timeout or 5xx and a plan is expensive.
//  3. Dispatch by event; acknowledge and drop anything unsubscribed.
//  4. Return 202 well inside GitHub's response budget. All real work goes to Cloud Tasks.
//
// Return 2xx even for events we ignore. A non-2xx makes GitHub retry, and a permanently
// failing endpoint eventually gets the App's webhook deliveries throttled.
func handleWebhook(w http.ResponseWriter, r *http.Request) {}

// dispatchPullRequest handles the pull_request event.
//
// opened / synchronize / reopened / ready_for_review:
//
//  1. Resolve the base branch tip *now* via ghapp.BranchTipSHA. Not pull_request.base.sha —
//     that is the base SHA at PR creation time and is stale the moment anything else merges,
//     so keying plans on it would key them to a base nobody is merging into.
//     1a. Compare base...head. `behind` or `diverged` → one failing check, "update your branch",
//     and stop before loading config or touching credentials. See DESIGN.md §4.1.
//  2. Load config via config.LoadFromTrustedRef — the app-side trusted ref, NOT the base SHA.
//     In a promotion flow the base may be a branch developers merge to freely, which makes its
//     config attacker-controlled (DESIGN.md §3.1.1). ErrNoConfig → 200, do nothing. A config that
//     fails validation → one failing check on the head SHA explaining why; validation fails
//     closed, so no plans run against a guessed mapping.
//  3. ghapp.AuthorizePlan. Forks and untrusted authors get an action_required check instead of
//     a plan — `terraform plan` executes untrusted code (DESIGN.md §8), and approval is scoped
//     to a head SHA so "approve then force-push" is not a credential-access primitive.
//  4. ChangedFiles(baseSHA, headSHA), then scope.Match. On compare truncation, scope every
//     candidate workspace rather than a silent subset: over-planning costs money,
//     under-planning is a correctness bug.
//  5. Supersede older runs for each (workspace, PR), create a queued check run per in-scope
//     workspace, enqueue one plan task each.
//  6. Empty scope → one skipped check naming the candidate workspaces, so "why did nothing
//     plan?" is answerable from the PR.
//
// closed with merged == true: for each workspace with a StatePlanned run, enqueue an apply
// task. The payload is a hint only — apply.Verify re-derives everything (DESIGN.md §6.1).
//
// closed with merged == false: mark runs superseded so /reconcile cannot resurrect them.
func dispatchPullRequest(w http.ResponseWriter, r *http.Request) {}

// dispatchCheckRun handles a rerequested check run: re-plan that one workspace at the freshly
// resolved (base_sha, head_sha) key.

// Note the base tip is re-resolved, so a re-run after someone else merged surfaces the
// up-to-date failure rather than silently re-planning against a base nobody is merging into. Deliberately re-plans rather than serving the cached artifact —
// someone clicking "Re-run" wants fresh output, usually because they changed something outside
// the repo.
func dispatchCheckRun(w http.ResponseWriter, r *http.Request) {}

// dispatchIssueComment handles "/plan" on a fork PR from someone with write access, and is the
// only way an untrusted PR gets planned. Re-resolve the head SHA at comment time and scope the
// authorization to it.
func dispatchIssueComment(w http.ResponseWriter, r *http.Request) {}

// handlePlanTask runs one plan task.
//
// VerifyOIDC first, then plan.Worker.Run. Map the error through plan.Retryable: retryable →
// 503 so Cloud Tasks backs off, terminal → 200 so it stops retrying, having already recorded
// the failure on the check run. Returning 5xx for a terminal failure retries a plan that cannot
// succeed until someone pushes.
func handlePlanTask(w http.ResponseWriter, r *http.Request) {}
