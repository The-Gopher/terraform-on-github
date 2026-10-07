// Package ghapp is the GitHub API adapter: it implements the narrow client interfaces that
// config, scope and apply declare, over go-github.
package ghapp

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"

	"github.com/the-gopher/terraform-on-github/internal/apply"
)

// Client is an authenticated GitHub API client. It implements config.ContentsReader,
// scope.GitHubClient and apply.GitHubClient.
type Client struct {
	gh *github.Client
}

// NewClient returns a Client that authenticates with a static token. ctx is used only to
// build the underlying HTTP client.
func NewClient(ctx context.Context, token string) *Client {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	return &Client{
		gh: github.NewClient(tc),
	}
}

// GetContents returns the content of the file at path in repo at ref.
func (c *Client) GetContents(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	file, _, resp, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{
		Ref: ref,
	})
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("file not found")
	}
	if err != nil {
		return nil, err
	}

	content, err := file.GetContent()
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

// ReadFileAtSHA implements config.ContentsReader: reads a path at an exact
// commit SHA, which is what the trusted-ref loader reads at.
func (c *Client) ReadFileAtSHA(ctx context.Context, owner, repo, path, sha string) ([]byte, error) {
	return c.GetContents(ctx, owner, repo, path, sha)
}

// normalizeRef expands a bare branch name into a fully qualified ref. The
// GitHub refs API only accepts qualified refs (heads/main, tags/v1), but PR
// payloads carry bare branch names.
func normalizeRef(ref string) string {
	switch {
	case strings.HasPrefix(ref, "refs/"), strings.HasPrefix(ref, "heads/"), strings.HasPrefix(ref, "tags/"):
		return ref
	default:
		return "heads/" + ref
	}
}

// ResolveRef returns the commit SHA ref currently points at. A bare branch name is accepted.
func (c *Client) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	refObj, _, err := c.gh.Git.GetRef(ctx, owner, repo, normalizeRef(ref))
	if err != nil {
		return "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}
	return refObj.GetObject().GetSHA(), nil
}

// ResolveBaseRef resolves a PR's base ref to its current tip — the same
// resolution §3.2 scoping does, so the artifact key composes from the same
// base SHA the plan side used.
func (c *Client) ResolveBaseRef(ctx context.Context, owner, repo string, pr *github.PullRequest) (string, error) {
	return c.ResolveRef(ctx, owner, repo, pr.GetBase().GetRef())
}

// GetPullRequest returns PR number in repo.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get PR %d: %w", number, err)
	}
	return pr, nil
}

// GetApplyPullRequest fetches a PR and returns it in apply.PullRequest shape,
// satisfying apply.GitHubClient.
func (c *Client) GetApplyPullRequest(ctx context.Context, owner, repo string, number int) (apply.PullRequest, error) {
	pr, err := c.GetPullRequest(ctx, owner, repo, number)
	if err != nil {
		return apply.PullRequest{}, err
	}
	return apply.PullRequest{
		Merged:         pr.GetMerged(),
		MergeCommitSHA: pr.GetMergeCommitSHA(),
		BaseRef:        pr.GetBase().GetRef(),
		BaseSHA:        pr.GetBase().GetSHA(),
		HeadSHA:        pr.GetHead().GetSHA(),
	}, nil
}

// ApplyPullRequestState adapts a *github.PullRequest to apply.PullRequest.
func (c *Client) ApplyPullRequestState(pr *github.PullRequest) apply.PullRequest {
	return apply.PullRequest{
		Merged:         pr.GetMerged(),
		MergeCommitSHA: pr.GetMergeCommitSHA(),
		BaseSHA:        pr.GetBase().GetSHA(),
		HeadSHA:        pr.GetHead().GetSHA(),
	}
}

// Comparison is the result of comparing two commits: the files changed between them and how
// far head is ahead of and behind base.
type Comparison struct {
	Files    []*github.CommitFile
	AheadBy  int
	BehindBy int
}

// CompareCommits compares base...head.
func (c *Client) CompareCommits(ctx context.Context, owner, repo, base, head string) (*Comparison, error) {
	res, _, err := c.gh.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return nil, fmt.Errorf("compare %s...%s: %w", base, head, err)
	}
	return &Comparison{
		Files:    res.Files,
		AheadBy:  res.GetAheadBy(),
		BehindBy: res.GetBehindBy(),
	}, nil
}

// GetCommitTree returns the tree SHA a commit points at. §4.1 makes the head's
// tree the planned tree; §6.2 compares it against the merge commit's tree.
func (c *Client) GetCommitTree(ctx context.Context, owner, repo, sha string) (string, error) {
	commit, _, err := c.gh.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return "", fmt.Errorf("get commit %s: %w", sha, err)
	}
	return commit.GetCommit().GetTree().GetSHA(), nil
}

// CommitParents returns a commit's parent SHAs, first-parent first — §6.1's
// `merge_commit^1 == base_sha` assertion reads parents[0].
func (c *Client) CommitParents(ctx context.Context, owner, repo, sha string) ([]string, error) {
	commit, _, err := c.gh.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return nil, fmt.Errorf("get commit %s: %w", sha, err)
	}
	parents := make([]string, 0, len(commit.Parents))
	for _, p := range commit.Parents {
		parents = append(parents, p.GetSHA())
	}
	return parents, nil
}
