# The Lambda ARN, for the infra repo to reference the function directly (e.g. API Gateway
# integration wiring). String (not SecureString) — an ARN is not a secret.
resource "aws_ssm_parameter" "ingest_function_arn" {
  name        = "/portfolio/${var.environment}/ingest-function-arn"
  type        = "String"
  value       = aws_lambda_function.ingest.arn
  description = "Portfolio contact ingest Lambda ARN (published for the infra repo)"
}
