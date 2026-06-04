# --- AWS contact backend ---
output "ingest_function_name" {
  description = "Ingest Lambda function name (for `aws lambda invoke` testing)"
  value       = aws_lambda_function.ingest.function_name
}

output "ingest_function_arn_ssm_param" {
  description = "SSM parameter name where the ingest Lambda ARN is published for the infra repo"
  value       = aws_ssm_parameter.ingest_function_arn.name
}
