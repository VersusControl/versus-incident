output "role_arn" {
  description = "ARN of the versus-cost-reader role; paste it into the Versus connect wizard as the cost source."
  value       = aws_iam_role.versus_cost_reader.arn
}

output "role_name" {
  description = "Name of the versus-cost-reader role created in this account."
  value       = aws_iam_role.versus_cost_reader.name
}
