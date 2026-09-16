package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sampleserve/terraform-on-github/internal/config"
)

func showJSON(t *testing.T, actions ...[]string) []byte {
	t.Helper()
	type change struct {
		Actions []string `json:"actions"`
	}
	type rc struct {
		Address string `json:"address"`
		Change  change `json:"change"`
	}
	doc := struct {
		ResourceChanges []rc `json:"resource_changes"`
	}{}
	for i, a := range actions {
		doc.ResourceChanges = append(doc.ResourceChanges, rc{
			Address: "res.r" + string(rune('a'+i%26)),
			Change:  change{Actions: a},
		})
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A replace is an action pair, not its own action string. Counting it as one create plus one
// delete understates destruction in the headline, which is the one number reviewers read.
func TestReplaceIsOneChange(t *testing.T) {
	c, err := Counts(showJSON(t, []string{"delete", "create"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Replace != 1 || c.Create != 0 || c.Delete != 0 {
		t.Fatalf("got %+v, want one replace and nothing else", c)
	}
}

func TestCreateBeforeDestroyPairIsAlsoAReplace(t *testing.T) {
	c, _ := Counts(showJSON(t, []string{"create", "delete"}))
	if c.Replace != 1 {
		t.Fatalf("got %+v, want one replace", c)
	}
}

func TestCountsTalliesEveryAction(t *testing.T) {
	c, _ := Counts(showJSON(t,
		[]string{"create"},
		[]string{"update"},
		[]string{"delete"},
		[]string{"delete", "create"},
		[]string{"create", "delete"},
		[]string{"read"},
		[]string{"no-op"},
	))
	want := struct{ cr, up, del, rep, rd int }{1, 1, 1, 2, 1}
	if c.Create != want.cr || c.Update != want.up || c.Delete != want.del || c.Replace != want.rep || c.Read != want.rd {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestNoOpAndReadAreNotChanges(t *testing.T) {
	c, _ := Counts(showJSON(t, []string{"no-op"}, []string{"read"}))
	if c.Any() {
		t.Errorf("a plan of only no-ops and reads has no changes: %+v", c)
	}
}

func TestHeadlineMentionsReplaceOnlyWhenPresent(t *testing.T) {
	plain, _ := Counts(showJSON(t, []string{"create"}))
	if strings.Contains(Headline(plain), "replace") {
		t.Errorf("a trailing '0 to replace' on every plan is noise: %q", Headline(plain))
	}
	withReplace, _ := Counts(showJSON(t, []string{"delete", "create"}))
	if !strings.Contains(Headline(withReplace), "1 to replace") {
		t.Errorf("a non-zero replace must stand out: %q", Headline(withReplace))
	}
}

func TestAddressLinesSymbolsAndSorting(t *testing.T) {
	body := []byte(`{"resource_changes":[
		{"address":"z.last","change":{"actions":["create"]}},
		{"address":"a.first","change":{"actions":["delete","create"]}},
		{"address":"m.mid","change":{"actions":["update"]}},
		{"address":"d.data","change":{"actions":["read"]}},
		{"address":"n.noop","change":{"actions":["no-op"]}}
	]}`)
	lines, omitted, err := AddressLines(body, 0)
	if err != nil {
		t.Fatal(err)
	}
	if omitted != 0 {
		t.Errorf("omitted = %d, want 0", omitted)
	}
	if len(lines) != 3 {
		t.Fatalf("reads and no-ops are not changes; got %q", lines)
	}
	if !strings.HasSuffix(lines[0], "a.first") || !strings.Contains(lines[0], "-/+") {
		t.Errorf("sorted by address, replace marked -/+; got %q", lines[0])
	}
	if !strings.HasSuffix(lines[2], "z.last") {
		t.Errorf("sorted by address; got %q", lines)
	}
}

func TestAddressLinesCapReportsOmissions(t *testing.T) {
	actions := make([][]string, 250)
	for i := range actions {
		actions[i] = []string{"create"}
	}
	lines, omitted, err := AddressLines(showJSON(t, actions...), MaxRenderedAddresses)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != MaxRenderedAddresses {
		t.Errorf("got %d lines, want the cap of %d", len(lines), MaxRenderedAddresses)
	}
	if omitted != 250-MaxRenderedAddresses {
		t.Errorf("omitted = %d, want %d", omitted, 250-MaxRenderedAddresses)
	}
}

func TestCountsRejectsGarbage(t *testing.T) {
	if _, err := Counts([]byte("not json")); err == nil {
		t.Fatal("unparseable plan.json must be an error, not zero counts that read as 'no changes'")
	}
}

func TestRedactHonoursSummaryDetail(t *testing.T) {
	if got := Redact("a diff", config.SummaryAddresses); got != "" {
		t.Errorf("addresses detail must inline nothing; got %q", got)
	}
	if got := Redact("a diff", config.SummaryFull); got != "a diff" {
		t.Errorf("full detail should pass the text through; got %q", got)
	}
}

func TestRedactTruncatesAndSaysSo(t *testing.T) {
	long := strings.Repeat("x", MaxRenderedDiffBytes+500)
	got := Redact(long, config.SummaryFull)
	if len(got) <= MaxRenderedDiffBytes {
		t.Fatal("expected the truncation note to be appended")
	}
	if !strings.Contains(got, "truncated") || !strings.Contains(got, "500 more bytes") {
		t.Errorf("truncation must say how much was dropped; got tail %q", got[len(got)-60:])
	}
}
