// Package config models .terraform-on-github.yaml — a repo's declaration of which base
// branches control which Terraform root modules, and which identities each may assume.
//
// The file is always loaded from the tip of a PR's base branch (see Loader). Loading it
// from the PR head would let any pull request repoint `Impersonate.Apply` at a privileged
// service account, so that rule is a security boundary, not a convention.
package config

import (
	"fmt"
	"time"
)

// Duration is a time.Duration written the way Go writes one — "30s", "20m", "24h", "1h30m" —
// rather than as a bare number of seconds, so the config reads like the design document.
//
// A named type rather than a plain time.Duration because yaml.v3 decodes a duration only from an
// integer count of nanoseconds. The usual trick of shadowing the field in an anonymous struct
// does not work here either: yaml.v3 ignores an un-tagged embedded pointer silently, and panics
// on a duplicated key when the embed is tagged `,inline`. So the conversion belongs on the type.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts any form time.ParseDuration accepts. An absent or empty value decodes to
// zero, which is what lets Config.Normalize tell "unset" from "explicitly zero".
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf("expected a duration like 30s, 20m or 24h: %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration; write it as 30s, 20m or 24h", s)
	}
	if parsed < 0 {
		return fmt.Errorf("%q is negative", s)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML writes the human form back, so a round-trip does not turn "20m" into 1200000000000.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// SchemaVersion is the only accepted value of the top-level `version` key. Bumping it is a
// breaking change requiring a migration path for every consuming repo.
const SchemaVersion = 1

// Filename is the fixed path within a repository. Not configurable: a configurable config
// location is one more thing a PR could try to redirect.
const Filename = ".terraform-on-github.yaml"

// Config is the whole file.
type Config struct {
	Version int `yaml:"version"`

	Defaults Defaults `yaml:"defaults"`

	// ModuleSources allowlists module `source` values by glob. Enforced by tf.CheckModuleSources
	// before `terraform init` runs, because init fetches and can execute arbitrary code and
	// Terraform has no native allowlist. Empty means local paths only.
	ModuleSources []string `yaml:"module_sources"`

	Workspaces []Workspace `yaml:"workspaces"`
}

// Defaults supplies values for any Workspace field left unset. Applied by Config.Normalize.
type Defaults struct {
	TerraformVersion string        `yaml:"terraform_version"`
	PlanTimeout      Duration      `yaml:"plan_timeout"`
	ApplyTimeout     Duration      `yaml:"apply_timeout"`
	SummaryDetail    SummaryDetail `yaml:"summary_detail"`
	VarFiles         []string      `yaml:"var_files"`
	PlanArgs         []string      `yaml:"plan_args"`
}

// Workspace binds one base branch to one Terraform root module and its two identities.
type Workspace struct {
	// Name appears in the check-run name and in several GCS object keys. With no database, those
	// key names *are* the schema, so renaming a workspace orphans its plan history and its pending
	// markers: treat it as immutable once merged.
	Name string `yaml:"name"`

	// Branch is the base branch this workspace is bound to. Glob syntax allowed ("release/*").
	// A PR is scoped by its base branch, never by its head branch.
	Branch string `yaml:"branch"`

	// Dir is the repo-relative root module directory. Validated to stay inside the repo.
	Dir string `yaml:"dir"`

	// Watch lists extra globs whose modification invalidates this workspace — shared modules,
	// typically. Explicit rather than derived from the module graph: deriving it requires
	// running `terraform init` on untrusted config before deciding whether we should.
	Watch []string `yaml:"watch"`

	// TerraformWorkspace is an escape hatch for `terraform workspace select`. Prefer a
	// separate backend prefix per environment; see DESIGN.md §13.1.
	TerraformWorkspace string `yaml:"terraform_workspace"`

	Backend     Backend     `yaml:"backend"`
	Impersonate Impersonate `yaml:"impersonate"`
	Apply       ApplyPolicy `yaml:"apply"`

	// Inherited from Defaults when unset.
	TerraformVersion string        `yaml:"terraform_version"`
	PlanTimeout      Duration      `yaml:"plan_timeout"`
	ApplyTimeout     Duration      `yaml:"apply_timeout"`
	SummaryDetail    SummaryDetail `yaml:"summary_detail"`
	VarFiles         []string      `yaml:"var_files"`
	PlanArgs         []string      `yaml:"plan_args"`
}

// Backend is the GCS remote state location. GCS only in v1.
type Backend struct {
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
}

// Impersonate names the two per-workspace target service accounts.
//
// Neither Cloud Run runtime service account holds permission on a target project. They hold
// roles/iam.serviceAccountTokenCreator on these accounts individually — tf-plan@ on Plan,
// tf-apply@ on Apply, never at project scope. That is the IAM boundary the two-service split
// exists to create.
// Because these fields name credentials, they are the reason config is read from a trusted ref
// rather than the PR's base branch (DESIGN.md §3.1). The hardening step, when merge rights to the
// trusted ref are broader than the blast radius you want, is to stop trusting them from the repo
// at all: key authorization app-side on (repo, branch, workspace) and let the repo declare only
// layout. See DESIGN.md §3.1.5.
type Impersonate struct {
	// Plan is read-only on the target project and read-only on the state bucket.
	// `terraform plan -lock=false` needs no write access to state; see DESIGN.md §4.2.
	Plan string `yaml:"plan"`

	// Apply holds curated write roles on the target project and objectAdmin on the state prefix.
	Apply string `yaml:"apply"`
}

// ApplyPolicy governs what happens after merge.
type ApplyPolicy struct {
	// Enabled false makes this a plan-only workspace.
	Enabled *bool `yaml:"enabled"`

	// ApprovalTimeout bounds how long the worker will poll for environment approval. On expiry
	// the run is abandoned, never applied.
	ApprovalTimeout Duration `yaml:"approval_timeout"`

	// OnStale decides what to do when the saved plan no longer matches reality. See
	// DESIGN.md §6.3 — `fail` is the correct default for anything you would page about.
	OnStale StalePolicy `yaml:"on_stale"`

	// RequireProtectedBase refuses to apply unless Workspace.Branch actually has required
	// reviews, checked via the branch-protection API.
	//
	// This asserts at runtime the thing §3.1's original justification merely assumed — that a
	// base branch is reviewed — and would have caught that bug. It costs the App the
	// `administration: read` permission, a real widening of §7.3's minimum set, so it is
	// per-workspace rather than a global default: worth it for prod-tier workspaces, not for
	// integration ones that are open to developers on purpose.
	RequireProtectedBase bool `yaml:"require_protected_base"`
}

// SummaryDetail controls how much of the plan is rendered into the GitHub check.
type SummaryDetail string

const (
	// SummaryAddresses renders resource address + action + counts only. Default, because a
	// plan file embeds a state snapshot and Terraform's `sensitive` marking does not cover
	// secrets a human pasted into a non-sensitive attribute.
	SummaryAddresses SummaryDetail = "addresses"

	// SummaryFull inlines the diff. Opt in per workspace.
	SummaryFull SummaryDetail = "full"
)

// StalePolicy is the response to a saved plan that Terraform will no longer accept.
type StalePolicy string

const (
	// StaleFail posts a failing check and requires a human to re-request. Default.
	StaleFail StalePolicy = "fail"

	// StaleReplanIfEquivalent is CUT, not deferred, and is retained only so the value is
	// recognized and rejected with an explanation rather than a bare "unknown".
	//
	// It re-planned at the merge commit and applied if a normalized projection of plan.json
	// matched the reviewed one. Two things killed it. The up-to-date requirement in DESIGN.md
	// §4.1 forces the second of two concurrent PRs to update and re-plan, so the case it
	// automated barely occurs; and the normalization needs the provider schema to tell sets from
	// lists, where a false positive applies an unreviewed change to production. Bad trade.
	//
	// The answer for a repo that cannot enforce up-to-dateness is GitHub's merge queue, not a
	// semantic plan-differ. See DESIGN.md §6.3.
	StaleReplanIfEquivalent StalePolicy = "replan_if_equivalent"
)

func (w Workspace) Environment() string {
	return w.Name
}

// Workspace looks up a workspace by name.
func (c *Config) Workspace(name string) (Workspace, bool) {
	for _, w := range c.Workspaces {
		if w.Name == name {
			return w, true
		}
	}
	return Workspace{}, false
}
