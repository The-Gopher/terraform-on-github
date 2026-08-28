package tf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrBranchBehindBase means the base branch has commits the PR head does not contain, so the
// head is not the merge result and planning it would plan the wrong tree.
//
// Not retryable: it needs a human to update the branch. See DESIGN.md §4.1.
var ErrBranchBehindBase = errors.New("tf: PR branch is behind its base; update the branch and push")

// Divergence is the answer to "is the head already the merge result?".
//
// Derived from GET /compare/{base}...{head}, which is synchronous and authoritative. An earlier
// design fetched GitHub's test-merge commit (refs/pull/N/merge) instead — a mutable ref GitHub
// recomputes asynchronously, which needed a parent assertion, a retryable not-ready error and
// backoff. One compare call replaces all of it.
type Divergence struct {
	// UpToDate is true when base is an ancestor of head (compare status "ahead" or "identical").
	// Only then is the head's tree equal to the tree every merge strategy would produce.
	UpToDate bool

	// BehindBy counts commits on base that head lacks — the number to put in the check so the
	// author knows what they are being asked to pull in.
	BehindBy int

	// Conflicted is true when the branches cannot merge cleanly. Reported alongside BehindBy
	// rather than as a separate failure: both are answered by "update your branch", so they are
	// one message.
	Conflicted bool
}

// Checkout is a working tree on tmpfs, discarded when the instance goes away.
type Checkout struct {
	Root string // /workspace/<run-id>, tmpfs, non-root, rest of the rootfs read-only

	// CommitSHA is what was checked out: the PR head when planning, the merge commit when
	// applying.
	CommitSHA string

	// TreeSHA is CommitSHA^{tree} — the content identity of this working tree.
	//
	// Recorded at plan time as store.Meta.PlannedTreeSHA and compared at apply time. Because
	// §4.1 guarantees the head already contains all of base, every merge strategy — merge commit,
	// squash, rebase — produces a commit whose tree equals the head's tree. So this one field
	// survives the merge, where the commit SHA cannot. See DESIGN.md §6.2.
	TreeSHA string

	// worktreeOf is the clone this tree was added to, when it came from `git worktree` rather
	// than a fresh clone. Cleanup has to unregister it there, not just delete the directory.
	worktreeOf string
}

// CheckoutHead fetches the PR head at an exact SHA, after the caller has confirmed
// Divergence.UpToDate.
//
// Planning the head is only correct under that precondition. Without it the head is missing
// whatever landed on base since the PR was cut, and the plan can show a clean diff for a change
// that conflicts semantically with what is already there.
//
// The alternative — planning a synthetic merge of head into base — is correct without the
// precondition but produces a plan containing other people's resource changes, which the author
// then has to disentangle from their own inside their own check. §4.1 explains why the trade goes
// this way for a Terraform repo, and why the merge queue is the escape hatch when the rebase cost
// bites.
func CheckoutHead(ctx context.Context, root, cloneURL, token, headSHA string) (*Checkout, error) {
	return clone(ctx, root, cloneURL, token, headSHA, nil)
}

// CheckoutCommit fetches one commit by SHA. Used at apply time on the real merge commit.
func CheckoutCommit(ctx context.Context, root, cloneURL, token, sha string) (*Checkout, error) {
	return clone(ctx, root, cloneURL, token, sha, nil)
}

// FetchRefs are refs to try when a bare SHA is not directly fetchable. GitHub always serves
// refs/pull/<n>/head; individual SHAs only when the server permits it.
type FetchRefs []string

// PullRequestRefs returns the refs worth trying for a PR head.
func PullRequestRefs(pr int, baseRef string) FetchRefs {
	refs := FetchRefs{fmt.Sprintf("refs/pull/%d/head", pr)}
	if baseRef != "" {
		refs = append(refs, baseRef)
	}
	return refs
}

// CheckoutFrom materialises sha in a throwaway working tree, preferring a `git worktree` off an
// existing local clone.
//
// The local path is there for the local commands in cmd/tfog-*: a worktree is fast, and it reuses
// whatever credentials the operator's clone already has, so nothing here ever handles a token.
// When sourceRepo is empty — or is not a clone of this repository — it falls back to a shallow
// clone, which is the path the services take.
func CheckoutFrom(ctx context.Context, root, sourceRepo, cloneURL, token, sha string, refs FetchRefs) (*Checkout, error) {
	if sourceRepo != "" {
		if c, err := worktree(ctx, root, sourceRepo, sha, refs); err == nil {
			return c, nil
		} else if !errors.Is(err, errNotAvailableLocally) {
			return nil, err
		}
	}
	return clone(ctx, root, cloneURL, token, sha, refs)
}

var errNotAvailableLocally = errors.New("tf: commit not obtainable from the local clone")

func worktree(ctx context.Context, root, sourceRepo, sha string, refs FetchRefs) (*Checkout, error) {
	if _, err := os.Stat(filepath.Join(sourceRepo, ".git")); err != nil {
		return nil, errNotAvailableLocally
	}
	if err := ensureCommit(ctx, sourceRepo, sha, refs); err != nil {
		return nil, errNotAvailableLocally
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return nil, err
	}
	if _, err := git(ctx, sourceRepo, "worktree", "add", "--detach", "--quiet", root, sha); err != nil {
		return nil, err
	}
	tree, err := git(ctx, root, "rev-parse", sha+"^{tree}")
	if err != nil {
		return nil, err
	}
	return &Checkout{Root: root, CommitSHA: sha, TreeSHA: tree, worktreeOf: sourceRepo}, nil
}

func clone(ctx context.Context, root, cloneURL, token, sha string, refs FetchRefs) (*Checkout, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if _, err := git(ctx, root, "init", "--quiet"); err != nil {
		return nil, err
	}
	remote := cloneURL
	if token != "" {
		// The token lands in the temp clone's .git/config, which Cleanup removes. Passing it in
		// the URL rather than a credential helper keeps this one process self-contained.
		remote = withToken(cloneURL, token)
	}
	if _, err := git(ctx, root, "remote", "add", "origin", remote); err != nil {
		return nil, err
	}
	if err := ensureCommit(ctx, root, sha, refs); err != nil {
		return nil, err
	}
	if _, err := git(ctx, root, "checkout", "--quiet", "--detach", sha); err != nil {
		return nil, err
	}
	head, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if head != sha {
		// A ref fetch can land on a different commit than asked for if the ref moved between the
		// API read and the fetch. Planning that tree would plan something nobody reviewed.
		return nil, fmt.Errorf("tf: checkout landed on %s, expected %s — the ref moved; retry", shortSHA(head), shortSHA(sha))
	}
	tree, err := git(ctx, root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	return &Checkout{Root: root, CommitSHA: sha, TreeSHA: tree}, nil
}

// ensureCommit makes sha available in repo, trying the bare SHA first and then each ref.
func ensureCommit(ctx context.Context, repo, sha string, refs FetchRefs) error {
	if hasCommit(ctx, repo, sha) {
		return nil
	}
	attempts := append([]string{sha}, refs...)
	var problems []string
	for _, target := range attempts {
		if _, err := git(ctx, repo, "fetch", "--quiet", "--depth=1", "origin", target); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s", target, firstLine(err.Error())))
			continue
		}
		if hasCommit(ctx, repo, sha) {
			return nil
		}
		problems = append(problems, fmt.Sprintf("%s: fetched, but %s is still missing", target, shortSHA(sha)))
	}
	return fmt.Errorf("tf: could not fetch %s:\n  %s", shortSHA(sha), strings.Join(problems, "\n  "))
}

func hasCommit(ctx context.Context, repo, sha string) bool {
	_, err := git(ctx, repo, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func withToken(cloneURL, token string) string {
	if rest, ok := strings.CutPrefix(cloneURL, "https://"); ok {
		return "https://x-access-token:" + token + "@" + rest
	}
	return cloneURL
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// LocalRepoRoot returns the root of the git repository containing dir, or "" if there is none.
func LocalRepoRoot(ctx context.Context, dir string) string {
	root, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return root
}

// TreeSHAOf returns commit^{tree} for a commit already present in repo.
func TreeSHAOf(ctx context.Context, repo, sha string) (string, error) {
	return git(ctx, repo, "rev-parse", sha+"^{tree}")
}

// CheckDivergence enforces DESIGN.md 4.1 from a Divergence.
//
// Only UpToDate means the head already contains its base, and therefore that the head's tree is
// the tree every merge strategy would produce. Anything else and a plan of the head would show a
// clean diff for a change that may conflict with what already landed.
func CheckDivergence(d Divergence) error {
	if d.UpToDate {
		return nil
	}
	detail := fmt.Sprintf("%d commit(s) on the base branch are missing from the head", d.BehindBy)
	if d.BehindBy == 0 {
		detail = "the head does not contain its base"
	}
	if d.Conflicted {
		detail += ", and the branches do not merge cleanly"
	}
	return fmt.Errorf("%w (%s)", ErrBranchBehindBase, detail)
}

// Cleanup removes the working tree. Best-effort: the instance is ephemeral and the tree is on
// tmpfs, but plan files and checkouts are large and an instance may serve several runs — and
// locally the tree is on a real disk with a `git worktree` registration to unwind.
func (c *Checkout) Cleanup() error {
	if c == nil {
		return nil
	}
	if c.worktreeOf != "" {
		if _, err := git(context.Background(), c.worktreeOf, "worktree", "remove", "--force", c.Root); err == nil {
			return nil
		}
		// Fall through to a plain delete, then prune the stale registration.
		defer func() { _, _ = git(context.Background(), c.worktreeOf, "worktree", "prune") }()
	}
	return os.RemoveAll(c.Root)
}
