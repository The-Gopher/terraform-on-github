// Command apply-service is the write tier: the only component that can mutate infrastructure.
//
// Runs as tf-apply@, which holds:
//   - iam.serviceAccountTokenCreator on each per-workspace *apply* service account, individually
//   - storage.objectViewer on the plan bucket; objectAdmin on the apply-log bucket
//   - cloudkms.publicKeyViewer on the plan-signing key — verify, never sign
//   - objectAdmin on the coordination bucket; secretmanager.secretAccessor on the App key only
//
// And explicitly not: the webhook secret, KMS sign, any write to the plan bucket, any ability
// to enqueue onto its own queue. See DESIGN.md §7.2.
//
// Ingress is internal-only, with invoker restricted to tf-apply-invoker@. It has no public
// surface: there is no path by which a request from the internet reaches this binary.
//
// The governing rule, and the reason the two-service split is worth the complexity: this
// service trusts nothing it is told. Every field of every task is re-derived from GitHub and
// from a signature-checked plan before Terraform runs. See internal/apply.Verify.
package main

import (
	"net/http"
)

func main() {
	mux := http.NewServeMux()

	// Cloud Tasks (apply-queue) only.
	mux.HandleFunc("POST /tasks/apply", handleApplyTask)

	// Cloud Scheduler tick. Backstop for lost apply tasks, and — if you drop
	// cloudtasks.enqueuer on apply-queue from tf-plan@ — the only path to an apply at all.
	mux.HandleFunc("POST /reconcile", handleReconcile)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// TODO: wire ghapp.AppAuth (ApplyScope only), config.Loader, store.Coordination,
	// store.GCS reader, store.KMSVerifier, apply.Worker; then http.ListenAndServe on $PORT.
	_ = mux
}

// handleApplyTask applies one reviewed plan.
//
// Verify OIDC, decode the task, then apply.Worker.Run — which verifies before it clones, mints
// credentials, or touches Terraform. A forged task should cost one GitHub read and nothing else.
//
// Status mapping via apply.Retryable: pending environment approval and lease contention → 503
// so Cloud Tasks backs off (this is how the approval wait is implemented without a worker
// blocking for hours); anything from apply.Verify or a failed apply → 200, having already
// recorded the outcome on the check run and the deployment. A retried apply of a partially
// applied change is how a bad afternoon becomes an outage.
func handleApplyTask(w http.ResponseWriter, r *http.Request) {}

// handleReconcile sweeps StatePlanned runs whose PR has merged and enqueues their applies.
//
// Guard against overlapping ticks with a short lease — a slow sweep plus a one-minute schedule
// otherwise enqueues the same apply repeatedly. Idempotency downstream makes that survivable
// rather than harmless; the lease keeps it out of the logs.
func handleReconcile(w http.ResponseWriter, r *http.Request) {}
