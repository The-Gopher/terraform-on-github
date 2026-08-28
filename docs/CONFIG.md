# `.terraform-on-github.yaml`

The repo's declaration of which branches control which Terraform.

> **Read from one trusted ref, always.** The app fetches this file at the tip of a single ref
> declared outside the repo — not the PR head, and *not the PR's base branch*. A PR that edits
> this file does not change how it is itself planned; the edit takes effect once it reaches the
> trusted ref. See [DESIGN.md §3.1](./DESIGN.md#31-the-config-is-read-from-one-trusted-ref-not-from-the-prs-base).

---

## Top level

| Key | Type | Required | Notes |
|---|---|---|---|
| `version` | int | yes | Must be `1`. |
| `defaults` | object | no | Per-workspace defaults; any workspace key may override. |
| `module_sources` | []string | no | Allowlist of glob patterns for module `source` values. Empty = local (`./`, `../`) paths only. Enforced before `terraform init`. |
| `workspaces` | []object | yes | At least one. `name` must be unique. |

## `defaults` / per-workspace overrides

| Key | Type | Default | Notes |
|---|---|---|---|
| `terraform_version` | string | required | Exact version, no ranges. Must be a version baked into a published runner image. |
| `plan_timeout` | duration | `20m` | Any form `time.ParseDuration` accepts (`30s`, `20m`, `1h30m`). Bounded by the Cloud Tasks 30m dispatch deadline. |
| `apply_timeout` | duration | `45m` | |
| `summary_detail` | enum | `addresses` | `addresses` = address + action only. `full` = inline diff. See DESIGN §8. |
| `var_files` | []string | `[]` | Relative to `dir`. |
| `plan_args` | []string | `[]` | Extra `terraform plan` flags. `-lock`, `-out`, `-input`, `-state`, `-var-file` and `-chdir` are reserved and rejected. |

## `workspaces[]`

| Key | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | `[a-z0-9-]{1,48}`. Appears in the check name and in several GCS object keys — and with no database, those key names are the schema. Treat as immutable. |
| `branch` | string | yes | Base branch this workspace is bound to. Glob allowed (`release/*`). |
| `dir` | string | yes | Repo-relative root module directory. No `..`. |
| `watch` | []string | no | Extra globs whose change invalidates this workspace (shared modules). |
| `terraform_workspace` | string | no | Escape hatch for `terraform workspace select`. Prefer separate backends — DESIGN §13.1. |
| `backend` | object | yes | `bucket`, `prefix`. GCS backend only in v1. |
| `impersonate.plan` | string | yes | Read-only SA. `tf-plan@` must hold `tokenCreator` on it. |
| `impersonate.apply` | string | yes | Writer SA. `tf-apply@` must hold `tokenCreator` on it. |
| `apply` | object | no | See below. |

Both identities are required, and validated as service-account emails. The config describes what
the workspace *needs*, not what a particular runner happens to hold.

## `workspaces[].apply`

| Key | Type | Default | Notes |
|---|---|---|---|
| `enabled` | bool | `true` | `false` = plan-only workspace. |
| `environment` | string | `name` | GitHub Environment used as the approval gate. Protection rules live in GitHub, not here. |
| `approval_timeout` | duration | `24h` | After this the run is abandoned, not applied. |
| `on_stale` | enum | `fail` | `fail` is the only accepted value. `replan_if_equivalent` was **cut** — §4.1's up-to-date requirement removes the case it existed for, and it is rejected at validation with that explanation (DESIGN §6.3). |
| `require_protected_base` | bool | `false` | Refuse to apply unless `branch` actually has required reviews. Asserts what §3.1's original justification assumed. Costs the App `administration: read`, so enable per workspace — sensible for prod, pointless for an integration branch that is open by design. |

---

## Validation rules

Rejected at load time, surfaced as one failing check on every PR against the branch:

- `version != 1`
- duplicate `name`; `name` not matching `[a-z0-9-]{1,48}`
- `dir` absolute, containing `..`, or escaping the repo root
- two workspaces on the same `branch` with overlapping `dir`
- an `impersonate` value that is not a valid service-account email
- `plan_args` containing `-lock`, `-out`, `-input`, `-state` or `-var-file`
- a `terraform_version` that is not exact (`~> 1.9` is rejected; a range cannot be pinned to a
  runner image, and cannot be asserted at apply time against the plan that was reviewed)
- a `terraform_version` with no matching runner image
- a module `source` in the tree matching neither `module_sources` nor a local path

Validation is intentionally strict and fails closed: a repo whose config does not parse gets
no plans, rather than plans against a guessed mapping.

Separately, a PR whose head does not already contain its base branch gets no plan either — one
failing check saying "update your branch". See [DESIGN.md §4.1](./DESIGN.md#41-require-the-branch-to-be-up-to-date-then-plan-the-head).

---

## Worked example

```yaml
version: 1

defaults:
  terraform_version: "1.9.8"
  plan_timeout: 20m
  summary_detail: addresses

module_sources:
  - "git::ssh://git@github.com/acme/terraform-modules//*?ref=v*"
  - "registry.terraform.io/acme/*"

workspaces:
  # ---- production, gated ----
  - name: prod-networking
    branch: main
    dir: envs/prod/networking
    watch: ["modules/vpc/**"]
    backend: { bucket: acme-tfstate-prod, prefix: prod/networking }
    impersonate:
      plan:  tf-prod-networking-plan@acme-tf.iam.gserviceaccount.com
      apply: tf-prod-networking-apply@acme-tf.iam.gserviceaccount.com
    apply:
      environment: prod-networking
      on_stale: fail
      require_protected_base: true

  - name: prod-data
    branch: main
    dir: envs/prod/data
    backend: { bucket: acme-tfstate-prod, prefix: prod/data }
    impersonate:
      plan:  tf-prod-data-plan@acme-tf.iam.gserviceaccount.com
      apply: tf-prod-data-apply@acme-tf.iam.gserviceaccount.com
    apply:
      environment: prod-data
      on_stale: fail

  # ---- integration: developers merge here freely ----
  #
  # This entry is why the config must come from the trusted ref. A developer with merge rights to
  # `integration` cannot add a workspace here, so the worst they can reach is this identity.
  - name: integration
    branch: integration
    dir: envs/integration
    watch: ["modules/**"]
    backend: { bucket: acme-tfstate-integration, prefix: integration }
    summary_detail: full
    impersonate:
      plan:  tf-integration-plan@acme-tf.iam.gserviceaccount.com
      apply: tf-integration-apply@acme-tf.iam.gserviceaccount.com
    apply:
      environment: integration   # no required reviewers on this one

  # ---- a long-lived release branch, plan-only ----
  - name: prod-networking-next
    branch: "release/*"
    dir: envs/prod/networking
    backend: { bucket: acme-tfstate-prod, prefix: prod/networking }
    impersonate:
      plan:  tf-prod-networking-plan@acme-tf.iam.gserviceaccount.com
      apply: tf-prod-networking-apply@acme-tf.iam.gserviceaccount.com
    apply:
      enabled: false
```
