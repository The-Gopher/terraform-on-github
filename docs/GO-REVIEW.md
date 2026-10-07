# Go Code Review — terraform-on-github

Reviewed at `c643470`. Scope: `cmd/tfgh` and everything under `internal/` (about 2,700 lines without tests).

How it was checked: the code was read file by file, and these were run: `gofmt -l`, `go vet`, `go test ./...`,
`go mod tidy -diff` and `golangci-lint run`.

**Overall:** The domain code is well thought out. The comments explain *why* (security boundaries,
crash windows), errors are wrapped with `%w`, sentinel errors are used, the important logic sits behind small
interfaces with in-memory fakes, and the store ordering tests are good. Most of the issues are at the edges:
the CLI's `main.go`, the GitHub adapter, and a few spots where the code and DESIGN.md have drifted apart.
The review also turned up **several real bugs** (§1). Fix those before the style items.

---

## 1. Bugs found during review (fix first)

| # | Where | Problem | Suggested fix |
|---|---|---|---|
| B1 | `store/store.go:100` `PendingKey` | Format is `pending-apply/%s/%s/%d/%s/%s__%s`, filled with `owner, repo, pr, baseSHA, headSHA, headSHA`. DESIGN §5.4 says `pending-apply/<owner>/<repo>/<pr>/<workspace>/<base>__<head>`. **The workspace is missing.** When one PR touches two workspaces, the second marker `Create` collides and is treated as "idempotent", so the second workspace never gets its own marker and `/reconcile` never sees it. `stage_test.go:312` asserts the buggy `b2__b2` output. | Add a `workspace` parameter, use `owner, repo, pr, workspace, base, head`, and fix the test. |
| B2 | `cmd/tfgh/main.go:368` (`runApply`) | `baseSHA` is computed as `ResolveRef(pull.BaseRef)` **after the merge**. By then the base branch tip is the merge commit or later, not the base the plan was keyed on. So the `meta.json` lookup misses, and even if it hit, `parents[0] != baseSHA` would always fail. `verify_test.go` passes `baseSHA` in directly, so this path is never tested. | Derive the planned base from `merge_commit^1` (or from `pull.BaseSHA`), and do it inside `Verifier`, not in `main`. Add a test that drives the derivation. |
| B3 | `ghapp/client.go:29-49` `GetContents` | go-github returns an error for a 404, so the `resp.StatusCode == 404` branch can never run. Even if it did, it returns `fmt.Errorf("file not found")` instead of `config.ErrFileNotFound`. As a result, **`config.ErrNoConfig` can never be returned by the real client**: an un-onboarded repo looks like a transport failure. There is also a duplicate `if err != nil`, and a `nil` `file` (when the path is a directory) panics in `GetContent()`. | Check for `*github.ErrorResponse` with status 404 (`errors.As`) and return `config.ErrFileNotFound`. Handle `file == nil`. |
| B4 | `scope/scope.go:135` `pathUnderDir` | A workspace with `dir: .` (root module) gives the prefix `./`, which GitHub file paths never have, so a root-module workspace is **never in scope** (unless something in `watch` matches). | Return `true` when `path.Clean(dir) == "."`. Add a test. |
| B5 | `tf/executor.go:89` | `plan_args` are passed as `tfexec.Var(arg)`, which produces `-var <arg>`. CONFIG.md describes them as "extra `terraform plan` flags", so `-parallelism=5` becomes `-var -parallelism=5`. | Either rename the field to `vars` or pass the arguments through as raw flags. tfexec has no raw-arg option, so this may need a check against a whitelist of flags. |
| B6 | `apply/verify.go:196` | "Workspace no longer exists on the trusted ref" returns `ErrTreeMismatch`. Callers that branch on the sentinel will report the wrong cause. | Add `ErrWorkspaceDeauthorized`. |
| B7 | `apply/verify.go:233` `WouldApply` | Prints `v.Meta.MergeCommitSHA`, but `Stage` never sets that field (it can't be known at plan time), so the "merge" line is always blank. | Print the merge SHA from the verification result (store it on `Verdict`), and drop the field from `Meta` or document it as reserved. |
| B8 | `config/load.go:162` `Parse` | `if err := dec.Decode(&extra); err == nil` only rejects a *valid* second document. A malformed second document returns a non-EOF error and is silently accepted. | `if err := dec.Decode(&extra); !errors.Is(err, io.EOF) { … }` |
| B9 | `config/load.go:261` `Validate` | Ranges over a `map[string]string` to check `plan`/`apply`, so the order of errors is random from run to run (flaky messages and flaky tests). | Range over a slice of `{field, value}` pairs. |
| B10 | `store/stage.go` | `ProviderLockSHA256` and `ConfigSHA` are never filled in by any caller, so signed `meta.json` always records them as empty, even though `LoadFromTrustedRef`'s doc says the config SHA exists *so it can be recorded in Meta*. | Get `configSHA` through `config.Loader` on the plan side as well (see §3.2), and hash `.terraform.lock.hcl`. |

Lower-severity correctness notes:

- `plan.Run` returns an error for every failed plan, and `runPlan` then calls `fatalf` on any error. So
  `res.Failed()` is **never true** in practice, the `failed` flag is dead, and one failing workspace stops all
  the remaining ones. The comment "A timeout is not a plan failure" sits above a `fatalf` that treats both the
  same way.
- `store.countChanges`: the case `a.Create() && a.Delete()` can never be true, because tfjson's `Create()` and
  `Delete()` each require a single-element action list. Replacements are caught by `a.Replace()` further down.
  The same applies to `tf.FormatAction`: the `CreateBeforeDestroy`/`DestroyBeforeCreate` branches come after
  `Replace()` and can never be reached.
- `store.ensureRun` returns `1` as the generation after a create. Real GCS generations are microsecond
  timestamps. The value is thrown away (`_`) today, but the next caller that trusts it will run a CAS against
  the wrong generation. Either return the real generation (`w.Attrs().Generation`) or return nothing.
- The `Stage` doc comment says a crash between steps 1 and 3 leaves the run object in "planning". But the run
  object is only created *in* step 3 (`flipRunPlanned` → `ensureRun`), so that crash leaves no run object at
  all. Either create the run object first (claim, then artifacts, then flip) or correct the comment and
  `TestStage_CrashWindows`.
- The plan file is always written to `<workDir>/tfplan`. Two workspaces that share a `dir` (the
  `terraform_workspace` escape hatch) overwrite each other's plan file and `.terraform/` directory in the same
  invocation.
- `TestPlan_Timeout` takes about 30s instead of about 2s. The fake `sleep 30` grandchild keeps the stdout pipe
  open, so `Wait` blocks after the context kills the shell. A real `terraform` with provider child processes
  can hang the same way after a timeout. Worth confirming. Go's fix is `exec.Cmd.WaitDelay`, which tfexec does
  not expose, so you may need a process-group kill or a wrapper.

---

## 2. Making it more canonical Go

### 2.1 `cmd/tfgh/main.go`: the `run() error` pattern

The CLI is one 575-line file. It calls `os.Exit` through `fatalf` from deep inside helpers (`setup`,
`loadConfig`, `printJSON`).

- **Deferred cleanups don't run.** `runApply` does `defer gcs.Close()` and then `fatalf`. `os.Exit` skips the
  defers.
- None of it can be unit-tested (`cmd/tfgh` has no tests).

The usual shape:

```go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(exitCode(err)) // 2 for usage errors, 1 otherwise
	}
}

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) error { … }
```

Related items:

- **Ctrl-C:** use `signal.NotifyContext` so that Ctrl-C cancels `terraform plan` cleanly. Today
  `context.Background()` is used everywhere, including inside `setup` (`ghapp.NewClient(context.Background(), …)`).
- **PR flag:** `--pr` is a string flag parsed with `fmt.Sscanf` that **silently becomes 0** on bad input. Use
  `fs.Int("pr", 0, …)` and reject values `<= 0`.
- **Usage errors:** they are handled three ways: `fs.Usage(); os.Exit(1)`, `fatalf(...)`, and exit 2 in
  `main`. Pick one. The convention is exit code 2 for usage errors.
- **Shared flags:** `--repo`, `--pr`, `--config-ref`, `--config` and `--json` are declared again in every
  subcommand. Put them in a small `commonFlags` struct with a `register(fs *flag.FlagSet)` method.
- **Boolean mode argument:** `loadConfig(..., check bool)`, called with the `validate`/`normalizeOnly`
  constants, is a boolean-parameter smell. Use two functions instead, or reuse `config.ParseAndValidate` and
  `config.Parse`+`Normalize` directly.
- **Splitting `owner/repo`:** `strings.Split(repo, "/")` plus a length check is cleaner as
  `owner, name, ok := strings.Cut(repo, "/")`, then reject an empty `owner` or `name`, or a `name` that
  contains `/`.
- **Output streams:** write to an injected `io.Writer`, not straight to `fmt.Printf`, so output can be tested
  with golden files.
- **Split the file:** `config.go`, `scope.go`, `plan.go` and `apply.go` within `package main`, one per verb.
  (Or use cobra/ff if more verbs are coming. The standard library is fine at four.)
- **Move `kmsVerifier` (about 50 lines of crypto) into `internal/store`** next to `KMSSigner`, as
  `store.KMSVerifier`. That keeps the signer and the verifier side by side where they can be tested together,
  and lets `main` stick to wiring. Cache the public key: it never changes for a given key *version*.
- **Clients created per workspace:** `stageToGCS` builds a new GCS client **and** a new KMS client for every
  workspace in the loop, and calls `GetCommitTree` for the same head SHA every time. Create the clients and the
  head tree once, before the loop.
- **Long parameter list:** `stageToGCS` takes 13 parameters, 9 of them `string`, so arguments are easy to mix
  up. It mostly re-packs `store.StageInput`. Build the `StageInput` in the caller instead.

### 2.2 Package and identifier naming

- **Stutter:** `scope.ScopeResults` → `scope.Results`. (There is already a `scope.Result`, so consider
  `scope.Match` for the single entry and `scope.Result` for the whole result.)
- **Missing package docs:** `scope`, `ghapp` and `tf` have no package doc comment. `tf`'s doc lives in
  `summary.go` ("Package tf — plan summary rendering"), which describes one file, not the package. Add a
  `doc.go` or put it in `executor.go`.
- **Missing doc comments on exported identifiers:** `scope.Status`, `scope.Result`, `scope.GitHubClient`,
  `scope.NewScoper`, `Scoper.Scope`, `ghapp.NewClient`, `ghapp.Client.GetContents/ResolveRef/...`,
  `ghapp.Comparison`, `config.Workspace.Environment`, and the `tf.tfexecExecutor` methods.
- **Comments that don't match their target:**
  - `plan.WorkspaceResult.Failed`'s comment starts with "Error distinguishes…".
  - `ghapp.ResolveBaseRef`'s comment says "BaseRefResolution…".
  - `config/load.go:377`: the `saRE` comment sits above `exactVersionRE`, and the two declarations are in the
    wrong order.
- **Error message style:** `ghapp` uses `"failed to get PR %d: %w"`. Go convention is to leave out "failed
  to" (every error is a failure), so the chain reads `get PR 12: …`. The rest of the codebase already does this
  (`"resolve base ref: %w"`).
- **Import alias:** `tf/executor.go` imports `"github.com/hashicorp/terraform-json"` without an alias, while
  `plan`, `store` and `summary.go` alias it as `tfjson`. Alias it everywhere: the import path and the package
  name differ.

### 2.3 Accept interfaces, return structs

- `tf.NewExecutor` returns the `Executor` interface and hides `*tfexecExecutor`. Return a concrete exported
  `*TFExec` (or similar). Callers already depend on the interface wherever they need to.
- On the positive side: `scope.GitHubClient`, `apply.GitHubClient` and `config.ContentsReader` are defined by
  the consumer, which is idiomatic. Keep that.

### 2.4 Methods that should be functions

`Scoper.branchMatches`, `pathUnderDir`, `pathMatchesGlob` and `deriveStatus` never use `s`. Make them plain
functions. `deriveStatus(cmp)` is also called once per workspace inside the loop even though the result never
changes. Compute it once.

### 2.5 Use the standard library

| Hand-rolled | Use instead |
|---|---|
| `pathUnderDir`'s manual slice compare | `strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")` |
| `MapBucket.List`'s `name[:len(prefix)] == prefix` | `strings.HasPrefix` |
| `store.readAll(r)` wrapper around `io.ReadAll` | call `io.ReadAll` directly |
| `sort.Strings` | `slices.Sort` (Go 1.21+; the module targets 1.26) |
| collect map keys, then sort (`stage.go:126`) | `slices.Sorted(maps.Keys(m))` |
| `apply.first(parents)` | fine as is, or use `parents[0]` after a length check you already do |
| `WouldApply` building `counts` with `+=` | `strings.Join` over a slice, or `strings.Builder` |
| scope's `inScope` loop in `runPlan` | `slices.ContainsFunc` |
| `fmt.Errorf("contains a NUL byte")` with no format verbs | `errors.New` |

### 2.6 YAML: use the yaml.v3 interface

`config.Duration.UnmarshalYAML(unmarshal func(any) error)` is the **yaml.v2** signature, which v3 only keeps
for compatibility. The v3 form is:

```go
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil { … }
	…
}
```

Better still, implement `encoding.TextUnmarshaler`/`TextMarshaler`. yaml.v3 honors them, and so do JSON and
`flag.TextVar`, so one implementation covers all three. `scope --json` currently prints `PlanTimeout` as
nanoseconds, because `Duration` has no `MarshalJSON`/`MarshalText`.

### 2.7 Errors

- **Return the verdict *or* an error, not both.** `apply.Verifier.Verify` returns
  `(Verdict{Reason: …}, ErrX)`, and `main` has to inspect `verdict.Reason` when `err != nil`. In idiomatic Go a
  non-nil error makes the other return values meaningless. Use a typed error instead:

  ```go
  type RejectedError struct {
  	Reason string // operator-facing, carries the § reference
  	Err    error  // ErrNotMerged, ErrBaseMoved, …
  }
  func (e *RejectedError) Error() string { return e.Reason }
  func (e *RejectedError) Unwrap() error { return e.Err }
  ```

  Callers still use `errors.Is(err, apply.ErrBaseMoved)`, and `errors.As` gets them the reason. `Verdict` then
  only describes success.
- **Unused sentinel:** `apply.ErrStale` is declared and never returned. Either implement the mapping or remove
  it.
- **Exported error constructor:** `plan.TimeoutError(timeout) error` is an exported function that builds an
  error. Since `ErrPlanTimeout` is already wrapped by the executor (`executor.go:120`), `plan.Run` wraps it a
  *second* time with a different message. Wrap once, in the executor. That also makes
  `PlanResult.TimedOut` unnecessary: callers can use `errors.Is(err, tf.ErrPlanTimeout)`.
- **Function too long:** `apply.Verify` has 7 positional parameters, 4 of them adjacent `string`s
  (`workspace, baseSHA, headSHA`). Use a `Request` struct.

### 2.8 Formatting and tooling

- **`gofmt -l` reports 8 files** (`plan`, `tf/*`, several tests). Alignment in `tf.PlanOptions` and
  `tf.ValidatePlanArgs`, and missing final newlines. golangci-lint still reports **0 issues**, because
  `.golangci.yml` is just `version: "2"`, which in v2 enables no formatters and only the `standard` linters.
  Suggested config:

  ```yaml
  version: "2"
  linters:
    enable: [errorlint, gocritic, misspell, revive, unparam, unused, bodyclose, nilerr, gosec]
  formatters:
    enable: [gofmt, goimports]
  ```

  `revive`'s `exported` rule would have caught the missing docs, `unparam` the `applyTimeout` parameter, and
  `unused` the dead functions in §4.
- **`//nolint` spacing:** `// nolint:errcheck` on `printConfig` has a space, so golangci-lint **does not
  recognize it**. The directive is `//nolint:errcheck`. Better to drop it altogether: write into a
  `bytes.Buffer`/`bufio.Writer` and check the error once at the end, or give `printConfig` an `error` return.
- **`go.mod` is not tidy:** `cloud.google.com/go/kms`, `cloud.google.com/go/storage` and `github.com/gobwas/glob`
  are imported directly but listed as `// indirect`. Run `go mod tidy`, and add `go mod tidy -diff` to the
  pre-commit hook or CI.
- **Pre-commit hook:** it only runs golangci-lint. Add `go vet ./...` and `go test -race -short ./...`. Mark
  `TestPlan_Timeout` with `testing.Short()` until the 30s hang is fixed.
- **No CI workflow:** there is no `.github/workflows/`. For a project named terraform-on-*github*, a lint plus
  test workflow is cheap.

---

## 3. Code structure

### 3.1 Dependency direction

Current import graph (arrow = "imports"):

```
cmd/tfgh ─► apply, config, ghapp, plan, scope, store, tf
ghapp    ─► apply          ◄── adapter depends on a domain package
store    ─► plan           ◄── persistence depends on orchestration
plan     ─► config, tf
tf       ─► config         (summary.go only, for SummaryDetail)
scope    ─► config, ghapp  (for ghapp.Comparison)
apply    ─► config, store
```

Problems:

1. **`ghapp → apply`.** The GitHub adapter imports `apply` just to return `apply.PullRequest`. When the M6
   worker or `scope` needs PR state, it pulls in the apply domain. Have `ghapp` return its own `ghapp.PullRequest`
   (or `*github.PullRequest`) and convert in a small adapter, either in `cmd/tfgh` or in a function in `apply`.
   `ApplyPullRequestState` and `ResolveBaseRef` are unused leftovers of this coupling (§4).
2. **`store → plan`.** `store.Stage` takes a whole `plan.WorkspaceResult` but only uses `PlanJSON`, `PlanRaw`,
   `PlanFile` and `HasChanges`. Persistence should not depend on orchestration. Move those four fields into
   `StageInput` (or a `store.Artifacts` struct), and let `plan`/`main` fill them in. Then `store` depends on
   nothing internal except tfjson.
3. **`scope → ghapp`** for `Comparison`. The `scope.GitHubClient` interface leaks a concrete adapter type, and
   leaks `*github.CommitFile` / `*github.PullRequest` from go-github v60. Define
   `scope.Comparison{Files []string; AheadBy, BehindBy int}` and a `scope.PR{BaseRef, HeadSHA string}` so that
   scope's tests don't need go-github, and a go-github major version bump only touches `ghapp`.
4. **`tf → config`** only so `BuildPlanSummary` can switch on `config.SummaryDetail`. Either move summary
   rendering into `plan`, or take a `full bool`.

Target shape: `config`, `tf`, `store` and `scope` are leaf packages, `plan` and `apply` orchestrate, `ghapp`
adapts, and `cmd/tfgh` wires everything together.

### 3.2 Two config-loading paths

`config.Loader`'s doc says **"LoadFromTrustedRef is the only load path"** and explains why accepting a ref from
the caller is the §3.1.1 bug. But:

- `cmd/tfgh.loadConfig` uses `client.GetContents(…, ref)`, with a caller-supplied `--config-ref` that
  defaults to `refs/heads/main` rather than the default branch.
- `apply.Verify` uses `config.Loader` with `ConfigRef`, which defaults to *the default branch*.

So `plan` and `apply verify` can read config from **different refs** for the same repo, and plan-side staging
never learns `configSHA` (B10). Make the CLI go through `config.Loader` too: build `TrustedRefs` from
`--config-ref`, keep `--config` for a local file through `config.LoadFromFile`, which already exists and is
unused, and pass `configSHA` into `StageInput`.

Also, `apply.Verify` builds a `config.Loader` inline on every call
(`(&config.Loader{…}).LoadFromTrustedRef`). Inject a `*config.Loader`, or an interface with
`LoadFromTrustedRef`, on the `Verifier` instead of `Trusted` plus `ConfigRef`.

### 3.3 Test doubles in production code

`store.MapBucket` and `store.FakeSigner` are exported from the production package and compiled into the
binary. The convention is a `storetest` sub-package (as in `net/http/httptest` and `testing/fstest`), or
`export_test.go` if only `store`'s own tests use them. `apply`'s tests import them, so `storetest` is the right
choice. `MapBucket` is also not goroutine-safe. Add a `sync.Mutex` before anyone uses it from a concurrent test.

### 3.4 Glob handling is split three ways

- `scope` compiles `gobwas/glob` patterns **on every file × pattern × workspace**, and silently falls back to
  string equality when a pattern is invalid.
- `config.matchGlob` is a separate prefix-only approximation for overlap detection.
- Neither validates patterns when the config is loaded.

Compile `Branch` and `Watch` once in `Normalize`/`Validate` (storing the `glob.Glob` in an unexported field),
reject invalid patterns as config errors, and use the same matcher for overlap checks. Also document whether
`*` crosses `/`. With `gobwas/glob` and no separators it does, so `watch: modules/*` matches
`modules/a/b/c.tf`. That is probably intended, but it is not what `path.Match` users expect.

---

## 4. Code smells

**Dead or duplicate code**

| Item | Location |
|---|---|
| `tf.ValidatePlanArgs` is unused and duplicates `config.reservedPlanArgs`, with different rules (it also rejects `--flag` forms). Keep one, in `config`, including the `--` forms. | `tf/executor.go:168` |
| `ghapp.Client.ResolveBaseRef` and `ApplyPullRequestState` are unused. The latter also leaves out `BaseRef`. | `ghapp/client.go:80,109` |
| `config.Loader.LoadAtSHA` and `config.LoadFromFile` are unused (the latter should be used, §3.2). | `config/load.go` |
| A commented-out `cache` field with its doc comment. Delete it, or open an issue. | `config/load.go:53` |
| `WouldApply(…, applyTimeout time.Duration)` then `_ = applyTimeout`. Remove the parameter. | `apply/verify.go:226,252` |
| `PlanOptions.TerraformVersion` and `PlanOptions.BackendConfig` are set by `plan.Run` but never read by the executor. | `tf/types.go` |
| `formatFullSummary` just calls `FormatAddressesSummary`. Fine as a placeholder, but `summary_detail: full` currently accepts the setting and does nothing. Consider rejecting it in `Validate` until it is implemented. | `tf/summary.go:85` |
| `short(sha)` is defined twice, with different lengths (7 in `apply`, 12 in `config`). | |
| Two string spellings of the same `Status`/run-status idea: `RunPlanning`/`RunPlanned` are untyped `string` constants. Give them a named type, like `scope.Status`. | `store/stage.go:234` |

**Design smells**

- **Primitive obsession.** Owner, repo, workspace, base SHA and head SHA travel as five adjacent `string`s
  through `Key`, `RunKey`, `PendingKey`, `Stage` and `Verify`. B1 is exactly the bug this invites. A
  `store.ArtifactID{Owner, Repo, Workspace, BaseSHA, HeadSHA string}` with `Key(name)`, `RunKey()` and
  `PendingKey(pr)` methods would make argument mix-ups impossible and remove about 10 repeated
  `RunKey(in.Owner, in.Repo, in.Workspace, in.BaseSHA, in.HeadSHA)` calls in `stage.go`. Same for SHAs: a
  `type SHA string` costs nothing and documents intent.
- **Key formats exported as `const` format strings** (`RunObjectFormat`, `ArtifactObjectFormat`, …). Callers
  could `Sprintf` them with the wrong arity. Keep them unexported and expose only the builder functions.
- **`Workspace.Environment()` returns `Name`.** A method whose only job is to be an alias raises the question
  of whether it is supposed to diverge someday. If so, document it. If not, inline it.
- **Mixed responsibilities in `config.Validate`** (about 75 lines). It is easy to follow today, but splitting
  out `(w Workspace) validate() []error` would make adding rules cheaper and give per-workspace errors one
  prefix.
- **`Meta.Digest` relies on `encoding/json` field order for canonicalization.** That is stable for a struct,
  but **map** fields (`ResourceChangeCounts`) depend on `encoding/json` sorting map keys. It does, but that is
  an implicit contract the signature depends on. Add a test that pins the exact digest bytes for a fixture
  `Meta`, so a future field reorder or `omitempty` change cannot silently invalidate every stored signature.
- **Comment density versus code.** The "why" comments are a strength, but many cite DESIGN sections (§5.2,
  §6.1, …). Where code and DESIGN disagree (B1, the `Stage` crash-window comment, "only load path") the
  comment makes the reader trust the wrong one. Consider a test per cited invariant. Several already exist,
  which is great.
- **Silent fallbacks.** `parsePR` → 0, an invalid glob → equality match, `GetContents` on a directory → panic.
  Code that cares this much about failing closed should fail loudly in these spots too.

---

## 5. Tests

What works well: fakes based on interfaces, `t.Run` subtests, a crash-window test for the stage ordering, and
a fixture (`testdata/plan-with-secret.json`) that proves the redaction boundary.

Suggestions:

- **No tests for `cmd/tfgh` or `ghapp`.** After the `run() error` refactor, add CLI tests with fakes. For
  `ghapp`, use `httptest.Server` with `github.NewClient(nil).WithEnterpriseURLs(srv.URL, srv.URL)` to cover
  the 404 → `ErrFileNotFound` mapping (B3) and ref normalization.
- **No `t.Parallel()` anywhere.** Most of these tests are pure and can run in parallel.
- **Run with `-race`** in CI. Nothing is concurrent yet, but M6 will be.
- **Fix the test that locks in B1** (`stage_test.go:312`) and add one that stages two workspaces for one PR.
- **Use `cmp.Diff`** (`github.com/google/go-cmp`) for struct comparisons instead of field-by-field checks,
  and golden files (`testdata/*.golden` with an `-update` flag) for `printConfig` and `WouldApply` output.

---

## 6. Suggested order of work

1. Bugs B1–B4 and B6, each with a regression test. B2 and B3 together mean the `apply verify` and the
   no-config paths have never worked end to end against real GitHub.
2. `gofmt -w .`, `go mod tidy`, enable the formatters and linters, fix the `//nolint` directive.
3. Refactor `main.go` into `run() error` with `signal.NotifyContext`. Move `kmsVerifier` into `store`.
4. Unify config loading on `config.Loader` (§3.2, B10).
5. Fix the dependency direction (§3.1): `ghapp` stops importing `apply`, `store` stops importing `plan`.
6. Introduce an `ArtifactID` value type. Move the fakes to `storetest`. Make `Verify` use a typed rejection
   error.
7. Clean up the remaining dead code and docs (§4, §2.2).
