// Package scope decides which workspaces a pull request affects: those bound to the PR's base
// branch whose directory or watch globs cover a changed path.
package scope

import (
	"context"
	"fmt"
	"strings"

	"github.com/gobwas/glob"
	"github.com/google/go-github/v60/github"

	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/ghapp"
)

// Status is where a PR's head stands relative to the current tip of its base branch.
type Status string

// Status values. Only StatusAhead and StatusUpToDate may be planned (§4.1).
const (
	StatusAhead    Status = "ahead"
	StatusBehind   Status = "behind"
	StatusDiverged Status = "diverged"
	StatusUpToDate Status = "up-to-date"
)

// Match is one workspace a PR affects, with the changed path that pulled it in.
type Match struct {
	Workspace config.Workspace
	Reason    string
	Status    Status
}

// GitHubClient is the subset of the GitHub API scoping reads.
type GitHubClient interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	ResolveRef(ctx context.Context, owner, repo, ref string) (string, error)
	CompareCommits(ctx context.Context, owner, repo, base, head string) (*ghapp.Comparison, error)
}

// Results is the outcome of scoping one PR.
type Results struct {
	Workspaces    []Match
	ConfigChanged bool

	// BaseSHA and HeadSHA are the coordinates the plan key composes from (§5):
	// base is the tip of the base ref resolved now, head is the PR head at
	// scope time. Staging needs them to key the artifacts; apply verifies
	// against them again from GitHub (§6.1).
	BaseSHA string
	HeadSHA string
}

// Scoper matches a PR's changes against a config's workspaces.
type Scoper struct {
	client GitHubClient
	cfg    *config.Config
}

// NewScoper returns a Scoper that reads PR state through client and matches against cfg.
func NewScoper(client GitHubClient, cfg *config.Config) *Scoper {
	return &Scoper{
		client: client,
		cfg:    cfg,
	}
}

// Scope returns the workspaces PR prNumber affects. A workspace is in scope when its branch
// pattern matches the PR's base branch and a changed path is under its dir or matches one of
// its watch globs.
func (s *Scoper) Scope(ctx context.Context, owner, repo string, prNumber int) (*Results, error) {
	pr, err := s.client.GetPullRequest(ctx, owner, repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("get pr: %w", err)
	}

	baseRef := pr.GetBase().GetRef()
	headSHA := pr.GetHead().GetSHA()

	baseSHA, err := s.client.ResolveRef(ctx, owner, repo, baseRef)
	if err != nil {
		return nil, fmt.Errorf("resolve base ref: %w", err)
	}

	cmp, err := s.client.CompareCommits(ctx, owner, repo, baseSHA, headSHA)
	if err != nil {
		return nil, fmt.Errorf("compare commits: %w", err)
	}

	configChanged := false
	for _, f := range cmp.Files {
		if f.GetFilename() == config.Filename {
			configChanged = true
			break
		}
	}

	status := deriveStatus(cmp)
	var matches []Match
	for _, w := range s.cfg.Workspaces {
		if !globMatches(w.Branch, baseRef) {
			continue
		}
		reason, inScope := isWorkspaceAffected(w, cmp.Files)
		if !inScope {
			continue
		}
		matches = append(matches, Match{
			Workspace: w,
			Reason:    reason,
			Status:    status,
		})
	}

	return &Results{
		Workspaces:    matches,
		ConfigChanged: configChanged,
		BaseSHA:       baseSHA,
		HeadSHA:       headSHA,
	}, nil
}

func isWorkspaceAffected(w config.Workspace, files []*github.CommitFile) (string, bool) {
	for _, f := range files {
		path := f.GetFilename()
		if pathUnderDir(path, w.Dir) {
			return fmt.Sprintf("changed path %q is under %q", path, w.Dir), true
		}
		for _, watch := range w.Watch {
			if globMatches(watch, path) {
				return fmt.Sprintf("changed path %q matches watch %q", path, watch), true
			}
		}
	}
	return "", false
}

func pathUnderDir(path, dir string) bool {
	if path == dir {
		return true
	}
	if dir == "" {
		return false
	}
	return strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// globMatches reports whether s matches pattern. An invalid pattern matches only itself.
func globMatches(pattern, s string) bool {
	g, err := glob.Compile(pattern)
	if err != nil {
		return pattern == s
	}
	return g.Match(s)
}

func deriveStatus(cmp *ghapp.Comparison) Status {
	ahead := cmp.AheadBy > 0
	behind := cmp.BehindBy > 0

	switch {
	case ahead && behind:
		return StatusDiverged
	case behind:
		return StatusBehind
	case ahead:
		return StatusAhead
	default:
		return StatusUpToDate
	}
}
