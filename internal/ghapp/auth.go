// Package ghapp wraps the GitHub App surface: authentication, webhook verification, check
// runs and deployments.
package ghapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-github/v66/github"
	"golang.org/x/oauth2"
)

var errNotImplemented = errors.New("not implemented")

// ErrNotFound distinguishes "repo has no config" from a transport failure, so callers can
// treat an un-onboarded repository as a no-op rather than a retry.
var ErrNotFound = errors.New("github: not found")

// Permission levels for a down-scoped installation token.
type Permission string

const (
	PermRead  Permission = "read"
	PermWrite Permission = "write"
)

// TokenScope is the subset of the App's permissions a service asks for.
//
// The App is installed with the union of what both services need, but
// POST /app/installations/{id}/access_tokens accepts `permissions` and `repositories`, so each
// worker mints a token narrower than the installation itself and pinned to one repo.
//
// This is defence in depth, not a boundary: both services can read the App private key, so
// either *could* mint a full token. Hard separation needs two GitHub Apps with separate keys —
// see DESIGN.md §7.3 for the tradeoff.
type TokenScope struct {
	Repositories []string
	Permissions  map[string]Permission
}

// PlanScope is what the plan service needs: read the tree, write check runs.
func PlanScope(repo string) TokenScope {
	return TokenScope{
		Repositories: []string{repo},
		Permissions: map[string]Permission{
			"metadata":      PermRead,
			"contents":      PermRead,
			"pull_requests": PermRead,
			"checks":        PermWrite,
		},
	}
}

// ApplyScope adds deployments — the approval gate and the apply audit trail — and nothing else.
// Notably absent: any write to contents.
func ApplyScope(repo string) TokenScope {
	return TokenScope{
		Repositories: []string{repo},
		Permissions: map[string]Permission{
			"metadata":      PermRead,
			"contents":      PermRead,
			"pull_requests": PermRead,
			"checks":        PermWrite,
			"deployments":   PermWrite,
		},
	}
}

// AppAuth mints installation tokens from the App's private key.
//
// The key lives in Secret Manager and is fetched once per instance. Rotation is handled by
// adding a new secret version and letting instances recycle; both versions are valid to
// GitHub during overlap only if both are registered on the App, so rotation is: add key to
// App → add secret version → redeploy → remove old key from App.
type AppAuth struct {
	AppID int64

	// SecretName is the Secret Manager resource for the PEM, e.g.
	// projects/acme-tf/secrets/github-app-key/versions/latest
	SecretName string

	// cache of installation tokens keyed by (installationID, canonical scope). GitHub tokens
	// last 1h; refresh at 50m.
}

// InstallationToken returns a token for one installation, down-scoped to scope.
func (a *AppAuth) InstallationToken(ctx context.Context, installationID int64, scope TokenScope) (string, time.Time, error) {
	// jwt := signAppJWT(a.privateKey, a.AppID)  // 10m max lifetime per GitHub
	// POST /app/installations/{id}/access_tokens  {permissions, repositories}
	return "", time.Time{}, errNotImplemented
}

// NewClient creates a GitHub client using a personal access token.
func NewClient(ctx context.Context, token string) (*Client, error) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	return &Client{
		api: github.NewClient(tc),
	}, nil
}
// Client is a per-installation, per-scope GitHub client.
type Client struct {
	api   *github.Client
	Owner string
	Repo  string
}

// ReadFileAtSHA reads a blob at an exact commit. Satisfies config.ContentsReader.
//
// Takes a SHA rather than a ref on purpose: the config loader must not be able to read from a
// mutable ref, and a caller holding only a ref has not yet decided which commit it trusts.
func (c *Client) ReadFileAtSHA(ctx context.Context, owner, repo, path, sha string) ([]byte, error) {
	file, _, _, err := c.api.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{
		Ref: sha,
	})
	if err != nil {
		return nil, fmt.Errorf("github read file: %w", err)
	}

	content, err := file.GetContent()
	if err != nil {
		return nil, fmt.Errorf("github decode content: %w", err)
	}

	return []byte(content), nil
}

// ResolveRef resolves a ref to a commit SHA, or the repository's default branch tip when ref is
// empty. Satisfies config.ContentsReader, and is how the trusted config ref becomes a commit.
func (c *Client) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	refObj, _, err := c.api.Git.GetRef(ctx, owner, repo, ref)
	if err != nil {
		return "", fmt.Errorf("github resolve ref: %w", err)
	}

	return refObj.GetObject().GetSHA(), nil
}

// BranchTipSHA resolves a branch to its current tip.
//
// Used instead of pull_request.base.sha, which is the base SHA at PR *creation* time and is
// stale the moment anything else merges. Every plan is keyed against a freshly resolved base.
func (c *Client) BranchTipSHA(ctx context.Context, branch string) (string, error) {
	return "", errNotImplemented
}

// ChangedFiles returns repo-relative paths changed between two commits, following pagination.
//
// The compare API truncates at 300 files (`files` is capped even though `total_commits` is
// not). On truncation, fall back to scoping every candidate workspace on the branch rather
// than silently planning a subset — over-planning is a cost, under-planning is a correctness
// bug.
func (c *Client) ChangedFiles(ctx context.Context, baseSHA, headSHA string) (paths []string, truncated bool, err error) {
	return nil, false, errNotImplemented
}

// PullRequest is the subset of PR state the workers reason about.
type PullRequest struct {
	Number         int
	BaseRef        string
	HeadSHA        string
	Merged         bool
	MergeCommitSHA string

	// AuthorAssociation drives fork/first-contributor gating: OWNER, MEMBER, COLLABORATOR are
	// trusted to trigger a plan; CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR and NONE are not.
	AuthorAssociation string
	FromFork          bool
}

// GetPullRequest fetches live PR state. The apply worker calls this to re-derive
// authorization; it never trusts the merge signal carried in its task payload.
func (c *Client) GetPullRequest(ctx context.Context, number int) (PullRequest, error) {
	return PullRequest{}, errNotImplemented
}

// CommitTreeSHA returns the tree SHA of a commit — the content identity of the working tree,
// independent of commit metadata.
//
// This is the primitive behind merge-time verification. Because §4.1 requires the head to
// already contain its base, every merge strategy — merge commit, squash, rebase — produces a
// commit whose tree equals the planned head's tree, even though the commit SHA cannot match.
// Comparing trees proves the applied content is the reviewed content. See DESIGN.md §6.2.
func (c *Client) CommitTreeSHA(ctx context.Context, commitSHA string) (string, error) {
	return "", errNotImplemented
}

// CompareStatus is the `status` field of the compare API: "identical", "ahead", "behind" or
// "diverged", from the perspective of base...head.
type CompareStatus string

const (
	StatusIdentical CompareStatus = "identical"
	StatusAhead     CompareStatus = "ahead" // head is ahead of base: base is an ancestor. Good.
	StatusBehind    CompareStatus = "behind"
	StatusDiverged  CompareStatus = "diverged"
)

// Compare answers whether the PR head already contains its base.
//
// Only `identical` and `ahead` mean base is an ancestor of head — the precondition that makes the
// head's tree equal to the tree any merge strategy would produce, and therefore the precondition
// for planning the head at all (DESIGN.md §4.1). `behind` and `diverged` get a failure check
// telling the author to update the branch, with behindBy so the message is specific.
//
// This replaces an earlier design that fetched refs/pull/N/merge and asserted its parents.
// That ref is mutable and recomputed asynchronously, so it needed a retryable not-ready error and
// backoff; compare is synchronous and answers directly.
func (c *Client) Compare(ctx context.Context, baseSHA, headSHA string) (status CompareStatus, behindBy int, err error) {
	return "", 0, errNotImplemented
}

// BranchProtected reports whether a branch requires reviews.
//
// Used only by workspaces setting `require_protected_base`, because it needs the App's
// `administration: read` permission — a real widening of the minimum set in §7.3. It turns
// "this base branch is reviewed" from an assumption into an assertion, which is exactly the
// assumption that made reading config from the base branch look safe. See DESIGN.md §3.1.6.
func (c *Client) BranchProtected(ctx context.Context, branch string) (bool, error) {
	return false, errNotImplemented
}
