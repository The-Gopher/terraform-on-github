package apply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/the-gopher/terraform-on-github/internal/plan"
	"github.com/the-gopher/terraform-on-github/internal/store"
)

// fixture is a fully-verified baseline: a staged artifact, a GitHub client
// saying the PR merged as a merge-commit onto the planned base, and a config
// that still contains the workspace. Each test breaks exactly one property.
type fixture struct {
	artifacts   *store.MapBucket
	github      *fakeGitHub
	verifierSig store.FakeSigner
	contents    *fakeContents
	pr          int
	baseSHA     string
	headSHA     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		artifacts:   store.NewMapBucket(),
		github:      newFakeGitHub(),
		verifierSig: store.FakeSigner{},
		contents:    newFakeContents(),
		pr:          412,
		baseSHA:     "base1111",
		headSHA:     "head2222",
	}
	if err := f.stage(t); err != nil {
		t.Fatalf("staging fixture: %v", err)
	}
	return f
}

// stage runs store.Stage against the fixture's buckets, producing a valid
// signed artifact set.
func (f *fixture) stage(t *testing.T) error {
	t.Helper()
	planFile := filepath.Join(t.TempDir(), "tfplan")
	if err := os.WriteFile(planFile, []byte("tfplan-bytes"), 0644); err != nil {
		return err
	}
	res := plan.WorkspaceResult{
		Workspace:  "prod",
		HasChanges: true,
		ExitCode:   2,
		PlanFile:   planFile,
		PlanRaw:    "human diff",
		PlanJSON:   &tfjson.Plan{},
	}
	_, err := store.Stage(context.Background(), f.artifacts, store.NewMapBucket(), f.verifierSig, res, store.StageInput{
		Owner:            "acme",
		Repo:             "infra",
		PR:               f.pr,
		Workspace:        "prod",
		BaseSHA:          f.baseSHA,
		HeadSHA:          f.headSHA,
		PlannedTreeSHA:   "tree3333",
		TerraformVersion: "1.9.8",
		Now:              time.Date(2026, 8, 19, 14, 2, 11, 0, time.UTC),
	})
	return err
}

// fakeGitHub returns canned §6.1 state.
type fakeGitHub struct {
	merged      bool
	mergeCommit string
	parents     []string
	mergeTree   string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{merged: true, mergeCommit: "merge4444", parents: []string{"base1111", "head2222"}, mergeTree: "tree3333"}
}

func (f *fakeGitHub) GetApplyPullRequest(_ context.Context, _, _ string, _ int) (PullRequest, error) {
	return PullRequest{Merged: f.merged, MergeCommitSHA: f.mergeCommit, BaseSHA: "base1111", HeadSHA: "head2222"}, nil
}

func (f *fakeGitHub) GetCommitTree(_ context.Context, _, _, _ string) (string, error) {
	return f.mergeTree, nil
}

func (f *fakeGitHub) CommitParents(_ context.Context, _, _, _ string) ([]string, error) {
	return f.parents, nil
}

// fakeContents serves the trusted-ref config: one workspace named prod.
type fakeContents struct{ missingWorkspace bool }

func newFakeContents() *fakeContents { return &fakeContents{} }

const fixtureConfig = `version: 1
defaults:
  terraform_version: "1.9.8"
workspaces:
  - name: prod
    branch: main
    dir: envs/prod
    backend: {bucket: b, prefix: p}
    impersonate:
      plan: tf-prod-plan@example.iam.gserviceaccount.com
      apply: tf-prod-apply@example.iam.gserviceaccount.com
`

const fixtureConfigOther = `version: 1
defaults:
  terraform_version: "1.9.8"
workspaces:
  - name: other
    branch: main
    dir: envs/other
    backend: {bucket: b, prefix: p}
    impersonate:
      plan: tf-other-plan@example.iam.gserviceaccount.com
      apply: tf-other-apply@example.iam.gserviceaccount.com
`

func (f *fakeContents) ReadFileAtSHA(_ context.Context, _, _, _, _ string) ([]byte, error) {
	if f.missingWorkspace {
		return []byte(fixtureConfigOther), nil
	}
	return []byte(fixtureConfig), nil
}

func (f *fakeContents) ResolveRef(_ context.Context, _, _, _ string) (string, error) {
	return "cfgsha", nil
}

func (f *fixture) verifier() *Verifier {
	return &Verifier{
		GitHub:    f.github,
		Artifacts: f.artifacts,
		KMS:       f.verifierSig,
		Trusted:   f.contents,
	}
}

// TestVerify_ApplicableOnMergeCommit is the happy path with a true merge
// commit (two parents, tree == head's tree).
func TestVerify_ApplicableOnMergeCommit(t *testing.T) {
	f := newFixture(t)
	v, err := f.verifier().Verify(context.Background(), "acme", "infra", f.pr, "prod", f.baseSHA, f.headSHA)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !v.Applicable {
		t.Fatalf("verdict: %+v", v)
	}
	if v.Meta.PR != f.pr || v.Meta.Workspace != "prod" {
		t.Errorf("meta identity: %+v", v.Meta)
	}
}

// TestVerify_TreeEqualityAcrossMergeStrategies is the §6.2 core: squash and
// rebase produce a commit that never existed before, so the SHA check would
// fail — but the tree is byte-identical to the planned tree under every
// strategy, so the tree check passes all three.
func TestVerify_TreeEqualityAcrossMergeStrategies(t *testing.T) {
	strategies := []struct {
		name        string
		mergeCommit string // new SHA under squash/rebase; same under merge-commit
		mergeTree   string // always the head's tree per §4.1
	}{
		{"merge-commit", "merge4444", "tree3333"},
		{"squash", "squash5555", "tree3333"},
		{"rebase", "rebase6666", "tree3333"},
	}
	for _, s := range strategies {
		t.Run(s.name, func(t *testing.T) {
			f := newFixture(t)
			f.github.mergeCommit = s.mergeCommit
			f.github.mergeTree = s.mergeTree

			v, err := f.verifier().Verify(context.Background(), "acme", "infra", f.pr, "prod", f.baseSHA, f.headSHA)
			if err != nil {
				t.Fatalf("%s: Verify: %v", s.name, err)
			}
			if !v.Applicable {
				t.Errorf("%s: verdict %q", s.name, v.Reason)
			}
		})
	}
}

// TestVerify_TreeMismatchRejectsMovedBase is §6.2's backstop: a base that
// moved between plan and merge changes the merged tree, and the check fails —
// correctly, without needing a git-ancestry argument.
func TestVerify_TreeMismatchRejectsMovedBase(t *testing.T) {
	f := newFixture(t)
	f.github.mergeTree = "tree-after-others-merged"

	v, err := f.verifier().Verify(context.Background(), "acme", "infra", f.pr, "prod", f.baseSHA, f.headSHA)
	if !errors.Is(err, ErrTreeMismatch) {
		t.Fatalf("err = %v, want ErrTreeMismatch", err)
	}
	if v.Applicable {
		t.Error("moved base must not be applicable")
	}
	if v.Reason == "" {
		t.Error("verdict should carry the §6.2 explanation")
	}
}

// TestVerify_RejectsUnsignedMeta is §5.3: a forged meta.json fails signature
// verification before any GitHub call.
func TestVerify_RejectsUnsignedMeta(t *testing.T) {
	f := newFixture(t)
	metaKey := store.Key("acme", "infra", "prod", f.baseSHA, f.headSHA, store.NameMeta)
	raw, _, err := f.artifacts.Read(context.Background(), metaKey)
	if err != nil {
		t.Fatal(err)
	}
	var meta store.Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	meta.Signature = "bm90LXRoZS1zaWduYXR1cmU="
	tampered, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.artifacts.Create(context.Background(), metaKey, tampered, 1); err != nil {
		t.Fatal(err)
	}

	v, err := f.verifier().Verify(context.Background(), "acme", "infra", f.pr, "prod", f.baseSHA, f.headSHA)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
	if v.Applicable {
		t.Error("forged meta must not be applicable")
	}
}

// TestVerify_RejectsDriftKind is §6.6: a drift artifact is produced by the
// same worker with the same key, and its planned_tree_sha would match a
// fast-forwardable merge — kind is the only guard, it is inside the
// signature, and it is a whitelist.
func TestVerify_RejectsDriftKind(t *testing.T) {
	f := newFixture(t)
	metaKey := store.Key("acme", "infra", "prod", f.baseSHA, f.headSHA, store.NameMeta)
	raw, gen, err := f.artifacts.Read(context.Background(), metaKey)
	if err != nil {
		t.Fatal(err)
	}
	var meta store.Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	meta.Kind = "drift"
	// Re-sign with the tampered kind so the signature itself is valid — the
	// whitelist check is what must reject it, not the signature.
	digest, err := meta.Digest()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := f.verifierSig.Sign(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	meta.Signature = sig
	signed, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.artifacts.Create(context.Background(), metaKey, signed, gen); err != nil {
		t.Fatal(err)
	}

	v, err := f.verifier().Verify(context.Background(), "acme", "infra", f.pr, "prod", f.baseSHA, f.headSHA)
	if !errors.Is(err, ErrInapplicableKind) {
		t.Fatalf("err = %v, want ErrInapplicableKind", err)
	}
	if v.Applicable {
		t.Error("drift plan must never be applicable (§6.6)")
	}
}

// TestVerify_RejectsPlanByteSwap proves the signature binds the plan bytes:
// swapping the tfplan on the plan bucket fails the digest check even with a
// valid meta signature.
func TestVerify_RejectsPlanByteSwap(t *testing.T) {
	f := newFixture(t)
	planKey := store.Key("acme", "infra", "prod", f.baseSHA, f.headSHA, store.NamePlan)
	if err := f.artifacts.Create(context.Background(), planKey, []byte("attacker-plan"), 1); err != nil {
		t.Fatal(err)
	}

	_, err := f.verifier().Verify(context.Background(), "acme", "infra", f.pr, "prod", f.baseSHA, f.headSHA)
	if !errors.Is(err, ErrPlanSHA) {
		t.Fatalf("err = %v, want ErrPlanSHA", err)
	}
}

// TestVerify_NotMerged is the §6.1 precondition.
func TestVerify_NotMerged(t *testing.T) {
	f := newFixture(t)
	f.github.merged = false

	_, err := f.verifier().Verify(context.Background(), "acme", "infra", f.pr, "prod", f.baseSHA, f.headSHA)
	if !errors.Is(err, ErrNotMerged) {
		t.Fatalf("err = %v, want ErrNotMerged", err)
	}
}

// TestVerify_BaseMovedFirstParent is §6.1's parent assertion: the merge landed
// on a different base than the plan was built against.
func TestVerify_BaseMovedFirstParent(t *testing.T) {
	f := newFixture(t)
	f.github.parents = []string{"newbase9999", f.headSHA}

	_, err := f.verifier().Verify(context.Background(), "acme", "infra", f.pr, "prod", f.baseSHA, f.headSHA)
	if !errors.Is(err, ErrBaseMoved) {
		t.Fatalf("err = %v, want ErrBaseMoved", err)
	}
}

// TestVerify_WorkspaceDeauthorizedAfterPlan is §3.1 fail-closed: a workspace
// removed from the trusted ref between plan and merge must not apply.
func TestVerify_WorkspaceDeauthorizedAfterPlan(t *testing.T) {
	f := newFixture(t)
	f.contents.missingWorkspace = true

	v, err := f.verifier().Verify(context.Background(), "acme", "infra", f.pr, "prod", f.baseSHA, f.headSHA)
	if err == nil {
		t.Fatal("deauthorized workspace must not verify")
	}
	if v.Applicable {
		t.Error("deauthorized workspace must not be applicable")
	}
}
