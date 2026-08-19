package tf

import (
	"context"
	"errors"
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
	// git init; git remote add origin <url with installation token>
	// git fetch --depth=1 origin <headSHA>
	// git checkout FETCH_HEAD; TreeSHA = git rev-parse FETCH_HEAD^{tree}
	return nil, errNotImplemented
}

// CheckoutCommit fetches one commit by SHA. Used at apply time on the real merge commit.
func CheckoutCommit(ctx context.Context, root, cloneURL, token, sha string) (*Checkout, error) {
	return nil, errNotImplemented
}

// Cleanup removes the working tree. Best-effort: the instance is ephemeral and the tree is on
// tmpfs, but plan files and checkouts are large and an instance may serve several runs.
func (c *Checkout) Cleanup() error { return nil }
