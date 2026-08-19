// Package config models .terraform-on-github.yaml — a repo's declaration of which base
// branches control which Terraform root modules, and which identities each may assume.
//
// The file is always loaded from the tip of a PR's base branch (see Loader). Loading it
// from the PR head would let any pull request repoint `Impersonate.Apply` at a privileged
// service account, so that rule is a security boundary, not a convention.
package config

import "time"

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
	PlanTimeout      time.Duration `yaml:"plan_timeout"`
	ApplyTimeout     time.Duration `yaml:"apply_timeout"`
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
	PlanTimeout      time.Duration `yaml:"plan_timeout"`
	ApplyTimeout     time.Duration `yaml:"apply_timeout"`
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

	// Environment is the GitHub Environment used as the approval gate. Protection rules
	// (required reviewers, wait timer) are configured in GitHub, deliberately not here, so the
	// audit trail lives with the repo. Defaults to Name.
	Environment string `yaml:"environment"`

	// ApprovalTimeout bounds how long the worker will poll for environment approval. On expiry
	// the run is abandoned, never applied.
	ApprovalTimeout time.Duration `yaml:"approval_timeout"`

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

// ApplyEnabled reports whether merges to this workspace's branch should apply.
func (w Workspace) ApplyEnabled() bool {
	return w.Apply.Enabled == nil || *w.Apply.Enabled
}

// Environment returns the GitHub Environment name, defaulting to the workspace name.
func (w Workspace) Environment() string {
	if w.Apply.Environment != "" {
		return w.Apply.Environment
	}
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
