data "aws_iam_policy_document" "lambda_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

# --- Ingest Lambda role ---
resource "aws_iam_role" "ingest" {
  name               = "${local.name_prefix}-ingest"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

resource "aws_iam_role_policy_attachment" "ingest_logs" {
  role       = aws_iam_role.ingest.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

# Exactly what the handler does: send one SES email from the verified identity, and read the
# recipient address from Secrets Manager. No S3/DynamoDB/SQS.
data "aws_iam_policy_document" "ingest" {
  statement {
    sid       = "SendEmail"
    actions   = ["ses:SendEmail", "ses:SendRawEmail"]
    resources = ["*"] # SES SendEmail does not support resource-level ARNs for the action itself
    condition {
      test     = "StringEquals"
      variable = "ses:FromAddress"
      values   = [local.from_email]
    }
  }
  statement {
    sid       = "ReadContactEmailSecret"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.contact_email.arn]
  }
}

resource "aws_iam_role_policy" "ingest" {
  name   = "${local.name_prefix}-ingest"
  role   = aws_iam_role.ingest.id
  policy = data.aws_iam_policy_document.ingest.json
}
