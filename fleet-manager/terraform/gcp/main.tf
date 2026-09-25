terraform {
  required_version = ">= 1.5"
  required_providers {
    http   = { source = "hashicorp/http", version = "~> 3.4" }
    google = { source = "hashicorp/google", version = "~> 7.0" }
    random = { source = "hashicorp/random", version = "~> 3.7" }
  }
}

provider "google" {
  project = var.project
  region  = var.region
}

locals {
  bucket_name            = coalesce(var.bucket, "${var.project}-trajectories")
  object_root            = "projects/_/buckets/${local.bucket_name}/objects/"
  admin_credential       = "fma1.${replace(replace(trimsuffix(random_bytes.admin_credential.base64, "="), "+", "-"), "/", "_")}"
  admin_credential_key   = "v1/control/admin/credential.json"
  telemetry_identity_key = "private/fleet-manager/telemetry-identity.json"

  # The reporter credential may report collection health and do nothing else. It exists so that
  # whatever reads the archive and tells fleet manager what it found cannot also create
  # organizations, revoke installs or rewrite served configuration. Its prefix differs so a
  # credential pasted into the wrong place is obvious at a glance.
  reporter_credential     = "fmr1.${replace(replace(trimsuffix(random_bytes.reporter_credential.base64, "="), "+", "-"), "/", "_")}"
  reporter_credential_key = "v1/control/reporter/credential.json"
  services = toset([
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "run.googleapis.com",
    "storage.googleapis.com",
  ])
}

resource "random_bytes" "admin_credential" {
  length = 32
}

resource "random_bytes" "reporter_credential" {
  length = 32
}

resource "google_project_service" "required" {
  for_each           = local.services
  project            = var.project
  service            = each.value
  disable_on_destroy = false
}

resource "google_storage_bucket" "fleet" {
  name                        = local.bucket_name
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  versioning { enabled = true }
  depends_on = [google_project_service.required]
}

resource "google_storage_bucket_object" "admin_credential" {
  bucket       = google_storage_bucket.fleet.name
  name         = local.admin_credential_key
  content_type = "application/json"
  content = jsonencode({
    schema        = 1
    secret_digest = sha256(local.admin_credential)
  })
}

# Same record, same rotation: replace the random resource and apply, and the old one stops working.
# A deployment that never reads this output simply has no reporter, and every reporter credential
# presented to it fails.
resource "google_storage_bucket_object" "reporter_credential" {
  bucket       = google_storage_bucket.fleet.name
  name         = local.reporter_credential_key
  content_type = "application/json"
  content = jsonencode({
    schema        = 1
    secret_digest = sha256(local.reporter_credential)
  })
}

resource "google_service_account" "fleet_manager" {
  account_id   = var.name
  display_name = "Fleet manager runtime"
  depends_on   = [google_project_service.required]
}

resource "google_project_iam_custom_role" "control" {
  role_id     = "${replace(var.name, "-", "_")}_control"
  title       = "Fleet manager control objects"
  permissions = ["storage.objects.get", "storage.objects.list", "storage.objects.create", "storage.objects.update", "storage.objects.delete"]
}

resource "google_project_iam_custom_role" "data" {
  role_id     = "${replace(var.name, "-", "_")}_data"
  title       = "Fleet manager trajectory writes"
  permissions = ["storage.objects.create", "storage.objects.delete"]
}

# Separate from the data role rather than a `get` added to it: that role covers every object under
# an install's root, and this one may only ever read the name.
resource "google_project_iam_custom_role" "names" {
  role_id     = "${replace(var.name, "-", "_")}_names"
  title       = "Fleet manager install names"
  permissions = ["storage.objects.get"]
}

# Read, never write: this Terraform provisions both credential records, and the service only
# compares a presented credential with the digest. A runtime that could write them could mint itself
# a new administrator.
resource "google_project_iam_custom_role" "credentials" {
  role_id     = "${replace(var.name, "-", "_")}_credentials"
  title       = "Fleet manager credential records"
  permissions = ["storage.objects.get"]
}

resource "google_project_iam_custom_role" "versioning" {
  role_id     = "${replace(var.name, "-", "_")}_versioning"
  title       = "Fleet manager bucket version verification"
  permissions = ["storage.buckets.get"]
}

resource "google_storage_bucket_iam_member" "control" {
  bucket = google_storage_bucket.fleet.name
  role   = google_project_iam_custom_role.control.name
  member = "serviceAccount:${google_service_account.fleet_manager.email}"
  condition {
    title      = "control-prefix-only"
    expression = "resource.name.matches('^${local.object_root}v1/organization=[a-z0-9][a-z0-9._-]{0,63}/control/.*$') || resource.name == '${local.object_root}${local.telemetry_identity_key}' || api.getAttribute('storage.googleapis.com/objectListPrefix', '').matches('^v1/organization=[a-z0-9][a-z0-9._-]{0,63}/control/.*$') || api.getAttribute('storage.googleapis.com/objectListPrefix', '') == 'v1/organization='"
  }
}

resource "google_storage_bucket_iam_member" "data" {
  bucket = google_storage_bucket.fleet.name
  role   = google_project_iam_custom_role.data.name
  member = "serviceAccount:${google_service_account.fleet_manager.email}"
  condition {
    title      = "install-prefix-write-only"
    expression = "resource.name.matches('^${local.object_root}v1/organization=[a-z0-9][a-z0-9._-]{0,63}/install=[0-9a-f-]{36}/.*$')"
  }
}

resource "google_storage_bucket_iam_member" "names" {
  bucket = google_storage_bucket.fleet.name
  role   = google_project_iam_custom_role.names.name
  member = "serviceAccount:${google_service_account.fleet_manager.email}"
  condition {
    title      = "install-names-only"
    expression = "resource.name.matches('^${local.object_root}v1/organization=[a-z0-9][a-z0-9._-]{0,63}/install=[0-9a-f-]{36}/tags\\.json$')"
  }
}

resource "google_storage_bucket_iam_member" "credentials" {
  bucket = google_storage_bucket.fleet.name
  role   = google_project_iam_custom_role.credentials.name
  member = "serviceAccount:${google_service_account.fleet_manager.email}"
  condition {
    title      = "credential-records-read-only"
    expression = "resource.name == '${local.object_root}${local.admin_credential_key}' || resource.name == '${local.object_root}${local.reporter_credential_key}'"
  }
}

resource "google_storage_bucket_iam_member" "versioning" {
  bucket = google_storage_bucket.fleet.name
  role   = google_project_iam_custom_role.versioning.name
  member = "serviceAccount:${google_service_account.fleet_manager.email}"
}

resource "google_service_account_iam_member" "signer" {
  service_account_id = google_service_account.fleet_manager.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${google_service_account.fleet_manager.email}"
}

resource "google_cloud_run_v2_service" "fleet_manager" {
  name     = var.name
  location = var.region
  template {
    service_account = google_service_account.fleet_manager.email
    containers {
      image = var.image
      startup_probe {
        initial_delay_seconds = 0
        timeout_seconds       = 2
        period_seconds        = 3
        failure_threshold     = 20
        http_get {
          path = "/health"
          port = 8080
        }
      }
      env {
        name  = "FLEET_MANAGER_PROVIDER"
        value = "gcp"
      }
      env {
        name  = "FLEET_MANAGER_BUCKET"
        value = google_storage_bucket.fleet.name
      }
      env {
        name  = "FLEET_MANAGER_ACCOUNT"
        value = google_service_account.fleet_manager.email
      }
      env {
        name  = "FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL"
        value = tostring(var.default_allow_quesma_etl)
      }
      env {
        name  = "FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL"
        value = var.default_telemetry_collector_url
      }
    }
  }
  depends_on = [google_storage_bucket_iam_member.control, google_storage_bucket_iam_member.credentials, google_storage_bucket_iam_member.data, google_storage_bucket_iam_member.versioning, google_service_account_iam_member.signer, google_storage_bucket_object.admin_credential]
}

resource "google_cloud_run_v2_service_iam_member" "public" {
  project  = google_cloud_run_v2_service.fleet_manager.project
  location = google_cloud_run_v2_service.fleet_manager.location
  name     = google_cloud_run_v2_service.fleet_manager.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# Read only public registration material after the service has provisioned its durable identity.
data "http" "fleet_manager_public_key" {
  url                = "${trimsuffix(google_cloud_run_v2_service.fleet_manager.uri, "/")}/v1/telemetry/public-key"
  request_timeout_ms = 5000
  retry {
    attempts     = 12
    min_delay_ms = 1000
    max_delay_ms = 5000
  }
  lifecycle {
    postcondition {
      condition     = self.status_code == 200
      error_message = "Fleet-manager must be running a telemetry-capable image and expose its public key."
    }
  }
  depends_on = [google_cloud_run_v2_service.fleet_manager, google_cloud_run_v2_service_iam_member.public]
}
