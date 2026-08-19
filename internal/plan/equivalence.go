package plan

// CUT — retained as a documented dead end, not a roadmap item. Nothing calls this file.
//
// The problem it solved. Two PRs merge into the same workspace minutes apart. The first apply
// bumps the state serial; the second PR's saved plan is stale and Terraform refuses it. Rather
// than fail, re-plan at the merge commit and apply the new plan if it proposes the same changes
// the reviewer approved.
//
// Why it is cut. Two independent reasons, and either alone would be enough:
//
//  1. DESIGN.md §4.1 requires a PR's head to already contain its base. Enforced at merge time,
//     that forces the second of two concurrent PRs to update and re-plan, so it never arrives at
//     apply with a stale plan. The case this file automates barely occurs any more; what remains
//     — an out-of-band apply from a laptop, a state restore — is something a human should look at.
//  2. Getting Equivalent() right needs the provider schema, because sets must be compared as sets
//     and lists as lists, and Terraform's JSON renders both as arrays. A false negative costs a
//     human one click. A false positive applies an unreviewed change to production. That is a bad
//     trade for automating a rare case.
//
// The answer for a repo that genuinely cannot enforce up-to-dateness is GitHub's merge queue,
// which plans against the speculative merge SHA and keeps the property without a rebase
// treadmill — not a semantic plan-differ. See DESIGN.md §6.3.
//
// Kept in the tree because the normalization notes below are the actual argument. Deleting them
// invites someone to re-propose this in six months and rediscover the set-versus-list problem the
// hard way.

import (
	"errors"

	"github.com/sampleserve/terraform-on-github/internal/store"
)

// Projection is the comparable shape of a plan: what it will do, with everything that
// legitimately varies between two runs of the same change removed.
type Projection struct {
	// Changes is sorted by address for stable comparison.
	Changes []ProjectedChange
}

// ProjectedChange is one resource's proposed change, normalized.
type ProjectedChange struct {
	Address string
	// Actions as reported by Terraform: ["create"], ["update"], ["delete","create"], …
	Actions []string
	// Attrs is the after-state with the exclusions below applied, canonically encoded.
	Attrs map[string]any
}

// Project reduces plan.json to a Projection.
//
// What must be dropped, and why each one is a real source of false negatives:
//
//   - Unknown ("after_unknown") values. Any attribute computed at apply time is unknown in both
//     plans, and comparing unknowns compares nothing. Drop the attribute, but keep the fact
//     that it was unknown — an attribute that was known in one plan and unknown in the other is
//     a genuine difference.
//   - Computed identity fields: id, self_link, fingerprint, etag, creation_timestamp,
//     generation, uid, and provider-specific equivalents. These change between runs without the
//     change meaning anything.
//   - Timestamps and any attribute whose value came from a timestamp() or uuid() function.
//   - Ordering inside sets. Terraform reports sets as JSON arrays with unstable order, so sets
//     must be compared as sets. Lists must NOT be — order is semantic there. Distinguishing the
//     two requires the provider schema, which is the single hardest part of this function and
//     the reason it needs a real corpus rather than a plausible-looking implementation.
//   - Sensitive values, which are already elided.
//
// What must NOT be dropped: anything destructive. A delete or replace that appears in the new
// plan and not the reviewed one is a difference even if every attribute matches, and the
// comparison must be biased so that "unsure" reads as "different".
func Project(planJSON []byte) (Projection, error) { return Projection{}, errNotImplemented }

// Equivalent reports whether two projections propose the same changes.
//
// Fails closed. Any parse error, any unrecognized action pair, any attribute the normalizer
// does not understand → false. The cost of a false negative is a human clicking re-run; the
// cost of a false positive is an unreviewed apply against production.
func Equivalent(reviewed, fresh Projection) (bool, Diff) { return false, Diff{} }

// Diff explains a non-equivalence, for the failing check. A reviewer seeing "plan superseded"
// needs to know what changed, or their only option is to re-read the whole plan.
type Diff struct {
	OnlyInReviewed []string          // addresses
	OnlyInFresh    []string          // addresses — the alarming column
	ActionChanged  map[string]string // address → "update → delete,create"
	AttrChanged    map[string][]string
}

// Empty reports whether the diff found nothing.
func (d Diff) Empty() bool {
	return len(d.OnlyInReviewed) == 0 && len(d.OnlyInFresh) == 0 &&
		len(d.ActionChanged) == 0 && len(d.AttrChanged) == 0
}

// CheckEquivalent is the entry point used by apply.OnStale.
//
// Also asserts the two plans were built with the same Terraform version and the same provider
// lock digest. Different provider code can produce an identical-looking projection from
// genuinely different behaviour, so version equality is a precondition of the comparison
// meaning anything at all.
func CheckEquivalent(reviewed store.Meta, reviewedJSON, freshJSON []byte, freshLockDigest, freshTFVersion string) (bool, Diff, error) {
	if reviewed.TerraformVersion != freshTFVersion || reviewed.ProviderLockSHA256 != freshLockDigest {
		return false, Diff{}, errors.New("plan: terraform or provider versions differ; equivalence undefined")
	}
	return false, Diff{}, errNotImplemented
}
