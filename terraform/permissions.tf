# Phase 3 wiring: allow the infra-owned API Gateway to invoke this Lambda, scoped to THIS
# env's API execution ARN. The execution ARN is published by the infra repo (phase 2) to SSM
# with no default, so a missing param fails loudly. "${exec_arn}/*/*" matches any stage/route
# of that API (exec ARN form: arn:aws:execute-api:<region>:<acct>:<api-id>).
data "aws_ssm_parameter" "api_execution_arn" {
  name = "/portfolio/${var.environment}/api-execution-arn"
}

resource "aws_lambda_permission" "apigw_invoke" {
  statement_id  = "AllowApiGatewayInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.ingest.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${data.aws_ssm_parameter.api_execution_arn.value}/*/*"
}
