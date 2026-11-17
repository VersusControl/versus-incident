variable "versus_principal_arn" {
  description = "Exact ARN of the Versus IAM role allowed to assume versus-reader, copied from the connect wizard. Do not use the account root ARN: trusting an account root lets every principal in that account assume the role."
  type        = string

  validation {
    condition     = can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$", var.versus_principal_arn))
    error_message = "versus_principal_arn must be the exact Versus IAM role ARN, for example arn:aws:iam::123456789012:role/versus-app. Account root ARNs and other non-role principals are not accepted."
  }
}

variable "external_id" {
  description = "Secret shared with Versus that binds an assume-role call to this account. Must match the ExternalId configured in Versus."
  type        = string
  sensitive   = true

  validation {
    condition     = can(regex("^[!-~]+$", var.external_id)) && length(var.external_id) >= 16 && length(var.external_id) <= 1224
    error_message = "external_id must be 16 to 1224 printable ASCII characters with no spaces, as required by sts:ExternalId."
  }
}

variable "optional_data" {
  description = "When true, adds cost reads (Cost Explorer, AWS Budgets, Cost Optimization Hub and Compute Optimizer), Security Hub findings, AWS Config advanced query and Trusted Advisor reads. Off by default; Cost Explorer is billed per request."
  type        = bool
  default     = false
}

variable "tags" {
  description = "Tags applied to the versus-reader role."
  type        = map(string)
  default     = {}
}
