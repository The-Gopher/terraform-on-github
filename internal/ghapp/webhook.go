package ghapp

import (
	"context"
	"net/http"
)

// Events this app subscribes to. Anything else is acknowledged and dropped.
const (
	EventPullRequest  = "pull_request"  // opened, synchronize, reopened, ready_for_review, closed
	EventCheckRun     = "check_run"     // rerequested → replan one workspace
	EventCheckSuite   = "check_suite"   // rerequested → replan all in-scope workspaces
	EventIssueComment = "issue_comment" // "/plan" to authorize a fork PR
)

// WebhookVerifier validates inbound deliveries.
type WebhookVerifier struct {
	// SecretName is the Secret Manager resource for the webhook secret. Only the plan service
	// has secretAccessor on it — the apply service has no reason to be able to forge a
	// delivery to itself.
	SecretName string
}

// Verify checks X-Hub-Signature-256 with a constant-time compare over the raw body.
//
// Compute the HMAC over the bytes exactly as received: any decode-then-re-encode in front of
// this makes the signature meaningless.
func (v *WebhookVerifier) Verify(r *http.Request, body []byte) error {
	// mac := hmac.New(sha256.New, secret); mac.Write(body)
	// hmac.Equal([]byte("sha256="+hex(mac.Sum(nil))), []byte(r.Header.Get("X-Hub-Signature-256")))
	return errNotImplemented
}

// Deduper suppresses replayed deliveries.
//
// GitHub retries on timeout or 5xx, and a plan is expensive, so dedupe on X-GitHub-Delivery
// before any work is scheduled. A create-if-absent object under deliveries/ is enough — the key
// is the whole fact — with a bucket lifecycle rule for expiry; the window only needs to outlive
// GitHub's retry schedule.
//
// This is idempotency at the edge. The plan worker has its own idempotency on
// (repo, workspace, base_sha, head_sha) — that one prevents duplicate *work*, this one
// prevents duplicate *check runs*.
type Deduper interface {
	// FirstSeen returns true if this delivery id had not been recorded before.
	FirstSeen(ctx context.Context, deliveryID string) (bool, error)
}

// PlanAuthorization is the fork / first-time-contributor gate.
//
// `terraform plan` executes untrusted code from the PR — `init` fetches and runs module and
// provider code, and `data "external"` runs a local program at plan time. Read-only target
// credentials contain the blast radius (DESIGN.md §8) but still expose a full inventory of the
// environment, so a plan is not free to hand out.
type PlanAuthorization struct {
	Authorized bool

	// Reason is rendered into the action_required check so a maintainer knows what to do.
	Reason string
}

// AuthorizePlan decides whether an event may trigger a plan without human intervention.
//
// Trusted: PRs from the same repository by OWNER, MEMBER or COLLABORATOR.
// Untrusted: forks, and anyone else — these need the `tf:plan-approved` label or a `/plan`
// comment from someone with write access.
//
// Note that the label and comment are checked against *current* repo permissions at event
// time, and re-checked on every push: approving a plan approves that head SHA, not the PR.
// Otherwise "approve, then force-push something else" is a credential-access primitive.
func AuthorizePlan(ctx context.Context, c *Client, pr PullRequest, labels []string) (PlanAuthorization, error) {
	return PlanAuthorization{}, errNotImplemented
}

// VerifyOIDC validates a Google-signed ID token on /tasks/* requests.
//
// The plan service must accept public ingress for webhooks, and Cloud Run has no per-path
// ingress control, so /tasks/plan is reachable from the internet and has to authenticate
// itself in-process. Check: issuer accounts.google.com, audience == this service's URL,
// signature against Google's JWKS, and email == the expected Cloud Tasks invoker SA.
// See DESIGN.md §7.5.
func VerifyOIDC(ctx context.Context, authzHeader, wantAudience, wantEmail string) error {
	return errNotImplemented
}
