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

	"github.com/sampleserve/terraform-on-github/internal/config"
)

var errNotImplemented = errors.New("not implemented")

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
	return Result{}, errNotImplemented
}

// matchBranch evaluates a Workspace.Branch pattern against a base ref. Supports a trailing
// "*" segment ("release/*"); anything more expressive invites mistakes in a security-relevant
// predicate.
func matchBranch(pattern, ref string) bool { return false }

// touches reports whether any changed path is inside w.Dir or matches a w.Watch glob.
//
// Dir matching is prefix-on-path-boundary: "envs/prod" must not match "envs/production".
// Watch globs use doublestar semantics so "modules/vpc/**" behaves as authors expect.
func touches(w config.Workspace, changedPaths []string) bool { return false }
