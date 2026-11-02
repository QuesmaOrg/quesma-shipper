variable "project" { type = string }
variable "region" {
  type    = string
  default = "europe-central2"
}
variable "name" {
  type    = string
  default = "fleet-manager"
}
variable "bucket" {
  type        = string
  default     = null
  description = "Bucket name; defaults to <project>-trajectories."
}
variable "image" {
  type    = string
  default = "docker.io/quesma/fleet-manager:latest"
}

# What an organization gets for a setting it never stated. An organization's own setting always
# wins, and these reach every organization that left it unset, including ones created before the
# setting existed.
#
# Quesma's recipient is off by default; true here turns it on for every organization that did not
# choose. History uploaded while it was off can be opened for Quesma later by a custodian resealing
# it (quesma-unseal -reseal-to). Telemetry is off unless a collector is named.
variable "default_allow_quesma_etl" {
  type        = bool
  default     = false
  description = "Seal the future uploads of every organization that did not choose to Quesma's built-in ETL recipient. An organization's own allow_quesma_etl always wins."
}
variable "default_telemetry_collector_url" {
  type        = string
  default     = ""
  description = "Where an organization that names no collector forwards shipper telemetry: a hostname or HTTPS URL, or empty for nowhere."
}
