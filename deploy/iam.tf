# ---------------------------------------------------------------------------
# Runtime service permissions in the app's own project.
#
# Read this file next to DESIGN.md §7.1 and §7.2. What is absent matters as much as what is
# present, so the deliberate omissions are stated at the bottom rather than left implicit.
# ---------------------------------------------------------------------------

# ---- plan service ---------------------------------------------------------

resource "google_secret_manager_secret_iam_member" "plan_app_key" {
  secret_id = google_secret_manager_secret.github_app_key.id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.plan.member
}

# Only the plan service reads the webhook secret. The apply service having it would let a
# compromised apply service forge a delivery to the plan service — which would then ask the
# apply service to apply something. Removing the capability removes the loop.
resource "google_secret_manager_secret_iam_member" "plan_webhook_secret" {
  secret_id = google_secret_manager_secret.github_webhook_secret.id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.plan.member
}

# Sign, never verify-with-private-key. Paired with apply_verify_plans below.
resource "google_kms_crypto_key_iam_member" "plan_signs" {
  crypto_key_id = google_kms_crypto_key.plan_signing.id
  role          = "roles/cloudkms.signer"
  member        = google_service_account.plan.member
}

resource "google_kms_crypto_key_iam_member" "plan_encrypts_artifacts" {
  crypto_key_id = google_kms_crypto_key.plan_artifacts.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = google_service_account.plan.member
}

resource "google_cloud_tasks_queue_iam_member" "plan_enqueues_plans" {
  name     = google_cloud_tasks_queue.plan.name
  location = var.region
  role     = "roles/cloudtasks.enqueuer"
  member   = google_service_account.plan.member
}

# The one edge the two-service split does not eliminate.
#
# Because the webhook receiver lives in the plan service, the plan service can enqueue an apply.
# Two things make that acceptable, and you should have both:
#
#   1. apply.Verify re-derives authorization from GitHub and from the KMS-signed plan metadata.
#      A forged task cannot survive it — the worst outcome is a verification failure.
#   2. The reconcile sweep in services.tf is an independent path to the same applies.
#
# If you want the edge gone entirely, delete this binding and the actAs below: /reconcile becomes
# the only route to an apply, at the cost of up to a minute of latency. See DESIGN.md §7.4.
resource "google_cloud_tasks_queue_iam_member" "plan_enqueues_applies" {
  name     = google_cloud_tasks_queue.apply.name
  location = var.region
  role     = "roles/cloudtasks.enqueuer"
  member   = google_service_account.plan.member
}

# Attaching an OIDC token to a task requires actAs on the identity being minted.
resource "google_service_account_iam_member" "plan_acts_as_plan_invoker" {
  service_account_id = google_service_account.plan_invoker.name
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.plan.member
}

resource "google_service_account_iam_member" "plan_acts_as_apply_invoker" {
  service_account_id = google_service_account.apply_invoker.name
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.plan.member
}

# ---- apply service --------------------------------------------------------

resource "google_secret_manager_secret_iam_member" "apply_app_key" {
  secret_id = google_secret_manager_secret.github_app_key.id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.apply.member
}

# Verify only: fetch the public key and check signatures locally. No signer role, so the service
# whose job is to validate plan provenance cannot produce it. Verification also costs no KMS call
# and so cannot be made to fail open by a KMS outage.
resource "google_kms_crypto_key_iam_member" "apply_verify_plans" {
  crypto_key_id = google_kms_crypto_key.plan_signing.id
  role          = "roles/cloudkms.publicKeyViewer"
  member        = google_service_account.apply.member
}

resource "google_kms_crypto_key_iam_member" "apply_decrypts_artifacts" {
  crypto_key_id = google_kms_crypto_key.plan_artifacts.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = google_service_account.apply.member
}

# The apply service can enqueue only to its own queue, and only from /reconcile — needed so a
# sweep can schedule applies with backoff rather than blocking on approval inside one request.
resource "google_cloud_tasks_queue_iam_member" "apply_enqueues_applies" {
  name     = google_cloud_tasks_queue.apply.name
  location = var.region
  role     = "roles/cloudtasks.enqueuer"
  member   = google_service_account.apply.member
}

resource "google_service_account_iam_member" "apply_acts_as_apply_invoker" {
  service_account_id = google_service_account.apply_invoker.name
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.apply.member
}

# ---------------------------------------------------------------------------
# Deliberately absent. Each of these would collapse part of the design; if a future change needs
# one, that is the moment to revisit the whole boundary rather than add the binding.
#
#   tf-plan@   roles/run.invoker on tf-apply           — every apply must arrive via the queue,
#                                                        so every apply passes apply.Verify.
#   tf-plan@   tokenCreator on any *-apply SA          — the tier that runs untrusted PR code
#                                                        must never reach a writer credential.
#   tf-plan@   objectViewer on the plan bucket         — write-once, and no harvesting other
#                                                        workspaces' state snapshots.
#   tf-plan@   any role on any target project          — access is only ever via impersonation.
#   tf-apply@  secretAccessor on the webhook secret    — cannot forge a delivery that would ask
#                                                        for its own invocation.
#   tf-apply@  cloudkms.signer on plan-signing         — cannot forge the provenance it checks.
#   tf-apply@  objectCreator on the plan bucket        — cannot author or alter a plan.
#   either     any access to the other's target SAs     — the two tiers never overlap.
#
# Both services do hold objectAdmin on the *coordination* bucket, which is wider than anything
# above. That is affordable only because the coordination bucket holds SHAs, states and timestamps
# and no plan output — no state snapshot, no secret. Putting a plan artifact in there would quietly
# undo §4.2, so don't.
#   tf-apply@  tokenCreator on any *-plan SA           — keeps the read-only tier's audit trail
#                                                        attributable to one service.
#   either     roles/editor, roles/owner anywhere      — obviously, but say it out loud.
# ---------------------------------------------------------------------------
