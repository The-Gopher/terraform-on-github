package tf

import (
	"os"
	"strings"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/the-gopher/terraform-on-github/internal/config"
)

// loadFixturePlan reads a stored plan JSON. The fixture carries a known secret
// ("hunter2supersecret") in a sensitive attribute — the exact case §8.1 exists for.
func loadFixturePlan(t *testing.T) *tfjson.Plan {
	t.Helper()
	raw, err := os.ReadFile("testdata/plan-with-secret.json")
	if err != nil {
		t.Fatal(err)
	}
	var plan tfjson.Plan
	if err := plan.UnmarshalJSON(raw); err != nil {
		t.Fatalf("fixture plan must parse: %v", err)
	}
	return &plan
}

// TestAddressesSummaryNeverRendersValues is the M3 done-when: the addresses renderer is
// verified against a plan containing a known secret. The secret must not appear in the
// summary, and the redacted line must still be present — a crash or empty summary would
// pass a naive "not contains" check.
func TestAddressesSummaryNeverRendersValues(t *testing.T) {
	plan := loadFixturePlan(t)

	got := FormatAddressesSummary(plan)

	want := strings.Join([]string{
		"  create: google_secret_manager_secret.db_password",
		"  create: random_password.api_key",
		"  update: google_compute_instance.web",
	}, "\n")
	if got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}

	if strings.Contains(got, "hunter2supersecret") {
		t.Error("secret value leaked into addresses summary")
	}
	// Attribute values are the leak vector; their absence is the point.
	for _, leaked := range []string{"e2-medium", "db_password\"", "secret_id"} {
		if strings.Contains(got, leaked) {
			t.Errorf("non-address value %q leaked into summary", leaked)
		}
	}
}

// TestFullSummaryAlsoRedacts: SummaryFull must not be a secret leak either. Until the
// full renderer exists, both modes render addresses — assert the invariant so a future
// full renderer cannot silently regress the boundary.
func TestFullSummaryAlsoRedacts(t *testing.T) {
	plan := loadFixturePlan(t)

	got := BuildPlanSummary(plan, config.SummaryFull)

	if strings.Contains(got, "hunter2supersecret") {
		t.Error("secret value leaked into full summary")
	}
	if !strings.Contains(got, "random_password.api_key") {
		t.Error("full summary should still list changed addresses")
	}
}

// TestNoChangesPlan: an all-no-op plan (or nil) renders "No changes." rather than an
// empty summary, so a check run never shows a blank plan body.
func TestNoChangesPlan(t *testing.T) {
	if got := FormatAddressesSummary(nil); got != "No changes." {
		t.Errorf("nil plan summary = %q", got)
	}

	var plan tfjson.Plan
	plan.ResourceChanges = []*tfjson.ResourceChange{}
	if got := FormatAddressesSummary(&plan); got != "No changes." {
		t.Errorf("empty plan summary = %q", got)
	}
}