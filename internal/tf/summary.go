// Package tf — plan summary rendering. Kept here rather than in cmd/ so the M6 worker and
// the CLI render identically from the same code.
package tf

import (
	"fmt"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/the-gopher/terraform-on-github/internal/config"
)

// BuildPlanSummary renders a plan per the workspace's summary_detail.
func BuildPlanSummary(plan *tfjson.Plan, detail config.SummaryDetail) string {
	switch detail {
	case config.SummaryFull:
		return formatFullSummary(plan)
	case config.SummaryAddresses:
		fallthrough
	default:
		return FormatAddressesSummary(plan)
	}
}

// FormatAddressesSummary renders resource address + action only (§8.1, the default
// summary_detail). Values are never rendered here — a plan file embeds a state snapshot
// and Terraform's `sensitive` marking does not cover secrets a human pasted into a
// non-sensitive attribute, so the address list is the redaction boundary.
func FormatAddressesSummary(plan *tfjson.Plan) string {
	if plan == nil || plan.ResourceChanges == nil {
		return "No changes."
	}

	var lines []string
	for _, rc := range plan.ResourceChanges {
		action := FormatAction(rc.Change.Actions)
		if action == "no-op" {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", action, rc.Address))
	}

	if len(lines) == 0 {
		return "No changes."
	}

	return strings.Join(lines, "\n")
}

// FormatAction converts Actions to a human-readable string.
func FormatAction(actions tfjson.Actions) string {
	if actions.NoOp() {
		return "no-op"
	}
	if actions.Create() {
		return "create"
	}
	if actions.Delete() {
		return "delete"
	}
	if actions.Update() {
		return "update"
	}
	if actions.Replace() {
		return "replace"
	}
	if actions.CreateBeforeDestroy() {
		return "create_before_destroy"
	}
	if actions.DestroyBeforeCreate() {
		return "destroy_before_create"
	}
	if actions.Forget() {
		return "forget"
	}
	if actions.Read() {
		return "read"
	}
	return "unknown"
}

// formatFullSummary inlines the diff. For v0.1 it is the address list; the full renderer
// lands with the M4 signed-URL path, where §8.1's redaction has somewhere to live.
func formatFullSummary(plan *tfjson.Plan) string {
	return FormatAddressesSummary(plan)
}