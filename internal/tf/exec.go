// Package tf wraps the Terraform CLI.
//
// The wrapper's job is not convenience — it is making sure every invocation runs with the
// flags, credentials and network posture the design assumes, so those decisions cannot be
// forgotten at a call site.
package tf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sampleserve/terraform-on-github/internal/config"
)

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
func (r *Runner) Init(ctx context.Context, o InitOpts) error {
	if o.Upgrade {
		// Refused rather than honoured. -upgrade would let a plan resolve provider versions
		// different from .terraform.lock.hcl, so the reviewed plan and the applied plan could be
		// built against different provider code. There is no legitimate caller.
		return errors.New("tf: init -upgrade is never permitted; it would decouple the plan from .terraform.lock.hcl")
	}
	args := []string{"init", "-input=false", "-no-color", "-reconfigure"}
	if o.Backend.Bucket != "" {
		args = append(args, "-backend-config=bucket="+o.Backend.Bucket)
	}
	if o.Backend.Prefix != "" {
		args = append(args, "-backend-config=prefix="+o.Backend.Prefix)
	}
	_, err := r.run(ctx, 10*time.Minute, args, 0)
	return err
}

// SelectWorkspace runs `terraform workspace select`. Only for the TerraformWorkspace escape
// hatch — a GCS backend shares one bucket prefix across terraform workspaces, which weakens
// per-environment state isolation. Directory-per-environment with separate backends is the
// intended model. See DESIGN.md §13.1.
func (r *Runner) SelectWorkspace(ctx context.Context, name string) error {
	_, err := r.run(ctx, 2*time.Minute, []string{"workspace", "select", "-or-create", "-no-color", name}, 0)
	return err
}

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
	args := []string{"plan", "-input=false", "-no-color", "-lock=false", "-detailed-exitcode", "-out=" + out}
	for _, vf := range w.VarFiles {
		args = append(args, "-var-file="+vf)
	}
	args = append(args, w.PlanArgs...)

	started := time.Now()
	// 2 is not a failure: -detailed-exitcode reports 0 = no changes, 2 = changes, 1 = error.
	code, _, err := r.runAllowing(ctx, w.PlanTimeout.Duration(), args, 0, 2)
	if err != nil {
		return PlanResult{}, err
	}
	res := PlanResult{HasChanges: code == 2, PlanFile: out, Duration: time.Since(started)}

	if res.JSON, err = r.ShowJSON(ctx, out); err != nil {
		return PlanResult{}, err
	}
	if res.Text, err = r.ShowText(ctx, out); err != nil {
		return PlanResult{}, err
	}
	return res, nil
}

// ShowJSON runs `terraform show -json` over a saved plan. Captured separately from the plan run
// because its stdout is the artifact, not log output.
func (r *Runner) ShowJSON(ctx context.Context, planFile string) ([]byte, error) {
	return r.capture(ctx, 5*time.Minute, "show", "-json", planFile)
}

// ShowText runs `terraform show` over a saved plan, for the human diff.
func (r *Runner) ShowText(ctx context.Context, planFile string) (string, error) {
	out, err := r.capture(ctx, 5*time.Minute, "show", "-no-color", planFile)
	return string(out), err
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
	started := time.Now()
	// No -auto-approve: a saved plan file is applied non-interactively by construction.
	_, out, err := r.runAllowing(ctx, 0, []string{"apply", "-input=false", "-no-color", planFile}, 0)
	res := ApplyResult{Duration: time.Since(started), Log: out}
	if err != nil {
		switch {
		case looksStale(out):
			return res, fmt.Errorf("%w: %s", ErrStalePlan,
				"state moved since the plan was created, so the reviewed plan is no longer applicable; re-plan the merge and review again")
		case looksLocked(out):
			return res, fmt.Errorf("%w: another operation holds the state lock", ErrLockHeld)
		}
		return res, err
	}
	return res, nil
}

func looksStale(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "saved plan is stale") ||
		strings.Contains(s, "saved plan does not match") ||
		strings.Contains(s, "plan is stale")
}

func looksLocked(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "error acquiring the state lock") || strings.Contains(s, "state blob is already locked")
}

// ---------------------------------------------------------------------------
// Process plumbing
// ---------------------------------------------------------------------------

// env builds the environment every invocation runs with.
func (r *Runner) env() []string {
	env := append(os.Environ(),
		"TF_IN_AUTOMATION=1",
		"TF_INPUT=0",
		"CHECKPOINT_DISABLE=1",
	)
	if r.CLIConfigFile != "" {
		env = append(env, "TF_CLI_CONFIG_FILE="+r.CLIConfigFile)
	}
	if r.ImpersonateSA != "" {
		// Honoured by both the google provider and the gcs backend, so one variable covers state
		// access and API calls without editing any HCL in the target repository — and no key
		// material exists anywhere. The runtime identity holds tokenCreator on this one account.
		env = append(env, "GOOGLE_IMPERSONATE_SERVICE_ACCOUNT="+r.ImpersonateSA)
	}
	return env
}

func (r *Runner) binary() string {
	if r.Binary != "" {
		return r.Binary
	}
	return "terraform"
}

func (r *Runner) run(ctx context.Context, timeout time.Duration, args []string, allow ...int) ([]byte, error) {
	_, out, err := r.runAllowing(ctx, timeout, args, allow...)
	return out, err
}

// runAllowing streams combined output to r.Log and returns the exit code, which the caller needs
// for -detailed-exitcode. A code outside `allow` is an error.
func (r *Runner) runAllowing(ctx context.Context, timeout time.Duration, args []string, allow ...int) (int, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, r.binary(), args...)
	cmd.Dir = r.WorkDir
	cmd.Env = r.env()

	var buf bytes.Buffer
	sink := io.Writer(&buf)
	if r.Log != nil {
		fmt.Fprintf(r.Log, "\n$ terraform %s\n", strings.Join(args, " "))
		sink = io.MultiWriter(&buf, r.Log)
	}
	cmd.Stdout = sink
	cmd.Stderr = sink

	err := cmd.Run()
	code := cmd.ProcessState.ExitCode()

	if ctx.Err() != nil && timeout > 0 {
		return code, buf.Bytes(), fmt.Errorf("terraform %s exceeded its %s timeout", args[0], timeout)
	}
	for _, ok := range allow {
		if code == ok {
			return code, buf.Bytes(), nil
		}
	}
	if err == nil {
		err = fmt.Errorf("exit %d", code)
	}
	return code, buf.Bytes(), fmt.Errorf("terraform %s failed (%w)\n%s", args[0], err, tail(buf.Bytes(), 40))
}

// capture runs a command whose stdout is data rather than log output.
func (r *Runner) capture(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.binary(), args...)
	cmd.Dir = r.WorkDir
	cmd.Env = r.env()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("terraform %s failed: %w\n%s", args[0], err, tail(stderr.Bytes(), 20))
	}
	return stdout.Bytes(), nil
}

func tail(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Version reports the version of the terraform binary this Runner will invoke.
//
// Worth asserting rather than assuming: a saved plan is version-specific, so an apply run by a
// different binary than the plan is not applying the reviewed plan.
func Version(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		binary = "terraform"
	}
	out, err := exec.CommandContext(ctx, binary, "version", "-json").Output()
	if err != nil {
		return "", fmt.Errorf("%s version -json: %w", binary, err)
	}
	var v struct {
		TerraformVersion string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", fmt.Errorf("%s version -json: %w", binary, err)
	}
	if v.TerraformVersion == "" {
		return "", fmt.Errorf("%s version -json: no terraform_version in output", binary)
	}
	return v.TerraformVersion, nil
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
func WriteCLIConfig(path, mirrorURL string) error {
	if mirrorURL == "" {
		return errors.New("tf: mirror URL required; a provider_installation block with no mirror would permit direct installs")
	}
	body := fmt.Sprintf("provider_installation {\n  network_mirror {\n    url = %q\n  }\n}\n", mirrorURL)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

// CheckModuleSources, CollectModuleSources and CheckModuleRefs live in modulesource.go — they
// are a pre-pass over the HCL rather than a Terraform setting, because Terraform has no native
// module allowlist and module fetches happen during init.

// ProviderLockDigest returns SHA256 of .terraform.lock.hcl, recorded in store.Meta so the
// apply can assert it resolved the same provider versions the plan did.
// An empty digest is a real answer, not a failure: it means the repo pins nothing, and an apply
// can then only assert that it also found nothing.
func ProviderLockDigest(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".terraform.lock.hcl"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
