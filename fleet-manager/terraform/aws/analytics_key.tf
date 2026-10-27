# The key an organization's analytics identity is encrypted under (`deploy.sh aws analytics-key`).
# Anyone in the account may encrypt with it; nobody may decrypt until analytics_decrypt_role_arns
# names a processor. Neither CreateGrant nor ReEncrypt is granted, so the key policy is the only way in.

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

resource "aws_kms_key" "analytics" {
  count = var.analytics_key ? 1 : 0

  description             = "${var.name}: holds each organization's analytics identity; decrypts only for analytics_decrypt_role_arns"
  deletion_window_in_days = 30
  policy                  = data.aws_iam_policy_document.analytics_key[0].json
}

resource "aws_kms_alias" "analytics" {
  count = var.analytics_key ? 1 : 0

  name          = "alias/${var.name}-analytics"
  target_key_id = aws_kms_key.analytics[0].key_id
}

data "aws_iam_policy_document" "analytics_key" {
  count = var.analytics_key ? 1 : 0

  statement {
    sid = "AccountAdministers"
    actions = [
      "kms:Describe*", "kms:Get*", "kms:List*", "kms:Enable*", "kms:Disable*", "kms:Put*", "kms:Update*",
      "kms:CreateAlias", "kms:DeleteAlias", "kms:RevokeGrant", "kms:TagResource", "kms:UntagResource",
      "kms:ScheduleKeyDeletion", "kms:CancelKeyDeletion",
    ]
    resources = ["*"]
    principals {
      type        = "AWS"
      identifiers = ["arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root"]
    }
  }

  statement {
    sid       = "AccountEncryptsAnalyticsIdentities"
    actions   = ["kms:Encrypt"]
    resources = ["*"]
    principals {
      type        = "AWS"
      identifiers = ["arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root"]
    }
    condition {
      test     = "StringEquals"
      variable = "kms:EncryptionContext:purpose"
      values   = ["analytics-identity"]
    }
    condition {
      test     = "ForAllValues:StringEquals"
      variable = "kms:EncryptionContextKeys"
      values   = ["purpose", "organization"]
    }
  }

  dynamic "statement" {
    for_each = length(var.analytics_decrypt_role_arns) > 0 ? [1] : []
    content {
      sid       = "ProcessorsDecryptAnalyticsIdentities"
      actions   = ["kms:Decrypt"]
      resources = ["*"]
      principals {
        type        = "AWS"
        identifiers = var.analytics_decrypt_role_arns
      }
      condition {
        test     = "StringEquals"
        variable = "kms:EncryptionContext:purpose"
        values   = ["analytics-identity"]
      }
    }
  }
}
