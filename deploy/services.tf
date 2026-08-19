# ---------------------------------------------------------------------------
# plan-service — the read-only tier.
#
# Public ingress, because GitHub has to be able to POST /webhook. Cloud Run has no per-path
# ingress control, so /tasks/plan is internet-reachable too and authenticates the Google-signed
# OIDC token in-process. See DESIGN.md §7.5.
# ---------------------------------------------------------------------------

resource "google_cloud_run_v2_service" "plan" {
  name     = "tf-plan"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.plan.email

    # Bounds concurrent live credentials and spend. Terraform is not concurrency-safe within a
    # working directory, and each request gets its own checkout, so one plan per instance.
    max_instance_request_concurrency = 1
    timeout                          = "1800s" # matches the Cloud Tasks 30m dispatch deadline

    scaling {
      max_instance_count = 30
    }

    # All egress through the VPC. This is the control that actually contains untrusted plan-time
    # code: `terraform init` fetches and runs module code, and `data "http"` can exfiltrate. The
    # provider mirror and module allowlist are defence in depth; the egress allowlist is the
    # thing that survives a creative attacker. See DESIGN.md §8.
    vpc_access {
      connector = var.vpc_connector
      egress    = "ALL_TRAFFIC"
    }

    containers {
      image = var.plan_image

      env {
        name  = "GITHUB_APP_ID"
        value = var.github_app_id
      }
      env {
        name  = "PLAN_BUCKET"
        value = google_storage_bucket.plans.name
      }
      env {
        name  = "COORDINATION_BUCKET"
        value = google_storage_bucket.coordination.name
      }
      env {
        name  = "PLAN_SIGNING_KEY"
        value = "${google_kms_crypto_key.plan_signing.id}/cryptoKeyVersions/1"
      }
      env {
        name  = "PROVIDER_MIRROR_URL"
        value = var.provider_mirror_url
      }
      # The trusted config ref per repo. Deployment config, never repository content —
      # see DESIGN.md §3.1.2.
      env {
        name  = "TRUSTED_CONFIG_REFS"
        value = jsonencode({ for k, v in var.repos : k => v.config_ref })
      }
      env {
        name  = "APPLY_QUEUE"
        value = google_cloud_tasks_queue.apply.id
      }
      env {
        name  = "PLAN_QUEUE"
        value = google_cloud_tasks_queue.plan.id
      }
      env {
        name  = "EXPECTED_INVOKER_SA"
        value = google_service_account.plan_invoker.email
      }

      resources {
        limits = {
          cpu    = "2"
          memory = "4Gi"
        }
      }

      # /workspace is the only writable path: checkouts and plan files, on tmpfs, discarded with
      # the instance. Nothing untrusted gets to persist.
      volume_mounts {
        name       = "workspace"
        mount_path = "/workspace"
      }
    }

    volumes {
      name = "workspace"
      empty_dir {
        medium     = "MEMORY"
        size_limit = "2Gi"
      }
    }
  }
}

# ---------------------------------------------------------------------------
# apply-service — the write tier.
#
# Internal ingress and an explicit invoker allowlist. There is no path from the internet to this
# binary at all. Combined with apply.Verify re-deriving authorization from GitHub and the plan
# signature, the trigger becomes a hint rather than a permission.
# ---------------------------------------------------------------------------

resource "google_cloud_run_v2_service" "apply" {
  name     = "tf-apply"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_INTERNAL_ONLY"

  template {
    service_account = google_service_account.apply.email

    max_instance_request_concurrency = 1
    timeout                          = "3600s"

    scaling {
      # Applies serialize per workspace via the advisory lease; this is a global ceiling.
      max_instance_count = 10
    }

    vpc_access {
      connector = var.vpc_connector
      egress    = "ALL_TRAFFIC"
    }

    containers {
      image = var.apply_image

      env {
        name  = "GITHUB_APP_ID"
        value = var.github_app_id
      }
      env {
        name  = "PLAN_BUCKET"
        value = google_storage_bucket.plans.name
      }
      env {
        name  = "APPLY_LOG_BUCKET"
        value = google_storage_bucket.apply_logs.name
      }
      env {
        name  = "COORDINATION_BUCKET"
        value = google_storage_bucket.coordination.name
      }
      # Verify only. The apply service holds publicKeyViewer, not signer.
      env {
        name  = "PLAN_SIGNING_KEY"
        value = "${google_kms_crypto_key.plan_signing.id}/cryptoKeyVersions/1"
      }
      env {
        name  = "PROVIDER_MIRROR_URL"
        value = var.provider_mirror_url
      }
      # The trusted config ref per repo. Deployment config, never repository content —
      # see DESIGN.md §3.1.2.
      env {
        name  = "TRUSTED_CONFIG_REFS"
        value = jsonencode({ for k, v in var.repos : k => v.config_ref })
      }
      env {
        name  = "EXPECTED_INVOKER_SA"
        value = google_service_account.apply_invoker.email
      }

      resources {
        limits = {
          cpu    = "2"
          memory = "4Gi"
        }
      }

      volume_mounts {
        name       = "workspace"
        mount_path = "/workspace"
      }
    }

    volumes {
      name = "workspace"
      empty_dir {
        medium     = "MEMORY"
        size_limit = "2Gi"
      }
    }
  }
}

# ---------------------------------------------------------------------------
# Who may invoke what.
#
# The plan service is NOT granted run.invoker on the apply service. It can enqueue an apply task
# (see iam.tf) but cannot call the apply service directly, so every apply arrives through the
# queue and is subject to apply.Verify.
# ---------------------------------------------------------------------------

resource "google_cloud_run_v2_service_iam_member" "plan_invoker" {
  name     = google_cloud_run_v2_service.plan.name
  location = var.region
  role     = "roles/run.invoker"
  member   = google_service_account.plan_invoker.member
}

resource "google_cloud_run_v2_service_iam_member" "apply_invoker" {
  name     = google_cloud_run_v2_service.apply.name
  location = var.region
  role     = "roles/run.invoker"
  member   = google_service_account.apply_invoker.member
}

# ---------------------------------------------------------------------------
# The reconcile sweep.
#
# Backstop for lost apply tasks — the worst failure mode here is a merged PR whose plan silently
# never applies, because nothing turns red. Also the hardening path: drop the
# plan_enqueues_applies binding in iam.tf and this becomes the only route to an apply, so the
# plan service loses even the ability to ask for one. Costs ~30s of latency. See DESIGN.md §7.4.
# ---------------------------------------------------------------------------

resource "google_cloud_scheduler_job" "reconcile" {
  name      = "tf-apply-reconcile"
  schedule  = "* * * * *"
  time_zone = "Etc/UTC"

  http_target {
    http_method = "POST"
    uri         = "${google_cloud_run_v2_service.apply.uri}/reconcile"

    oidc_token {
      service_account_email = google_service_account.apply_invoker.email
      audience              = google_cloud_run_v2_service.apply.uri
    }
  }
}

output "webhook_url" {
  description = "Set as the GitHub App's webhook URL."
  value       = "${google_cloud_run_v2_service.plan.uri}/webhook"
}
