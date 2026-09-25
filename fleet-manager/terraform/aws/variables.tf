variable "name" {
  type    = string
  default = "fleet-manager"
}
variable "bucket" { type = string }
variable "region" { type = string }
variable "image_uri" {
  type        = string
  default     = "docker.io/quesma/fleet-manager:latest"
  description = "Fleet manager container image."
}

# Who, besides this service, may READ the sealed objects in the bucket. Empty by default: a fleet
# manager deployment holds your trajectories and grants nobody.
#
# This is what lets an ETL that runs elsewhere -- in another account, as Quesma's does when you use
# its dashboards -- read the bucket directly while the shipper, this service and the bucket stay in
# yours. It is a bucket policy rather than a role to assume, because a bucket policy needs no code
# on either side: the reader presents its own role and S3 checks this list.
#
# It grants read, and read is not the same as decrypt. Every object is sealed to the
# organization's age recipients, and the reader can open them only because the organization keeps
# Quesma's built-in recipient -- the `allow_quesma_etl` setting, which is on unless the organization
# or `default_allow_quesma_etl` below turns it off, and separately revocable. Removing the
# recipient stops the decryption; emptying this list stops the reading.
#
# The ARNs are role ARNs, one per ETL that reads this bucket, supplied by whoever runs it.
variable "etl_reader_role_arns" {
  type        = list(string)
  default     = []
  description = "Role ARNs permitted to list the bucket below v1/ and read what is under each install's root and each organization's control/config.json, and nothing else."
}

# What an organization gets for a setting it never stated. An organization's own setting always
# wins, and these reach every organization that left it unset, including ones created before the
# setting existed.
#
# Quesma's recipient is on by default, because sealing cannot be added after the fact: turning it on
# later covers only uploads from then on, so analytics over an organization's history needs it from
# the start. It grants nothing by itself -- Quesma reads no object without `etl_reader_role_arns` --
# and false here turns it off for every organization that did not choose. Telemetry is off unless a
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
