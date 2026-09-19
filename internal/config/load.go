package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// nameRE constrains Workspace.Name: it becomes part of several GCS object keys and a GitHub
// check-run name.
var nameRE = regexp.MustCompile(`^[a-z0-9-]{1,48}$`)

// reservedPlanArgs are flags the worker sets itself. A repo that could pass -state or -out
// could redirect the plan artifact or the backend.
var reservedPlanArgs = []string{"-lock", "-out", "-input", "-state", "-var-file", "-chdir"}

// ContentsReader reads a blob from a repository at an exact commit, and resolves a ref to a
// commit. Implemented by ghapp.Client over the contents and commits APIs.
type ContentsReader interface {
	ReadFileAtSHA(ctx context.Context, owner, repo, path, sha string) ([]byte, error)
	ResolveRef(ctx context.Context, owner, repo, ref string) (string, error)
}

// TrustedRefs maps "owner/repo" to the single ref its config is read from.
//
// This map is app-side deployment configuration, not repository content, and that is the whole
// point. The trust anchor cannot be named by the thing it anchors: if a repo's YAML declared its
// own trusted ref, a hostile edit would declare its own branch. So it lives in deploy/, next to
// the IAM bindings that grant the identities the config references — same review surface, same
// change control.
//
// Empty value means "the repository's default branch". Reasonable, and usually right; pinning
// the ref explicitly is stronger, because a repo admin can change the default branch.
type TrustedRefs map[string]string

// Loader fetches and validates the config for a repository.
//
// LoadFromTrustedRef is the only load path, and it does not accept a ref or SHA from the caller
// at all — it derives one from TrustedRefs. That is deliberate: an API that took a ref would let
// a call site pass the PR's base, which is the bug described in DESIGN.md §3.1.1.
type Loader struct {
	Contents ContentsReader
	Trusted  TrustedRefs

	// cache is keyed by (owner, repo, configSHA) — the tuple is immutable, so entries never need
	// invalidating. Keeps a busy repo well inside the 15k/hr installation rate limit.
	// cache map[string]*Config
}

// LoadFromTrustedRef reads Filename at the repo's trusted ref, then normalizes and validates it.
//
// It returns the resolved commit SHA alongside the config so the caller can record it in
// store.Meta — plan and apply must be able to say which config version authorized a run, and a
// moving trusted ref would otherwise make that unanswerable after the fact.
//
// Never reads at the PR's base branch. In a promotion flow — feature → integration → prod —
// `integration` is open to developers by design, so its tip is attacker-controlled: a developer
// merges a workspace entry naming a production apply identity, and the next PR against
// `integration` is planned and applied with it. "Base branch" is a proxy for "reviewed" that only
// holds when the base happens to be protected. See DESIGN.md §3.1.
func (l *Loader) LoadFromTrustedRef(ctx context.Context, owner, repo string) (cfg *Config, configSHA string, err error) {
	// The ref comes from the Loader's own map, never from an argument. An API that accepted a ref
	// would let a call site pass the PR's base branch, which is the bug in DESIGN.md 3.1.1 — and
	// it would be indistinguishable from correct use at the call site.
	ref := l.Trusted[owner+"/"+repo] // "" means the repository's default branch

	configSHA, err = l.Contents.ResolveRef(ctx, owner, repo, ref)
	if err != nil {
		return nil, "", fmt.Errorf("resolving trusted ref %q for %s/%s: %w", refName(ref), owner, repo, err)
	}

	raw, err := l.Contents.ReadFileAtSHA(ctx, owner, repo, Filename, configSHA)
	if err != nil {
		if errors.Is(err, ErrFileNotFound) {
			return nil, configSHA, ErrNoConfig
		}
		return nil, configSHA, fmt.Errorf("reading %s at %s: %w", Filename, short(configSHA), err)
	}

	cfg, err = ParseAndValidate(raw)
	if err != nil {
		// Name the ref in the error. "Your config is invalid" sends people to the file in their
		// branch, which is not the file that was read.
		return nil, configSHA, fmt.Errorf("%s at %s (%s): %w", Filename, refName(ref), short(configSHA), err)
	}
	return cfg, configSHA, nil
}

// ErrFileNotFound is what a ContentsReader returns when the path does not exist at that commit.
// Distinguished from a transport failure because the two mean opposite things: a missing config
// is an un-onboarded repo, a failed read is a retry.
var ErrFileNotFound = errors.New("file not found at commit")

func refName(ref string) string {
	if ref == "" {
		return "the default branch"
	}
	return ref
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// LoadAtSHA reads config at an exact commit already known to be the trusted ref's tip at some
// point — for rendering a config diff, or for auditing which version authorized a past run.
//
// Not an authorization path: callers deciding whether a run may proceed use
// LoadFromTrustedRef, which reads the *current* tip. Config is fail-closed on change, so a
// workspace deauthorized between plan and merge must not apply. See apply.Verify.
func (l *Loader) LoadAtSHA(ctx context.Context, owner, repo, sha string) (*Config, error) {
	raw, err := l.Contents.ReadFileAtSHA(ctx, owner, repo, Filename, sha)
	if err != nil {
		if errors.Is(err, ErrFileNotFound) {
			return nil, ErrNoConfig
		}
		return nil, fmt.Errorf("reading %s at %s: %w", Filename, short(sha), err)
	}
	return ParseAndValidate(raw)
}

// ErrNoConfig means the repo has no config on its trusted ref — not an error, just an
// un-onboarded repository. Callers should return 200 and do nothing.
var ErrNoConfig = errors.New("repository has no " + Filename + " on its trusted ref")

// LoadFromFile reads and validates a config file from the local filesystem.
func LoadFromFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseAndValidate(raw)
}

// Parse decodes YAML with strict field matching, so a typo in a security-relevant key
// ("impersonat:") fails loudly instead of silently defaulting.
//
// Strictness is the point. A misspelled `impersonate` would otherwise leave the field empty and
// the workspace would look configured; a misspelled `require_protected_base` would silently mean
// false. Both fail closed only if the decoder refuses unknown keys.
func Parse(raw []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", Filename, err)
	}
	// A second document would be ambiguous about which one governs.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("%s: expected a single YAML document", Filename)
	}
	return &c, nil
}

// ParseAndValidate is Parse + Normalize + Validate, which is the only sequence a caller should
// ever want: an un-normalized config validates against unset defaults, and an unvalidated one is
// a mapping nobody checked.
func ParseAndValidate(raw []byte) (*Config, error) {
	c, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Normalize applies Defaults to every workspace field left unset, and fills built-in defaults
// where Defaults itself is silent. Runs before Validate.
func (c *Config) Normalize() {
	d := c.Defaults
	if d.PlanTimeout == 0 {
		d.PlanTimeout = Duration(20 * time.Minute)
	}
	if d.ApplyTimeout == 0 {
		d.ApplyTimeout = Duration(45 * time.Minute)
	}
	if d.SummaryDetail == "" {
		d.SummaryDetail = SummaryAddresses
	}

	for i := range c.Workspaces {
		w := &c.Workspaces[i]
		if w.TerraformVersion == "" {
			w.TerraformVersion = d.TerraformVersion
		}
		if w.PlanTimeout == 0 {
			w.PlanTimeout = d.PlanTimeout
		}
		if w.ApplyTimeout == 0 {
			w.ApplyTimeout = d.ApplyTimeout
		}
		if w.SummaryDetail == "" {
			w.SummaryDetail = d.SummaryDetail
		}
		if len(w.VarFiles) == 0 {
			w.VarFiles = d.VarFiles
		}
		if len(w.PlanArgs) == 0 {
			w.PlanArgs = d.PlanArgs
		}
		if w.Apply.OnStale == "" {
			w.Apply.OnStale = StaleFail
		}
		if w.Apply.ApprovalTimeout == 0 {
			w.Apply.ApprovalTimeout = Duration(24 * time.Hour)
		}
	}
}

// Validate fails closed: a repo whose config does not validate gets no plans at all, rather
// than plans against a guessed mapping. The caller surfaces the error as a single failing
// check run on every PR against that branch, so a broken config is impossible to miss.
//
// Errors accumulate — reporting one problem per push turns onboarding into twenty rounds.
func (c *Config) Validate() error {
	var errs []error

	if c.Version != SchemaVersion {
		errs = append(errs, fmt.Errorf("version: got %d, want %d", c.Version, SchemaVersion))
	}
	if len(c.Workspaces) == 0 {
		errs = append(errs, errors.New("workspaces: at least one required"))
	}

	seen := map[string]bool{}
	for _, w := range c.Workspaces {
		if !nameRE.MatchString(w.Name) {
			errs = append(errs, fmt.Errorf("workspaces[%q].name: must match %s", w.Name, nameRE))
		}
		if seen[w.Name] {
			errs = append(errs, fmt.Errorf("workspaces[%q]: duplicate name", w.Name))
		}
		seen[w.Name] = true

		// A workspace whose Dir escapes the repo could plan a root module outside review.
		if err := validateRepoPath(w.Dir); err != nil {
			errs = append(errs, fmt.Errorf("workspaces[%q].dir: %w", w.Name, err))
		}
		if w.Branch == "" {
			errs = append(errs, fmt.Errorf("workspaces[%q].branch: required", w.Name))
		}
		if w.Backend.Bucket == "" || w.Backend.Prefix == "" {
			errs = append(errs, fmt.Errorf("workspaces[%q].backend: bucket and prefix required", w.Name))
		}
		for field, sa := range map[string]string{"plan": w.Impersonate.Plan, "apply": w.Impersonate.Apply} {
			if !isServiceAccountEmail(sa) {
				errs = append(errs, fmt.Errorf("workspaces[%q].impersonate.%s: not a service account email", w.Name, field))
			}
		}
		for _, a := range w.PlanArgs {
			for _, r := range reservedPlanArgs {
				if a == r || strings.HasPrefix(a, r+"=") {
					errs = append(errs, fmt.Errorf("workspaces[%q].plan_args: %s is reserved", w.Name, r))
				}
			}
		}
		switch w.SummaryDetail {
		case SummaryAddresses, SummaryFull:
		default:
			errs = append(errs, fmt.Errorf("workspaces[%q].summary_detail: unknown %q", w.Name, w.SummaryDetail))
		}
		switch w.Apply.OnStale {
		case StaleFail:
		case StaleReplanIfEquivalent:
			// Recognized so the error can explain rather than say "unknown". See the constant.
			errs = append(errs, fmt.Errorf("workspaces[%q].apply.on_stale: replan_if_equivalent was removed; use fail, and enable GitHub's merge queue if the rebase cost bites (DESIGN.md 6.3)", w.Name))
		default:
			errs = append(errs, fmt.Errorf("workspaces[%q].apply.on_stale: unknown %q", w.Name, w.Apply.OnStale))
		}
		// TerraformVersion must correspond to a runner image tag we publish; downloading a
		// version at runtime would need egress we deliberately do not have. See DESIGN.md §11.
		//
		// And it must be an exact version, not a range. A saved plan is version-specific: the
		// apply has to assert it is running the same Terraform the plan was built with, and
		// "~> 1.9" makes that assertion unstateable. docs/CONFIG.md says exact; this enforces it.
		switch {
		case w.TerraformVersion == "":
			errs = append(errs, fmt.Errorf("workspaces[%q].terraform_version: required", w.Name))
		case !exactVersionRE.MatchString(w.TerraformVersion):
			errs = append(errs, fmt.Errorf(
				"workspaces[%q].terraform_version: %q is not an exact version; ranges cannot be pinned to a runner image or asserted at apply time (want e.g. 1.9.8)",
				w.Name, w.TerraformVersion))
		}
	}

	// Check for duplicates of same repo-dir and  same terraform workspace.
	errs = append(errs, c.validateNoOverlap()...)

	return errors.Join(errs...)
}

func (c *Config) validateNoOverlap() []error {
	var errs []error
	for i, a := range c.Workspaces {
		for _, b := range c.Workspaces[i+1:] {
			if dirsOverlap(a.Dir, b.Dir) && a.TerraformWorkspace == b.TerraformWorkspace {
				errs = append(errs, fmt.Errorf(
					"duplicate workspaces (%q) for dir (%q)",
					a.TerraformWorkspace, a.Dir))
			}
		}
	}
	return errs
}

// dirsOverlap compares on a path boundary, so "envs/prod" does not overlap "envs/production".
func dirsOverlap(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// validateRepoPath rejects absolute paths, "..", and anything that would resolve outside the
// repository root.
//
// Checked on the cleaned path rather than the raw string: "envs/../../etc" contains no leading
// ".." but still escapes, and rejecting only the literal prefix would miss it.
func validateRepoPath(p string) error {
	if p == "" {
		return errors.New("required")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("must be repo-relative, got %q", p)
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("contains a NUL byte")
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("escapes the repository root: %q", p)
	}
	return nil
}

// saRE matches a GCP service-account email. Deliberately strict: this value names a credential
// the runner is about to assume, so "close enough" is the wrong bar. Anything that is not
// obviously one identity should fail validation rather than be passed to gcloud.
// exactVersionRE requires major.minor.patch and nothing else — no ranges, no operators.
var exactVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

var saRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{4,28}[a-z0-9])@[a-z][-a-z0-9]{4,28}[a-z0-9]\.iam\.gserviceaccount\.com$`)

func isServiceAccountEmail(s string) bool { return saRE.MatchString(s) }
