terraform {
  required_version = ">= 1.5"
  required_providers {
    http   = { source = "hashicorp/http", version = "~> 3.4" }
    aws    = { source = "hashicorp/aws", version = "~> 6.50" }
    random = { source = "hashicorp/random", version = "~> 3.7" }
  }
}

provider "aws" { region = var.region }

locals {
  control_prefix         = "v1/organization=*/control/"
  data_prefix            = "v1/organization=*/install="
  admin_credential       = "fma1.${replace(replace(trimsuffix(random_bytes.admin_credential.base64, "="), "+", "-"), "/", "_")}"
  admin_credential_key   = "v1/control/admin/credential.json"
  telemetry_identity_key = "private/fleet-manager/telemetry-identity.json"

  # The reporter credential may report collection health and do nothing else. It exists so that
  # whatever reads the archive and tells fleet manager what it found cannot also create
  # organizations, revoke installs or rewrite served configuration. Its prefix differs so a
  # credential pasted into the wrong place is obvious at a glance.
  reporter_credential     = "fmr1.${replace(replace(trimsuffix(random_bytes.reporter_credential.base64, "="), "+", "-"), "/", "_")}"
  reporter_credential_key = "v1/control/reporter/credential.json"
}

resource "random_bytes" "admin_credential" {
  length = 32
}

resource "random_bytes" "reporter_credential" {
  length = 32
}

resource "aws_s3_bucket" "fleet" {
  bucket = var.bucket
}

resource "aws_s3_bucket_versioning" "fleet" {
  bucket = aws_s3_bucket.fleet.id
  versioning_configuration { status = "Enabled" }
}

# Telemetry is rewritten as shippers check in. Versioning keeps every one of those writes, so the
# service tags them and this rule expires the superseded copies. A prefix rule cannot do the job:
# the organization sits in the middle of the key and S3 prefixes take no wildcard.
resource "aws_s3_bucket_lifecycle_configuration" "ephemeral" {
  bucket = aws_s3_bucket.fleet.id
  rule {
    id     = "expire-superseded-ephemeral-records"
    status = "Enabled"
    filter {
      tag {
        key   = "lifecycle"
        value = "ephemeral"
      }
    }
    # Only superseded versions, and the clock starts when one is superseded: an install that goes
    # quiet keeps its last report for as long as it stays the current version. One day is the floor.
    noncurrent_version_expiration { noncurrent_days = 1 }
  }
  depends_on = [aws_s3_bucket_versioning.fleet]
}

resource "aws_s3_bucket_public_access_block" "fleet" {
  bucket                  = aws_s3_bucket.fleet.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_object" "admin_credential" {
  bucket       = aws_s3_bucket.fleet.id
  key          = local.admin_credential_key
  content_type = "application/json"
  content = jsonencode({
    schema        = 1
    secret_digest = sha256(local.admin_credential)
  })
  depends_on = [aws_s3_bucket_versioning.fleet]
}

# Same record, same rotation: replace the random resource and apply, and the old one stops working.
# A deployment that never reads this output simply has no reporter, and every reporter credential
# presented to it fails.
resource "aws_s3_object" "reporter_credential" {
  bucket       = aws_s3_bucket.fleet.id
  key          = local.reporter_credential_key
  content_type = "application/json"
  content = jsonencode({
    schema        = 1
    secret_digest = sha256(local.reporter_credential)
  })
  depends_on = [aws_s3_bucket_versioning.fleet]
}

data "aws_iam_policy_document" "ecs_task_assume_role" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  name_prefix        = "${var.name}-execution-"
  assume_role_policy = data.aws_iam_policy_document.ecs_task_assume_role.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role" "fleet_manager" {
  name               = "${var.name}-runtime"
  assume_role_policy = data.aws_iam_policy_document.ecs_task_assume_role.json
}

resource "aws_iam_role_policy" "objects" {
  role = aws_iam_role.fleet_manager.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "ControlObjects"
        Effect = "Allow"
        # PutObjectTagging is what lets a tagged write through; without it S3 refuses the whole PUT.
        Action = ["s3:GetObject", "s3:PutObject", "s3:PutObjectTagging"]
        Resource = [
          "${aws_s3_bucket.fleet.arn}/${local.control_prefix}*",
          "${aws_s3_bucket.fleet.arn}/${local.telemetry_identity_key}",
        ]
      },
      {
        # Read, never write: this Terraform provisions both credential records, and the service
        # only compares a presented credential with the digest. A runtime that could write them
        # could mint itself a new administrator.
        Sid    = "CredentialsRead"
        Effect = "Allow"
        Action = "s3:GetObject"
        Resource = [
          "${aws_s3_bucket.fleet.arn}/${local.admin_credential_key}",
          "${aws_s3_bucket.fleet.arn}/${local.reporter_credential_key}",
        ]
      },
      {
        Sid       = "ControlList"
        Effect    = "Allow"
        Action    = "s3:ListBucket"
        Resource  = aws_s3_bucket.fleet.arn
        Condition = { StringLike = { "s3:prefix" = ["v1/organization=", "${local.control_prefix}*", local.admin_credential_key, local.reporter_credential_key, local.telemetry_identity_key] } }
      },
      {
        # S3 answers a read of an absent object with 403 rather than 404 unless the caller may list
        # that key, and naming an install starts by reading a name that is not there yet. The prefix
        # is the one object name, so the listing reveals nothing about the payloads beside it.
        Sid       = "InstallNamesAbsence"
        Effect    = "Allow"
        Action    = "s3:ListBucket"
        Resource  = aws_s3_bucket.fleet.arn
        Condition = { StringLike = { "s3:prefix" = "${local.data_prefix}*/tags.json" } }
      },
      {
        Sid      = "TrajectoryWriteAndTagOnly"
        Effect   = "Allow"
        Action   = ["s3:PutObject", "s3:PutObjectTagging"]
        Resource = "${aws_s3_bucket.fleet.arn}/${local.data_prefix}*"
      },
      {
        # One object name, never the prefix: names are read back from each install's own root, and
        # a grant over that prefix would be read access to every sealed payload in the bucket.
        Sid      = "InstallNamesRead"
        Effect   = "Allow"
        Action   = "s3:GetObject"
        Resource = "${aws_s3_bucket.fleet.arn}/${local.data_prefix}*/tags.json"
      },
      {
        Sid      = "VerifyBucketVersioning"
        Effect   = "Allow"
        Action   = "s3:GetBucketVersioning"
        Resource = aws_s3_bucket.fleet.arn
      }
    ]
  })
}

data "aws_iam_policy_document" "ecs_infrastructure_assume_role" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "infrastructure" {
  name_prefix        = "${var.name}-infrastructure-"
  assume_role_policy = data.aws_iam_policy_document.ecs_infrastructure_assume_role.json
}

resource "aws_iam_role_policy_attachment" "infrastructure" {
  role       = aws_iam_role.infrastructure.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSInfrastructureRoleforExpressGatewayServices"
}

resource "aws_cloudwatch_log_group" "fleet_manager" {
  name              = "/ecs/${var.name}"
  retention_in_days = 30
}

resource "aws_ecs_express_gateway_service" "fleet_manager" {
  service_name            = var.name
  execution_role_arn      = aws_iam_role.execution.arn
  infrastructure_role_arn = aws_iam_role.infrastructure.arn
  task_role_arn           = aws_iam_role.fleet_manager.arn
  cpu                     = "256"
  memory                  = "512"
  health_check_path       = "/healthz"
  wait_for_steady_state   = true

  primary_container {
    image          = var.image_uri
    container_port = 8080
    # AWS returns an omitted command as an empty list. Keep the planned value identical so the
    # provider does not report a null-to-empty-list inconsistency after waiting for deployment.
    command = []
    aws_logs_configuration = [{
      log_group         = aws_cloudwatch_log_group.fleet_manager.name
      log_stream_prefix = "fleet-manager"
    }]

    environment {
      name  = "PORT"
      value = "8080"
    }
    environment {
      name  = "FLEET_MANAGER_PROVIDER"
      value = "aws"
    }
    environment {
      name  = "FLEET_MANAGER_BUCKET"
      value = aws_s3_bucket.fleet.bucket
    }
    environment {
      name  = "FLEET_MANAGER_REGION"
      value = var.region
    }
    environment {
      name  = "FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL"
      value = tostring(var.default_allow_quesma_etl)
    }
    environment {
      name  = "FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL"
      value = var.default_telemetry_collector_url
    }
  }

  depends_on = [
    aws_iam_role_policy.objects,
    aws_iam_role_policy_attachment.execution,
    aws_iam_role_policy_attachment.infrastructure,
    aws_s3_object.admin_credential,
    aws_s3_bucket_versioning.fleet,
  ]
}

# Read for an ETL, and nothing else: no write, no delete, no bucket configuration, and of the objects
# only the two kinds an ETL reads -- everything under an install's root, and each organization's own
# config.json, which carries its display name. The rest of the control prefix is invites, grants,
# install records and the credential digests, which a reader of trajectories has no business seeing.
#
# Current versions only. The reader notices a re-shipped object by its ETag and never asks for a
# version id, so ListBucketVersions and GetObjectVersion would be a grant over every superseded
# generation that nothing would ever use.
locals {
  etl_readable_objects = [
    "${aws_s3_bucket.fleet.arn}/${local.data_prefix}*",
    "${aws_s3_bucket.fleet.arn}/v1/organization=*/control/config.json",
  ]
}

data "aws_iam_policy_document" "etl_readers" {
  count = length(var.etl_reader_role_arns) > 0 ? 1 : 0

  # Two statements where one would read better: s3:prefix is a condition ListBucket understands and
  # GetBucketLocation does not, and S3 refuses a statement whose condition applies to only some of
  # its actions ("Conditions do not apply to combination of actions and resources").
  statement {
    sid       = "EtlReadersList"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.fleet.arn]

    principals {
      type        = "AWS"
      identifiers = var.etl_reader_role_arns
    }

    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["v1/*", "v1/"]
    }
  }

  statement {
    sid       = "EtlReadersLocation"
    actions   = ["s3:GetBucketLocation"]
    resources = [aws_s3_bucket.fleet.arn]

    principals {
      type        = "AWS"
      identifiers = var.etl_reader_role_arns
    }
  }

  statement {
    sid       = "EtlReadersGet"
    actions   = ["s3:GetObject"]
    resources = local.etl_readable_objects

    principals {
      type        = "AWS"
      identifiers = var.etl_reader_role_arns
    }
  }

  # The Allow above is all a reader in another account gets. A reader in this account may also hold
  # a grant of its own -- an ingest role allowed the whole archive prefix -- and an Allow here cannot
  # take that away; only a Deny can. So every other object is refused to these principals outright,
  # whatever their own policies say.
  statement {
    sid           = "EtlReadersNothingElse"
    effect        = "Deny"
    actions       = ["s3:GetObject"]
    not_resources = local.etl_readable_objects

    principals {
      type        = "AWS"
      identifiers = var.etl_reader_role_arns
    }
  }
}

resource "aws_s3_bucket_policy" "etl_readers" {
  count = length(var.etl_reader_role_arns) > 0 ? 1 : 0

  bucket = aws_s3_bucket.fleet.id
  policy = data.aws_iam_policy_document.etl_readers[0].json
}

# Read only public registration material after the service has provisioned its durable identity.
data "http" "fleet_manager_public_key" {
  url                = "${trimsuffix(aws_ecs_express_gateway_service.fleet_manager.ingress_paths[0].endpoint, "/")}/v1/telemetry/public-key"
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
  depends_on = [aws_ecs_express_gateway_service.fleet_manager]
}
