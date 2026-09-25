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
# Quesma's recipient is on by default, because sealing cannot be added after the fact: turning it on
# later covers only uploads from then on, so analytics over an organization's history needs it from
# the start. It grants nothing by itself -- Quesma reads no object unless it is explicitly granted
# access to the bucket, which this module never does -- and false here turns it off for every
# organization that did not choose. Telemetry is off unless a
# collector is named.
variable "default_allow_quesma_etl" {
  type        = bool
  default     = true
  description = "Seal an organization's future uploads to Quesma's built-in ETL recipient unless the organization turned it off. False opts every such organization out."
}
variable "default_telemetry_collector_url" {
  type        = string
  default     = ""
  description = "Where an organization that names no collector forwards shipper telemetry: a hostname or HTTPS URL, or empty for nowhere."
}
