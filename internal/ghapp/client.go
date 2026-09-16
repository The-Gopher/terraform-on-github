package ghapp

import (
	"context"
	"fmt"

	"github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"
)

// Client implements config.ContentsReader.
type Client struct {
	gh *github.Client
}

func NewClient(ctx context.Context, token string) *Client {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	return &Client{
		gh: github.NewClient(tc),
	}
}

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

func (c *Client) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	refObj, _, err := c.gh.Git.GetRef(ctx, owner, repo, ref)
	if err != nil {
		return "", fmt.Errorf("failed to resolve ref %q: %w", ref, err)
	}
	return refObj.GetObject().GetSHA(), nil
}

func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get PR %d: %w", number, err)
	}
	return pr, nil
}

type Comparison struct {
	Files    []*github.CommitFile
	AheadBy  int
	BehindBy int
}

func (c *Client) CompareCommits(ctx context.Context, owner, repo, base, head string) (*Comparison, error) {
	res, _, err := c.gh.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to compare %s...%s: %w", base, head, err)
	}
	return &Comparison{
		Files:    res.Files,
		AheadBy:  res.GetAheadBy(),
		BehindBy: res.GetBehindBy(),
	}, nil
}
