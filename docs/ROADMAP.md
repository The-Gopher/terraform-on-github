# Roadmap — v0.1, then v0.2

**Status:** proposed · 2026-08-29

[DESIGN.md](./DESIGN.md) describes a system with two Cloud Run services, a webhook receiver,
two GCS buckets, a KMS key and a per-workspace IAM lattice. None of that is where the risk is.
The risk is in five questions that have nothing to do with the deployment:

> Which config governs this repo? Which workspaces does this PR touch? What would it change?
> Is that plan still the one we reviewed? May it be applied?

**v0.1 answers those five questions from a terminal.** **v0.2 answers them without being
asked.**

---

## 1. The two releases

| | v0.1 | v0.2 |
|---|---|---|
| **Target** | a human runs each step by hand | GitHub events and a clock run every step |
| **Trigger** | `tfgh <verb>` | webhook · Cloud Tasks · Cloud Scheduler |
| **Interface** | terminal | checks, PR comments, Deployments (goal 8) |
| **Runs as** | the operator's own credentials | two services, disjoint IAM (§7) |
| **Delivers** | the **correctness** properties | the **containment** properties |
| **Ships** | one binary + a config registry file | that binary, plus `deploy/` and two services |

**The verbs do not change between them.** v0.2 adds callers and an interface, not logic — the
same `internal/` packages, reached by an HTTP handler instead of a `main`. That is the whole
reason to do v0.1 first: every hard question above gets answered and tested against real
repositories before a single webhook exists, and each answer is falsifiable on the spot by
running the verb against a PR whose answer you already know.

**What v0.1 deliberately does not give you.** The design's guarantees split cleanly across the
two releases, and it is worth being blunt about which ones arrive when:

- v0.1 delivers **correctness**: the config comes from the trusted ref (§3.1), scoping is right
  (§3.2), the plan is of an up-to-date head (§4.1), and the plan that gets applied is provably
  the plan that was reviewed (§6.2). All five are properties of the logic, and the logic is what
  v0.1 is.
- v0.1 delivers **none of the containment**. §7's point is that no single identity can both read
  and write infrastructure — and in v0.1 the operator's own credentials do both. §8's sandbox
  does not exist either: `terraform plan` runs PR-authored code on the machine that typed the
  command. **v0.1 is a correctness tool for repos you already trust, not a security boundary.**
  Say so in its release notes.

v0.1 is still genuinely useful on its own: it is a manual gate that gives a team the
reviewed-plan-is-the-applied-plan property today, without operating anything.

---

## 2. The milestones at a glance

| # | Rel | Verb | Package promoted | First requires | DESIGN |
|---|---|---|---|---|---|
| M1 | 0.1 | `tfgh config` | `internal/config` | GitHub token | §3.1, CONFIG.md |
| M2 | 0.1 | `tfgh scope` | `internal/scope` | ” | §3.2, §4.1 |
| M3 | 0.1 | `tfgh plan` | `internal/tf`, `internal/plan` | GCP creds, state, TF CLI | §4, §9 |
| M4 | 0.1 | `tfgh plan --stage` | `internal/store` | buckets, KMS | §5 |
| M5 | 0.1 | `tfgh apply verify` | `internal/apply` | — | §6 |
| M6 | 0.2 | the two services | `cmd/*`, `internal/ghapp`, `deploy/` | the whole IAM lattice | §2, §7 |
| M7 | 0.2 | scheduled drift | — | Cloud Scheduler | goal 7, §5.6, §6.6 |

**Note where GCP enters.** M1 and M2 need nothing but a GitHub token; M3 is the first milestone
that touches credentials, state or a Terraform binary. Two milestones of real progress land
before anyone provisions anything.

---

## 3. v0.1 — run the steps by hand

### M1 — Find the config

```
tfgh config --repo acme/infra [--config-ref refs/heads/main]
```

Resolve the trusted ref, fetch `.terraform-on-github.yaml` at *that* ref, parse, validate, print
the normalized workspace set.

**Done when** every validation rule in CONFIG.md has a fixture that fails for the stated reason —
including the two that are really design decisions in disguise: a non-exact `terraform_version`,
and `on_stale: replan_if_equivalent`, which must be rejected *with its explanation* rather than
silently ignored. The example YAML round-trips.

**What it forces you to decide.** §3.1 puts `config_ref` in `deploy/`, which will not exist for
months. The CLI needs it now, so v1 starts with a local registry file that `deploy/` later
generates. Keep `--config-ref` a **local override that the service build cannot read** — the
entire property in §3.1 is that this value is not nameable by anyone who can write to the repo,
and a convenience flag that survives into the service is exactly how that property dies.

Also settles open question §13.1 (directory-per-environment vs `terraform workspace`), because
the validator has to take a side.

---

### M2 — Given a PR, find the affected workspaces

```
tfgh scope --repo acme/infra --pr 412
```

Implement §3.2 exactly: resolve `base_sha` as the tip of the base ref *now* (not `pr.base.sha`),
filter candidates by `branch`, then by changed paths against `dir` and `watch`. Print each
in-scope workspace **with the path that pulled it in** — the "why" is what makes the command
worth running, and it is what you will be staring at when scoping is wrong.

**Done when** golden tests cover: empty scope → one `skipped` result; a `watch` glob dragging in
a shared module; `release/*` branch globbing; a config-file-only change producing the §3.1
advisory; paginated compare responses; and the §4.1 up-to-date check reporting `behind` and
`diverged` distinctly from `ahead`.

**Cheap and worth it here:** a `--json` flag. M6's webhook receiver needs this same output as a
task payload, and M2 is where its shape gets fixed.

---

### M3 — List the changes for one workspace

```
tfgh plan --repo acme/infra --pr 412 --workspace prod-networking
```

Checkout, `init -lock=false`, `plan -lock=false -out=tfplan -detailed-exitcode`, then
`show -json` and `show`. Render the redacted summary (§9) per `summary_detail`.

**Done when** exit codes 0/1/2 are mapped per §4.3 — *0 and 2 are both success*, the summary
carries the difference — `plan_timeout` is enforced and reported as a timeout rather than a
failure, and the `addresses` renderer is verified against a plan containing a known secret.

**This is the first milestone that runs untrusted code** (§8). On a laptop it runs on *your*
laptop. That is acceptable for M3 against your own repos and unacceptable the moment the app
plans a PR it did not write — see §4.

---

### M4 — Stage the plan

```
tfgh plan --repo … --pr … --workspace … --stage
```

Upload the artifacts to the §5 key, sign `meta.json` with KMS, write the run object and the
`pending-apply/` marker.

**Done when** the two properties that are easy to implement backwards both hold:

- **Write-once is a feature, not an error.** A 412 on upload means "already planned this pair" —
  reuse the artifact and refresh the check (§5.2). If the first collision you hit reads as a
  crash, the cache path is wrong.
- **Ordering.** `meta.json`, then the `pending-apply/` marker, then flip the run object to
  `planned` (§5.5). Test it by killing the process between each pair and asserting the failure
  lands on the recoverable side. This is the one race in the design that no amount of care at
  runtime will fix if the write order is wrong.

---

### M5 — Given a merged PR, decide whether the plan may be applied

```
tfgh apply verify --repo acme/infra --pr 412
```

**Recommendation: `verify`, not `apply`.** Everything interesting in §6 is verification —
re-derive authorization from GitHub (§6.1), assert `merge_commit^{tree} == planned_tree_sha`
(§6.2), verify the KMS signature, check `kind == "pr"` (§6.6), detect staleness (§6.3). The
mutation itself is `terraform apply tfplan`, one line that needs no design work. Splitting there
means the CLI prints a verdict and a would-apply summary and **holds no writer identity** —
the binary mirrors the service boundary instead of quietly being the hole in it. A developer
laptop that can apply to production is the thing §7 exists to prevent; it should not arrive as a
debugging convenience.

The manual workflow this leaves is the honest one for v0.1: `verify` says the plan is still
applicable and prints what it would do, and the operator runs `terraform apply` themselves with
credentials the tool never held. In v0.2 the apply worker does both halves — but only behind the
Environment gate (§6.4) and the lease (§6.5), which is precisely the part a laptop cannot
provide.

**Done when** the tree comparison is proven against all three merge strategies (§6.2) — squash
is the one that breaks naive implementations — a moved base is rejected, an unsigned or
`kind: "drift"` artifact is rejected, and a stale plan fails with the §6.3 explanation rather
than a raw Terraform error.

**v0.1 ships here.** The whole pipeline is exercisable end to end by hand, with no service
deployed and no webhook registered — five verbs, a config registry file, and a README saying
plainly that this is a correctness tool and not yet a boundary.

---

## 4. v0.2 — the same verbs, called automatically

v0.2 adds three callers and one interface. It should add **no new decisions about what a plan
means** — if it does, something belonged in v0.1 and was skipped.

| The v0.1 step | becomes, in v0.2 |
|---|---|
| you type `tfgh scope` | `pull_request` webhook → HMAC verify → dedupe → enqueue |
| you type `tfgh plan --stage` | Cloud Tasks → plan worker, read-only tier |
| you read the terminal | a check run per workspace, redacted summary (§9) |
| you decide to apply | Deployment → GitHub Environment approval (§6.4) |
| you type `tfgh apply verify`, then apply | apply worker verifies *then* applies, under lease |
| you remember to check for drift | Cloud Scheduler → plan worker, `drift/` key |

### M6 — The services

`internal/ghapp` (App auth, down-scoped installation tokens, checks, deployments), the webhook
receiver, Cloud Tasks, and `deploy/` — the buckets, the KMS key, and the per-service-account
`tokenCreator` bindings in §7. Then the two `cmd/` binaries, which are mostly wiring.

**Two of the design's headline properties become real only here**, which is why v0.2 is the
release that can be pointed at a repo you do not control:

- **§7's IAM split.** v0.1's operator reads and writes with one identity. M6 is where the
  planner becomes structurally incapable of writing.
- **§8's sandbox.** Egress allowlist, provider mirror, module-source allowlist, fork gating.
  See §5 — this one is a gate, not a task.

Goal 8 also lands here: until checks and Deployments exist, the interface is a terminal, and
"GitHub is the entire interface" is unproven. **Open question §13.6 (one GitHub App or two)
gates this milestone** — it changes the deploy shape — so decide it before `deploy/` is
written, not during.

### M7 — Scheduled drift

Cloud Scheduler → the plan worker, `drift/` keying (§5.6), issue open/close via `drift-open/`
markers. Goal 7 is not met without it, and it is small once M6 exists.

The *verb* is cheap enough to pull forward: `tfgh drift --workspace prod-networking` is M3 with
a different key and no PR, so land it in v0.1 if it costs an afternoon there. What genuinely
belongs to v0.2 is the schedule and the issue lifecycle — a drift detector nobody scheduled is
just a plan command, and one that opens a fresh issue every tick gets muted within a week.

---

## 5. What gates what

**§8 sandboxing gates the v0.2 release, absolutely.** `terraform plan` executes provider
binaries and module code from the pull request. The egress allowlist, the provider mirror, the
module-source allowlist and fork gating are not a task inside M6 that can slip — the first PR
from a fork that the deployed app plans is the first time an untrusted party runs code inside
the boundary, and unlike v0.1 nobody typed a command and chose to take that risk. Build it
during M3, when the runner image is being assembled anyway; v0.2 does not ship without it.

**M1 decides §13.1**, **M6 decides §13.6.** §13.2 (PR comments) and §13.3 (who may `/plan` a
fork PR) are v0.2 questions and can wait for M6; §13.4 (which base branches auto-apply) is
per-repo policy and needs no code.

---

## 6. After v0.2 — what would make it 1.0

v0.2 is the design working end to end. The version number is not the same claim. Between them:
every open question in §13 closed rather than deferred, §8 exercised against a real fork PR,
soak time on a repo that is not the author's, and the failure modes in §10 each triggered on
purpose once — particularly a stale plan (§6.3) and a `/reconcile` sweep picking up a marker
whose worker died mid-stage (§5.5), because those are the two paths that only ever run on a bad
day.

---

## 7. Not in either release

Unchanged from the non-goals in [DESIGN §1](./DESIGN.md#1-goals--non-goals) — automatic drift
remediation, policy-as-code, non-GCP providers, state migration and import, a web UI — plus
`replan_if_equivalent`, which is **cut rather than deferred** (§6.3) and whose only remaining
artifact is a validation error explaining why.
