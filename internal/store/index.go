package store

import (
	"context"
	"errors"
	"time"
)

// The run index, the claim, the dedupe set and the workspace lease are all built from two GCS
// preconditions and nothing else:
//
//	ifGenerationMatch=0    create-if-absent      → atomic claim, atomic dedupe
//	ifGenerationMatch=<g>  compare-and-set       → atomic state transition, atomic lease steal
//
// This is the same primitive Terraform's own GCS backend uses for state locking, so the app
// coordinates itself the way the tool it wraps already coordinates itself. GCS also gives
// strongly consistent reads and strongly consistent list, which is what makes the marker
// prefixes below usable as a work queue.
//
// The trade this accepts, stated up front: object *naming* becomes load-bearing schema. There
// are no queries, only prefix listings, so every question the app needs to ask has to be
// answerable from a key. Renaming a prefix is a migration. In exchange there is no database to
// run, and one less IAM surface on both services.
//
// AWS note: S3 gained conditional writes (If-None-Match) in 2024, and Terraform's own S3
// backend now locks with `use_lockfile` instead of requiring a DynamoDB table. Everything here
// ports.
var ErrPreconditionFailed = errors.New("store: object precondition failed")

// ErrNotClaimed means a caller tried to transition a run it does not hold.
var ErrNotClaimed = errors.New("store: run not claimed by this worker")

// RunState is the lifecycle of one (workspace, base_sha, head_sha) plan.
type RunState string

const (
	StatePlanning RunState = "planning"

	// StatePlanned means a signed plan exists in the artifact bucket. Paired with a
	// pending-apply marker; see PendingMarker.
	StatePlanned RunState = "planned"

	StatePlanFailed RunState = "plan_failed"

	StateApplying RunState = "applying"
	StateApplied  RunState = "applied"

	// StateApplyFailed is terminal and never auto-retried. A failed apply may have partially
	// mutated infrastructure; the next plan will show the remainder, and a human reads the
	// apply log before deciding. Automatic retry here is how a bad afternoon becomes an outage.
	StateApplyFailed RunState = "apply_failed"

	// StateSuperseded marks a plan invalidated by a newer push to the same PR. Kept rather than
	// deleted so the check history stays explicable.
	StateSuperseded RunState = "superseded"

	// StateAbandoned means environment approval never arrived within ApprovalTimeout.
	StateAbandoned RunState = "abandoned"
)

// Run is the index entry, stored as one JSON object at Key.RunObject().
//
// Generation is the object's GCS generation as last read. Every write passes it as
// ifGenerationMatch, so two workers cannot both transition the same run — the loser gets
// ErrPreconditionFailed and drops its task.
type Run struct {
	Key   PlanKey  `json:"key"`
	State RunState `json:"state"`

	PR             int   `json:"pr"`
	InstallationID int64 `json:"installation_id"`
	CheckRunID     int64 `json:"check_run_id"`

	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	PlannedTreeSHA string `json:"planned_tree_sha,omitempty"`

	HasChanges bool           `json:"has_changes"`
	Counts     ResourceCounts `json:"resource_change_counts"`

	// ClaimedAt bounds a stuck claim. A worker that dies mid-plan leaves StatePlanning behind;
	// another worker may take it over once ClaimedAt is older than the workspace's plan timeout
	// plus a margin. Safe to steal because a plan mutates nothing.
	ClaimedAt time.Time `json:"claimed_at"`

	Error string `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Generation is transport state, not payload. Never serialized.
	Generation int64 `json:"-"`
}

// Index is the run store, backed by the coordination bucket.
//
// Deliberately a separate bucket from the artifact bucket. The plan service holds
// objectCreator-only on artifacts — no read, no overwrite, which is what makes plans write-once
// (§4.2) — and it needs read plus CAS here. Splitting the buckets keeps both properties instead
// of trading one for the other.
type Index interface {
	// Claim creates the run object with ifGenerationMatch=0.
	//
	// Idempotency for expensive work: at-least-once task delivery meets a minutes-long operation
	// holding live credentials. On precondition failure the object already exists, so read it and
	// decide — StatePlanned means reuse the artifact and just refresh the check (this is the
	// (base_sha, head_sha) cache), StatePlanning with a fresh ClaimedAt means another worker has
	// it and this task should be dropped, StatePlanning with a stale ClaimedAt may be taken over.
	Claim(ctx context.Context, k PlanKey, r Run) (existing Run, already bool, err error)

	// Get reads a run and its current generation.
	Get(ctx context.Context, k PlanKey) (Run, error)

	// CAS writes r with ifGenerationMatch=r.Generation, returning ErrPreconditionFailed if the
	// object moved underneath. Callers re-read and retry, or drop the task.
	CAS(ctx context.Context, r Run) (Run, error)
}

// ---------------------------------------------------------------------------
// The pending-apply marker: object naming as the only index we get.
// ---------------------------------------------------------------------------

// Why a marker at all. GCS cannot answer "which runs are in state planned" — there is no query,
// only prefix listing. So state that has to be *searched* is encoded in a key rather than a
// field, and the set of outstanding work becomes the set of objects under one prefix. Listing it
// is O(outstanding) rather than O(every run ever), and a listing that keeps growing is itself a
// signal something is wrong — which is the right behaviour for a backstop.
//
// The name carries every field the two queries need, ordered widest-to-narrowest so both are
// prefix scans:
//
//	pending-apply/<owner>/<repo>/<pr>/<workspace>/<base_sha>__<head_sha>
//
//	/reconcile   lists  pending-apply/                      → all outstanding applies
//	Supersede    lists  pending-apply/<owner>/<repo>/<pr>/  → this PR's, to drop stale heads
//
// The object body is a small JSON pointer (installation id, check run id) so /reconcile need not
// read the run object just to act.
type PendingMarker struct {
	Key            PlanKey   `json:"key"`
	PR             int       `json:"pr"`
	InstallationID int64     `json:"installation_id"`
	CheckRunID     int64     `json:"check_run_id"`
	PlannedAt      time.Time `json:"planned_at"`
}

// Pending is the outstanding-apply set.
type Pending interface {
	// Put creates the marker. Written immediately after meta.json — the artifact's completion
	// marker — and before the run object flips to StatePlanned.
	//
	// That ordering matters because there is no transaction spanning two objects. A crash between
	// the two writes must fail toward "a marker exists for a complete plan", which /reconcile
	// picks up and re-verifies, and never toward "a complete plan nobody will ever apply". Note
	// this window is not a GCS limitation: the artifact upload and any index write live in
	// different systems regardless, so a database would not close it either.
	Put(ctx context.Context, m PendingMarker) error

	// List returns every outstanding marker under prefix. Strongly consistent, so a marker
	// written a moment ago is visible.
	List(ctx context.Context, prefix string) ([]PendingMarker, error)

	// Delete removes one marker. Called on apply success, on abandonment, and on supersede.
	Delete(ctx context.Context, k PlanKey, pr int) error

	// Supersede drops every marker for (owner, repo, pr) whose head SHA is not keepHeadSHA, so a
	// later push cannot leave /reconcile able to resurrect a plan nobody reviewed.
	//
	// Multi-object and therefore not atomic — and it does not need to be. It is idempotent
	// cleanup: a partial pass leaves a stale marker that /reconcile hands to apply.Verify, which
	// rejects it as superseded. The marker set is an optimization over re-verification, never a
	// substitute for it.
	Supersede(ctx context.Context, owner, repo string, pr int, keepHeadSHA string) error
}

// ---------------------------------------------------------------------------
// Dedupe
// ---------------------------------------------------------------------------

// Dedupe suppresses replayed webhook deliveries. Implements ghapp.Deduper.
//
//	deliveries/<X-GitHub-Delivery>
//
// FirstSeen is a create with ifGenerationMatch=0: success means first sight, precondition
// failure means replay. No body needed — the key is the whole fact.
//
// Expiry is a bucket lifecycle rule at age 1 day rather than a per-key TTL. Lifecycle is
// day-granular and runs asynchronously, so real retention is one to two days — coarser than a
// TTL field, and completely fine here: the window only has to outlive GitHub's retry schedule,
// and over-retention costs nothing but a few kilobytes.
type Dedupe interface {
	FirstSeen(ctx context.Context, deliveryID string) (bool, error)
}

// ---------------------------------------------------------------------------
// Lease
// ---------------------------------------------------------------------------

// Lease serializes applies for one workspace:
//
//	leases/<owner>_<repo>_<workspace>
//
// Acquire creates with ifGenerationMatch=0. On precondition failure, read the holder; if
// ExpiresAt has passed, steal it with ifGenerationMatch=<the generation just read>, so exactly
// one of several stealers wins.
//
// This lease is advisory, and that is the important thing about it. Terraform's own .tflock in
// the state bucket is the correctness lock, and a saved plan additionally carries the state
// lineage and serial it was built against — so two applies cannot both mutate one state even if
// this lease is wrong in both directions. What the lease buys is losing the race in
// milliseconds instead of after a checkout and an init, and giving the loser something coherent
// to put in the check ("waiting on apply of PR #410").
//
// Because it is advisory, TTL-based stealing is safe and no fencing token is needed. An earlier
// draft carried one; it was protecting an invariant Terraform already protects, which is the
// kind of complexity that reads as rigour and is really just a second mechanism to keep correct.
//
// Note the difference from Terraform's own lock, which has no expiry at all and needs
// `force-unlock` when a process dies. That is right for a lock guarding state and wrong here:
// Cloud Run instances are evicted routinely, and a permanently stuck workspace is a worse
// failure than a rare wasted run.
type Lease interface {
	Acquire(ctx context.Context, workspaceID string, ttl time.Duration) (Held, error)
	Renew(ctx context.Context, h Held, ttl time.Duration) (Held, error)
	Release(ctx context.Context, h Held) error
}

// Held is an acquired lease. Generation is passed as ifGenerationMatch on renew and release, so
// a holder that has already been stolen from fails its next call instead of quietly continuing.
type Held struct {
	WorkspaceID string    `json:"-"`
	Holder      string    `json:"holder"` // Cloud Run instance id, for the loser's message
	PR          int       `json:"pr"`
	ExpiresAt   time.Time `json:"expires_at"`
	Generation  int64     `json:"-"`
}

// Acquire the lease *after* environment approval, never before.
//
// The approval wait is implemented as a retryable task response, so the instance is gone between
// polls and cannot renew anything. A lease held across the wait would have to outlive
// ApprovalTimeout — up to 24h — which is not a lease, it is an outage waiting for a crashed
// worker. Order: verify, create the deployment, wait for approval, then acquire and apply. Two
// PRs approved at once simply race for the lease at that point, which is what it is for.

// ---------------------------------------------------------------------------
// Implementation
// ---------------------------------------------------------------------------

// Coordination implements Index, Pending, Dedupe and Lease against one bucket.
//
//	runs/<owner>/<repo>/<workspace>/<base_sha>/<head_sha>.json
//	pending-apply/<owner>/<repo>/<pr>/<workspace>/<base_sha>__<head_sha>
//	leases/<owner>_<repo>_<workspace>
//	deliveries/<delivery-id>
//
// Write-rate note: GCS sustains roughly one write per second to a single object name. Every
// object here is per-run, per-workspace or per-delivery, so contention is structurally low. The
// one object written repeatedly is a lease under renewal, at a period measured in tens of
// seconds.
type Coordination struct {
	Bucket string
}
