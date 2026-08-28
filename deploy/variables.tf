variable "project_id" {
  description = "Project hosting the app itself (services, buckets, KMS). Separate from every target project."
  type        = string
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "github_app_id" {
  type = string
}

variable "plan_image" {
  description = "Fully qualified image for plan-service, with the Terraform CLI baked in."
  type        = string
}

variable "apply_image" {
  type = string
}

variable "provider_mirror_url" {
  description = <<-EOT
    Internal Terraform provider network mirror. Set as TF_CLI_CONFIG_FILE's network_mirror with
    no `direct` block, so only mirrored providers can be installed. This is what keeps the
    `external` provider — whose data source executes a local program at plan time — out of a
    sandbox holding live credentials. See DESIGN.md §8.
  EOT
  type        = string
}

variable "repos" {
  description = <<-EOT
    Onboarded repositories, keyed "owner/name", and the single ref each one's
    .terraform-on-github.yaml is read from.

    This lives here rather than in the repo because the trust anchor cannot be named by the thing
    it anchors: a repo whose own YAML declared its trusted ref could declare its own branch. It
    sits next to the tokenCreator bindings that grant the identities that config references — the
    same review surface, the same change control.

    Do NOT read config from a PR's base branch. In a feature -> integration -> prod flow,
    `integration` is writable by developers by design, so a developer can merge a workspace entry
    naming a production apply identity and have the next PR against `integration` apply with it.
    See DESIGN.md 3.1.1.

    config_ref empty means the repository's default branch. Pinning it explicitly is stronger,
    since a repo admin can change the default branch.
  EOT
  type = map(object({
    config_ref = optional(string, "")
  }))
  default = {}
}

variable "workspaces" {
  description = <<-EOT
    One entry per Terraform workspace any onboarded repo declares. Creates the two target
    service accounts and grants the runtime services tokenCreator on them individually.

    This list is the review surface for infrastructure access: adding a workspace is a
    reviewable Terraform change here, not a self-service edit to a repo's YAML. A repo can only
    name identities that already exist and that it has been granted.

    `plan_roles` should stay read-only. `apply_roles` should be curated per workspace, never
    roles/editor — a workspace that only manages DNS has no business being able to delete a
    database.
  EOT
  type = map(object({
    target_project = string
    state_bucket   = string
    state_prefix   = string
    plan_roles     = optional(list(string), ["roles/viewer"])
    apply_roles    = list(string)
  }))
}

variable "plan_bucket_name" {
  type = string
}

variable "coordination_bucket_name" {
  description = <<-EOT
    Bucket holding the run index, outstanding-apply markers, advisory apply leases and the webhook
    dedupe set — what replaces a database. Separate from the plan bucket so plan artifacts keep
    their write-once, unreadable-to-their-writer property while coordination gets the read-plus-CAS
    access it needs.
  EOT
  type        = string
}

variable "apply_log_bucket_name" {
  type = string
}

variable "log_retention_days" {
  description = "Lifecycle delete on plan artifacts. Plans embed a state snapshot, so this is a secret-retention window, not a storage-cost knob."
  type        = number
  default     = 90
}

variable "vpc_connector" {
  description = "Serverless VPC connector. Both services route all egress through it so outbound traffic hits an allowlist rather than the open internet."
  type        = string
}
