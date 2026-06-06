# Infra publishes the per-account Lambda artifact bucket name here (shadowspire-<env>-lambda-artifacts-<acct>).
# The ingest zip is built + uploaded by GitHub Actions (.github/workflows/deploy.yml), keyed by commit SHA;
# this stack only references the already-uploaded object — it no longer builds the zip in-band.
data "aws_ssm_parameter" "artifact_bucket" {
  name = "/shadowspire/${var.environment}/lambda-artifacts-bucket"
}

# Origin-verify shared secret, published by the infra repo in phase 2 (SecureString).
# No default — a missing param fails the apply loudly (infra phase 2 must run first).
data "aws_ssm_parameter" "origin_verify" {
  name            = "/portfolio/${var.environment}/origin-verify-secret"
  with_decryption = true
}

resource "aws_lambda_function" "ingest" {
  function_name = "${local.name_prefix}-ingest"
  role          = aws_iam_role.ingest.arn
  # Go on the custom runtime: the `bootstrap` binary is the entrypoint. Built for arm64/Graviton.
  runtime       = "provided.al2023"
  handler       = "bootstrap"
  architectures = ["arm64"]
  # Artifact comes from S3, not an in-band archive_file. The key is the deploying commit's SHA
  # (TF_VAR_artifact_key, set per-deploy by GitHub Actions before triggering the Spacelift run),
  # so a new deploy = new key = the function code updates. No source_code_hash needed: the changed
  # s3_key is itself the change signal.
  s3_bucket   = data.aws_ssm_parameter.artifact_bucket.value
  s3_key      = var.artifact_key
  timeout     = 10
  memory_size = 256

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
