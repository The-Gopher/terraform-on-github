# ---------------------------------------------------------------------------
# Per-workspace target identities — the tier that actually holds infrastructure permission.
#
# Two service accounts per workspace. The plan one can read; the apply one can write. A repo's
# .terraform-on-github.yaml names them, but naming an identity is not access: the tokenCreator
# bindings below are what grant it, and they live here, in a reviewed Terraform change, not in a
# repo's YAML.
#
# That separation is why reading the config from the base branch (DESIGN.md §3.1) is sufficient
# rather than merely helpful. Even a PR that rewrote its own mapping could only name accounts
# that already exist and that the runtime service has already been granted — and the config it
# rewrote would not be read until after merge anyway.
# ---------------------------------------------------------------------------

resource "google_service_account" "workspace_plan" {
  for_each = var.workspaces

  account_id   = "tf-${each.key}-plan"
  display_name = "terraform ${each.key} — plan (read-only)"
}

resource "google_service_account" "workspace_apply" {
  for_each = var.workspaces

  account_id   = "tf-${each.key}-apply"
  display_name = "terraform ${each.key} — apply (write)"
}

# ---------------------------------------------------------------------------
# The impersonation boundary.
#
# tokenCreator is bound on each individual service account, never at project level. A
# project-level grant would let tf-plan@ mint any account in the project, including every apply
# identity, and would collapse the whole two-service design into decoration.
#
# Adding a workspace therefore means adding exactly two bindings — which is precisely the review
# surface you want for "who can now write to production".
# ---------------------------------------------------------------------------

resource "google_service_account_iam_member" "plan_may_impersonate_plan_sa" {
  for_each = var.workspaces

  service_account_id = google_service_account.workspace_plan[each.key].name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = google_service_account.plan.member
}

resource "google_service_account_iam_member" "apply_may_impersonate_apply_sa" {
  for_each = var.workspaces

  service_account_id = google_service_account.workspace_apply[each.key].name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = google_service_account.apply.member
}

# Deliberately absent, and worth stating as an invariant rather than an omission:
#
#   * tf-plan@  is NEVER granted tokenCreator on any workspace_apply account.
#   * tf-apply@ is NEVER granted tokenCreator on any workspace_plan account.
#
# The first is the load-bearing one: the service that executes untrusted code from pull requests
# cannot obtain a credential that can change anything. The second is hygiene — it keeps the
# read-only tier's audit trail attributable to the plan service alone.
#
# Note the one asymmetry it creates: apply.onStale's replan_if_equivalent path re-plans using
# the *apply* identity, because the apply service holds no plan identity. Another reason to
# prefer GitHub's merge queue, which avoids the stale-plan situation instead of recovering
# from it. See DESIGN.md §6.3.

# ---------------------------------------------------------------------------
# Target project permissions
# ---------------------------------------------------------------------------

locals {
  plan_role_bindings = merge([
    for ws, cfg in var.workspaces : {
      for role in cfg.plan_roles : "${ws}/${role}" => {
        workspace = ws
        project   = cfg.target_project
        role      = role
      }
    }
  ]...)

  apply_role_bindings = merge([
    for ws, cfg in var.workspaces : {
      for role in cfg.apply_roles : "${ws}/${role}" => {
        workspace = ws
        project   = cfg.target_project
        role      = role
      }
    }
  ]...)
}

resource "google_project_iam_member" "workspace_plan_roles" {
  for_each = local.plan_role_bindings

  project = each.value.project
  role    = each.value.role
  member  = google_service_account.workspace_plan[each.value.workspace].member
}

resource "google_project_iam_member" "workspace_apply_roles" {
  for_each = local.apply_role_bindings

  project = each.value.project
  role    = each.value.role
  member  = google_service_account.workspace_apply[each.value.workspace].member
}

# ---------------------------------------------------------------------------
# State bucket access
#
# The plan identity gets objectViewer and NOT objectAdmin, which is only possible because plans
# run with -lock=false. The GCS backend implements locking by writing a .tflock object into the
# state bucket, so a locking plan would need write access to the one thing the read-only tier
# must never be able to touch.
#
# The race that admits — an apply landing mid-plan — is already covered: Terraform refuses to
# apply a saved plan whose state serial has moved. The lock would buy nothing the staleness
# check does not already provide, at the cost of the read-only property. See DESIGN.md §4.2.
# ---------------------------------------------------------------------------

resource "google_storage_bucket_iam_member" "workspace_plan_state_read" {
  for_each = var.workspaces

  bucket = each.value.state_bucket
  role   = "roles/storage.objectViewer"
  member = google_service_account.workspace_plan[each.key].member

  condition {
    title       = "state prefix only"
    description = "Confine to this workspace's state prefix so one workspace's plan identity cannot read another's state."
    expression  = "resource.name.startsWith(\"projects/_/buckets/${each.value.state_bucket}/objects/${each.value.state_prefix}/\")"
  }
}

resource "google_storage_bucket_iam_member" "workspace_apply_state_write" {
  for_each = var.workspaces

  bucket = each.value.state_bucket
  role   = "roles/storage.objectAdmin"
  member = google_service_account.workspace_apply[each.key].member

  condition {
    title       = "state prefix only"
    description = "Confine writes to this workspace's state prefix."
    expression  = "resource.name.startsWith(\"projects/_/buckets/${each.value.state_bucket}/objects/${each.value.state_prefix}/\")"
  }
}
