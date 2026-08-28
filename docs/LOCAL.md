# The first iteration: two local scripts

`docs/DESIGN.md` describes two Cloud Run services, a GitHub App, three GCS buckets, Cloud
Tasks, Cloud Scheduler and a KMS key. None of that exists yet, and none of it is needed to find
out whether the *flow* is right.

So the first iteration is two scripts an operator runs on their own machine:

```
scripts/tfog-plan  <pr>    plan the PR's Terraform, store the plan, post it to the PR
scripts/tfog-apply <pr>    verify the merged PR against the stored plan, apply it, post the outcome
```

They are the same pipeline as the design, on one machine, with the distributed-systems
machinery removed and the guarantees kept. The point of building it this way round is that the
guarantees are the hard part and the infrastructure is the easy part.

---

## 1. Install

```bash
gh auth login                      # the scripts use your own GitHub credentials
gcloud auth application-default login
terraform version                  # must match the version the repo's config pins
```

Requires `python3` (3.9+) with PyYAML, `git`, `gh`, and `terraform`. Nothing else — no Go build,
nothing from `deploy/`.

The plan store defaults to `$XDG_DATA_HOME/terraform-on-github` (`~/.local/share/…`). Override
with `$TFOG_STORE` or `--store`.

## 2. Use

```bash
cd ~/src/acme-infra                        # a clone of the Terraform repo

scripts/tfog-plan 412                      # plan every workspace PR #412 touches
scripts/tfog-plan 412 --no-comment         # …and print the summary instead of posting it
scripts/tfog-plan 412 --workspace prod-data

# after the PR merges
scripts/tfog-apply 412 --dry-run           # verify everything, stop before applying
scripts/tfog-apply 412                     # type the workspace name to confirm
```

`tfog-plan` exits `0` when it planned (with or without changes — DESIGN 4.3), `3` when nothing
was in scope, `4` when the branch needs updating, `1` on error. `tfog-apply` exits `0` applied,
`5` nothing to apply, `1` on error or verification failure.

Useful environment variables:

| Variable | Effect |
|---|---|
| `TFOG_STORE` | Plan store root. |
| `TFOG_TRUSTED_REF` | Default for `--trusted-ref`. |
| `TFOG_TERRAFORM_BIN` | The `terraform` binary to use — how you run a version other than the one on `PATH`. |
| `TFOG_ALLOW_ANY_MODULE_SOURCE=1` | Skip the module-source allowlist. Prints a warning; see §4. |

---

## 3. What is preserved

These are the reasons the design is worth having, and they are all here. Each is a real check
that will refuse to proceed, not a comment.

**The config comes from one trusted ref.** `--trusted-ref` (default: the repo's default branch),
never the PR head and never the PR's base branch. A PR that edits `.terraform-on-github.yaml`
gets an advisory comment saying so and is planned with the trusted ref's version anyway.
DESIGN 3.1.

**The head must already contain its base.** One `compare` call; anything but `ahead` or
`identical` and nothing is planned, because the head's tree is not the tree a merge would
produce. DESIGN 4.1.

**The plan is keyed by `(base_sha, head_sha)` and written once.** Same key layout as the bucket,
on a local filesystem, with `O_EXCL` and mode `0600`. Re-running `tfog-plan` on an unchanged pair
reuses the stored plan rather than replacing its bytes; `--force` deletes the key first, out
loud. DESIGN 5, 5.2.

**Plan runs read-only, including against state.** `terraform plan -lock=false`, and
`impersonate.plan` if set. DESIGN 4.2.

**Module sources are checked before `terraform init`.** Init is what fetches module code and can
run it, and Terraform has no native allowlist, so the check is a pre-pass over the HCL —
transitively through local modules, because a local module can pull a remote one. DESIGN 8.

**The tree test.** `tfog-plan` records `planned_tree_sha`; `tfog-apply` refuses unless
`tree(merge_commit)` equals it, and then checks the tree that actually landed on disk as well.
One comparison proves the applied content is the reviewed content, and it holds under squash and
rebase merges where the commit SHA cannot match by construction. DESIGN 6.2.

**The apply applies the stored plan file.** Not a fresh plan. Terraform refuses a saved plan
whose state lineage or serial has moved, which is the guarantee the whole design leans on — the
applied plan is the reviewed plan, or nothing happens. `on_stale: fail` is the only accepted
policy, so a stale plan is terminal and a human re-plans. DESIGN 6.3.

**Authorization is re-derived, not trusted.** `tfog-apply` re-reads the PR, re-loads the config
from the trusted ref *at its current tip* (so a workspace deauthorized between plan and merge
does not apply), and re-checks every field of the stored `meta.json` against the key.
`--workspace` selects among already-authorized workspaces; it cannot create an authorization.
DESIGN 6.1.

**Config validation fails closed.** A config that does not parse, names a bad service account,
sets `on_stale: replan_if_equivalent`, or passes a reserved `-out`/`-state` in `plan_args`, gets
no plan — not a plan against a guessed mapping.

---

## 4. What is gone, and what it costs

Be honest about this list. Some of it is fine to lose locally; some of it is the whole reason the
service version exists.

### The IAM boundary — this is the big one

DESIGN 7's premise is that the planner *cannot* mint a writer identity, because `tokenCreator`
is bound on individual service accounts and `tf-plan@` only ever holds it on `…-plan@`. One
operator running both scripts holds both grants. The scripts still set
`GOOGLE_IMPERSONATE_SERVICE_ACCOUNT` per tier — `impersonate.plan` for plan,
`impersonate.apply` for apply — so the *mechanism* is the same and the config is exercised. But
the ceiling is the operator's own grants, not a service boundary.

Consequence: the local iteration is fine for a workspace you would let that operator apply by
hand anyway. It is not a substitute for the boundary on anything where "who could have done
this" matters. That is the line where you build the services.

### The KMS signature

`meta.json` carries a `digest` — SHA-256 over its own canonical encoding — and `plan_sha256`
over the `tfplan` bytes. `tfog-apply` verifies both, which catches a careless edit or a
half-written artifact. It stops nobody who can write to the store, because the store's owner
computes the digest. On one machine under one operator the filesystem is the boundary; do not
move this store to a shared host and call it signed. DESIGN 5.3.

### Check runs → PR comments

A check run needs `checks:write` on a head SHA the App owns, which an operator's token does not
have. Plan output is a sticky PR comment per workspace instead, carrying the same content — and
an invisible `<!-- tfog-meta … -->` block.

`tfog-apply` locates the plan in the local store on its own and *cross-checks* that block. So a
forged comment can make an apply refuse; it cannot make one apply something else. That asymmetry
is why the comment can serve as an index without being treated as an authority. DESIGN 9, 13.2.

### The GitHub Environment approval gate

Replaced by a typed confirmation: `tfog-apply` prints the counts, the addresses, the state
prefix and the identity, then requires the workspace name typed back. `--yes` skips it, and
without a TTY the script refuses rather than applying unconfirmed. What is lost is the audit
trail living next to the code, and the ability to require *a different person*. DESIGN 6.4.

### The coordination bucket, leases and `/reconcile`

`state.json` beside each plan records `planned` / `applying` / `applied` / `apply_failed`, which
is enough to refuse to re-apply a plan already applied, and to stop after a failed apply until a
human has read the log. There is no compare-and-set because there is no second worker to lose a
race to, no workspace lease because Terraform's own state lock is the correctness backstop
anyway, and no `/reconcile` because you are the reconciler — a merged PR with no stored plan
prints a message and exits `5`, which is the local form of "never silently skip". DESIGN 5.4,
6.5, 10.

### The plan sandbox

No egress allowlist, no provider network mirror, no read-only rootfs. `terraform plan` runs the
PR's module code on your workstation with your credentials. The module-source allowlist is the
only part of DESIGN 8 that survives, and it is the cheap part.

Consequence: run `tfog-plan` only on PRs from people who could already run `terraform` against
that state. Never on a fork PR from a stranger. `TFOG_ALLOW_ANY_MODULE_SOURCE=1` removes even
this, and prints a warning because it should feel like a decision.

### Webhooks, Cloud Tasks, retries, dedupe

You are the trigger. Idempotency comes from the write-once store and the `(base_sha, head_sha)`
key, which is where it came from in the design too.

---

## 5. Layout

```
scripts/tfog-plan            plan a PR, store, comment
scripts/tfog-apply           verify a merged PR, apply the stored plan, comment
scripts/tfog/cfg.py          config schema, validation, scoping, module-source allowlist
scripts/tfog/ghub.py         GitHub via the `gh` CLI
scripts/tfog/store.py        key layout, write-once artifacts, meta.json, run state
scripts/tfog/terra.py        git checkout, Terraform CLI wrappers
scripts/tfog/render.py       plan.json → the summary a reviewer reads
scripts/tests/test_tfog.py   tests for the logic above — no network, no Terraform
```

```bash
python3 scripts/tests/test_tfog.py
```

The tests cover the decisions that have to be right before either script touches real
infrastructure: which workspaces a diff selects, whether a module source is allowed, whether a
replace is counted as one change, and whether the store refuses to overwrite a reviewed plan.
The module-source scanner has the most cases because it is the one that failed first — a
line-oriented version missed `module "vpc" { source = "./x" }`, and a missed module block is a
module source that never reaches the allowlist.

The store, identical in layout to the bucket in DESIGN 5:

```
$TFOG_STORE/plans/<owner>/<repo>/<workspace>/<base_sha>/<head_sha>/
    tfplan         the opaque binary — the thing that gets applied
    plan.json      terraform show -json
    plan.txt       terraform show
    meta.json      provenance, digest-checked
    state.json     planned | applying | applied | apply_failed
    apply-1.log
```

## 6. When to stop using these

Move to the services in `docs/DESIGN.md` when any of these becomes true:

- More than one person needs to plan or apply, and "who did this" has to be answerable from
  something other than a shell history.
- A workspace's blast radius is larger than you would hand a single operator — the IAM boundary
  is the thing you are buying, and it is the one thing these scripts cannot approximate.
- You need to plan PRs from authors you would not hand your credentials to. That needs the
  sandbox, not a policy.
- Nobody remembers to run `tfog-apply` after a merge. That is what `/reconcile` is for.

Until then, these scripts are the same flow, and the guarantees in §3 are the ones worth
testing first.
