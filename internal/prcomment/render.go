// Package prcomment renders the PR comments the local commands post.
//
// Separate from internal/plan, which renders check-run output for the services. The content is
// the same; the container is not, and the two have different size limits and different Markdown
// affordances. Keeping them apart means neither has to be written to suit the other.
package prcomment

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/plan"
	"github.com/sampleserve/terraform-on-github/internal/store"
)

// metaOpen and metaClose delimit the machine-readable block.
const (
	metaOpen  = "<!-- tfog-meta"
	metaClose = "-->"
)

// PlanInput is everything a plan comment renders from.
type PlanInput struct {
	Workspace config.Workspace
	Meta      store.Meta
	Digest    string
	ShowJSON  []byte
	ShowText  string
	StorePath string
	Marker    string
	PlannedBy string
}

// Plan renders the sticky plan comment.
func Plan(in PlanInput) (string, error) {
	counts, err := plan.Counts(in.ShowJSON)
	if err != nil {
		return "", err
	}
	changed := counts.Any()

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n### `terraform plan` — **%s**\n\n", in.Marker, in.Workspace.Name)

	verdict := plan.Headline(counts)
	if !changed {
		verdict = "**No changes.** Infrastructure matches the configuration."
	}
	fmt.Fprintf(&b, "`%s` · %s\n\n", in.Workspace.Dir, verdict)

	lines, omitted, err := plan.AddressLines(in.ShowJSON, plan.MaxRenderedAddresses)
	if err != nil {
		return "", err
	}
	if len(lines) > 0 {
		b.WriteString("```diff\n")
		for _, l := range lines {
			fmt.Fprintf(&b, "%s\n", l)
		}
		if omitted > 0 {
			fmt.Fprintf(&b, "… and %d more resource change(s) not shown\n", omitted)
		}
		b.WriteString("```\n\n")
	}

	if diff := plan.Redact(in.ShowText, in.Workspace.SummaryDetail); diff != "" && changed {
		fmt.Fprintf(&b, "<details><summary>Full diff</summary>\n\n```terraform\n%s\n```\n\n</details>\n\n", diff)
	}

	fmt.Fprintf(&b, "plan `%s…%s` · terraform %s · %s · planned by @%s\n\n",
		short(in.Meta.BaseSHA, 8), short(in.Meta.HeadSHA, 8), in.Meta.TerraformVersion,
		plan.HumanDuration(in.Meta.Duration), in.PlannedBy)

	fmt.Fprintf(&b, "<sub>tree `%s` · plan `%s` · config from `%s` @ `%s` · artifacts `%s`</sub>\n\n",
		short(in.Meta.PlannedTreeSHA, 12), short(in.Meta.PlanSHA256, 12),
		in.Meta.ConfigRef, short(in.Meta.ConfigRefSHA, 8), in.StorePath)

	if in.Workspace.SummaryDetail != config.SummaryFull && changed {
		b.WriteString("<sub>Only addresses and actions are shown: a plan file embeds a snapshot of prior " +
			"state. The full diff is in `plan.txt` in the store above.</sub>\n\n")
	}
	if !in.Workspace.ApplyEnabled() {
		b.WriteString("<sub>This workspace is **plan-only** — merging will not apply it.</sub>\n\n")
	}

	block, err := metaBlock(in.Meta, in.Digest)
	if err != nil {
		return "", err
	}
	b.WriteString(block)
	return b.String(), nil
}

// ApplyInput is everything an apply comment renders from.
type ApplyInput struct {
	Workspace config.Workspace
	Meta      store.Meta
	Counts    store.ResourceCounts
	OK        bool
	Detail    string
	LogPath   string
	Marker    string
}

// Apply renders the outcome comment. Posted fresh rather than edited in place, so the timeline
// reads as one story: plan, then what happened.
func Apply(in ApplyInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n### `terraform apply` — **%s**\n\n", in.Marker, in.Workspace.Name)

	if in.OK {
		fmt.Fprintf(&b, "Applied the reviewed plan. %s.\n\n", plan.Headline(in.Counts))
	} else {
		b.WriteString("**Apply failed.**\n\n")
	}
	if d := strings.TrimSpace(in.Detail); d != "" {
		fmt.Fprintf(&b, "```\n%s\n```\n\n", d)
	}
	fmt.Fprintf(&b, "plan `%s…%s` · merge commit `%s` · tree `%s` verified\n\n",
		short(in.Meta.BaseSHA, 8), short(in.Meta.HeadSHA, 8),
		short(in.Meta.MergeCommitSHA, 8), short(in.Meta.PlannedTreeSHA, 12))
	fmt.Fprintf(&b, "<sub>terraform %s · log `%s`</sub>\n", in.Meta.TerraformVersion, in.LogPath)

	if !in.OK {
		b.WriteString("\n<sub>An apply that fails part-way has already changed some infrastructure. " +
			"Read the log before re-running anything — the next plan shows what remains.</sub>\n")
	}
	return b.String()
}

// Notice renders a standalone advisory or blocking comment.
func Notice(marker, title, detail string) string {
	return fmt.Sprintf("%s\n### %s\n\n%s\n", marker, title, detail)
}

// ---------------------------------------------------------------------------
// The machine-readable block
// ---------------------------------------------------------------------------

// metaBlock embeds meta.json in an HTML comment: invisible in the timeline, readable by
// cmd/tfog-apply.
//
// This is the portable half of the key, and it is an index rather than an authority. tfog-apply
// locates the plan in the local store itself and only cross-checks these values, so a forged
// comment can make an apply *refuse* — never make it apply something else. That asymmetry is what
// lets the comment be trusted for lookup without being trusted for authorization.
func metaBlock(m store.Meta, digest string) (string, error) {
	payload := struct {
		store.Meta
		Digest string `json:"digest"`
	}{Meta: m, Digest: digest}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\n%s\n%s", metaOpen, body, metaClose), nil
}

// Published is what a plan comment claims about the plan it describes.
type Published struct {
	store.Meta
	Digest string `json:"digest"`
}

// ParseMeta extracts the block from a comment body. Returns nil when there is none.
func ParseMeta(body string) *Published {
	start := strings.Index(body, metaOpen)
	if start < 0 {
		return nil
	}
	rest := body[start+len(metaOpen):]
	end := strings.Index(rest, metaClose)
	if end < 0 {
		return nil
	}
	var p Published
	if err := json.Unmarshal([]byte(rest[:end]), &p); err != nil {
		return nil
	}
	return &p
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
