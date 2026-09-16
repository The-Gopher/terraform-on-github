package plan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/store"
)

// MaxRenderedDiffBytes bounds an inlined `full` diff. Both `summary` and `text` cap at 65535
// bytes and GitHub rejects rather than truncates, so the cap has to be ours.
const MaxRenderedDiffBytes = 30000

// planJSON is the part of `terraform show -json` this package reads.
type planJSON struct {
	ResourceChanges []resourceChange `json:"resource_changes"`
}

type resourceChange struct {
	Address string `json:"address"`
	Change  struct {
		Actions []string `json:"actions"`
	} `json:"change"`
}

// Action is a resource change reduced to one of the five things a reviewer cares about.
type Action string

const (
	ActionNone    Action = ""
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"
	ActionDelete  Action = "delete"
	ActionReplace Action = "replace"
	ActionRead    Action = "read"
)

// Symbol is the marker used in the rendered address list.
func (a Action) Symbol() string {
	switch a {
	case ActionCreate:
		return "+"
	case ActionUpdate:
		return "~"
	case ActionDelete:
		return "-"
	case ActionReplace:
		return "-/+"
	case ActionRead:
		return "<="
	}
	return "?"
}

// classify reduces one resource_changes entry to an Action.
//
// A replace is an action *pair* — ["delete","create"], or ["create","delete"] under
// create_before_destroy — not its own action string. Counting it as one create plus one delete
// understates destructive changes in the headline, which is the one number reviewers read.
func classify(actions []string) Action {
	switch len(actions) {
	case 1:
		switch actions[0] {
		case "create":
			return ActionCreate
		case "update":
			return ActionUpdate
		case "delete":
			return ActionDelete
		case "read":
			return ActionRead
		case "no-op":
			return ActionNone
		}
	case 2:
		a, b := actions[0], actions[1]
		if (a == "delete" && b == "create") || (a == "create" && b == "delete") {
			return ActionReplace
		}
	}
	return ActionNone
}

// MaxRenderedAddresses caps how many resource addresses the summary lists. Check fields cap at
// 65535 bytes and GitHub rejects rather than truncates oversize payloads, so bound the list
// before formatting and say how many were omitted.
const MaxRenderedAddresses = 200

// Summary is the rendered check-run output.
type Summary struct {
	Title      string
	Body       string
	Conclusion string
}

// Render turns plan.json into check-run output.
//
//	prod-networking · envs/prod/networking · 3 to add, 1 to change, 0 to destroy
//
//	  + google_compute_subnetwork.app["c"]
//	  ~ google_compute_firewall.allow_health_checks
//	  + google_service_account.runner
//
//	plan a1b2c3d…c3d4e5f · terraform 1.9.8 · 42s
//	[view full diff] (expires in 15m)
//
// Both "no changes" and "has changes" are success conclusions — the counts carry the
// difference. Making "has changes" fail would paint every substantive PR red and train people
// to ignore the check.
func Render(w config.Workspace, m store.Meta, showJSON []byte, fullDiffURL string) (Summary, error) {
	counts, err := Counts(showJSON)
	if err != nil {
		return Summary{}, err
	}
	changed := counts.Any()

	title := fmt.Sprintf("%s · %s · %s", w.Name, w.Dir, Headline(counts))
	if !changed {
		title = fmt.Sprintf("%s · %s · no changes", w.Name, w.Dir)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", title)

	lines, omitted, err := AddressLines(showJSON, MaxRenderedAddresses)
	if err != nil {
		return Summary{}, err
	}
	if len(lines) > 0 {
		b.WriteString("\n")
		for _, l := range lines {
			fmt.Fprintf(&b, "%s\n", l)
		}
		if omitted > 0 {
			fmt.Fprintf(&b, "  … and %d more resource change(s) not shown\n", omitted)
		}
	}

	fmt.Fprintf(&b, "\nplan %s…%s · terraform %s · %s",
		shortSHA(m.BaseSHA), shortSHA(m.HeadSHA), m.TerraformVersion, HumanDuration(m.Duration))
	if fullDiffURL != "" {
		fmt.Fprintf(&b, "\n[view full diff] %s", fullDiffURL)
	}

	// Both "no changes" and "has changes" are success. Making "has changes" fail would paint
	// every substantive PR red and train people to ignore the check.
	return Summary{Title: title, Body: b.String(), Conclusion: NoOpConclusion()}, nil
}

// Headline is the counts line: "3 to add, 1 to change, 0 to destroy".
func Headline(c store.ResourceCounts) string {
	parts := []string{
		fmt.Sprintf("%d to add", c.Create),
		fmt.Sprintf("%d to change", c.Update),
		fmt.Sprintf("%d to destroy", c.Delete),
	}
	if c.Replace > 0 {
		// Only when non-zero: a trailing "0 to replace" on every plan is noise, and a non-zero
		// one needs to stand out.
		parts = append(parts, fmt.Sprintf("%d to replace", c.Replace))
	}
	return strings.Join(parts, ", ")
}

// AddressLines renders "  + google_compute_subnetwork.app" style lines, sorted by address, and
// reports how many were omitted by the cap.
func AddressLines(showJSON []byte, limit int) (lines []string, omitted int, err error) {
	var doc planJSON
	if err := json.Unmarshal(showJSON, &doc); err != nil {
		return nil, 0, fmt.Errorf("plan: parsing plan.json: %w", err)
	}
	for _, rc := range doc.ResourceChanges {
		a := classify(rc.Change.Actions)
		if a == ActionNone || a == ActionRead {
			continue
		}
		lines = append(lines, fmt.Sprintf("%3s %s", a.Symbol(), rc.Address))
	}
	sort.Slice(lines, func(i, j int) bool { return address(lines[i]) < address(lines[j]) })
	if limit > 0 && len(lines) > limit {
		omitted = len(lines) - limit
		lines = lines[:limit]
	}
	return lines, omitted, nil
}

func address(line string) string {
	if i := strings.LastIndexByte(line, ' '); i >= 0 {
		return line[i+1:]
	}
	return line
}

// HumanDuration keeps a tenth of a second below a minute, so a fast plan does not render as "0s",
// which reads as if nothing ran.
func HumanDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// redact strips values from a rendered diff.
//
// Two reasons the default is addresses-only. First, a plan file embeds a snapshot of prior
// state, so every secret in state is in the plan. Second, Terraform's `sensitive` marking is
// not a security boundary — it covers what the provider schema and the config declare
// sensitive, and not a token someone pasted into a startup script or a JSON blob in a
// metadata attribute.
//
// So `addresses` renders address + action + counts and nothing else, and `full` is an explicit
// per-workspace opt-in for workspaces whose diffs are known to be dull. Even under `full`,
// anything Terraform *does* mark sensitive is masked — that part is cheap and strictly
// additive.
func Redact(showText string, detail config.SummaryDetail) string {
	if detail != config.SummaryFull {
		return ""
	}
	// Under `full`, the text from `terraform show` is returned as Terraform produced it — which
	// already renders anything it considers sensitive as "(sensitive value)". That masking is the
	// cheap, strictly-additive part, and it is all that is on offer: Terraform marks what the
	// provider schema and the config declare sensitive, and nothing else. A token pasted into a
	// startup script or a JSON blob in a metadata attribute is not marked, and appears here in
	// full. That is why `full` is per-workspace opt-in and `addresses` is the default.
	if len(showText) > MaxRenderedDiffBytes {
		return showText[:MaxRenderedDiffBytes] +
			fmt.Sprintf("\n… truncated; %d more bytes in plan.txt", len(showText)-MaxRenderedDiffBytes)
	}
	return showText
}

// Counts tallies resource_changes from plan.json.
//
// A "replace" is an action pair — ["delete","create"] or ["create","delete"] for
// create_before_destroy — not its own action string, and must be counted as one replace rather
// than one create plus one delete. Getting this wrong understates destructive changes in the
// headline, which is the one number reviewers actually read.
func Counts(showJSON []byte) (store.ResourceCounts, error) {
	var doc planJSON
	if err := json.Unmarshal(showJSON, &doc); err != nil {
		return store.ResourceCounts{}, fmt.Errorf("plan: parsing plan.json: %w", err)
	}
	var c store.ResourceCounts
	for _, rc := range doc.ResourceChanges {
		switch classify(rc.Change.Actions) {
		case ActionCreate:
			c.Create++
		case ActionUpdate:
			c.Update++
		case ActionDelete:
			c.Delete++
		case ActionReplace:
			c.Replace++
		case ActionRead:
			c.Read++
		}
	}
	return c, nil
}

// NoOpConclusion returns the conclusion for a workspace with no changes.
func NoOpConclusion() string { return "success" }
