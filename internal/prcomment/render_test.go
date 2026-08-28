package prcomment

import (
	"strings"
	"testing"
	"time"

	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghcli"
	"github.com/sampleserve/terraform-on-github/internal/store"
)

func meta() store.Meta {
	return store.Meta{
		Schema: store.SchemaVersion, Owner: "acme", Repo: "infra", PR: 412,
		Workspace: "prod-networking",
		BaseSHA:   strings.Repeat("a", 40), HeadSHA: strings.Repeat("c", 40),
		PlannedTreeSHA: strings.Repeat("9", 40),
		ConfigRef:      "main", ConfigRefSHA: strings.Repeat("b", 40),
		TerraformVersion: "1.9.8", ProviderLockSHA256: strings.Repeat("d", 64),
		PlanSHA256: strings.Repeat("e", 64), HasChanges: true,
		Counts:    store.ResourceCounts{Create: 2, Replace: 1},
		PlannedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC), Duration: 42 * time.Second,
	}
}

func workspace(detail config.SummaryDetail) config.Workspace {
	return config.Workspace{
		Name: "prod-networking", Dir: "envs/prod/networking", SummaryDetail: detail,
		Backend: config.Backend{Bucket: "b", Prefix: "p"},
	}
}

const someJSON = `{"resource_changes":[
	{"address":"google_compute_subnetwork.app","change":{"actions":["create"]}},
	{"address":"google_sql_database_instance.main","change":{"actions":["delete","create"]}}
]}`

func planInput(detail config.SummaryDetail) PlanInput {
	m := meta()
	digest, _ := m.DigestHex()
	return PlanInput{
		Workspace: workspace(detail), Meta: m, Digest: digest,
		ShowJSON: []byte(someJSON), ShowText: "the full terraform diff",
		StorePath: "/store/path", Marker: ghcli.Marker("plan", "prod-networking"),
		PlannedBy: "someone",
	}
}

func TestPlanBodyCarriesTheMarkerAndHeadline(t *testing.T) {
	body, err := Plan(planInput(config.SummaryAddresses))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		ghcli.Marker("plan", "prod-networking"),
		"envs/prod/networking",
		"1 to add, 0 to change, 0 to destroy, 1 to replace",
		"google_compute_subnetwork.app",
		"terraform 1.9.8",
		"planned by @someone",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body should contain %q", want)
		}
	}
}

// The default is a security default, not a terseness one: a plan file embeds a state snapshot.
func TestAddressesDetailDoesNotInlineTheDiff(t *testing.T) {
	body, _ := Plan(planInput(config.SummaryAddresses))
	if strings.Contains(body, "the full terraform diff") {
		t.Error("addresses detail must not inline the diff")
	}
	if !strings.Contains(body, "embeds a snapshot of prior state") {
		t.Error("the body should say why the diff is not shown")
	}
}

func TestFullDetailInlinesTheDiff(t *testing.T) {
	body, _ := Plan(planInput(config.SummaryFull))
	if !strings.Contains(body, "the full terraform diff") {
		t.Error("full detail should inline the diff")
	}
	if !strings.Contains(body, "<details>") {
		t.Error("a long diff belongs behind a disclosure")
	}
}

func TestPlanOnlyWorkspaceSaysSo(t *testing.T) {
	in := planInput(config.SummaryAddresses)
	disabled := false
	in.Workspace.Apply.Enabled = &disabled
	body, _ := Plan(in)
	if !strings.Contains(body, "plan-only") {
		t.Error("a reviewer must know merging will not apply this")
	}
}

func TestNoChangesReadsAsNoChanges(t *testing.T) {
	in := planInput(config.SummaryAddresses)
	in.ShowJSON = []byte(`{"resource_changes":[{"address":"a","change":{"actions":["no-op"]}}]}`)
	body, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "No changes.") {
		t.Errorf("expected a no-changes verdict; got:\n%s", body)
	}
}

func TestMetaBlockRoundTrips(t *testing.T) {
	body, err := Plan(planInput(config.SummaryAddresses))
	if err != nil {
		t.Fatal(err)
	}
	got := ParseMeta(body)
	if got == nil {
		t.Fatal("the machine-readable block must be parseable back out")
	}
	want := meta()
	if got.PlanSHA256 != want.PlanSHA256 || got.PlannedTreeSHA != want.PlannedTreeSHA ||
		got.BaseSHA != want.BaseSHA || got.HeadSHA != want.HeadSHA {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// The digest recorded in the comment must match the digest of the record in the comment, or
	// the cross-check at apply time is comparing a value to itself.
	recomputed, err := got.Meta.DigestHex()
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != recomputed {
		t.Errorf("recorded digest %s does not match the record it covers (%s)", got.Digest, recomputed)
	}
}

func TestParseMetaOnBodiesWithoutABlock(t *testing.T) {
	for _, body := range []string{
		"just prose",
		"<!-- tfog-meta\nnot json\n-->",
		"<!-- tfog-meta {\"unterminated\": true}",
	} {
		if got := ParseMeta(body); got != nil {
			t.Errorf("ParseMeta(%q) = %+v, want nil", body, got)
		}
	}
}

func TestApplyBodySucceededAndFailed(t *testing.T) {
	m := meta()
	m.MergeCommitSHA = strings.Repeat("f", 40)
	in := ApplyInput{
		Workspace: workspace(config.SummaryAddresses), Meta: m, Counts: m.Counts,
		OK: true, Detail: "Apply complete!", LogPath: "/logs/apply-1.log",
		Marker: ghcli.Marker("apply", "prod-networking"),
	}
	ok := Apply(in)
	if !strings.Contains(ok, "Applied the reviewed plan") || !strings.Contains(ok, "tree") {
		t.Errorf("success body:\n%s", ok)
	}
	if !strings.Contains(ok, ghcli.Marker("apply", "prod-networking")) {
		t.Error("the apply comment needs its own marker")
	}

	in.OK = false
	in.Detail = "Error: boom"
	failed := Apply(in)
	if !strings.Contains(failed, "Apply failed") {
		t.Errorf("failure body:\n%s", failed)
	}
	if !strings.Contains(failed, "Read the log before re-running") {
		t.Error("a partial apply must warn against blind retry")
	}
}

func TestNotice(t *testing.T) {
	body := Notice("<!-- m -->", "A title", "some detail")
	for _, want := range []string{"<!-- m -->", "### A title", "some detail"} {
		if !strings.Contains(body, want) {
			t.Errorf("notice should contain %q; got %q", want, body)
		}
	}
}
