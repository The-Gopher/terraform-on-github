package config

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var errNotImplemented = errors.New("not implemented")

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
	// ref := l.Trusted[owner+"/"+repo]           // "" → default branch
	// configSHA, err = l.Contents.ResolveRef(ctx, owner, repo, ref)
	// raw, err := l.Contents.ReadFileAtSHA(ctx, owner, repo, Filename, configSHA)
	// if errors.Is(err, ghapp.ErrNotFound) { return nil, configSHA, ErrNoConfig }
	// cfg, err = Parse(raw); cfg.Normalize(); return cfg, configSHA, cfg.Validate()
	return nil, "", errNotImplemented
}

// LoadAtSHA reads config at an exact commit already known to be the trusted ref's tip at some
// point — for rendering a config diff, or for auditing which version authorized a past run.
//
// Not an authorization path: callers deciding whether a run may proceed use
// LoadFromTrustedRef, which reads the *current* tip. Config is fail-closed on change, so a
// workspace deauthorized between plan and merge must not apply. See apply.Verify.
func (l *Loader) LoadAtSHA(ctx context.Context, owner, repo, sha string) (*Config, error) {
	return nil, errNotImplemented
}

// ErrNoConfig means the repo has no config on its trusted ref — not an error, just an
// un-onboarded repository. Callers should return 200 and do nothing.
var ErrNoConfig = errors.New("repository has no " + Filename + " on its trusted ref")

// Parse decodes YAML with strict field matching, so a typo in a security-relevant key
// ("impersonat:") fails loudly instead of silently defaulting.
func Parse(raw []byte) (*Config, error) {
	// dec := yaml.NewDecoder(bytes.NewReader(raw)); dec.KnownFields(true)
	return nil, errNotImplemented
}

// Normalize applies Defaults to every workspace field left unset, and fills built-in defaults
// where Defaults itself is silent. Runs before Validate.
func (c *Config) Normalize() {
	d := c.Defaults
	if d.PlanTimeout == 0 {
		d.PlanTimeout = 20 * time.Minute
	}
	if d.ApplyTimeout == 0 {
		d.ApplyTimeout = 45 * time.Minute
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
			w.Apply.ApprovalTimeout = 24 * time.Hour
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
		if w.TerraformVersion == "" {
			errs = append(errs, fmt.Errorf("workspaces[%q].terraform_version: required", w.Name))
		}
	}

	// Two workspaces on the same branch with overlapping dirs would both plan the same root
	// module against different backends — almost always a copy-paste mistake, and ambiguous.
	errs = append(errs, c.validateNoOverlap()...)

	return errors.Join(errs...)
}

func (c *Config) validateNoOverlap() []error { return nil }

// validateRepoPath rejects absolute paths, "..", and anything that would resolve outside the
// repository root.
func validateRepoPath(p string) error { return errNotImplemented }

func isServiceAccountEmail(s string) bool { return false }
