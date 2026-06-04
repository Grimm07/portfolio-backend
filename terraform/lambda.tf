# Zip the ingest bundle. The bundle file is index.mjs at the zip root; ESM handler = "index.handler".
data "archive_file" "ingest" {
  type        = "zip"
  source_file = "${path.module}/../backend/dist/ingest/index.mjs"
  output_path = "${path.module}/.build/ingest.zip"
}

# Origin-verify shared secret, published by the infra repo in phase 2 (SecureString).
# No default — a missing param fails the apply loudly (infra phase 2 must run first).
data "aws_ssm_parameter" "origin_verify" {
  name            = "/portfolio/${var.environment}/origin-verify-secret"
  with_decryption = true
}

resource "aws_lambda_function" "ingest" {
  function_name    = "${local.name_prefix}-ingest"
  role             = aws_iam_role.ingest.arn
  runtime          = "nodejs20.x"
  handler          = "index.handler"
  filename         = data.archive_file.ingest.output_path
  source_code_hash = data.archive_file.ingest.output_base64sha256
  timeout          = 10
  memory_size      = 256

  environment {
    variables = {
      FROM_EMAIL               = local.from_email
      CONTACT_EMAIL_SECRET_ARN = aws_secretsmanager_secret.contact_email.arn
      # Unlike CONTACT_EMAIL (ARN only, fetched at runtime), the origin-verify secret is
      # injected as plaintext: it's a low-sensitivity 40-char random shared token (not PII),
      # so the simpler env-var path is an accepted trade-off vs. a runtime SSM/KMS fetch.
      ORIGIN_VERIFY_SECRET = data.aws_ssm_parameter.origin_verify.value
    }
  }
}
