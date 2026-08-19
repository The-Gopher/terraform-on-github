// Package store holds the plan artifacts, the run index and the plan signature.
//
// Three storage concerns, deliberately separate:
//
//	artifact bucket      write-once plan artifacts. Plan service can create and not read; apply
//	                     service can read and not write.
//	coordination bucket  run index, outstanding-apply markers, advisory leases, webhook dedupe.
//	                     Both services read and CAS. Separate bucket so the artifact bucket keeps
//	                     its write-once property — see index.go.
//	KMS                  an asymmetric signature over plan provenance. Plan signs, apply verifies.
package store

import (
	"errors"
	"fmt"
	"time"
)

var errNotImplemented = errors.New("not implemented")

// PlanKey identifies one plan: a workspace, at a base commit, with a head commit merged in.
//
// The (BaseSHA, HeadSHA) pair is the whole idea. It makes the plan content-addressed by the
// exact inputs a reviewer saw, so "the reviewed plan" and "the applied plan" are the same
// object rather than two things that ought to agree.
type PlanKey struct {
	Owner     string
	Repo      string
	Workspace string
	BaseSHA   string
	HeadSHA   string
}

// Artifact names within a key's prefix.
const (
	ArtifactPlan     = "tfplan"    // the opaque binary; the thing that gets applied
	ArtifactPlanJSON = "plan.json" // terraform show -json: summary, equivalence, future policy checks
	ArtifactPlanText = "plan.txt"  // terraform show: human diff, served via signed URL only
	ArtifactMeta     = "meta.json" // signed provenance
)

// Prefix is the GCS object prefix for this plan.
func (k PlanKey) Prefix() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", k.Owner, k.Repo, k.Workspace, k.BaseSHA, k.HeadSHA)
}

// Object returns the full object name for one artifact.
func (k PlanKey) Object(artifact string) string { return k.Prefix() + "/" + artifact }

// ---------------------------------------------------------------------------
// Coordination-bucket names.
//
// With no database there are no queries, only prefix listings, so these names are the schema:
// every question the app asks has to be answerable from a key. Field order is
// widest-to-narrowest so each question is one prefix scan. Renaming any of them is a migration.
// ---------------------------------------------------------------------------

// RunObject is the index entry for this plan.
func (k PlanKey) RunObject() string {
	return fmt.Sprintf("runs/%s/%s/%s/%s/%s.json", k.Owner, k.Repo, k.Workspace, k.BaseSHA, k.HeadSHA)
}

// PendingObject is the outstanding-apply marker.
//
// PR sits ahead of workspace so Supersede can scan one PR's markers, while /reconcile scans the
// bare prefix for all of them. Base and head are joined with "__" rather than "/" to keep one
// marker per object instead of a nested pair.
func (k PlanKey) PendingObject(pr int) string {
	return fmt.Sprintf("pending-apply/%s/%s/%d/%s/%s__%s", k.Owner, k.Repo, pr, k.Workspace, k.BaseSHA, k.HeadSHA)
}

// PendingPRPrefix scopes a listing to one pull request's markers.
func PendingPRPrefix(owner, repo string, pr int) string {
	return fmt.Sprintf("pending-apply/%s/%s/%d/", owner, repo, pr)
}

// PendingAllPrefix scopes a listing to every outstanding apply. A listing that keeps growing is
// itself the alert.
const PendingAllPrefix = "pending-apply/"

// LeaseObject is the advisory apply lease for this workspace. Deliberately not keyed by SHA —
// the lease serializes the workspace, not the run.
func (k PlanKey) LeaseObject() string {
	return fmt.Sprintf("leases/%s_%s_%s", k.Owner, k.Repo, k.Workspace)
}

// DeliveryObject is the webhook dedupe marker. The key is the whole fact; the body is empty.
func DeliveryObject(deliveryID string) string { return "deliveries/" + deliveryID }

// Meta is the signed provenance record stored alongside every plan.
//
// Everything the apply worker needs to convince itself that this plan is the one a human
// reviewed for this merge — and nothing it would have to take on trust from its trigger.
type Meta struct {
	Schema    int    `json:"schema"`
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	PR        int    `json:"pr"`
	Workspace string `json:"workspace"`

	BaseSHA        string `json:"base_sha"`
	HeadSHA        string `json:"head_sha"`
	MergeCommitSHA string `json:"merge_commit_sha"`

	// PlannedTreeSHA is the tree of the test-merge commit that was planned.
	//
	// The load-bearing field. At apply time we compare it to the tree of the real merge commit:
	// equal trees prove the applied content is the reviewed content, and the comparison holds
	// under squash and rebase merges where commit SHAs cannot match by construction.
	// See DESIGN.md §6.2.
	PlannedTreeSHA string `json:"planned_tree_sha"`

	// ConfigRefSHA is the commit of the trusted ref whose config authorized this run.
	//
	// Recorded because the trusted ref moves. Without it, "which version of the mapping said this
	// workspace could use this identity" is unanswerable after the fact — and that is the question
	// you most want answered when reviewing an apply that should not have happened.
	// See DESIGN.md §3.1.
	ConfigRefSHA string `json:"config_ref_sha"`

	TerraformVersion string `json:"terraform_version"`

	// ProviderLockSHA256 digests .terraform.lock.hcl. The apply must resolve the same provider
	// versions the plan did, or the saved plan is not meaningfully the same plan.
	ProviderLockSHA256 string `json:"provider_lock_sha256"`

	// PlanSHA256 digests the tfplan bytes. Signed, so a swapped artifact cannot pass.
	PlanSHA256 string `json:"plan_sha256"`

	HasChanges bool           `json:"has_changes"`
	Counts     ResourceCounts `json:"resource_change_counts"`

	PlannedAt time.Time     `json:"planned_at"`
	Duration  time.Duration `json:"duration"`

	// Signature is a Cloud KMS asymmetric signature over Digest(). Excluded from the digest.
	Signature []byte `json:"signature"`
}

// ResourceCounts is the headline of the check summary.
type ResourceCounts struct {
	Create  int `json:"create"`
	Update  int `json:"update"`
	Delete  int `json:"delete"`
	Replace int `json:"replace"`
	Read    int `json:"read"`
}

// Any reports whether the plan proposes changes.
func (c ResourceCounts) Any() bool {
	return c.Create+c.Update+c.Delete+c.Replace > 0
}

// Digest returns the bytes to sign: a canonical (stable field order, no float formatting)
// encoding of Meta with Signature zeroed.
//
// Canonicalization has to be exact and version-stable — a signature that verifies only under
// the encoder that produced it is not a signature. Encode explicit fields in a fixed order
// rather than marshalling the struct, so adding a field cannot silently invalidate history.
func (m Meta) Digest() ([]byte, error) { return nil, errNotImplemented }
