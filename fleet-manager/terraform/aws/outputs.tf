output "service_url" { value = aws_ecs_express_gateway_service.fleet_manager.ingress_paths[0].endpoint }
output "admin_url" { value = "${aws_ecs_express_gateway_service.fleet_manager.ingress_paths[0].endpoint}/admin/" }
output "admin_credential" {
  value     = local.admin_credential
  sensitive = true
}
output "reporter_credential" {
  value     = local.reporter_credential
  sensitive = true
}
output "bucket" { value = aws_s3_bucket.fleet.bucket }
output "provider" { value = "aws" }
output "region" { value = var.region }

output "etl_reader_role_arns" {
  description = "Who outside this account may read the sealed objects. Empty unless the deployment granted someone."
  value       = var.etl_reader_role_arns
}

output "fleet_manager_public_key" {
  description = "Base64 Ed25519 public key to register with the telemetry collector."
  value       = jsondecode(data.http.fleet_manager_public_key.response_body).public_key
}

output "analytics_key_arn" {
  description = "The KMS key analytics identities are encrypted under, or empty when analytics_key is off."
  value       = var.analytics_key ? aws_kms_key.analytics[0].arn : ""
}
