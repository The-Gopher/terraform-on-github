# terraform-on-github

A GitHub App that plans Terraform on pull requests and applies **the reviewed plan** on
merge — with the planner and the applier as two Cloud Run services that cannot do each
other's job.

> **Status.** `docs/DESIGN.md` is the design. The packages both tiers share —
> `internal/config`, `internal/scope`, `internal/tf`, `internal/plan`, `internal/store` — are
> implemented and tested: the trusted-ref config load, workspace scoping, checkout, the
> Terraform wrapper, the module-source allowlist, plan keying and summary rendering.
>
> What is still a scaffold is the service half: App auth, GCS artifacts, the coordination
> bucket, KMS signing, and the two workers in `cmd/*-service`, which return
> `errNotImplemented`. Nothing runs end to end yet. `deploy/` is likewise ahead of the code.

## The shape

```
PR opened / pushed
  → read .terraform-on-github.yaml @ the repo's trusted ref   (never the PR head or base)
  → which workspaces does this diff touch?
  → for each: plan the test-merge commit, read-only credentials
  → store tfplan keyed by (base_sha, head_sha), sign it
  → one GitHub check run per workspace, redacted summary

PR merged
  → re-derive authorization from GitHub, not from the trigger
  → prove merge_commit^{tree} == the tree we planned
  → verify the plan signature
  → GitHub Environment approval gate
  → terraform apply <that exact plan>
```

## Why two services

Not for scaling — for the IAM boundary. Neither service holds any permission on a target
project; each may only *impersonate* one tier of per-workspace service account.

```
tf-plan@  --tokenCreator-->  tf-<ws>-plan@   -->  target project (read-only)
tf-apply@ --tokenCreator-->  tf-<ws>-apply@  -->  target project (write)
```

`tokenCreator` is bound on individual service accounts, never at project level. The plan
service cannot mint a writer identity even if fully compromised. It also holds
`storage.objectCreator` and nothing else on the plan bucket — create, no read, no overwrite.

Full table: [DESIGN.md §7](docs/DESIGN.md#7-the-iam-boundary).

## Three ideas worth reading first

- **[§3.1](docs/DESIGN.md#31-the-config-is-read-from-one-trusted-ref-not-from-the-prs-base)** —
  the config comes from one ref declared outside the repo. Not the PR head, and *not the PR's
  base*: in a `feature → integration → prod` flow, `integration` is writable by developers, so
  its config is attacker-controlled. "Base branch" is only a proxy for "reviewed" when the base
  happens to be protected.
- **[§6.2](docs/DESIGN.md#62-tree-sha-equality-survives-squash-merges)** — compare
  `merge_commit^{tree}` to the planned tree. One comparison proves the applied tree is the
  reviewed tree, under squash, rebase or merge-commit.
- **[§8](docs/DESIGN.md#8-untrusted-code-is-the-real-threat)** — `terraform plan` runs
  untrusted code. Egress allowlist, provider mirror, module-source allowlist, fork gating.

## Layout

```
docs/DESIGN.md            architecture, threat model, failure modes, open questions
docs/CONFIG.md            .terraform-on-github.yaml schema
.terraform-on-github.example.yaml

cmd/plan-service/         webhook receiver + plan worker  (read-only tier)
cmd/apply-service/        apply worker + /reconcile       (write tier)

internal/config/          config schema, load-from-trusted-ref, validation
internal/scope/           base branch + changed files → workspaces
internal/ghapp/           App auth, down-scoped tokens, webhooks, checks, deployments
internal/store/           GCS artifacts, key layout, GCS-CAS run index, KMS sign/verify
internal/tf/              Terraform CLI wrapper, checkout, provider mirror config
internal/plan/            plan orchestration, summary rendering, plan equivalence
internal/apply/           apply orchestration, merge-time verification

deploy/                   Terraform for the app's own GCP infra (services, IAM, buckets)
```

## Open questions before implementation

Directory-per-environment vs `terraform workspace`; one GitHub App or two; which workspaces
pilot `replan_if_equivalent`. See [DESIGN.md §13](docs/DESIGN.md#13-open-questions).
