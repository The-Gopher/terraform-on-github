package scope

import (
	"context"
	"fmt"

	"github.com/gobwas/glob"
	"github.com/google/go-github/v60/github"
	"github.com/the-gopher/terraform-on-github/internal/config"
	"github.com/the-gopher/terraform-on-github/internal/ghapp"
)

type Status string

const (
	StatusAhead    Status = "ahead"
	StatusBehind   Status = "behind"
	StatusDiverged Status = "diverged"
	StatusUpToDate Status = "up-to-date"
)

type Result struct {
	Workspace config.Workspace
	Reason    string
	Status    Status
}

type GitHubClient interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	ResolveRef(ctx context.Context, owner, repo, ref string) (string, error)
	CompareCommits(ctx context.Context, owner, repo, base, head string) (*ghapp.Comparison, error)
}

type ScopeResults struct {
	Workspaces    []Result
	ConfigChanged bool
}

func NewScoper(client GitHubClient, cfg *config.Config) *Scoper {
	return &Scoper{
		client: client,
		cfg:    cfg,
	}
}
type Scoper struct {
	client GitHubClient
	cfg    *config.Config
}

func (s *Scoper) Scope(ctx context.Context, owner, repo string, prNumber int) (*ScopeResults, error) {
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

	var results []Result
	configChanged := false

	for _, f := range cmp.Files {
		if f.GetFilename() == config.Filename {
			configChanged = true
			break
		}
	}

	for _, w := range s.cfg.Workspaces {
		if !s.branchMatches(w.Branch, baseRef) {
			continue
		}

		reason, inScope := s.isWorkspaceAffected(w, cmp.Files)
		if !inScope {
			continue
		}

		status := s.deriveStatus(cmp)
		results = append(results, Result{
			Workspace: w,
			Reason:    reason,
			Status:    status,
		})
	}

	return &ScopeResults{
		Workspaces:    results,
		ConfigChanged: configChanged,
	}, nil
}

func (s *Scoper) branchMatches(pattern, branch string) bool {
	g, err := glob.Compile(pattern)
	if err != nil {
		return pattern == branch
	}
	return g.Match(branch)
}
func (s *Scoper) isWorkspaceAffected(w config.Workspace, files []*github.CommitFile) (string, bool) {
	for _, f := range files {
		path := f.GetFilename()
		if s.pathUnderDir(path, w.Dir) {
			return fmt.Sprintf("changed path %q is under %q", path, w.Dir), true
		}
		for _, watch := range w.Watch {
			if s.pathMatchesGlob(path, watch) {
				return fmt.Sprintf("changed path %q matches watch %q", path, watch), true
			}
		}
	}
	return "", false
}

func (s *Scoper) pathUnderDir(path, dir string) bool {
	if path == dir {
		return true
	}
	if len(dir) == 0 {
		return false
	}
	d := dir
	if d[len(d)-1] != '/' {
		d += "/"
	}
	return len(path) >= len(d) && path[:len(d)] == d
}

func (s *Scoper) pathMatchesGlob(path, pattern string) bool {
	g, err := glob.Compile(pattern)
	if err != nil {
		return path == pattern
	}
	return g.Match(path)
}

func (s *Scoper) deriveStatus(cmp *ghapp.Comparison) Status {
	ahead := cmp.AheadBy > 0
	behind := cmp.BehindBy > 0

	if ahead && behind {
		return StatusDiverged
	}
	if behind {
		return StatusBehind
	}
	if ahead {
		return StatusAhead
	}
	return StatusUpToDate
}

