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

# The management account controls the whole Organization, so this role reads
# cost and nothing else: no inventory, no change history, no account details.
resource "aws_iam_role" "versus_cost_reader" {
  name                 = "versus-cost-reader"
  description          = "Versus cost reader for a management account: Cost Explorer, Budgets, Cost Optimization Hub and Compute Optimizer reads only; every other action is denied."
  max_session_duration = 3600
  assume_role_policy   = data.aws_iam_policy_document.assume_role.json

  tags = var.tags
}

# Cost reads for every linked account, served by the management account.
data "aws_iam_policy_document" "cost_reads" {
  statement {
    sid    = "CostReads"
    effect = "Allow"

    resources = ["*"]

    actions = [
      # Cost Explorer, billed per request by AWS
      "ce:GetCostAndUsage",
      "ce:GetCostAndUsageWithResources",
      "ce:GetCostForecast",
      "ce:GetSavingsPlansCoverage",
      # Cost Explorer, free
      "ce:ListCostAllocationTags",
      # AWS Budgets definitions, read-only
      "budgets:ViewBudget",
      # Rightsizing and idle advice
      "cost-optimization-hub:ListRecommendations",
      "compute-optimizer:GetEC2InstanceRecommendations",
    ]
  }
}

resource "aws_iam_role_policy" "cost_reads" {
  name   = "versus-cost-reader-cost-reads"
  role   = aws_iam_role.versus_cost_reader.id
  policy = data.aws_iam_policy_document.cost_reads.json
}

# Explicit denies: every action outside the cost reads, so a managed policy
# attached later cannot widen the role, plus the billing account, invoice,
# payment, tax and account settings APIs named on their own.
data "aws_iam_policy_document" "deny_reads" {
  statement {
    sid    = "DenyEverythingButCostReads"
    effect = "Deny"

    resources = ["*"]

    not_actions = [
      "ce:GetCostAndUsage",
      "ce:GetCostAndUsageWithResources",
      "ce:GetCostForecast",
      "ce:GetSavingsPlansCoverage",
      "ce:ListCostAllocationTags",
      "budgets:ViewBudget",
      "cost-optimization-hub:ListRecommendations",
      "compute-optimizer:GetEC2InstanceRecommendations",
    ]
  }

  statement {
    sid    = "DenyBillingAccountSurfaces"
    effect = "Deny"

    resources = ["*"]

    actions = [
      "account:*",
      "aws-portal:*",
      "billing:*",
      "consolidatedbilling:*",
      "invoicing:*",
      "payments:*",
      "purchase-orders:*",
      "tax:*",
    ]
  }
}

resource "aws_iam_role_policy" "deny_reads" {
  name   = "versus-cost-reader-deny"
  role   = aws_iam_role.versus_cost_reader.id
  policy = data.aws_iam_policy_document.deny_reads.json
}
