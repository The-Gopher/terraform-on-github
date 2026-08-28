# terraform-on-github

A GitHub App that plans Terraform on pull requests and applies **the reviewed plan** on
merge — with the planner and the applier as two Cloud Run services that cannot do each
other's job.

> **Status.** `docs/DESIGN.md` is the design. The **first iteration runs today** as two local
> commands — `cmd/tfog-plan` and `cmd/tfog-apply` — which do the whole flow on one machine: plan
> a PR, store the plan, post it; then verify the merge against that stored plan, apply it, post
> the outcome. See [docs/LOCAL.md](docs/LOCAL.md).
>
> They are built on the same `internal/` packages the services will use, so the shared half —
> config loading, scoping, checkout, the Terraform wrapper, plan keying and rendering — is real
> and tested. What is still a scaffold is the service half: App auth, GCS artifacts, the
> coordination bucket, KMS signing, and the two workers in `cmd/*-service`, which return
> `errNotImplemented`. `deploy/` is likewise ahead of the code.

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

## Start here: the local commands

```bash
go build -o bin/ ./cmd/tfog-plan ./cmd/tfog-apply
gh auth login && gcloud auth application-default login
cd ~/src/acme-infra                  # a clone of the Terraform repo

bin/tfog-plan 412                    # plan what PR #412 touches, store it, comment on the PR
bin/tfog-apply 412 --dry-run         # after merge: verify everything, stop before applying
bin/tfog-apply 412                   # type the workspace name to confirm
```

Same pipeline, one machine, no infrastructure. It keeps the parts that are hard to get right —
config from a trusted ref, up-to-date-or-nothing, a write-once plan keyed by
`(base_sha, head_sha)`, the tree test at merge, and applying *the stored plan file* rather than a
fresh one — and drops the parts that are merely work. What that costs, in detail, is
[§4 of docs/LOCAL.md](docs/LOCAL.md#4-what-is-gone-and-what-it-costs); the short version is that
the IAM boundary is the thing you cannot have locally, and it is the reason the services exist.

## Layout

```
docs/DESIGN.md            architecture, threat model, failure modes, open questions
docs/LOCAL.md             the local scripts: what they preserve, what they drop
docs/CONFIG.md            .terraform-on-github.yaml schema
.terraform-on-github.example.yaml

cmd/tfog-plan/            local: plan a PR, store, comment
cmd/tfog-apply/           local: verify a merged PR, apply the stored plan, comment
cmd/plan-service/         webhook receiver + plan worker  (read-only tier)
cmd/apply-service/        apply worker + /reconcile       (write tier)

internal/cli/             shared argument, store and output plumbing for the two commands
internal/config/          config schema, load-from-trusted-ref, strict decode, validation
internal/scope/           base branch + changed files → workspaces
internal/ghapp/           App auth, down-scoped tokens, webhooks, checks, deployments
internal/ghcli/           GitHub via the `gh` CLI — what the local commands use instead
internal/store/           key layout, Meta + digest, the local store, GCS/CAS/KMS scaffold
internal/tf/              Terraform CLI wrapper, checkout, provider mirror config
internal/plan/            plan orchestration, summary rendering, plan equivalence
internal/prcomment/       PR comment bodies for the local commands
internal/apply/           apply orchestration, merge-time verification

deploy/                   Terraform for the app's own GCP infra (services, IAM, buckets)
```

## Open questions before implementation

Directory-per-environment vs `terraform workspace`; one GitHub App or two; which workspaces
pilot `replan_if_equivalent`. See [DESIGN.md §13](docs/DESIGN.md#13-open-questions).
