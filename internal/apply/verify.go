// Package apply implements the verification half of DESIGN §6 — everything
// before `terraform apply tfplan`, which is the only part with design
// substance. The mutation is one line; the question "may this plan be
// applied?" is five independent checks, each re-derived from GitHub and the
// signed artifact rather than trusted from any trigger (§6.1).
//
// The package holds no writer identity and takes no mutating action: Verify
// returns a verdict and a would-apply summary. In v0.1 the operator applies
// with credentials the tool never held; in v0.2 the apply worker runs the same
// Verifier before its single mutation.
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/store"
)

// Verdict is what a passing Verify concludes. A failing Verify returns no verdict, only a
// *RejectedError (or an operational error).
type Verdict struct {
	// Reason summarizes what was verified, for logs.
	Reason string

	// Meta is the verified artifact's provenance, for the would-apply summary
	// printed after a pass.
	Meta store.Meta
}

// GitHubClient is what Verify re-derives the authorization from (§6.1). Every
// field of the task/CLI input is a pointer; this interface is the permission.
type GitHubClient interface {
	// GetApplyPullRequest returns the PR's current state: merged,
	// merge_commit_sha, base and head SHAs.
	GetApplyPullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error)
	// GetCommitTree returns the tree SHA a commit points at.
	GetCommitTree(ctx context.Context, owner, repo, sha string) (string, error)
	// CommitParents returns the merge commit's parent SHAs, first-parent first.
	CommitParents(ctx context.Context, owner, repo, sha string) ([]string, error)
}

// PullRequest is the subset of PR state verification reads.
type PullRequest struct {
	Merged         bool
	MergeCommitSHA string
	BaseRef        string
	BaseSHA        string
	HeadSHA        string
}

// Request names the staged artifact to verify: the (repo, PR, workspace) and the (base, head)
// pair its key composes from (§5).
type Request struct {
	Owner     string
	Repo      string
	PR        int
	Workspace string
	BaseSHA   string
	HeadSHA   string
}

// Verifier runs the §6 checks for one (repo, PR, workspace).
type Verifier struct {
	GitHub GitHubClient
	// Artifacts is the plan bucket. Verify reads back exactly the objects the
	// plan side wrote — this is the apply service's objectViewer grant (§7.2),
	// legitimate here because the apply identity is allowed to read plans.
	Artifacts store.Bucket
	// KMS checks the KMS signature over meta's canonical digest.
	KMS store.Verifier
	// Trusted resolves the config ref the plan must still be authorized by.
	Trusted config.ContentsReader
	// ConfigRef is the trusted ref for the repo (§3.1); empty = default branch.
	ConfigRef string
}

// Errors a failing verification maps onto; callers branch on them for the
// check-run conclusion.
var (
	// ErrNotMerged is the §6.1 precondition: the PR has not merged yet.
	ErrNotMerged = errors.New("pull request has not merged")
	// ErrBaseMoved is the §6.1 assertion: merge_commit^1 != the planned base_sha.
	ErrBaseMoved = errors.New("base branch moved between plan and merge")
	// ErrTreeMismatch is the §6.2 backstop: the merge commit's tree differs
	// from planned_tree_sha. Squash/rebase make the commit SHA new; the tree
	// must not be.
	ErrTreeMismatch = errors.New("merge commit tree differs from planned_tree_sha")
	// ErrBadSignature is the §5.3 KMS verification failure.
	ErrBadSignature = errors.New("meta.json signature verification failed")
	// ErrPlanSHA is the §5.3 integrity check: the fetched tfplan does not match
	// the digest recorded (and signed) in meta.
	ErrPlanSHA = errors.New("tfplan does not match the signed plan_sha256")
	// ErrInapplicableKind is the §6.6 whitelist: kind != "pr" never applies.
	ErrInapplicableKind = errors.New("artifact kind is not applicable")
	// ErrStale is the §6.3 saved-plan refusal: Terraform will not apply a plan
	// whose state lineage or serial moved. on_stale: fail is the only v1
	// behaviour — a human re-plans, the tool never re-plans-and-applies.
	ErrStale = errors.New("saved plan is stale")
)

// RejectedError is a verification that ran to a verdict and refused: the plan is not
// applicable. Reason is operator-facing and carries the § reference to act on; Err is one of
// the sentinels above, so callers branch with errors.Is.
//
// Any other error from Verify is operational (a failed fetch, an unreadable config) — a retry,
// not a verdict.
type RejectedError struct {
	Reason string
	Err    error
}

func (e *RejectedError) Error() string { return e.Reason }

func (e *RejectedError) Unwrap() error { return e.Err }

func reject(sentinel error, format string, args ...any) error {
	return &RejectedError{Reason: fmt.Sprintf(format, args...), Err: sentinel}
}

// Verify runs the whole gauntlet and returns the first failure as a *RejectedError.
// Order matters for error quality: cheap local checks (signature, kind,
// digest) before GitHub calls, so a forged artifact fails without network
// round trips and a merged-but-moved PR fails with the specific reason.
//
// The checks, in order:
//
//  1. §5.3 KMS signature over meta's canonical digest (forgery dies first)
//  2. §6.6 whitelist: kind == "pr" — a drift plan is never applicable, and the
//     tree comparison would otherwise pass it (a drift scan of main plans
//     exactly main's tip tree)
//  3. §5.3 integrity: fetched tfplan bytes hash to meta.PlanSHA256
//  4. §6.1 re-derivation: PR merged, merge_commit_sha^1 == meta.BaseSHA
//  5. §6.2 tree equality: merge_commit^{tree} == meta.PlannedTreeSHA — survives
//     squash/rebase/merge-commit, rejects a moved base
//  6. §3.1 config re-authorization: the workspace still exists on the trusted
//     ref's current tip (fail-closed on change, per LoadFromTrustedRef)
func (v *Verifier) Verify(ctx context.Context, req Request) (Verdict, error) {
	owner, repo, baseSHA := req.Owner, req.Repo, req.BaseSHA
	metaKey := store.Key(owner, repo, req.Workspace, baseSHA, req.HeadSHA, store.NameMeta)
	planKey := store.Key(owner, repo, req.Workspace, baseSHA, req.HeadSHA, store.NamePlan)

	metaRaw, _, err := v.Artifacts.Read(ctx, metaKey)
	if err != nil {
		return Verdict{}, fmt.Errorf("fetching meta.json: %w", err)
	}
	var meta store.Meta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return Verdict{}, fmt.Errorf("decoding meta.json: %w", err)
	}

	// 1. §5.3 — signature first: nothing after this trusts meta's fields.
	digest, err := meta.Digest()
	if err != nil {
		return Verdict{}, err
	}
	if err := v.KMS.Verify(ctx, digest, meta.Signature); err != nil {
		return Verdict{}, reject(ErrBadSignature, "KMS signature over meta.json failed: %v (§5.3)", err)
	}

	// 2. §6.6 — whitelist, not denylist. The tree check would pass a drift plan
	// of main (its planned_tree_sha IS main's tip tree); kind is what
	// distinguishes reviewed from scheduled, and it is inside the signature.
	if err := meta.Validate(); err != nil {
		return Verdict{}, reject(ErrInapplicableKind, "%v (§6.6)", err)
	}

	// 3. §5.3 — the plan bytes on disk must be the ones the signature covered.
	planRaw, _, err := v.Artifacts.Read(ctx, planKey)
	if err != nil {
		return Verdict{}, fmt.Errorf("fetching tfplan: %w", err)
	}
	if store.PlanSHA(planRaw) != meta.PlanSHA256 {
		return Verdict{}, reject(ErrPlanSHA, "tfplan bytes do not match the signed plan_sha256 (§5.3)")
	}

	// 4. §6.1 — nothing in the trigger is trusted; re-derive from GitHub.
	pull, err := v.GitHub.GetApplyPullRequest(ctx, owner, repo, req.PR)
	if err != nil {
		return Verdict{}, fmt.Errorf("re-deriving PR state: %w", err)
	}
	if !pull.Merged {
		return Verdict{}, reject(ErrNotMerged, "pull request has not merged (§6.1)")
	}
	if pull.MergeCommitSHA == "" {
		return Verdict{}, reject(ErrNotMerged, "merged PR has no merge_commit_sha yet (§6.1)")
	}
	if meta.BaseSHA != baseSHA {
		return Verdict{}, reject(ErrBaseMoved, "artifact planned base %s, PR base is %s (§6.1)", short(meta.BaseSHA), short(baseSHA))
	}

	// merge_commit^1 == base_sha: the merge landed on the same base the plan
	// was built against. A moved base changes the tree too, so §6.2 would also
	// catch it — but the parent check names the cause instead of the symptom.
	parents, err := v.GitHub.CommitParents(ctx, owner, repo, pull.MergeCommitSHA)
	if err != nil {
		return Verdict{}, fmt.Errorf("reading merge commit parents: %w", err)
	}
	if len(parents) == 0 || parents[0] != baseSHA {
		return Verdict{}, reject(ErrBaseMoved, "merge commit's first parent is %s, planned base was %s (§6.1)", short(first(parents)), short(baseSHA))
	}

	// 5. §6.2 — tree-SHA equality. The merge commit SHA is new under squash and
	// rebase; its tree is byte-identical to the reviewed tree under every
	// strategy (merge, squash, rebase), given §4.1's up-to-date head.
	mergeTree, err := v.GitHub.GetCommitTree(ctx, owner, repo, pull.MergeCommitSHA)
	if err != nil {
		return Verdict{}, fmt.Errorf("reading merge commit tree: %w", err)
	}
	if mergeTree != meta.PlannedTreeSHA {
		return Verdict{}, reject(ErrTreeMismatch, "merge commit tree %s != planned_tree_sha %s — the merged tree is not the reviewed tree (§6.2)", short(mergeTree), short(meta.PlannedTreeSHA))
	}

	// 6. §3.1 — the workspace must still be authorized by the config on the
	// trusted ref's *current* tip. A workspace deauthorized between plan and
	// merge must not apply (config is fail-closed on change).
	cfg, _, err := (&config.Loader{Contents: v.Trusted, Trusted: config.TrustedRefs{owner + "/" + repo: v.ConfigRef}}).LoadFromTrustedRef(ctx, owner, repo)
	if err != nil {
		return Verdict{}, fmt.Errorf("re-reading trusted config: %w", err)
	}
	if _, ok := cfg.Workspace(req.Workspace); !ok {
		return Verdict{}, reject(ErrTreeMismatch, "workspace %q no longer exists on the trusted ref (§3.1)", req.Workspace)
	}

	// Staleness is not decidable here without running terraform: the §6.3
	// refusal happens when Terraform itself rejects the plan. The verdict
	// carries the expectation; the caller (or the v0.2 worker) maps Terraform's
	// refusal to ErrStale with the §6.3 explanation.
	return Verdict{
		Reason: fmt.Sprintf("plan verified: merged as %s, tree %s matches, signed and kind=pr", short(pull.MergeCommitSHA), short(mergeTree)),
		Meta:   meta,
	}, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func first(parents []string) string {
	if len(parents) == 0 {
		return ""
	}
	return parents[0]
}

// WouldApply renders what verification passed and what applying would do —
// the v0.1 terminal output. It never applies.
func WouldApply(v Verdict, workspace string, applyTimeout time.Duration) string {
	lines := []string{
		"Verified — this plan may be applied:",
		fmt.Sprintf("  repo       %s", v.Meta.Repo),
		fmt.Sprintf("  pr         #%d", v.Meta.PR),
		fmt.Sprintf("  workspace  %s", workspace),
		fmt.Sprintf("  base/head  %s..%s", short(v.Meta.BaseSHA), short(v.Meta.HeadSHA)),
		fmt.Sprintf("  merge      %s (tree %s)", short(v.Meta.MergeCommitSHA), short(v.Meta.PlannedTreeSHA)),
		fmt.Sprintf("  terraform  %s", v.Meta.TerraformVersion),
	}
	var counts []string
	for _, action := range []string{"create", "update", "delete", "replace"} {
		if n := v.Meta.ResourceChangeCounts[action]; n > 0 {
			counts = append(counts, fmt.Sprintf("%d to %s", n, action))
		}
	}
	changes := "no resource changes"
	if len(counts) > 0 {
		changes = strings.Join(counts, ", ")
	}
	lines = append(lines, fmt.Sprintf("  changes    %s", changes))
	lines = append(lines, "",
		"Run `terraform apply <downloaded tfplan>` with your own credentials.",
		"The tool holds no write identity by design (§7).")
	_ = applyTimeout
	return strings.Join(lines, "\n")
}
