// Package ghcli reaches GitHub through the `gh` CLI.
//
// This is the local counterpart to internal/ghapp, and the difference between them is the point.
// ghapp mints a down-scoped installation token from an App private key, so a compromised plan
// worker holds contents:read and checks:write and nothing else. ghcli runs as whoever ran
// `gh auth login` — no App, no JWT, no scoping. The local commands therefore have exactly the
// operator's access: no more, and no less.
//
// That is a real reduction and docs/LOCAL.md says so. What it buys is a first iteration with no
// App registration, no webhook endpoint and no secret to store.
package ghcli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sampleserve/terraform-on-github/internal/config"
)

// Client is a `gh` invoker. The zero value works and uses `gh` from PATH.
type Client struct {
	// Binary overrides the gh executable. Empty means "gh".
	Binary string
}

// ErrNotFound is any 404 from the API.
var ErrNotFound = errors.New("not found")

func (c *Client) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "gh"
}

// Preflight checks that gh exists and is authenticated, so a missing login is one clear error at
// startup rather than a confusing 404 later.
func (c *Client) Preflight(ctx context.Context) error {
	if _, err := exec.LookPath(c.binary()); err != nil {
		return fmt.Errorf("%s not found on PATH; install the GitHub CLI (https://cli.github.com)", c.binary())
	}
	cmd := exec.CommandContext(ctx, c.binary(), "auth", "status")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("`%s auth status` failed; run `%s auth login`\n%s",
			c.binary(), c.binary(), strings.TrimSpace(out.String()))
	}
	return nil
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		reason := lastLine(stderr.String())
		if reason == "" {
			reason = lastLine(stdout.String())
		}
		reason = strings.TrimPrefix(reason, "gh: ")
		if strings.Contains(strings.ToLower(reason), "not found") {
			return nil, fmt.Errorf("%s: %w", shown(args), ErrNotFound)
		}
		if reason == "" {
			reason = err.Error()
		}
		return nil, fmt.Errorf("%s: %s", shown(args), reason)
	}
	return stdout.Bytes(), nil
}

func (c *Client) api(ctx context.Context, path string, out any) error {
	body, err := c.run(ctx, "api", "-H", "Accept: application/vnd.github+json", path)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *Client) apiWith(ctx context.Context, method, path string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.binary(), "api", "-X", method,
		"-H", "Accept: application/vnd.github+json", path, "--input", "-")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api -X %s %s: %s", method, path, lastLine(stderr.String()))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(stdout.Bytes(), out)
}

// shown renders the command as an operator would type it, with our boilerplate headers dropped.
func shown(args []string) string {
	var keep []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-H" {
			i++
			continue
		}
		keep = append(keep, args[i])
	}
	return "gh " + strings.Join(keep, " ")
}

func lastLine(s string) string {
	var last string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			last = strings.TrimSpace(l)
		}
	}
	return last
}

// ---------------------------------------------------------------------------
// Repository and identity
// ---------------------------------------------------------------------------

// Repo is an owner/name pair.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string   { return r.Owner + "/" + r.Name }
func (r Repo) CloneURL() string { return "https://github.com/" + r.String() + ".git" }
func ParseRepo(s string) (Repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" {
		return Repo{}, fmt.Errorf("%q is not owner/repo", s)
	}
	return Repo{Owner: owner, Name: name}, nil
}

// RepoFromCwd resolves the repository from the current directory's git remotes.
func (c *Client) RepoFromCwd(ctx context.Context) (Repo, error) {
	body, err := c.run(ctx, "repo", "view", "--json", "owner,name")
	if err != nil {
		return Repo{}, err
	}
	var v struct {
		Owner struct{ Login string } `json:"owner"`
		Name  string                 `json:"name"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return Repo{}, err
	}
	return Repo{Owner: v.Owner.Login, Name: v.Name}, nil
}

// ViewerLogin is the authenticated user, recorded in Meta so a plan says who produced it.
func (c *Client) ViewerLogin(ctx context.Context) (string, error) {
	var v struct{ Login string }
	if err := c.api(ctx, "user", &v); err != nil {
		return "", err
	}
	return v.Login, nil
}

// DefaultBranch is the fallback trusted ref.
func (c *Client) DefaultBranch(ctx context.Context, r Repo) (string, error) {
	var v struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.api(ctx, "repos/"+r.String(), &v); err != nil {
		return "", err
	}
	return v.DefaultBranch, nil
}

// ---------------------------------------------------------------------------
// config.ContentsReader
// ---------------------------------------------------------------------------

// ResolveRef turns a ref into a commit SHA. An empty ref means the default branch.
func (c *Client) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	r := Repo{Owner: owner, Name: repo}
	if ref == "" {
		b, err := c.DefaultBranch(ctx, r)
		if err != nil {
			return "", err
		}
		ref = b
	}
	var v struct{ SHA string }
	if err := c.api(ctx, fmt.Sprintf("repos/%s/commits/%s", r, ref), &v); err != nil {
		return "", err
	}
	return v.SHA, nil
}

// ReadFileAtSHA reads one blob at an exact commit.
//
// Returns config.ErrFileNotFound for a 404 so the Loader can tell an un-onboarded repository
// from a failed read — the two mean opposite things.
func (c *Client) ReadFileAtSHA(ctx context.Context, owner, repo, path, sha string) ([]byte, error) {
	var v struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	err := c.api(ctx, fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s", owner, repo, path, sha), &v)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: %s at %s", config.ErrFileNotFound, path, sha)
	}
	if err != nil {
		return nil, err
	}
	if v.Encoding != "base64" {
		return nil, fmt.Errorf("%s: unexpected encoding %q", path, v.Encoding)
	}
	// The API wraps base64 at 60 columns.
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(v.Content, "\n", ""))
}

// Compile-time assertion that this satisfies the Loader's interface, so the trusted-ref load path
// is the one the local commands use rather than a shortcut around it.
var _ config.ContentsReader = (*Client)(nil)

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

// PullRequest is the subset of a PR the commands act on.
type PullRequest struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		SHA   string `json:"sha"`
		Label string `json:"label"`
	} `json:"head"`
}

func (c *Client) PullRequest(ctx context.Context, r Repo, number int) (PullRequest, error) {
	var pr PullRequest
	err := c.api(ctx, fmt.Sprintf("repos/%s/pulls/%d", r, number), &pr)
	if errors.Is(err, ErrNotFound) {
		return pr, fmt.Errorf("no pull request #%d in %s", number, r)
	}
	return pr, err
}

// Commit returns a commit and its tree SHA.
func (c *Client) Commit(ctx context.Context, r Repo, ref string) (sha, treeSHA string, err error) {
	var v struct {
		SHA    string `json:"sha"`
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if err := c.api(ctx, fmt.Sprintf("repos/%s/commits/%s", r, ref), &v); err != nil {
		return "", "", err
	}
	return v.SHA, v.Commit.Tree.SHA, nil
}

// compareFileCap is GitHub's limit on the files array of a compare response.
const compareFileCap = 300

// Comparison is GET /compare/{base}...{head}, with the file list paginated.
type Comparison struct {
	Status         string
	BehindBy       int
	MergeBaseSHA   string
	ChangedPaths   []string
	FilesTruncated bool
}

// Compare answers the question DESIGN 4.1 asks. It also reports whether the file list may be
// incomplete, which callers must treat as "scope every candidate workspace".
func (c *Client) Compare(ctx context.Context, r Repo, base, head string) (Comparison, error) {
	type page struct {
		Status          string `json:"status"`
		BehindBy        int    `json:"behind_by"`
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}

	var out Comparison
	for p := 1; ; p++ {
		var d page
		url := fmt.Sprintf("repos/%s/compare/%s...%s?per_page=100&page=%d", r, base, head, p)
		if err := c.api(ctx, url, &d); err != nil {
			return Comparison{}, err
		}
		if p == 1 {
			out.Status = d.Status
			out.BehindBy = d.BehindBy
			out.MergeBaseSHA = d.MergeBaseCommit.SHA
		}
		for _, f := range d.Files {
			out.ChangedPaths = append(out.ChangedPaths, f.Filename)
		}
		if len(d.Files) < 100 || len(out.ChangedPaths) >= compareFileCap {
			break
		}
	}
	out.FilesTruncated = len(out.ChangedPaths) >= compareFileCap
	return out, nil
}

// BranchRequiresReviews backs apply.require_protected_base.
//
// Three-valued on purpose. A nil result means the question could not be answered — the token
// cannot read branch protection — which the caller must treat as a refusal, not a pass. A token
// that cannot confirm the premise has not confirmed it.
func (c *Client) BranchRequiresReviews(ctx context.Context, r Repo, branch string) (*bool, error) {
	var v struct {
		Protected  bool `json:"protected"`
		Protection *struct {
			RequiredPullRequestReviews *json.RawMessage `json:"required_pull_request_reviews"`
		} `json:"protection"`
	}
	if err := c.api(ctx, fmt.Sprintf("repos/%s/branches/%s", r, branch), &v); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("branch %q not found in %s", branch, r)
		}
		return nil, nil // most likely a 403: unreadable, not unprotected
	}
	if !v.Protected {
		no := false
		return &no, nil
	}
	if v.Protection == nil {
		return nil, nil
	}
	yes := v.Protection.RequiredPullRequestReviews != nil
	return &yes, nil
}

// ---------------------------------------------------------------------------
// Comments
//
// The design publishes check runs. A local command cannot: a check run needs checks:write on a
// head SHA the App owns. PR comments are the closest thing an operator's own token can post, and
// they carry the same content. DESIGN 13.2 asked whether comments should be on by default;
// locally they are the only channel.
// ---------------------------------------------------------------------------

// Comment is one issue comment.
type Comment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

// Marker is the hidden anchor that makes a comment addressable.
func Marker(kind, workspace string) string {
	return fmt.Sprintf("<!-- tfog:%s:%s -->", kind, workspace)
}

func (c *Client) ListComments(ctx context.Context, r Repo, number int) ([]Comment, error) {
	body, err := c.run(ctx, "api", "--paginate", "-H", "Accept: application/vnd.github+json",
		fmt.Sprintf("repos/%s/issues/%d/comments", r, number))
	if err != nil {
		return nil, err
	}
	// --paginate concatenates JSON arrays; normalise them into one.
	var all []Comment
	dec := json.NewDecoder(bytes.NewReader(body))
	for {
		var batch []Comment
		if err := dec.Decode(&batch); err != nil {
			break
		}
		all = append(all, batch...)
	}
	return all, nil
}

// FindComment returns the comment carrying mark, or nil.
func (c *Client) FindComment(ctx context.Context, r Repo, number int, mark string) (*Comment, error) {
	comments, err := c.ListComments(ctx, r, number)
	if err != nil {
		return nil, err
	}
	for i := range comments {
		if strings.Contains(comments[i].Body, mark) {
			return &comments[i], nil
		}
	}
	return nil, nil
}

func (c *Client) AddComment(ctx context.Context, r Repo, number int, body string) (Comment, error) {
	var out Comment
	err := c.apiWith(ctx, "POST", fmt.Sprintf("repos/%s/issues/%d/comments", r, number),
		map[string]string{"body": body}, &out)
	return out, err
}

// UpsertComment edits the comment carrying mark, or posts a new one.
//
// Plan comments are sticky. A push supersedes the previous plan, and leaving both in the timeline
// invites a reviewer to read the wrong one; the body always names the head SHA it describes, so
// which push it belongs to stays unambiguous.
func (c *Client) UpsertComment(ctx context.Context, r Repo, number int, mark, body string) (Comment, error) {
	if !strings.Contains(body, mark) {
		body = mark + "\n" + body
	}
	existing, err := c.FindComment(ctx, r, number, mark)
	if err != nil {
		return Comment{}, err
	}
	if existing == nil {
		return c.AddComment(ctx, r, number, body)
	}
	var out Comment
	err = c.apiWith(ctx, "PATCH", fmt.Sprintf("repos/%s/issues/comments/%s", r, strconv.FormatInt(existing.ID, 10)),
		map[string]string{"body": body}, &out)
	return out, err
}
