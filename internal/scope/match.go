// Package scope answers the first question of every event: given a PR's base branch and the
// files it changes, which workspaces need a plan?
//
// Everything here operates on a config already loaded from the repo's trusted ref. Two inputs
// with two very different trust levels meet in this package, and keeping them straight is the
// point: the config decides *which identity* may be assumed and comes from a ref the PR author
// cannot write to; the base ref and changed paths come from the PR and decide only *which of
// the already-authorized workspaces is relevant*.
package scope

import (
	"errors"
	"path"
	"strings"

	"github.com/sampleserve/terraform-on-github/internal/config"
)

// Result is the outcome of scoping one PR.
type Result struct {
	// InScope lists the workspaces to plan, in config order so check runs appear predictably.
	InScope []config.Workspace

	// Candidates are the workspaces bound to this base branch regardless of the diff. Used to
	// explain "why did nothing plan?" in the skipped check.
	Candidates []config.Workspace

	// ConfigChanged is true when the PR modifies config.Filename. Such a change takes effect
	// only after merge, since the config is read from the base branch — the caller posts an
	// advisory check saying so. See DESIGN.md §3.1.
	ConfigChanged bool
}

// Match selects the workspaces a pull request affects.
//
//	baseRef      the PR's base branch ("main", "release/2026-08")
//	changedPaths repo-relative paths from GET /compare/{base_sha}...{head_sha}, fully paginated
//
// A workspace is in scope when its Branch glob matches baseRef AND at least one changed path
// falls under its Dir or matches one of its Watch globs.
//
// Note the asymmetry: Branch matching selects *which mapping applies* from a config that arrived
// on the trusted ref. Path matching is a filter on *relevance*, and is the only place PR-supplied
// data enters the decision. Getting path matching wrong over-plans or under-plans; getting the
// config's provenance wrong hands out the wrong credentials — which is why baseRef is used to
// select among authorized entries and never to decide where the config itself came from.
func Match(cfg *config.Config, baseRef string, changedPaths []string) (Result, error) {
	if cfg == nil {
		return Result{}, errors.New("scope: nil config")
	}
	if baseRef == "" {
		return Result{}, errors.New("scope: empty base ref")
	}

	var r Result
	for _, p := range changedPaths {
		if p == config.Filename {
			r.ConfigChanged = true
			break
		}
	}

	for _, w := range cfg.Workspaces {
		// Branch first, and note what each half of this decides. The branch pattern selects which
		// mapping applies, from a config that arrived on the trusted ref — that is the half that
		// decides which credentials may be assumed. Path matching is a relevance filter, and is
		// the only place PR-supplied data enters. Getting the filter wrong over- or under-plans;
		// getting the provenance wrong hands out the wrong identity.
		if !matchBranch(w.Branch, baseRef) {
			continue
		}
		r.Candidates = append(r.Candidates, w)
		if touches(w, changedPaths) {
			r.InScope = append(r.InScope, w)
		}
	}
	return r, nil
}

// MatchAllCandidates scopes every workspace bound to baseRef, ignoring the diff.
//
// For the case where the changed-path list cannot be trusted to be complete — GitHub's compare
// endpoint caps at 300 files. Over-planning wastes a few minutes; silently not planning a
// workspace whose files changed is the failure nobody notices, so the truncated case fails
// toward doing more work rather than less.
func MatchAllCandidates(cfg *config.Config, baseRef string, changedPaths []string) (Result, error) {
	r, err := Match(cfg, baseRef, changedPaths)
	if err != nil {
		return r, err
	}
	r.InScope = r.Candidates
	return r, nil
}

// matchBranch evaluates a Workspace.Branch pattern against a base ref. Supports a trailing
// "*" segment ("release/*"); anything more expressive invites mistakes in a security-relevant
// predicate.
func matchBranch(pattern, ref string) bool {
	if pattern == "" || ref == "" {
		return false
	}
	if pattern == ref {
		return true
	}
	// One trailing "*" segment, and nothing more expressive. This is a security-relevant
	// predicate — it chooses which identity a run may assume — so it has to be obvious at a
	// glance what it does and does not match. "release/*" matches "release/2026-08" and not
	// "release/a/b".
	prefix, ok := strings.CutSuffix(pattern, "/*")
	if !ok {
		return false
	}
	rest, ok := strings.CutPrefix(ref, prefix+"/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// touches reports whether any changed path is inside w.Dir or matches a w.Watch glob.
//
// Dir matching is prefix-on-path-boundary: "envs/prod" must not match "envs/production".
// Watch globs use doublestar semantics so "modules/vpc/**" behaves as authors expect.
func touches(w config.Workspace, changedPaths []string) bool {
	dir := path.Clean(w.Dir)
	for _, p := range changedPaths {
		// Prefix on a path boundary: "envs/prod" must not match "envs/production".
		if dir == "." || p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
		for _, g := range w.Watch {
			if matchPathGlob(g, p) {
				return true
			}
		}
	}
	return false
}
