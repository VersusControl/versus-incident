terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

# Trust policy: only the Versus principal, and only with the shared ExternalId.
data "aws_iam_policy_document" "assume_role" {
  statement {
    sid     = "VersusPrincipalWithExternalId"
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "AWS"
      identifiers = [var.versus_principal_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "sts:ExternalId"
      values   = [var.external_id]
    }
  }
}

resource "aws_iam_role" "versus_reader" {
  name                 = "versus-reader"
  description          = "Versus metadata reader: Describe, List and configuration reads only, with explicit denies for data-plane and secret reads."
  max_session_duration = 3600
  assume_role_policy   = data.aws_iam_policy_document.assume_role.json

  tags = var.tags
}

# Metadata reads: the service catalog probes plus shared inventory helpers.
data "aws_iam_policy_document" "metadata_reads" {
  statement {
    sid    = "MetadataReads"
    effect = "Allow"

    resources = ["*"]

    actions = [
      # API Gateway
      "apigateway:GET",
      # Auto Scaling
      "autoscaling:Describe*",
      # AWS Health
      "health:Describe*",
      # CloudFormation
      "cloudformation:Describe*",
      # CloudFront
      "cloudfront:List*",
      # CloudTrail
      "cloudtrail:LookupEvents",
      # CloudWatch
      "cloudwatch:DescribeAlarms*",
      # AWS Config recorders
      "config:Describe*",
      # DynamoDB tables, never items
      "dynamodb:DescribeTable",
      "dynamodb:ListTables",
      # EC2 metadata only
      "ec2:Describe*",
      # ECS
      "ecs:Describe*",
      "ecs:List*",
      # EKS
      "eks:Describe*",
      "eks:List*",
      # ElastiCache
      "elasticache:Describe*",
      # Elastic Load Balancing v2
      "elasticloadbalancing:Describe*",
      # EventBridge
      "events:Describe*",
      "events:List*",
      # IAM roles, without inline or managed policy documents
      "iam:GetRole",
      "iam:List*",
      # Kinesis
      "kinesis:Describe*",
      "kinesis:List*",
      # Lambda configuration, without function code
      "lambda:GetFunctionConfiguration",
      "lambda:List*",
      # RDS
      "rds:Describe*",
      # Route 53
      "route53:GetHealthCheckStatus",
      "route53:List*",
      # S3 bucket metadata and security flags, never objects
      "s3:GetBucketLocation",
      "s3:GetBucketPolicyStatus",
      "s3:GetBucketPublicAccessBlock",
      "s3:GetEncryptionConfiguration",
      "s3:ListAllMyBuckets",
      # SNS
      "sns:GetTopicAttributes",
      "sns:List*",
      # SQS
      "sqs:GetQueueAttributes",
      "sqs:List*",
      # Step Functions
      "states:Describe*",
      "states:List*",
      # Shared inventory helpers
      "resource-explorer-2:Search",
      "servicecatalog:ListApplications",
      "tag:GetResources",
    ]
  }
}

resource "aws_iam_role_policy" "metadata_reads" {
  name   = "versus-reader-metadata-reads"
  role   = aws_iam_role.versus_reader.id
  policy = data.aws_iam_policy_document.metadata_reads.json
}

# Optional data: cost, posture and check reads that are off by default.
data "aws_iam_policy_document" "optional_data" {
  statement {
    sid    = "OptionalDataReads"
    effect = "Allow"

    resources = ["*"]

    actions = [
      # Cost Explorer, billed per request by AWS
      "ce:GetAnomalies",
      "ce:GetCostAndUsage",
      # Security Hub findings
      "securityhub:GetFindings",
      # AWS Config advanced query
      "config:SelectResourceConfig",
      # Trusted Advisor checks
      "trustedadvisor:Describe*",
    ]
  }
}

resource "aws_iam_role_policy" "optional_data" {
  count = var.optional_data ? 1 : 0

  name   = "versus-reader-optional-data"
  role   = aws_iam_role.versus_reader.id
  policy = data.aws_iam_policy_document.optional_data.json
}

# Explicit denies: data-plane, secret and log-body reads stay out even if a
# managed policy is later attached with a broader allow.
data "aws_iam_policy_document" "deny_reads" {
  statement {
    sid    = "DenyDataPlaneAndSecretReads"
    effect = "Deny"

    resources = ["*"]

    actions = [
      "s3:GetObject",
      "secretsmanager:GetSecretValue",
      "ssm:GetParameter*",
      "kms:Decrypt",
      "dynamodb:GetItem",
      "dynamodb:Query",
      "dynamodb:Scan",
      "lambda:GetFunction",
      "ec2:GetConsoleOutput",
      "logs:GetLogEvents",
    ]
  }

  statement {
    sid    = "DenyBillingAccountSurfaces"
    effect = "Deny"

    resources = ["*"]

    actions = [
      "aws-portal:*",
      "billing:*",
      "invoicing:*",
      "payments:*",
    ]
  }
}

resource "aws_iam_role_policy" "deny_reads" {
  name   = "versus-reader-deny-reads"
  role   = aws_iam_role.versus_reader.id
  policy = data.aws_iam_policy_document.deny_reads.json
}
