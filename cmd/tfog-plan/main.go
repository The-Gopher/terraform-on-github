// Command tfog-plan plans a pull request's Terraform, stores the plan, and posts it to the PR.
//
//	tfog-plan 412
//	tfog-plan 412 --workspace prod-networking --trusted-ref main
//
// The local, single-machine form of the plan service in docs/DESIGN.md. It keeps the four
// properties that make the design worth having and drops the infrastructure around them:
//
//  1. Config comes from ONE TRUSTED REF (--trusted-ref, default the repo's default
//     branch) — never the PR head, and never the PR's base branch.        DESIGN 3.1
//  2. The PR head must already contain its base, or nothing is planned.   DESIGN 4.1
//  3. The plan is keyed by (base_sha, head_sha) and written once.         DESIGN 5, 5.2
//  4. The tree that was planned is recorded, so the apply can prove it
//     applied the reviewed content.                                      DESIGN 6.2
//
// What is gone: the IAM boundary between planner and applier (one operator runs both), the KMS
// signature, the coordination bucket, the egress sandbox, and check runs — the output is a PR
// comment. See docs/LOCAL.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sampleserve/terraform-on-github/internal/cli"
	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghcli"
	"github.com/sampleserve/terraform-on-github/internal/plan"
	"github.com/sampleserve/terraform-on-github/internal/prcomment"
	"github.com/sampleserve/terraform-on-github/internal/scope"
	"github.com/sampleserve/terraform-on-github/internal/store"
	"github.com/sampleserve/terraform-on-github/internal/tf"
)

// Exit codes. 0 covers both "changes" and "no changes": DESIGN 4.3 makes both a success, because
// a failing check on every substantive PR trains people to ignore red.
const (
	exitOK             = 0
	exitError          = 1
	exitNothingInScope = 3
	exitBranchBehind   = 4
)

type options struct {
	pr                   int
	repo                 string
	trustedRef           string
	workspaces           []string
	storeRoot            string
	noComment            bool
	force                bool
	allowVersionMismatch bool
	noImpersonate        bool
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

	code, err := planPR(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return code
	}
	return code
}

func parseArgs(argv []string) (options, error) {
	var o options
	var workspaces cli.StringList

	leading, argv := cli.TakeLeadingArg(argv)

	fs := flag.NewFlagSet("tfog-plan", flag.ContinueOnError)
	fs.StringVar(&o.repo, "repo", "", "owner/repo (default: the repo in the current directory)")
	fs.StringVar(&o.trustedRef, "trusted-ref", os.Getenv("TFOG_TRUSTED_REF"),
		"ref to read "+config.Filename+" from (default: the repo's default branch). Never the PR head.")
	fs.Var(&workspaces, "workspace", "limit to this workspace (repeatable)")
	fs.StringVar(&o.storeRoot, "store", "", "plan store root (default: $TFOG_STORE, else XDG data dir)")
	fs.BoolVar(&o.noComment, "no-comment", false, "print the summary; do not post to the PR")
	fs.BoolVar(&o.force, "force", false, "discard and replace an existing plan for this exact (base, head) pair")
	fs.BoolVar(&o.allowVersionMismatch, "allow-version-mismatch", false,
		"plan with the local terraform even if it is not the version the config pins")
	fs.BoolVar(&o.noImpersonate, "no-impersonate", false, "ignore impersonate.plan; use ambient credentials")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: tfog-plan <pr> [flags]\n\nPlan a PR's Terraform and post it to the PR.\n\n")
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

func planPR(ctx context.Context, o options) (int, error) {
	gh := &ghcli.Client{}
	if err := gh.Preflight(ctx); err != nil {
		return exitError, err
	}

	repo, err := cli.ResolveRepo(ctx, gh, o.repo)
	if err != nil {
		return exitError, err
	}
	st := &store.Local{Root: cli.StoreRoot(o.storeRoot)}

	pr, err := gh.PullRequest(ctx, repo, o.pr)
	if err != nil {
		return exitError, err
	}
	cli.Logf("PR #%d: %s → %s  head %s", pr.Number, pr.Head.Label, pr.Base.Ref, cli.Short(pr.Head.SHA))

	// -- 1. config, from the trusted ref ---------------------------------
	//
	// Through config.Loader, whose only load path derives the ref from its own map. Going through
	// it rather than reading the file directly means the local command cannot accidentally do what
	// DESIGN 3.1.1 forbids — there is no argument here that could be the PR's base.
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

	loader := &config.Loader{
		Contents: gh,
		Trusted:  config.TrustedRefs{repo.String(): o.trustedRef},
	}
	cfg, configSHA, err := loader.LoadFromTrustedRef(ctx, repo.Owner, repo.Name)
	if err != nil {
		if errors.Is(err, config.ErrNoConfig) {
			return exitError, fmt.Errorf("%s has no %s on %s; nothing to plan", repo, config.Filename, cli.RefLabel(trustedRef))
		}
		return exitError, err
	}
	cli.Logf("config: %s @ %s (%s), %d workspace(s)", config.Filename, cli.RefLabel(trustedRef), cli.Short(configSHA), len(cfg.Workspaces))

	// -- 2. is the head already the merge result? ------------------------
	//
	// Before anything expensive and before any credential is used: if the head does not contain
	// its base, every plan below would describe a tree that will never exist.
	cmp, err := gh.Compare(ctx, repo, pr.Base.Ref, pr.Head.SHA)
	if err != nil {
		return exitError, err
	}
	div := tf.Divergence{
		UpToDate:   cmp.Status == "ahead" || cmp.Status == "identical",
		BehindBy:   cmp.BehindBy,
		Conflicted: cmp.Status == "diverged",
	}
	if err := tf.CheckDivergence(div); err != nil {
		cli.Logf("blocked: %v", err)
		// Phrased for the comment rather than reusing the error string, which carries a package
		// prefix meant for logs.
		behind := "does not contain its base"
		if div.BehindBy > 0 {
			behind = fmt.Sprintf("is missing %d commit(s) from `%s`", div.BehindBy, pr.Base.Ref)
		}
		conflict := ""
		if div.Conflicted {
			conflict = " The branches also do not merge cleanly."
		}
		detail := fmt.Sprintf("This branch %s.%s\n\nUpdate the branch and push. A plan of a head that is "+
			"missing commits from `%s` describes a tree that will never exist, so there is nothing safe "+
			"to review — and this is not retryable without a push (docs/DESIGN.md 4.1).",
			behind, conflict, pr.Base.Ref)
		if !o.noComment {
			mark := ghcli.Marker("plan", "_blocked")
			if _, err := gh.UpsertComment(ctx, repo, o.pr, mark,
				prcomment.Notice(mark, "Terraform plan skipped", detail)); err != nil {
				cli.Logf("warning: could not post the blocking comment: %v", err)
			}
		}
		return exitBranchBehind, nil
	}
	baseSHA := cmp.MergeBaseSHA

	// -- 3. which workspaces does this diff touch? ----------------------
	if cmp.FilesTruncated {
		cli.Logf("warning: GitHub returned the maximum %d changed files; scoping every candidate workspace", 300)
	}
	matcher := scope.Match
	if cmp.FilesTruncated {
		matcher = scope.MatchAllCandidates
	}
	sc, err := matcher(cfg, pr.Base.Ref, cmp.ChangedPaths)
	if err != nil {
		return exitError, err
	}

	selected, err := selectWorkspaces(cfg, sc, o.workspaces, pr.Base.Ref)
	if err != nil {
		return exitError, err
	}

	if sc.ConfigChanged && !o.noComment {
		mark := ghcli.Marker("plan", "_config")
		detail := fmt.Sprintf("The config used for these plans was read from **`%s`** (`%s`), not from this "+
			"branch. Your edit changes nothing about how this PR is planned; it takes effect once it reaches "+
			"the trusted ref (docs/DESIGN.md 3.1).", cli.RefLabel(trustedRef), cli.Short(configSHA))
		if _, err := gh.UpsertComment(ctx, repo, o.pr, mark,
			prcomment.Notice(mark, "This PR edits `"+config.Filename+"`", detail)); err != nil {
			cli.Logf("warning: could not post the config advisory: %v", err)
		}
	}

	if len(selected) == 0 {
		names := cli.WorkspaceNames(sc.Candidates)
		if names == "" {
			names = "none"
		}
		cli.Logf("nothing in scope. Workspaces bound to %q: %s", pr.Base.Ref, names)
		cli.Logf("changed paths (%d): %s", len(cmp.ChangedPaths), cli.Preview(cmp.ChangedPaths, 10))
		return exitNothingInScope, nil
	}
	cli.Logf("in scope: %s", cli.WorkspaceNames(selected))

	// -- 4. plan each workspace ------------------------------------------
	sourceRepo := tf.LocalRepoRoot(ctx, ".")
	viewer, err := gh.ViewerLogin(ctx)
	if err != nil {
		return exitError, err
	}

	type failure struct {
		workspace string
		err       error
	}
	var failures []failure

	for _, ws := range selected {
		key := store.PlanKey{
			Owner: repo.Owner, Repo: repo.Name, Workspace: ws.Name,
			BaseSHA: baseSHA, HeadSHA: pr.Head.SHA,
		}
		err := planWorkspace(ctx, planArgs{
			opts: o, gh: gh, st: st, cfg: cfg, ws: ws, key: key, repo: repo,
			pr: pr, trustedRef: trustedRef, configSHA: configSHA,
			sourceRepo: sourceRepo, viewer: viewer,
		})
		if err == nil {
			continue
		}
		failures = append(failures, failure{ws.Name, err})
		cli.Logf("\n%s: FAILED — %v", ws.Name, err)
		if !o.noComment {
			mark := ghcli.Marker("plan", ws.Name)
			body := prcomment.Notice(mark,
				fmt.Sprintf("`terraform plan` failed — **%s**", ws.Name),
				fmt.Sprintf("```\n%s\n```\n\n<sub>head `%s` · `%s`</sub>", cli.Truncate(err.Error(), 4000), cli.Short(pr.Head.SHA), ws.Dir))
			if _, err := gh.UpsertComment(ctx, repo, o.pr, mark, body); err != nil {
				cli.Logf("warning: could not post the failure comment: %v", err)
			}
		}
	}

	if len(failures) > 0 {
		cli.Logf("\n%d of %d workspace(s) failed to plan:", len(failures), len(selected))
		for _, f := range failures {
			cli.Logf("  %s: %s", f.workspace, cli.FirstLine(f.err.Error()))
		}
		return exitError, nil
	}
	return exitOK, nil
}

type planArgs struct {
	opts       options
	gh         *ghcli.Client
	st         *store.Local
	cfg        *config.Config
	ws         config.Workspace
	key        store.PlanKey
	repo       ghcli.Repo
	pr         ghcli.PullRequest
	trustedRef string
	configSHA  string
	sourceRepo string
	viewer     string
}

func planWorkspace(ctx context.Context, a planArgs) error {
	cli.Logf("\n=== %s · %s · key %s", a.ws.Name, a.ws.Dir, a.key.Prefix())

	if a.st.Complete(a.key) {
		if !a.opts.force {
			// The (base_sha, head_sha) cache from DESIGN 4.2 and the write-once property from
			// 5.2: these exact inputs already have a reviewed plan, and silently replacing its
			// bytes is precisely what the design forbids.
			cli.Logf("already planned; reusing the stored plan (pass --force to discard and re-plan)")
			meta, err := a.st.ReadMeta(a.key)
			if err != nil {
				return err
			}
			return publish(ctx, a, meta)
		}
		cli.Logf("--force: discarding the stored plan for this pair")
		if err := a.st.Purge(a.key); err != nil {
			return err
		}
	}

	binary := os.Getenv("TFOG_TERRAFORM_BIN")
	localVersion, err := tf.Version(ctx, binary)
	if err != nil {
		return err
	}
	if localVersion != a.ws.TerraformVersion {
		msg := fmt.Sprintf("local terraform is %s, config pins %s. A saved plan is version-specific, and "+
			"tfog-apply requires the same version it was planned with", localVersion, a.ws.TerraformVersion)
		if !a.opts.allowVersionMismatch {
			return fmt.Errorf("%s.\nEither install %s, point $TFOG_TERRAFORM_BIN at it, or pass "+
				"--allow-version-mismatch to plan with what you have", msg, a.ws.TerraformVersion)
		}
		cli.Logf("warning: %s (--allow-version-mismatch)", msg)
	}

	impersonate := a.ws.Impersonate.Plan
	if a.opts.noImpersonate {
		impersonate = ""
	}
	cli.Logf("identity: %s", cli.IdentityLabel(impersonate))

	root, err := os.MkdirTemp("", "tfog-plan-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	co, err := tf.CheckoutFrom(ctx, filepath.Join(root, "src"), a.sourceRepo, a.repo.CloneURL(), "",
		a.pr.Head.SHA, tf.PullRequestRefs(a.pr.Number, a.pr.Base.Ref))
	if err != nil {
		return err
	}
	defer co.Cleanup()
	cli.Logf("checkout %s · tree %s", cli.Short(co.CommitSHA), cli.Short(co.TreeSHA))

	workdir, err := tf.RootModuleDir(co.Root, a.ws.Dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(workdir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s does not exist at %s", a.ws.Dir, cli.Short(a.pr.Head.SHA))
	}

	// Before init, because init is what fetches module code and can run it.
	if os.Getenv("TFOG_ALLOW_ANY_MODULE_SOURCE") == "1" {
		cli.Logf("warning: TFOG_ALLOW_ANY_MODULE_SOURCE=1 — module sources are not being checked")
	} else {
		refs, err := tf.CollectModuleSources(co.Root, workdir)
		if err != nil {
			return err
		}
		if err := tf.CheckModuleRefs(refs, a.cfg.ModuleSources); err != nil {
			return err
		}
		cli.Logf("module sources: %d checked against the allowlist", len(refs))
	}

	runner := &tf.Runner{Binary: binary, WorkDir: workdir, ImpersonateSA: impersonate, Log: os.Stderr}
	if err := runner.Init(ctx, tf.InitOpts{Backend: a.ws.Backend}); err != nil {
		return err
	}
	if a.ws.TerraformWorkspace != "" {
		if err := runner.SelectWorkspace(ctx, a.ws.TerraformWorkspace); err != nil {
			return err
		}
	}
	lockDigest, err := tf.ProviderLockDigest(workdir)
	if err != nil {
		return err
	}

	planFile := filepath.Join(root, "tfplan")
	res, err := runner.Plan(ctx, planFile, a.ws)
	if err != nil {
		return err
	}

	counts, err := plan.Counts(res.JSON)
	if err != nil {
		return err
	}
	// -detailed-exitcode and plan.json must agree. If they do not, one of them is being misread,
	// and the counts are the number reviewers trust most.
	if res.HasChanges != counts.Any() {
		return fmt.Errorf("-detailed-exitcode says has_changes=%t but plan.json tallies %+v; refusing to "+
			"publish a summary that may misstate the change", res.HasChanges, counts)
	}

	// tfplan first, meta.json last: meta's presence is what marks the plan complete.
	if err := a.st.PutFile(a.key, store.ArtifactPlan, planFile); err != nil {
		return err
	}
	if err := a.st.Put(a.key, store.ArtifactPlanJSON, strings.NewReader(string(res.JSON))); err != nil {
		return err
	}
	if err := a.st.Put(a.key, store.ArtifactPlanText, strings.NewReader(res.Text)); err != nil {
		return err
	}
	planSHA, err := a.st.SHA256(a.key, store.ArtifactPlan)
	if err != nil {
		return err
	}

	meta, err := a.st.PutMeta(a.key, store.Meta{
		Schema:    store.SchemaVersion,
		Owner:     a.key.Owner,
		Repo:      a.key.Repo,
		PR:        a.pr.Number,
		Workspace: a.ws.Name,
		BaseSHA:   a.key.BaseSHA,
		HeadSHA:   a.key.HeadSHA,
		// No merge commit at plan time, by construction. PlannedTreeSHA is the field that
		// survives the merge (DESIGN 6.2).
		PlannedTreeSHA:     co.TreeSHA,
		ConfigRef:          cli.RefLabel(a.trustedRef),
		ConfigRefSHA:       a.configSHA,
		TerraformVersion:   localVersion,
		ProviderLockSHA256: lockDigest,
		PlanSHA256:         planSHA,
		HasChanges:         res.HasChanges,
		Counts:             counts,
		PlannedAt:          time.Now().UTC().Truncate(time.Second),
		Duration:           res.Duration,
	})
	if err != nil {
		return err
	}
	if err := a.st.WriteRun(a.key, store.LocalRun{State: store.StatePlanned, PR: a.pr.Number}); err != nil {
		return err
	}

	cli.Logf("stored: %s", a.st.KeyDir(a.key))
	if res.HasChanges {
		cli.Logf("%s: %s", a.ws.Name, plan.Headline(counts))
	} else {
		cli.Logf("%s: no changes", a.ws.Name)
	}
	return publish(ctx, a, meta)
}

// publish renders and posts the sticky plan comment.
//
// Older plans for this PR are left in the store deliberately, the way DESIGN 5.4 keeps a
// superseded run rather than deleting it — the store stays an explicable history. What must not
// survive is a *reviewable* pointer to a stale plan, and the sticky comment is that pointer:
// updating it in place is what supersedes the previous head.
func publish(ctx context.Context, a planArgs, meta store.Meta) error {
	showJSON, err := a.st.ReadArtifact(a.key, store.ArtifactPlanJSON)
	if err != nil {
		return err
	}
	showText, err := a.st.ReadArtifact(a.key, store.ArtifactPlanText)
	if err != nil {
		return err
	}
	digest, err := meta.DigestHex()
	if err != nil {
		return err
	}

	body, err := prcomment.Plan(prcomment.PlanInput{
		Workspace: a.ws,
		Meta:      meta,
		Digest:    digest,
		ShowJSON:  showJSON,
		ShowText:  string(showText),
		StorePath: a.st.KeyDir(a.key),
		Marker:    ghcli.Marker("plan", a.ws.Name),
		PlannedBy: a.viewer,
	})
	if err != nil {
		return err
	}

	if a.opts.noComment {
		cli.Logf("\n--- comment body (not posted) ---")
		fmt.Println(body)
		return nil
	}
	c, err := a.gh.UpsertComment(ctx, a.repo, a.pr.Number, ghcli.Marker("plan", a.ws.Name), body)
	if err != nil {
		return err
	}
	cli.Logf("posted: %s", c.HTMLURL)
	return nil
}

// selectWorkspaces applies --workspace on top of the scoping result.
//
// An explicit --workspace still has to be bound to this base branch. The branch binding is what
// authorizes the identity; a flag must not be able to route around it.
func selectWorkspaces(cfg *config.Config, sc scope.Result, wanted []string, baseRef string) ([]config.Workspace, error) {
	if len(wanted) == 0 {
		return sc.InScope, nil
	}
	known := map[string]bool{}
	for _, w := range cfg.Workspaces {
		known[w.Name] = true
	}
	bound := map[string]bool{}
	for _, w := range sc.Candidates {
		bound[w.Name] = true
	}

	var unknown, offBranch []string
	for _, name := range wanted {
		switch {
		case !known[name]:
			unknown = append(unknown, name)
		case !bound[name]:
			offBranch = append(offBranch, name)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("no such workspace(s): %s", strings.Join(unknown, ", "))
	}
	if len(offBranch) > 0 {
		return nil, fmt.Errorf("%s is not bound to base branch %q; --workspace selects among "+
			"authorized workspaces, it cannot authorize one", strings.Join(offBranch, ", "), baseRef)
	}

	want := map[string]bool{}
	for _, n := range wanted {
		want[n] = true
	}
	var out []config.Workspace
	for _, w := range sc.Candidates {
		if want[w.Name] {
			out = append(out, w)
		}
	}
	return out, nil
}
