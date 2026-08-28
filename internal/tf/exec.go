// Package tf wraps the Terraform CLI.
//
// The wrapper's job is not convenience — it is making sure every invocation runs with the
// flags, credentials and network posture the design assumes, so those decisions cannot be
// forgotten at a call site.
package tf

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/sampleserve/terraform-on-github/internal/config"
)

var errNotImplemented = errors.New("not implemented")

// Errors callers branch on.
var (
	// ErrStalePlan is Terraform refusing a saved plan because state moved under it ("Saved plan
	// is stale"). Expected, not exceptional: two PRs merging into one workspace produces it.
	// Handled by apply.OnStale — see DESIGN.md §6.3.
	ErrStalePlan = errors.New("tf: saved plan is stale")

	// ErrLockHeld is a concurrent state lock.
	ErrLockHeld = errors.New("tf: state lock held")

	// ErrModuleSourceDenied is a module `source` outside the base-branch allowlist, detected
	// before init runs.
	ErrModuleSourceDenied = errors.New("tf: module source not in allowlist")
)

// Runner executes Terraform in one checked-out working directory.
type Runner struct {
	// Binary is the version-pinned terraform executable, baked into the image. Downloading a
	// binary at runtime would need egress the sandbox deliberately does not have, and would put
	// an unverified download inside the plan sandbox.
	Binary string

	// WorkDir is the root module directory: <checkout>/<workspace.Dir>.
	WorkDir string

	// ImpersonateSA is the per-workspace target service account.
	//
	// Set from Workspace.Impersonate.Plan or .Apply. Passed to the provider as
	// impersonate_service_account, so no key material exists anywhere: the runtime SA holds
	// roles/iam.serviceAccountTokenCreator on this one account and mints a short-lived token.
	// tf-plan@ can only ever name a plan SA here, tf-apply@ only an apply SA.
	ImpersonateSA string

	// CLIConfigFile is TF_CLI_CONFIG_FILE, written by WriteCLIConfig. Restricts provider
	// installation to the internal network mirror.
	CLIConfigFile string

	Log io.Writer
}

// InitOpts configures `terraform init`.
type InitOpts struct {
	Backend config.Backend

	// Upgrade must stay false. `-upgrade` would let a plan resolve provider versions different
	// from .terraform.lock.hcl, so the reviewed plan and the applied plan could be built
	// against different provider code.
	Upgrade bool
}

// Init runs `terraform init -input=false -backend-config=...`.
//
// This is the dangerous step: init fetches providers and modules from wherever the config
// points, and module fetches can execute code. Call CheckModuleSources first, and rely on the
// egress allowlist and provider mirror to contain what gets in. See DESIGN.md §8.
func (r *Runner) Init(ctx context.Context, o InitOpts) error { return errNotImplemented }

// SelectWorkspace runs `terraform workspace select`. Only for the TerraformWorkspace escape
// hatch — a GCS backend shares one bucket prefix across terraform workspaces, which weakens
// per-environment state isolation. Directory-per-environment with separate backends is the
// intended model. See DESIGN.md §13.1.
func (r *Runner) SelectWorkspace(ctx context.Context, name string) error { return errNotImplemented }

// PlanResult is the outcome of a plan.
type PlanResult struct {
	HasChanges bool // from -detailed-exitcode: 0 = none, 2 = changes, 1 = error
	PlanFile   string
	JSON       []byte // terraform show -json
	Text       string // terraform show -no-color
	Duration   time.Duration
}

// Plan runs `terraform plan -lock=false -input=false -out=<f> -detailed-exitcode`, then
// `show -json` and `show`.
//
// -lock=false is deliberate. The GCS backend locks by writing a .tflock object into the state
// bucket, so a locking plan would need write access to state — which would give the plan
// identity write access to the one thing it must never touch. The race it admits (an apply
// lands mid-plan) is already covered, because Terraform refuses to apply a saved plan whose
// state serial has moved. The lock would buy nothing the staleness check does not already
// provide, at the cost of the read-only property. See DESIGN.md §4.2.
func (r *Runner) Plan(ctx context.Context, out string, w config.Workspace) (PlanResult, error) {
	return PlanResult{}, errNotImplemented
}

// ApplyResult is the outcome of an apply.
type ApplyResult struct {
	Duration time.Duration
	Log      []byte
}

// Apply runs `terraform apply <planFile>`.
//
// A saved plan file needs no -auto-approve: Terraform applies it non-interactively and, more
// to the point, refuses it outright if the state lineage or serial has changed since the plan
// was created. That refusal is the correctness guarantee the whole design leans on — the
// applied plan is the reviewed plan or nothing happens. Returns ErrStalePlan so the caller can
// route to the OnStale policy.
func (r *Runner) Apply(ctx context.Context, planFile string) (ApplyResult, error) {
	return ApplyResult{}, errNotImplemented
}

// WriteCLIConfig writes a TF_CLI_CONFIG_FILE restricting provider installation to an internal
// network mirror, with no `direct` block:
//
//	provider_installation {
//	  network_mirror { url = "https://tf-mirror.internal.acme.dev/" }
//	}
//
// Providers that are not mirrored cannot be installed at all. That is how the `external`
// provider — whose data source executes an arbitrary local program at plan time — is kept out
// of a plan sandbox holding live credentials.
func WriteCLIConfig(path, mirrorURL string) error { return errNotImplemented }

// CheckModuleSources parses every `module` block under dir and matches each `source` against
// the base-branch allowlist, before init runs.
//
// Terraform has no native module allowlist, and module fetches happen during init, so this has
// to be a pre-pass over the HCL rather than a Terraform setting. Local paths (./ and ../) are
// always permitted; they are already part of the reviewed tree.
func CheckModuleSources(dir string, allow []string) error { return errNotImplemented }

// ProviderLockDigest returns SHA256 of .terraform.lock.hcl, recorded in store.Meta so the
// apply can assert it resolved the same provider versions the plan did.
func ProviderLockDigest(dir string) (string, error) { return "", errNotImplemented }
