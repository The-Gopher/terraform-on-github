terraform {
  required_version = "~> 1.9"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

# ---------------------------------------------------------------------------
# Runtime identities.
#
# Neither of these holds a single permission on a target project. They hold the right to
# impersonate — nothing more. All infrastructure permission lives on the per-workspace target
# service accounts in workspaces.tf, and the tokenCreator bindings that connect the two tiers
# are granted on individual accounts, never at project scope.
#
# The property this buys: a fully compromised plan service can mint read-only credentials for
# one workspace at a time, and cannot mint a writer credential for anything, ever.
# ---------------------------------------------------------------------------

resource "google_service_account" "plan" {
  account_id   = "tf-plan"
  display_name = "terraform-on-github plan service (read-only tier)"
}

resource "google_service_account" "apply" {
  account_id   = "tf-apply"
  display_name = "terraform-on-github apply service (write tier)"
}

# Cloud Tasks OIDC identities. Separate from the runtime accounts so "may be invoked as" and
# "may act as" are distinct grants and show up separately in an IAM review.
resource "google_service_account" "plan_invoker" {
  account_id   = "tf-plan-invoker"
  display_name = "Cloud Tasks → plan-service"
}

resource "google_service_account" "apply_invoker" {
  account_id   = "tf-apply-invoker"
  display_name = "Cloud Tasks / Scheduler → apply-service"
}

# ---------------------------------------------------------------------------
# Queues
# ---------------------------------------------------------------------------

resource "google_cloud_tasks_queue" "plan" {
  name     = "tf-plan-queue"
  location = var.region

  rate_limits {
    # Bounds concurrent plans, and therefore concurrent live credentials and spend.
    max_concurrent_dispatches = 20
    max_dispatches_per_second = 10
  }

  retry_config {
    max_attempts       = 5
    min_backoff        = "10s"
    max_backoff        = "300s"
    max_retry_duration = "3600s"
  }
}

resource "google_cloud_tasks_queue" "apply" {
  name     = "tf-apply-queue"
  location = var.region

  rate_limits {
    # Applies serialize per workspace via the advisory lease; this is a global spend ceiling.
    max_concurrent_dispatches = 10
    max_dispatches_per_second = 5
  }

  retry_config {
    # Long window because "waiting for GitHub Environment approval" is implemented as a
    # retryable response. ApplyPolicy.ApprovalTimeout (default 24h) is the real bound; this
    # just has to be longer than the backoff schedule needs.
    max_attempts       = 200
    min_backoff        = "30s"
    max_backoff        = "600s"
    max_retry_duration = "90000s" # 25h
  }
}

# ---------------------------------------------------------------------------
# Secrets
# ---------------------------------------------------------------------------

resource "google_secret_manager_secret" "github_app_key" {
  secret_id = "github-app-key"
  replication {
    auto {}
  }
}

# Only the plan service can read this. The apply service has no reason to be able to forge a
# webhook delivery to a service that would then ask it to apply something.
resource "google_secret_manager_secret" "github_webhook_secret" {
  secret_id = "github-webhook-secret"
  replication {
    auto {}
  }
}

# ---------------------------------------------------------------------------
# Plan signing key
#
# Asymmetric, not HMAC. A shared secret would put material sufficient to forge plan provenance
# in the hands of the service whose job is to check it. Bucket IAM already stops the plan
# service overwriting a published plan; this signature is the layer that still holds when
# somebody widens a bucket binding by accident.
# ---------------------------------------------------------------------------

resource "google_kms_key_ring" "tf" {
  name     = "terraform-on-github"
  location = var.region
}

resource "google_kms_crypto_key" "plan_signing" {
  name     = "plan-signing"
  key_ring = google_kms_key_ring.tf.id
  purpose  = "ASYMMETRIC_SIGN"

  version_template {
    algorithm        = "EC_SIGN_P256_SHA256"
    protection_level = "SOFTWARE"
  }
}

# Coordination state lives in a GCS bucket, not a database — see storage.tf. The app claims runs
# and leases workspaces with ifGenerationMatch preconditions, the same primitive Terraform's own
# GCS backend uses to lock state.
