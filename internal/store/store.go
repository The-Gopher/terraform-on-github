// Package store implements DESIGN.md §5: plan artifacts keyed by
// (base SHA, head SHA) in a write-once bucket, coordination in a second
// bucket, and a KMS signature over the provenance record.
//
// Two buckets, because their IAM contracts are opposite (§5.2/§5.4):
//
//   - the plan bucket holds tfplan, plan.json, plan.txt and meta.json. The plan
//     service holds objectCreator only — it cannot read or overwrite an object —
//     so write-once is enforced by IAM, not by code. Cache-hit detection never
//     looks here.
//   - the coordination bucket (tf-runs) holds the run object and the
//     pending-apply marker. It needs read plus compare-and-swap, which is what
//     makes "have we already planned this pair" answerable.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Top-level prefixes and key shapes (§5). Object names are the schema — with
// no database, every question the app asks has to be answerable from a key.
const (
	// ArtifactPrefix is the plan-artifact top-level prefix. Drift keys use
	// "drift/" and never reach this package: §6.6 holds by construction, since
	// an apply composes its key with "pr/" and no value of head_sha can reach
	// a drift object.
	ArtifactPrefix = "pr"

	// RunObjectFormat is the coordination-bucket run object (§5.4).
	RunObjectFormat = "runs/pr/%s/%s/%s/%s/%s.json"

	// PendingMarkerFormat is the pending-apply marker (§5.5). Field order is
	// widest-to-narrowest so the two queries that matter are single prefix
	// scans: /reconcile lists "pending-apply/", supersede lists
	// "pending-apply/<owner>/<repo>/<pr>/".
	PendingMarkerFormat = "pending-apply/%s/%s/%d/%s/%s__%s"

	// ArtifactObjectFormat is the plan-bucket object path (§5).
	ArtifactObjectFormat = "pr/%s/%s/%s/%s/%s/%s"
)

// Artifact names under one (base, head) key.
const (
	NamePlan     = "tfplan"
	NamePlanJSON = "plan.json"
	NamePlanText = "plan.txt"
	NameMeta     = "meta.json"
)

// KindPR is the only artifact kind that may apply (§6.6). The whitelist lives
// on Meta.Validate: the next artifact kind this design grows is inapplicable
// by default rather than applicable until someone remembers a deny list.
const KindPR = "pr"

// Meta is the signed provenance record (§5, meta.json). Everything the apply
// side re-derives for authorization binds through this object: repo, PR,
// workspace, base SHA, head SHA, and the tree the plan describes.
type Meta struct {
	Schema               int            `json:"schema"`
	Kind                 string         `json:"kind"`
	Repo                 string         `json:"repo"`
	PR                   int            `json:"pr"`
	Workspace            string         `json:"workspace"`
	BaseSHA              string         `json:"base_sha"`
	HeadSHA              string         `json:"head_sha"`
	MergeCommitSHA       string         `json:"merge_commit_sha"`
	PlannedTreeSHA       string         `json:"planned_tree_sha"`
	TerraformVersion     string         `json:"terraform_version"`
	ProviderLockSHA256   string         `json:"provider_lock_sha256"`
	PlanSHA256           string         `json:"plan_sha256"`
	HasChanges           bool           `json:"has_changes"`
	ResourceChangeCounts map[string]int `json:"resource_change_counts"`
	PlannedAt            string         `json:"planned_at"`
	ConfigSHA            string         `json:"config_sha"`

	// Signature is the base64 KMS asymmetric signature over Digest. It is
	// excluded from the canonical encoding by Digest clearing it, so sign and
	// verify agree by construction.
	Signature string `json:"signature,omitempty"`
}

// Key composes the plan-artifact key for a (base, head) pair (§5). The
// workspace component is required: two workspaces on one base branch plan
// different roots, so their artifacts must not share a key.
func Key(owner, repo, workspace, baseSHA, headSHA, name string) string {
	return fmt.Sprintf(ArtifactObjectFormat, owner, repo, workspace, baseSHA, headSHA, name)
}

// RunKey composes the coordination-bucket run object key.
func RunKey(owner, repo, workspace, baseSHA, headSHA string) string {
	return fmt.Sprintf(RunObjectFormat, owner, repo, workspace, baseSHA, headSHA)
}

// PendingKey composes the pending-apply marker key.
func PendingKey(owner, repo string, pr int, baseSHA, headSHA string) string {
	return fmt.Sprintf(PendingMarkerFormat, owner, repo, pr, baseSHA, headSHA, headSHA)
}

// PendingPrefix composes the prefix /reconcile and supersede list under.
// pr == 0 returns the widest prefix (every outstanding apply).
func PendingPrefix(owner, repo string, pr int) string {
	if pr == 0 {
		return "pending-apply/" + owner + "/" + repo + "/"
	}
	return fmt.Sprintf("pending-apply/%s/%s/%d/", owner, repo, pr)
}

// PlanSHA returns the hex SHA-256 of a plan artifact, for meta.PlanSHA256.
func PlanSHA(plan []byte) string {
	sum := sha256.Sum256(plan)
	return hex.EncodeToString(sum[:])
}

// Digest returns the canonical bytes the KMS signature covers (§5.3): Meta's
// JSON encoding with Signature cleared. Encoding and clearing are the same
// operation on both sides, so sign and verify agree by construction.
func (m Meta) Digest() ([]byte, error) {
	canonical := m
	canonical.Signature = ""
	b, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing meta: %w", err)
	}
	return b, nil
}

// Validate rejects a meta that could not have come from Stage: kind must be
// the whitelist value "pr" (§6.6), and the identity fields must be present —
// an apply that cannot bind the plan to (repo, workspace, base, head, tree)
// has nothing to verify against.
func (m Meta) Validate() error {
	if m.Kind != KindPR {
		return fmt.Errorf("meta kind %q is not %q", m.Kind, KindPR)
	}
	var missing []string
	if m.Repo == "" {
		missing = append(missing, "repo")
	}
	if m.PR == 0 {
		missing = append(missing, "pr")
	}
	if m.Workspace == "" {
		missing = append(missing, "workspace")
	}
	if m.BaseSHA == "" {
		missing = append(missing, "base_sha")
	}
	if m.HeadSHA == "" {
		missing = append(missing, "head_sha")
	}
	if m.PlannedTreeSHA == "" {
		missing = append(missing, "planned_tree_sha")
	}
	if m.PlanSHA256 == "" {
		missing = append(missing, "plan_sha256")
	}
	if len(missing) > 0 {
		return fmt.Errorf("meta missing %s", strings.Join(missing, ", "))
	}
	return nil
}
