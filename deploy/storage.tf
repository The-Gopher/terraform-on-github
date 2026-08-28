# ---------------------------------------------------------------------------
# Plan artifact bucket.
#
# A Terraform plan file embeds a snapshot of prior state. Every secret in state is in the plan.
# So this bucket gets the same treatment as a state bucket, and the retention window is a
# secret-exposure window rather than a storage-cost decision.
# ---------------------------------------------------------------------------

resource "google_storage_bucket" "plans" {
  name     = var.plan_bucket_name
  location = var.region

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  versioning {
    enabled = true
  }

  # Object versioning plus write-once IAM (below) means a published plan cannot be replaced.
  lifecycle_rule {
    condition {
      age = var.log_retention_days
    }
    action {
      type = "Delete"
    }
  }

  lifecycle_rule {
    condition {
      num_newer_versions = 1
      age                = 7
    }
    action {
      type = "Delete"
    }
  }

  encryption {
    default_kms_key_name = google_kms_crypto_key.plan_artifacts.id
  }
}

resource "google_kms_crypto_key" "plan_artifacts" {
  name            = "plan-artifacts"
  key_ring        = google_kms_key_ring.tf.id
  purpose         = "ENCRYPT_DECRYPT"
  rotation_period = "7776000s" # 90d
}

# ---------------------------------------------------------------------------
# Coordination bucket: the run index, the outstanding-apply markers, the advisory apply leases
# and the webhook dedupe set. This is what replaces a database.
#
# Everything the app needs to coordinate is a single-key compare-and-swap — claim a run,
# transition it, steal an expired lease, record a delivery id — and `ifGenerationMatch` is
# exactly that. It is the same primitive Terraform's GCS backend uses for its own state lock, so
# the app coordinates itself the way the tool it wraps already does.
#
# What a database would still buy: real queries. The one question that is not a single-key lookup
# is "which runs are waiting to apply", and it is answered by object naming instead — a marker
# under pending-apply/ per outstanding run, listed by prefix. That works because GCS list is
# strongly consistent, and it costs a schema that lives in key names.
# ---------------------------------------------------------------------------

resource "google_storage_bucket" "coordination" {
  name     = var.coordination_bucket_name
  location = var.region

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  # No versioning: these objects are mutable by design, and their whole purpose is that the
  # current generation is the truth. Versioning would retain every lease renewal.

  # Webhook dedupe expiry. Day-granular and asynchronous, so real retention is one to two days —
  # coarser than a TTL field and entirely adequate: the window only has to outlive GitHub's retry
  # schedule, and over-retention costs a few kilobytes.
  lifecycle_rule {
    condition {
      age            = 1
      matches_prefix = ["deliveries/"]
    }
    action {
      type = "Delete"
    }
  }

  # Run index entries outlive the plan artifacts they describe, so a late question about an old
  # merge is still answerable after the plan itself has aged out.
  lifecycle_rule {
    condition {
      age            = 400
      matches_prefix = ["runs/"]
    }
    action {
      type = "Delete"
    }
  }
}

# Both services need read plus create plus overwrite here — that is what CAS is. objectAdmin is
# the least role that permits it; there is no "create and conditionally replace" role.
#
# The grant is wider than the artifact-bucket grant on purpose, and it is affordable precisely
# because the artifacts are elsewhere: this bucket holds SHAs, states and timestamps, and no
# plan output, so no state snapshot and no secret.
resource "google_storage_bucket_iam_member" "plan_coordination" {
  bucket = google_storage_bucket.coordination.name
  role   = "roles/storage.objectAdmin"
  member = google_service_account.plan.member
}

resource "google_storage_bucket_iam_member" "apply_coordination" {
  bucket = google_storage_bucket.coordination.name
  role   = "roles/storage.objectAdmin"
  member = google_service_account.apply.member
}

resource "google_storage_bucket" "apply_logs" {
  name     = var.apply_log_bucket_name
  location = var.region

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  lifecycle_rule {
    condition {
      age = 365
    }
    action {
      type = "Delete"
    }
  }
}

# ---------------------------------------------------------------------------
# The write-once boundary.
#
# roles/storage.objectCreator can create an object and cannot read one or overwrite an existing
# name. Granting the plan service that role and nothing else gives three properties for free:
#
#   1. It cannot swap a plan after publishing the check — the artifact a reviewer approved is
#      the artifact the apply service will read.
#   2. It cannot read plans belonging to other workspaces or other repos, so a compromised plan
#      run cannot harvest state snapshots from the whole estate.
#   3. Duplicate workers collide on a precondition failure rather than racing.
#
# The consequence is that cache-hit detection cannot be an existence check against *this* bucket
# — the plan service is not permitted to look. The coordination bucket below is the authority on
# "already planned"; this bucket is append-only cold storage. See DESIGN.md §4.2.
#
# Keeping the two in separate buckets is what lets both properties hold at once: artifacts stay
# write-once and unreadable to their writer, while coordination gets the read-plus-CAS access it
# needs. One bucket could not be both.
# ---------------------------------------------------------------------------

resource "google_storage_bucket_iam_member" "plan_writes_plans" {
  bucket = google_storage_bucket.plans.name
  role   = "roles/storage.objectCreator"
  member = google_service_account.plan.member
}

resource "google_storage_bucket_iam_member" "apply_reads_plans" {
  bucket = google_storage_bucket.plans.name
  role   = "roles/storage.objectViewer"
  member = google_service_account.apply.member
}

# Signed URLs for the human-readable plan text. The check summary renders addresses only by
# default; the full diff lives behind a 15-minute signed URL rather than in the PR timeline
# forever. Signing a URL needs the SA to sign a blob as itself.
resource "google_service_account_iam_member" "plan_self_sign" {
  service_account_id = google_service_account.plan.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = google_service_account.plan.member
}

resource "google_storage_bucket_iam_member" "apply_writes_logs" {
  bucket = google_storage_bucket.apply_logs.name
  role   = "roles/storage.objectAdmin"
  member = google_service_account.apply.member
}

# Plans and applies are read from these buckets by humans during incidents; log it.
resource "google_project_iam_audit_config" "storage_data_access" {
  project = var.project_id
  service = "storage.googleapis.com"

  audit_log_config {
    log_type = "DATA_READ"
  }
  audit_log_config {
    log_type = "DATA_WRITE"
  }
}
