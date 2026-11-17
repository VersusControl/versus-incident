output "role_arn" {
  description = "ARN of the versus-reader role; paste it into the Versus connect wizard."
  value       = aws_iam_role.versus_reader.arn
}

output "role_name" {
  description = "Name of the versus-reader role created in this account."
  value       = aws_iam_role.versus_reader.name
}
