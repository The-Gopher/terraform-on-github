// Command tfog-apply applies a merged PR's reviewed plan and posts the outcome to the PR.
//
//	tfog-apply 412
//	tfog-apply 412 --workspace prod-networking --dry-run
//
// The local, single-machine form of the apply service in docs/DESIGN.md. Its whole job is to
// re-derive, from GitHub and from the stored plan, the authorization to apply — and then to apply
// *that exact plan file*, never a fresh one.
//
// Nothing about the invocation is trusted. --workspace selects among already-authorized
// workspaces; it cannot create an authorization. The checks run in this order for a reason: each
// is cheap relative to the next, and each closes one specific way an unreviewed change could
// reach real infrastructure.
//
//  1. The PR is merged and has a merge commit.                            DESIGN 6.1
//  2. Config from the trusted ref at its CURRENT tip binds this workspace
//     to the PR's base branch and has apply enabled.                      DESIGN 3.1.1, 6.1
//  3. A complete stored plan exists for (workspace, *, head_sha), its meta
//     digest verifies, and the tfplan bytes hash to PlanSHA256.           DESIGN 5.2, 5.3
//  4. tree(merge_commit) == Meta.PlannedTreeSHA — the applied content is
//     the reviewed content, under squash, rebase or merge commit.         DESIGN 6.2
//  5. The tree on disk matches too, then terraform's version and the
//     provider lock match the plan's.
//  6. A human confirms.                                       DESIGN 6.4 (local stand-in)
//
// What is gone: the IAM boundary between planner and applier, the KMS signature, the GitHub
// Environment approval gate (a typed confirmation instead), the workspace lease (Terraform's own
// state lock is the real backstop), and /reconcile. See docs/LOCAL.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sampleserve/terraform-on-github/internal/apply"
	"github.com/sampleserve/terraform-on-github/internal/cli"
	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghcli"
	"github.com/sampleserve/terraform-on-github/internal/plan"
	"github.com/sampleserve/terraform-on-github/internal/prcomment"
	"github.com/sampleserve/terraform-on-github/internal/scope"
	"github.com/sampleserve/terraform-on-github/internal/store"
	"github.com/sampleserve/terraform-on-github/internal/tf"
)

const (
	exitOK      = 0
	exitError   = 1
	exitNothing = 5
)

type options struct {
	pr            int
	repo          string
	trustedRef    string
	workspaces    []string
	storeRoot     string
	noComment     bool
	yes           bool
	dryRun        bool
	force         bool
	noImpersonate bool
}

func main() {
	os.Exit(run())
}

func run() int {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitError
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := applyPR(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	return code
}

func parseArgs(argv []string) (options, error) {
	var o options
	var workspaces cli.StringList

	leading, argv := cli.TakeLeadingArg(argv)

	fs := flag.NewFlagSet("tfog-apply", flag.ContinueOnError)
	fs.StringVar(&o.repo, "repo", "", "owner/repo (default: the repo in the current directory)")
	fs.StringVar(&o.trustedRef, "trusted-ref", os.Getenv("TFOG_TRUSTED_REF"),
		"ref to read "+config.Filename+" from (default: the repo's default branch)")
	fs.Var(&workspaces, "workspace", "limit to this workspace (repeatable)")
	fs.StringVar(&o.storeRoot, "store", "", "plan store root (default: $TFOG_STORE, else XDG data dir)")
	fs.BoolVar(&o.noComment, "no-comment", false, "do not post the outcome to the PR")
	fs.BoolVar(&o.yes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&o.dryRun, "dry-run", false, "verify and init, then stop before applying")
	fs.BoolVar(&o.force, "force", false, "re-apply a plan already recorded as applied, or retry a failed one")
	fs.BoolVar(&o.noImpersonate, "no-impersonate", false, "ignore impersonate.apply; use ambient credentials")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: tfog-apply <pr> [flags]\n\nApply a merged PR's reviewed plan.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return o, err
	}
	n, err := cli.PRNumber(leading, fs.Args())
	if err != nil {
		fs.Usage()
		return o, err
	}
	o.pr, o.workspaces = n, workspaces
	return o, nil
}

func applyPR(ctx context.Context, o options) (int, error) {
	gh := &ghcli.Client{}
	if err := gh.Preflight(ctx); err != nil {
		return exitError, err
	}
	repo, err := cli.ResolveRepo(ctx, gh, o.repo)
	if err != nil {
		return exitError, err
	}
	st := &store.Local{Root: cli.StoreRoot(o.storeRoot)}

	// -- 1. the PR is merged ---------------------------------------------
	pr, err := gh.PullRequest(ctx, repo, o.pr)
	if err != nil {
		return exitError, err
	}
	if !pr.Merged {
		state := "closed without merging"
		if pr.State == "open" {
			state = "open"
		}
		return exitError, fmt.Errorf("%w: PR #%d is %s; there is nothing to apply", apply.ErrNotMerged, o.pr, state)
	}
	if pr.MergeCommitSHA == "" {
		return exitError, fmt.Errorf("PR #%d is merged but reports no merge commit", o.pr)
	}
	cli.Logf("PR #%d: merged into %s as %s · head %s", pr.Number, pr.Base.Ref,
		cli.Short(pr.MergeCommitSHA), cli.Short(pr.Head.SHA))

	// -- 2. config from the trusted ref, at its CURRENT tip --------------
	//
	// Current tip, not Meta.ConfigRefSHA. Config is fail-closed on change: a workspace
	// deauthorized, or repointed at a narrower identity, between plan and merge must not apply.
	// Meta.ConfigRefSHA is kept for the audit trail and is never used to authorize.
	// Resolve the ref's actual name when it was left to the default, so the comment and
	// Meta.ConfigRef name a branch rather than "the default branch". Which ref authorized a run is
	// the question you most want answered afterwards; "the default branch" does not answer it,
	// because the default branch can be changed.
	trustedRef := o.trustedRef
	if trustedRef == "" {
		if b, err := gh.DefaultBranch(ctx, repo); err == nil {
			trustedRef = b
		}
	}

	loader := &config.Loader{Contents: gh, Trusted: config.TrustedRefs{repo.String(): o.trustedRef}}
	cfg, configSHA, err := loader.LoadFromTrustedRef(ctx, repo.Owner, repo.Name)
	if err != nil {
		return exitError, err
	}
	cli.Logf("config: %s @ %s (%s)", config.Filename, cli.RefLabel(trustedRef), cli.Short(configSHA))

	// Branch binding decides which workspaces are authorized at all.
	sc, err := scope.MatchAllCandidates(cfg, pr.Base.Ref, nil)
	if err != nil {
		return exitError, err
	}
	bound := sc.Candidates
	if len(o.workspaces) > 0 {
		bound, err = restrict(bound, o.workspaces, pr.Base.Ref, trustedRef)
		if err != nil {
			return exitError, err
		}
	}

	// -- 3. which of them have a reviewed plan for this head? ------------
	var work []job
	for _, ws := range bound {
		keys, err := st.Find(repo.Owner, repo.Name, ws.Name, pr.Head.SHA)
		if err != nil {
			return exitError, err
		}
		switch len(keys) {
		case 0:
			continue
		case 1:
			work = append(work, job{ws, keys[0]})
		default:
			var bases []string
			for _, k := range keys {
				bases = append(bases, cli.Trim(k.BaseSHA, 8))
			}
			return exitError, fmt.Errorf("%s: %d stored plans for head %s (%s); the store is ambiguous, "+
				"remove the ones that were not reviewed", ws.Name, len(keys), cli.Short(pr.Head.SHA),
				strings.Join(bases, ", "))
		}
	}

	if len(work) == 0 {
		names := cli.WorkspaceNames(bound)
		if names == "" {
			names = "none"
		}
		cli.Logf("no stored plan for head %s in any workspace bound to %q (%s).", cli.Short(pr.Head.SHA), pr.Base.Ref, names)
		cli.Logf("A merged PR with no plan is never applied and never silently skipped — this is the")
		cli.Logf("message. Run tfog-plan against the pre-merge head, or apply by hand and say so.")
		return exitNothing, nil
	}
	cli.Logf("to apply: %s", cli.WorkspaceNames(workspacesOf(work)))

	_, mergeTreeSHA, err := gh.Commit(ctx, repo, pr.MergeCommitSHA)
	if err != nil {
		return exitError, err
	}
	sourceRepo := tf.LocalRepoRoot(ctx, ".")

	var applied, skipped int
	type failure struct {
		workspace string
		err       error
	}
	var failures []failure

	for _, j := range work {
		did, err := applyWorkspace(ctx, applyArgs{
			opts: o, gh: gh, st: st, ws: j.ws, key: j.key, repo: repo, pr: pr,
			mergeTreeSHA: mergeTreeSHA, sourceRepo: sourceRepo,
		})
		switch {
		case err != nil:
			failures = append(failures, failure{j.ws.Name, err})
			cli.Logf("\n%s: %v", j.ws.Name, err)
		case did:
			applied++
		default:
			skipped++
		}
	}

	cli.Logf("")
	cli.Logf("of %d workspace(s): applied %d, skipped %d, failed %d", len(work), applied, skipped, len(failures))
	for _, f := range failures {
		cli.Logf("  %s: %s", f.workspace, cli.FirstLine(f.err.Error()))
	}
	if len(failures) > 0 {
		return exitError, nil
	}
	return exitOK, nil
}

// job pairs an authorized workspace with the stored plan that will be applied to it.
type job struct {
	ws  config.Workspace
	key store.PlanKey
}

type applyArgs struct {
	opts         options
	gh           *ghcli.Client
	st           *store.Local
	ws           config.Workspace
	key          store.PlanKey
	repo         ghcli.Repo
	pr           ghcli.PullRequest
	mergeTreeSHA string
	sourceRepo   string
}

func applyWorkspace(ctx context.Context, a applyArgs) (bool, error) {
	cli.Logf("\n=== %s · %s · key %s", a.ws.Name, a.ws.Dir, a.key.Prefix())

	if !a.ws.ApplyEnabled() {
		return false, fmt.Errorf("%w: plan-only workspace", apply.ErrApplyDisabled)
	}

	// require_protected_base asserts what DESIGN 3.1's original justification only assumed: that
	// the base branch really does require review. An unanswerable question here is a refusal, not
	// a pass — a token that cannot read protection has not confirmed the premise.
	if a.ws.Apply.RequireProtectedBase {
		protected, err := a.gh.BranchRequiresReviews(ctx, a.repo, a.ws.Branch)
		if err != nil {
			return false, err
		}
		switch {
		case protected == nil:
			return false, fmt.Errorf("require_protected_base is set but branch protection on %q could not "+
				"be read (the token likely lacks admin read). Refusing to apply", a.ws.Branch)
		case !*protected:
			return false, fmt.Errorf("require_protected_base is set but %q does not require pull request reviews", a.ws.Branch)
		}
		cli.Logf("base branch %q: required reviews confirmed", a.ws.Branch)
	}

	prior := a.st.ReadRun(a.key)
	switch {
	case prior.State == store.StateApplied && !a.opts.force:
		cli.Logf("already applied at %s; nothing to do (pass --force to re-apply)", prior.UpdatedAt.Format("2006-01-02 15:04:05Z"))
		return false, nil
	case prior.State == store.StateApplyFailed && !a.opts.force:
		// Terminal, and never auto-retried. A failed apply may have partially mutated
		// infrastructure; the next plan shows the remainder, and a human reads the log before
		// deciding. Automatic retry here is how a bad afternoon becomes an outage.
		return false, fmt.Errorf("a previous apply of this plan failed at %s and may have changed "+
			"infrastructure part-way. Read %s/apply-*.log, then re-plan — or pass --force if you have "+
			"and you mean it", prior.UpdatedAt.Format("2006-01-02 15:04:05Z"), a.st.KeyDir(a.key))
	}

	// -- the stored plan -------------------------------------------------
	meta, err := a.st.ReadMeta(a.key) // verifies the meta digest, or fails
	if err != nil {
		return false, err
	}
	if err := checkMetaMatchesKey(meta, a.key, a.pr.Number, a.ws); err != nil {
		return false, err
	}

	planSHA, err := a.st.SHA256(a.key, store.ArtifactPlan)
	if err != nil {
		return false, err
	}
	if planSHA != meta.PlanSHA256 {
		return false, fmt.Errorf("%s hashes to %s, meta records %s — the plan artifact changed after it "+
			"was stored. Never work around this by re-planning",
			a.st.Path(a.key, store.ArtifactPlan), cli.Short(planSHA), cli.Short(meta.PlanSHA256))
	}
	cli.Logf("plan artifact verified: %s", cli.Short(planSHA))

	if err := crossCheckComment(ctx, a, meta); err != nil {
		return false, err
	}

	if !meta.HasChanges {
		cli.Logf("the reviewed plan proposes no changes; nothing to apply")
		return false, a.st.WriteRun(a.key, store.LocalRun{
			State: store.StateApplied, PR: a.pr.Number,
			MergeCommitSHA: a.pr.MergeCommitSHA, Note: "no changes to apply",
		})
	}

	// -- 4. the tree test ------------------------------------------------
	//
	// The one check that subsumes several others. Equal trees mean the content about to be applied
	// is byte-identical to the content that was planned and reviewed — which also proves the base
	// did not move, since a moved base yields a different tree. And it holds under squash and
	// rebase, where the merge commit SHA cannot match by construction.
	if a.mergeTreeSHA != meta.PlannedTreeSHA {
		return false, fmt.Errorf("%w: merge commit %s has tree %s, the plan was built on tree %s. The "+
			"merged content is not the reviewed content — most likely the base branch moved after the "+
			"plan. Re-plan", apply.ErrTreeMismatch, cli.Short(a.pr.MergeCommitSHA),
			cli.Short(a.mergeTreeSHA), cli.Short(meta.PlannedTreeSHA))
	}
	cli.Logf("tree %s matches the planned tree", cli.Short(a.mergeTreeSHA))

	binary := os.Getenv("TFOG_TERRAFORM_BIN")
	localVersion, err := tf.Version(ctx, binary)
	if err != nil {
		return false, err
	}
	if localVersion != meta.TerraformVersion {
		return false, fmt.Errorf("this plan was built with terraform %s, the local binary is %s. A saved "+
			"plan is version-specific; point $TFOG_TERRAFORM_BIN at the right one", meta.TerraformVersion, localVersion)
	}

	impersonate := a.ws.Impersonate.Apply
	if a.opts.noImpersonate {
		impersonate = ""
	}
	cli.Logf("identity: %s", cli.IdentityLabel(impersonate))

	root, err := os.MkdirTemp("", "tfog-apply-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(root)

	co, err := tf.CheckoutFrom(ctx, filepath.Join(root, "src"), a.sourceRepo, a.repo.CloneURL(), "",
		a.pr.MergeCommitSHA, tf.FetchRefs{a.pr.Base.Ref})
	if err != nil {
		return false, err
	}
	defer co.Cleanup()

	// 5. The API comparison above checked what GitHub says; this checks what actually landed on
	// disk, which is the thing Terraform is about to read.
	if co.TreeSHA != meta.PlannedTreeSHA {
		return false, fmt.Errorf("%w: checked-out tree is %s, expected %s",
			apply.ErrTreeMismatch, cli.Short(co.TreeSHA), cli.Short(meta.PlannedTreeSHA))
	}

	workdir, err := tf.RootModuleDir(co.Root, a.ws.Dir)
	if err != nil {
		return false, err
	}
	if fi, err := os.Stat(workdir); err != nil || !fi.IsDir() {
		return false, fmt.Errorf("%s does not exist at %s", a.ws.Dir, cli.Short(a.pr.MergeCommitSHA))
	}

	runner := &tf.Runner{Binary: binary, WorkDir: workdir, ImpersonateSA: impersonate, Log: os.Stderr}
	if err := runner.Init(ctx, tf.InitOpts{Backend: a.ws.Backend}); err != nil {
		return false, err
	}
	if a.ws.TerraformWorkspace != "" {
		if err := runner.SelectWorkspace(ctx, a.ws.TerraformWorkspace); err != nil {
			return false, err
		}
	}
	lockDigest, err := tf.ProviderLockDigest(workdir)
	if err != nil {
		return false, err
	}
	if lockDigest != meta.ProviderLockSHA256 {
		return false, fmt.Errorf("provider lock digest is %s, the plan was built against %s. Different "+
			"provider code applying a plan built against other provider code is not the reviewed change",
			orNone(lockDigest), orNone(meta.ProviderLockSHA256))
	}
	cli.Logf("provider lock verified: %s", orNone(lockDigest))

	// -- 6. a human confirms ---------------------------------------------
	ok, err := confirm(a, meta)
	if err != nil {
		return false, err
	}
	if !ok {
		cli.Logf("not confirmed; nothing applied")
		return false, nil
	}
	if a.opts.dryRun {
		cli.Logf("--dry-run: verified and initialised; stopping before apply")
		return false, nil
	}

	staged := filepath.Join(workdir, "tfog.tfplan")
	planBytes, err := a.st.ReadArtifact(a.key, store.ArtifactPlan)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(staged, planBytes, 0o600); err != nil {
		return false, err
	}

	logPath, attempt, err := a.st.NextApplyLog(a.key)
	if err != nil {
		return false, err
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	runner.Log = io.MultiWriter(os.Stderr, logFile)

	if err := a.st.WriteRun(a.key, store.LocalRun{
		State: store.StateApplying, PR: a.pr.Number, Attempt: attempt, MergeCommitSHA: a.pr.MergeCommitSHA,
	}); err != nil {
		logFile.Close()
		return false, err
	}

	meta.MergeCommitSHA = a.pr.MergeCommitSHA
	res, applyErr := runner.Apply(ctx, staged)
	logFile.Close()

	if applyErr != nil {
		_ = a.st.WriteRun(a.key, store.LocalRun{
			State: store.StateApplyFailed, PR: a.pr.Number, Attempt: attempt,
			MergeCommitSHA: a.pr.MergeCommitSHA, Error: cli.FirstLine(applyErr.Error()),
		})
		a.postOutcome(ctx, meta, false, cli.Truncate(applyErr.Error(), 4000), logPath)
		return false, applyErr
	}

	if err := a.st.WriteRun(a.key, store.LocalRun{
		State: store.StateApplied, PR: a.pr.Number, Attempt: attempt, MergeCommitSHA: a.pr.MergeCommitSHA,
	}); err != nil {
		return false, err
	}
	cli.Logf("%s: applied · log %s", a.ws.Name, logPath)
	a.postOutcome(ctx, meta, true, applyTail(string(res.Log), 12), logPath)
	return true, nil
}

func (a applyArgs) postOutcome(ctx context.Context, meta store.Meta, ok bool, detail, logPath string) {
	if a.opts.noComment {
		return
	}
	body := prcomment.Apply(prcomment.ApplyInput{
		Workspace: a.ws, Meta: meta, Counts: meta.Counts, OK: ok,
		Detail: detail, LogPath: logPath, Marker: ghcli.Marker("apply", a.ws.Name),
	})
	c, err := a.gh.AddComment(ctx, a.repo, a.pr.Number, body)
	if err != nil {
		cli.Logf("warning: could not post the outcome comment: %v", err)
		return
	}
	cli.Logf("posted: %s", c.HTMLURL)
}

// checkMetaMatchesKey closes "point the apply at a plan built for a different workspace".
func checkMetaMatchesKey(m store.Meta, k store.PlanKey, prNumber int, ws config.Workspace) error {
	for _, f := range []struct{ name, got, want string }{
		{"owner", m.Owner, k.Owner},
		{"repo", m.Repo, k.Repo},
		{"workspace", m.Workspace, k.Workspace},
		{"base_sha", m.BaseSHA, k.BaseSHA},
		{"head_sha", m.HeadSHA, k.HeadSHA},
	} {
		if f.got != f.want {
			return fmt.Errorf("meta.%s is %q, expected %q", f.name, f.got, f.want)
		}
	}
	if m.PR != prNumber {
		return fmt.Errorf("this plan was made for PR #%d, not #%d", m.PR, prNumber)
	}
	return nil
}

// crossCheckComment compares the stored plan against what was actually published for review.
//
// The comment is an index, not an authority: the plan being applied comes from the local store, so
// a forged or edited comment can only make this refuse — never make it apply something else.
func crossCheckComment(ctx context.Context, a applyArgs, meta store.Meta) error {
	c, err := a.gh.FindComment(ctx, a.repo, a.pr.Number, ghcli.Marker("plan", a.ws.Name))
	if err != nil {
		return err
	}
	if c == nil {
		cli.Logf("warning: no plan comment on the PR for this workspace — nothing was published for review")
		return nil
	}
	published := prcomment.ParseMeta(c.Body)
	if published == nil {
		cli.Logf("warning: the plan comment carries no machine-readable meta block; skipping cross-check")
		return nil
	}
	digest, err := meta.DigestHex()
	if err != nil {
		return err
	}
	publishedDigest, err := published.Meta.DigestHex()
	if err != nil {
		return fmt.Errorf("the plan comment's meta block is not a valid record: %w", err)
	}
	for _, f := range []struct{ name, got, want string }{
		{"plan_sha256", published.PlanSHA256, meta.PlanSHA256},
		{"planned_tree_sha", published.PlannedTreeSHA, meta.PlannedTreeSHA},
		{"base_sha", published.BaseSHA, meta.BaseSHA},
		{"head_sha", published.HeadSHA, meta.HeadSHA},
		{"digest", publishedDigest, digest},
		{"recorded digest", published.Digest, digest},
	} {
		if f.got != f.want {
			return fmt.Errorf("the plan published to the PR records %s=%s, the stored plan has %s. The "+
				"plan on disk is not the plan that was reviewed", f.name, cli.Short(f.got), cli.Short(f.want))
		}
	}
	cli.Logf("cross-checked against the plan comment (%s)", c.HTMLURL)
	return nil
}

func confirm(a applyArgs, meta store.Meta) (bool, error) {
	showJSON, err := a.st.ReadArtifact(a.key, store.ArtifactPlanJSON)
	if err != nil {
		return false, err
	}
	lines, omitted, err := plan.AddressLines(showJSON, 50)
	if err != nil {
		return false, err
	}

	cli.Logf("")
	cli.Logf("  %s · %s · environment %s", a.ws.Name, a.ws.Dir, a.ws.Environment())
	cli.Logf("  %s", plan.Headline(meta.Counts))
	cli.Logf("  state: gs://%s/%s", a.ws.Backend.Bucket, a.ws.Backend.Prefix)
	cli.Logf("  identity: %s", cli.IdentityLabel(a.ws.Impersonate.Apply))
	cli.Logf("")
	for _, l := range lines {
		cli.Logf("  %s", l)
	}
	if omitted > 0 {
		cli.Logf("  … and %d more (full list in %s)", omitted, a.st.Path(a.key, store.ArtifactPlanText))
	}
	cli.Logf("")

	if a.opts.yes {
		cli.Logf("--yes: confirmation skipped")
		return true, nil
	}
	if a.opts.dryRun {
		return true, nil
	}
	return cli.ConfirmByName(a.ws.Name)
}

// restrict applies --workspace, refusing a workspace that is not bound to this base branch.
func restrict(bound []config.Workspace, wanted []string, baseRef, trustedRef string) ([]config.Workspace, error) {
	have := map[string]bool{}
	for _, w := range bound {
		have[w.Name] = true
	}
	var off []string
	for _, n := range wanted {
		if !have[n] {
			off = append(off, n)
		}
	}
	if len(off) > 0 {
		return nil, fmt.Errorf("%w: %s is not bound to base branch %q on %s; --workspace selects among "+
			"authorized workspaces, it cannot authorize one", apply.ErrWorkspaceScope,
			strings.Join(off, ", "), baseRef, cli.RefLabel(trustedRef))
	}
	want := map[string]bool{}
	for _, n := range wanted {
		want[n] = true
	}
	var out []config.Workspace
	for _, w := range bound {
		if want[w.Name] {
			out = append(out, w)
		}
	}
	return out, nil
}

func workspacesOf(work []job) []config.Workspace {
	out := make([]config.Workspace, 0, len(work))
	for _, j := range work {
		out = append(out, j.ws)
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(no lock file)"
	}
	return cli.Short(s)
}

func applyTail(out string, n int) string {
	var kept []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}
