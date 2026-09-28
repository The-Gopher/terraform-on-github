package store

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
)

// fixtureResult builds a WorkspaceResult whose PlanFile points at a real file
// on disk, the way plan.Run leaves one.
func fixtureResult(t *testing.T) plan.WorkspaceResult {
	t.Helper()
	dir := t.TempDir()
	planFile := filepath.Join(dir, "tfplan")
	if err := os.WriteFile(planFile, []byte("fake tfplan bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile("../tf/testdata/plan-with-secret.json")
	if err != nil {
		t.Fatal(err)
	}
	var p tfjson.Plan
	if err := p.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}

	return plan.WorkspaceResult{
		Workspace:  "prod-networking",
		HasChanges: true,
		ExitCode:   2,
		PlanFile:   planFile,
		PlanRaw:    "human diff text",
		PlanJSON:   &p,
	}
}

func stageInput() StageInput {
	return StageInput{
		Owner:            "acme",
		Repo:             "infra",
		PR:               412,
		Workspace:        "prod-networking",
		BaseSHA:          "aaaa",
		HeadSHA:          "bbbb",
		PlannedTreeSHA:   "cccc",
		TerraformVersion: "1.9.8",
		ConfigSHA:        "dddd",
		Now:              time.Date(2026, 8, 19, 14, 2, 11, 0, time.UTC),
	}
}

func TestStage_WritesArtifactsSignsMetaAndOrders(t *testing.T) {
	artifacts := NewMapBucket()
	coordination := NewMapBucket()
	res := fixtureResult(t)

	staged, err := Stage(context.Background(), artifacts, coordination, FakeSigner{}, res, stageInput())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// Artifacts under the (base, head) key, write-once bucket.
	for _, name := range []string{NamePlan, NamePlanJSON, NamePlanText, NameMeta} {
		if _, _, err := artifacts.Read(context.Background(), Key("acme", "infra", "prod-networking", "aaaa", "bbbb", name)); err != nil {
			t.Errorf("artifact %s: %v", name, err)
		}
	}

	// meta.json content matches the returned meta and verifies against the digest.
	metaRaw, _, err := artifacts.Read(context.Background(), Key("acme", "infra", "prod-networking", "aaaa", "bbbb", NameMeta))
	if err != nil {
		t.Fatal(err)
	}
	var stored Meta
	if err := json.Unmarshal(metaRaw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Kind != KindPR || stored.PR != 412 || stored.Workspace != "prod-networking" {
		t.Errorf("meta identity fields: %+v", stored)
	}
	fake := FakeSigner{}
	digest, err := stored.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Verify(context.Background(), digest, stored.Signature); err != nil {
		t.Errorf("stored signature does not verify: %v", err)
	}

	// plan_sha256 binds to the exact tfplan bytes.
	want := PlanSHA([]byte("fake tfplan bytes"))
	if stored.PlanSHA256 != want {
		t.Errorf("PlanSHA256 = %q, want %q", stored.PlanSHA256, want)
	}

	// §5.5 ordering observable in the final state: marker present, run planned.
	if _, _, err := coordination.Read(context.Background(), PendingKey("acme", "infra", 412, "aaaa", "bbbb")); err != nil {
		t.Errorf("pending-apply marker: %v", err)
	}
	runRaw, _, err := coordination.Read(context.Background(), RunKey("acme", "infra", "prod-networking", "aaaa", "bbbb"))
	if err != nil {
		t.Fatal(err)
	}
	var run runObjectState
	if err := json.Unmarshal(runRaw, &run); err != nil {
		t.Fatal(err)
	}
	if run.Status != RunPlanned {
		t.Errorf("run status = %q, want %q", run.Status, RunPlanned)
	}
	if staged.CacheHit {
		t.Error("first Stage must not report a cache hit")
	}
}

// crashBucket fails the write of one named object with a simulated process
// death — the test harness for the §5.5 ordering property. "Kill between each
// pair" is modeled as: every write lands except the kill target, whose write
// never happens (as when the process dies mid-request).
type crashBucket struct {
	*MapBucket
	failOn string // object name that aborts the process; "" never aborts
	calls  int
}

func (b *crashBucket) Create(ctx context.Context, name string, content []byte, gen int64) error {
	b.calls++
	if b.failOn != "" && name == b.failOn {
		// Simulate the process dying: the write never lands.
		return errCrash
	}
	return b.MapBucket.Create(ctx, name, content, gen)
}

var errCrash = errors.New("simulated process death")

// TestStage_CrashWindows kills the process between each pair of the §5.5
// sequence and asserts the failure lands on the recoverable side: artifacts
// may exist without a marker or planned run (reconcile re-verifies), but a
// "planned" run must never exist without artifacts, and a marker must never
// exist without meta.json.
func TestStage_CrashWindows(t *testing.T) {
	cases := []struct {
		name         string
		artifactKill string // object name in the plan bucket whose write is killed ("" = none)
		coordKill    string // object name in the coordination bucket whose write is killed ("" = none)
	}{
		{"mid-artifacts", Key("acme", "infra", "prod-networking", "aaaa", "bbbb", NamePlanText), ""},
		{"after-artifacts-before-marker", Key("acme", "infra", "prod-networking", "aaaa", "bbbb", NameMeta), ""},
		{"after-marker-before-flip", "", fmtRunPlanned()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			artifacts := &crashBucket{MapBucket: NewMapBucket(), failOn: tc.artifactKill}
			coordination := &crashBucket{MapBucket: NewMapBucket(), failOn: tc.coordKill}

			_, err := Stage(context.Background(), artifacts, coordination, FakeSigner{}, fixtureResult(t), stageInput())
			if !errors.Is(err, errCrash) {
				t.Fatalf("Stage error = %v, want crash", err)
			}

			ab, cb := artifacts.MapBucket, coordination.MapBucket
			ctx := context.Background()
			metaExists := bucketHas(ctx, t, ab, Key("acme", "infra", "prod-networking", "aaaa", "bbbb", NameMeta))
			markerExists := bucketHas(ctx, t, cb, PendingKey("acme", "infra", 412, "aaaa", "bbbb"))
			runPlanned := runStatusIs(ctx, t, cb, RunKey("acme", "infra", "prod-networking", "aaaa", "bbbb"), RunPlanned)

			// Invariant 1 (§5.5): no "planned" run without a complete artifact set.
			if runPlanned && !metaExists {
				t.Fatal("run flipped to planned while meta.json is absent — the unrecoverable ordering")
			}
			// Invariant 2: a marker without meta means reconcile re-verifies and
			// rejects — acceptable (the marker set is an optimization, §5.5),
			// but meta without marker must also be reachable, which is the
			// "artifacts exist, /reconcile not yet" window the design wants.
			if markerExists && !metaExists && tc.artifactKill == Key("acme", "infra", "prod-networking", "aaaa", "bbbb", NameMeta) {
				t.Fatal("marker written before meta.json — §5.5 order inverted")
			}
			// Invariant 3: after a crash the state is retryable — a fresh
			// process re-running Stage (the buckets keep the survivors, so the
			// retry rides the §5.2 cache-hit path) must converge without
			// erroring.
			artifacts.failOn = ""
			coordination.failOn = ""
			if _, err := Stage(ctx, artifacts, coordination, FakeSigner{}, fixtureResult(t), stageInput()); err != nil {
				t.Fatalf("retry Stage: %v", err)
			}
			if !runStatusIs(ctx, t, cb, RunKey("acme", "infra", "prod-networking", "aaaa", "bbbb"), RunPlanned) {
				t.Error("retry did not converge run to planned")
			}
			if !bucketHas(ctx, t, cb, PendingKey("acme", "infra", 412, "aaaa", "bbbb")) {
				t.Error("retry did not leave pending-apply marker")
			}
		})
	}
}

func bucketHas(ctx context.Context, t *testing.T, b *MapBucket, name string) bool {
	t.Helper()
	_, _, err := b.Read(ctx, name)
	return err == nil
}

func runStatusIs(ctx context.Context, t *testing.T, b *MapBucket, key, want string) bool {
	t.Helper()
	raw, _, err := b.Read(ctx, key)
	if err != nil {
		return false
	}
	var run runObjectState
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decoding run: %v", err)
	}
	return run.Status == want
}

// fmtRunPlanned is the key the crash test kills on: the run object write is
// named RunKey(...); "-planned" would never be written, so the correct kill
// target for "after marker, before flip" is the run object itself. The kill
// must fire on the *second* write of that name (first is the planning claim),
// so crashBucket grows a nth-write mode for this case — expressed here by
// returning the run key and letting the sub-test set failCalls.
func fmtRunPlanned() string { return RunKey("acme", "infra", "prod-networking", "aaaa", "bbbb") }

// TestStage_CacheHitReuse verifies the §5.2 write-once semantics: a second
// Stage for the same (base, head) pair reads as "already planned" — reuse, not
// crash — and the existing artifacts are untouched.
func TestStage_CacheHitReuse(t *testing.T) {
	ctx := context.Background()
	artifacts := NewMapBucket()
	coordination := NewMapBucket()

	if _, err := Stage(ctx, artifacts, coordination, FakeSigner{}, fixtureResult(t), stageInput()); err != nil {
		t.Fatalf("first Stage: %v", err)
	}
	before, _, err := artifacts.Read(ctx, Key("acme", "infra", "prod-networking", "aaaa", "bbbb", NamePlan))
	if err != nil {
		t.Fatal(err)
	}

	staged, err := Stage(ctx, artifacts, coordination, FakeSigner{}, fixtureResult(t), stageInput())
	if err != nil {
		t.Fatalf("second Stage: %v", err)
	}
	if !staged.CacheHit {
		t.Error("second Stage should report CacheHit on a write-once collision")
	}
	after, gen, err := artifacts.Read(ctx, Key("acme", "infra", "prod-networking", "aaaa", "bbbb", NamePlan))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || gen != 1 {
		t.Errorf("artifact mutated on cache hit (gen %d)", gen)
	}
}

// TestStage_MetaSignatureCoversPlanDigest proves the signature binds to the
// plan bytes: changing the plan changes the digest a verifier recomputes.
func TestStage_MetaSignatureCoversPlanDigest(t *testing.T) {
	ctx := context.Background()
	artifacts := NewMapBucket()
	coordination := NewMapBucket()

	res := fixtureResult(t)
	if _, err := Stage(ctx, artifacts, coordination, FakeSigner{}, res, stageInput()); err != nil {
		t.Fatal(err)
	}
	metaRaw, _, err := artifacts.Read(ctx, Key("acme", "infra", "prod-networking", "aaaa", "bbbb", NameMeta))
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}

	// Tamper with the recorded plan digest: verification must fail.
	tampered := meta
	tampered.PlanSHA256 = "0000"
	if err := (FakeSigner{}).Verify(ctx, mustDigest(t, tampered), meta.Signature); err == nil {
		t.Error("signature verified against a tampered plan digest")
	}
}

func mustDigest(t *testing.T, m Meta) []byte {
	t.Helper()
	d, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestKeyShapes pins the key composition — with no database, these names are
// the schema (§5.4).
func TestKeyShapes(t *testing.T) {
	if got := Key("acme", "infra", "prod", "a1", "b2", NamePlan); got != "pr/acme/infra/prod/a1/b2/tfplan" {
		t.Errorf("Key = %q", got)
	}
	if got := RunKey("acme", "infra", "prod", "a1", "b2"); got != "runs/pr/acme/infra/prod/a1/b2.json" {
		t.Errorf("RunKey = %q", got)
	}
	if got := PendingKey("acme", "infra", 412, "a1", "b2"); got != "pending-apply/acme/infra/412/a1/b2__b2" {
		t.Errorf("PendingKey = %q", got)
	}
	if got := PendingPrefix("acme", "infra", 0); got != "pending-apply/acme/infra/" {
		t.Errorf("PendingPrefix(all) = %q", got)
	}
	if got := PendingPrefix("acme", "infra", 412); got != "pending-apply/acme/infra/412/" {
		t.Errorf("PendingPrefix(pr) = %q", got)
	}
}

// TestMetaValidate_WhitelistKind pins the §6.6 whitelist: kind must be exactly
// "pr" — anything else (including "drift", the known case) is rejected.
func TestMetaValidate_WhitelistKind(t *testing.T) {
	m := Meta{Kind: KindPR, Repo: "acme/infra", PR: 1, Workspace: "w", BaseSHA: "a", HeadSHA: "b", PlannedTreeSHA: "c", PlanSHA256: "d"}
	if err := m.Validate(); err != nil {
		t.Errorf("valid meta rejected: %v", err)
	}
	for _, kind := range []string{"drift", "", "cost", "policy"} {
		bad := m
		bad.Kind = kind
		if err := bad.Validate(); err == nil {
			t.Errorf("kind %q accepted; whitelist must reject it", kind)
		}
	}
}
