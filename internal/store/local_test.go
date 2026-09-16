package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKey() PlanKey {
	return PlanKey{
		Owner: "acme", Repo: "infra", Workspace: "prod-networking",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("c", 40),
	}
}

func testMeta(k PlanKey) Meta {
	return Meta{
		Schema: SchemaVersion, Owner: k.Owner, Repo: k.Repo, PR: 412, Workspace: k.Workspace,
		BaseSHA: k.BaseSHA, HeadSHA: k.HeadSHA,
		PlannedTreeSHA: strings.Repeat("9", 40),
		ConfigRef:      "main", ConfigRefSHA: strings.Repeat("b", 40),
		TerraformVersion: "1.9.8", ProviderLockSHA256: strings.Repeat("d", 64),
		PlanSHA256: strings.Repeat("e", 64),
		HasChanges: true, Counts: ResourceCounts{Create: 1},
		PlannedAt: time.Now().UTC().Truncate(time.Second), Duration: 42 * time.Second,
	}
}

func newStore(t *testing.T) *Local {
	t.Helper()
	return &Local{Root: t.TempDir()}
}

func TestKeyLayoutMatchesTheBucket(t *testing.T) {
	k := testKey()
	want := "acme/infra/prod-networking/" + k.BaseSHA + "/" + k.HeadSHA
	if got := k.Prefix(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriteOnce(t *testing.T) {
	l, k := newStore(t), testKey()
	if err := l.Put(k, ArtifactPlan, strings.NewReader("PLAN")); err != nil {
		t.Fatal(err)
	}
	err := l.Put(k, ArtifactPlan, strings.NewReader("SWAPPED"))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("a second write must be refused; got %v", err)
	}
	body, _ := l.ReadArtifact(k, ArtifactPlan)
	if string(body) != "PLAN" {
		t.Errorf("the original bytes must survive; got %q", body)
	}
}

// A plan file embeds a snapshot of prior state, so every secret in state is in this directory.
func TestArtifactsAreNotWorldReadable(t *testing.T) {
	l, k := newStore(t), testKey()
	if err := l.Put(k, ArtifactPlan, strings.NewReader("PLAN")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(l.Path(k, ArtifactPlan))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("artifact mode is %o, want 600", fi.Mode().Perm())
	}
	di, _ := os.Stat(l.KeyDir(k))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("key dir mode is %o, want 700", di.Mode().Perm())
	}
}

func TestCompleteOnlyAfterMeta(t *testing.T) {
	l, k := newStore(t), testKey()
	_ = l.Put(k, ArtifactPlan, strings.NewReader("PLAN"))
	if l.Complete(k) {
		t.Error("a plan with no meta.json is not complete")
	}
	if _, err := l.PutMeta(k, testMeta(k)); err != nil {
		t.Fatal(err)
	}
	if !l.Complete(k) {
		t.Error("meta.json is the completion marker")
	}
}

func TestReadMetaDetectsAnEdit(t *testing.T) {
	l, k := newStore(t), testKey()
	if _, err := l.PutMeta(k, testMeta(k)); err != nil {
		t.Fatal(err)
	}
	path := l.Path(k, ArtifactMeta)
	body, _ := os.ReadFile(path)
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	raw["planned_tree_sha"] = strings.Repeat("0", 40)
	edited, _ := json.Marshal(raw)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadMeta(k); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("an edited record must be refused; got %v", err)
	}
}

func TestReadMetaRefusesAMissingDigest(t *testing.T) {
	l, k := newStore(t), testKey()
	if err := l.Put(k, ArtifactMeta, strings.NewReader(`{"schema":1,"owner":"acme"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadMeta(k); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("a record with no digest must be refused; got %v", err)
	}
}

func TestReadMetaRoundTrips(t *testing.T) {
	l, k := newStore(t), testKey()
	want := testMeta(k)
	if _, err := l.PutMeta(k, want); err != nil {
		t.Fatal(err)
	}
	got, err := l.ReadMeta(k)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlannedTreeSHA != want.PlannedTreeSHA || got.Duration != want.Duration ||
		got.Counts != want.Counts || !got.PlannedAt.Equal(want.PlannedAt) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestReadMetaOnAnEmptyKey(t *testing.T) {
	l, k := newStore(t), testKey()
	if _, err := l.ReadMeta(k); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("got %v, want ErrNoPlan", err)
	}
}

// Every field in the digest must actually change it, or the digest is not covering it.
func TestDigestCoversEveryField(t *testing.T) {
	base := testMeta(testKey())
	baseline, err := base.DigestHex()
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Meta){
		"owner":                func(m *Meta) { m.Owner = "other" },
		"repo":                 func(m *Meta) { m.Repo = "other" },
		"pr":                   func(m *Meta) { m.PR = 999 },
		"workspace":            func(m *Meta) { m.Workspace = "other" },
		"base_sha":             func(m *Meta) { m.BaseSHA = strings.Repeat("f", 40) },
		"head_sha":             func(m *Meta) { m.HeadSHA = strings.Repeat("f", 40) },
		"merge_commit_sha":     func(m *Meta) { m.MergeCommitSHA = strings.Repeat("f", 40) },
		"planned_tree_sha":     func(m *Meta) { m.PlannedTreeSHA = strings.Repeat("f", 40) },
		"config_ref":           func(m *Meta) { m.ConfigRef = "other" },
		"config_ref_sha":       func(m *Meta) { m.ConfigRefSHA = strings.Repeat("f", 40) },
		"terraform_version":    func(m *Meta) { m.TerraformVersion = "1.10.0" },
		"provider_lock_sha256": func(m *Meta) { m.ProviderLockSHA256 = strings.Repeat("f", 64) },
		"plan_sha256":          func(m *Meta) { m.PlanSHA256 = strings.Repeat("f", 64) },
		"has_changes":          func(m *Meta) { m.HasChanges = false },
		"counts.create":        func(m *Meta) { m.Counts.Create = 99 },
		"counts.replace":       func(m *Meta) { m.Counts.Replace = 99 },
		"planned_at":           func(m *Meta) { m.PlannedAt = m.PlannedAt.Add(time.Hour) },
		"duration":             func(m *Meta) { m.Duration = 99 * time.Second },
	}
	for name, mutate := range mutations {
		m := base
		mutate(&m)
		got, err := m.DigestHex()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == baseline {
			t.Errorf("changing %s did not change the digest — the field is not covered", name)
		}
	}
}

// Length-prefixing is what stops two different field splits from digesting identically.
func TestDigestIsNotConcatenationAmbiguous(t *testing.T) {
	a := testMeta(testKey())
	a.Owner, a.Repo = "ab", "c"
	b := a
	b.Owner, b.Repo = "a", "bc"
	da, _ := a.DigestHex()
	db, _ := b.DigestHex()
	if da == db {
		t.Error(`("ab","c") and ("a","bc") must not digest identically`)
	}
}

func TestDigestIsStableAcrossCalls(t *testing.T) {
	m := testMeta(testKey())
	first, _ := m.DigestHex()
	for i := 0; i < 5; i++ {
		again, _ := m.DigestHex()
		if again != first {
			t.Fatal("the digest must not depend on map iteration order or anything else unstable")
		}
	}
}

func TestDigestRejectsAWrongSchema(t *testing.T) {
	m := testMeta(testKey())
	m.Schema = 99
	if _, err := m.Digest(); err == nil {
		t.Fatal("an unknown schema must not be digested as if it were understood")
	}
}

func TestFindByHeadSHA(t *testing.T) {
	l, k := newStore(t), testKey()
	_ = l.Put(k, ArtifactPlan, strings.NewReader("PLAN"))
	if _, err := l.PutMeta(k, testMeta(k)); err != nil {
		t.Fatal(err)
	}
	got, err := l.Find(k.Owner, k.Repo, k.Workspace, k.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Prefix() != k.Prefix() {
		t.Fatalf("got %+v", got)
	}
	other, _ := l.Find(k.Owner, k.Repo, k.Workspace, strings.Repeat("f", 40))
	if len(other) != 0 {
		t.Errorf("a different head must not match: %+v", other)
	}
	missing, err := l.Find("nobody", "nothing", "nowhere", "")
	if err != nil || len(missing) != 0 {
		t.Errorf("an absent path is empty, not an error: (%v, %v)", missing, err)
	}
}

func TestFindIgnoresIncompletePlans(t *testing.T) {
	l, k := newStore(t), testKey()
	_ = l.Put(k, ArtifactPlan, strings.NewReader("PLAN")) // no meta.json
	got, _ := l.Find(k.Owner, k.Repo, k.Workspace, "")
	if len(got) != 0 {
		t.Errorf("an incomplete plan is not findable: %+v", got)
	}
}

func TestRunStateTransitions(t *testing.T) {
	l, k := newStore(t), testKey()
	if l.ReadRun(k).State != "" {
		t.Error("no state.json should read as a zero run")
	}
	if err := l.WriteRun(k, LocalRun{State: StatePlanned, PR: 412}); err != nil {
		t.Fatal(err)
	}
	if got := l.ReadRun(k); got.State != StatePlanned || got.PR != 412 {
		t.Errorf("got %+v", got)
	}
	if err := l.WriteRun(k, LocalRun{State: StateApplied, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if got := l.ReadRun(k); got.State != StateApplied {
		t.Errorf("got %+v", got)
	}
	if got := l.ReadRun(k); got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be stamped on write")
	}
}

func TestApplyLogAttemptsIncrement(t *testing.T) {
	l, k := newStore(t), testKey()
	p1, a1, err := l.NextApplyLog(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p1, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, a2, _ := l.NextApplyLog(k)
	if a1 != 1 || a2 != 2 {
		t.Errorf("attempts = %d, %d; want 1, 2", a1, a2)
	}
	if p1 == p2 {
		t.Error("a second attempt must not overwrite the first log")
	}
}

func TestPurgeRemovesTheKey(t *testing.T) {
	l, k := newStore(t), testKey()
	_ = l.Put(k, ArtifactPlan, strings.NewReader("PLAN"))
	if err := l.Purge(k); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.KeyDir(k)); !os.IsNotExist(err) {
		t.Error("purge should remove the whole key")
	}
}

func TestSHA256OfStoredArtifact(t *testing.T) {
	l, k := newStore(t), testKey()
	_ = l.Put(k, ArtifactPlan, strings.NewReader("PLAN"))
	got, err := l.SHA256(k, ArtifactPlan)
	if err != nil {
		t.Fatal(err)
	}
	const want = "0ff15403106fefdaa7a0816f2c17f1fd4e4e1c065feb6194a07df811dc03d188" // sha256("PLAN")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStoreRootIsPrivate(t *testing.T) {
	l, k := newStore(t), testKey()
	_ = l.Put(k, ArtifactPlan, strings.NewReader("PLAN"))
	for _, p := range []string{l.Root, filepath.Join(l.Root, "plans")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode is %o, want 700", p, fi.Mode().Perm())
		}
	}
}
