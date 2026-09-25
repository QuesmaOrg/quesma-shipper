output "service_url" { value = google_cloud_run_v2_service.fleet_manager.uri }
output "admin_url" { value = "${google_cloud_run_v2_service.fleet_manager.uri}/admin/" }
output "reporter_credential" {
  value     = local.reporter_credential
  sensitive = true
}
output "admin_credential" {
  value     = local.admin_credential
  sensitive = true
}
output "bucket" { value = google_storage_bucket.fleet.name }
output "provider" { value = "gcp" }
output "region" { value = var.region }
output "account" { value = google_service_account.fleet_manager.email }
output "service_account" { value = google_service_account.fleet_manager.email }

output "fleet_manager_public_key" {
  description = "Base64 Ed25519 public key to register with the telemetry collector."
  value       = jsondecode(data.http.fleet_manager_public_key.response_body).public_key
}
