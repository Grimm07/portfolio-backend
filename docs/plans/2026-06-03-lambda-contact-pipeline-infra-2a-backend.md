# Lambda Contact Pipeline — Infrastructure Plan 2a: AWS Backend Resources

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Provision (in OpenTofu) the AWS backend resources the contact-ingestion application code needs — DynamoDB, SQS+DLQ, the S3 messages bucket, Secrets Manager, SES, IAM, and the two Lambdas (ingest via Function URL, notifier via SQS event-source mapping) — so a submission can flow S3 → DynamoDB → SQS → SES end-to-end, verifiable before any CDN exists.

**Architecture:** Extends the existing `terraform/` config (OpenTofu, GitLab HTTP state, cloudflare v5 provider) by adding an `aws` provider in **us-east-1** and the backend resource set. The two Lambdas run the bundles built from `backend/` (Plan 1). The ingest Lambda's Function URL uses `AWS_IAM` auth (only CloudFront will call it, wired in Plan 2b); until then the pipeline is verified with `aws lambda invoke`. CAPTCHA (AWS WAF) and the CloudFront/S3-site/ACM/DNS-cutover are **Plan 2b** — deliberately out of scope here.

**Tech Stack:** OpenTofu (`tofu`), AWS provider `~> 5.x`, `hashicorp/archive` for Lambda zips, existing `cloudflare` provider `~> 5.16` (for SES DKIM CNAMEs only). Node 20 Lambda runtime (ESM `.mjs`).

**Decisions carried in:** existing terraform config is *not the live deploy path* (greenfield AWS additions; no destroy of Cloudflare resources in this plan); all resources in **us-east-1**; Secrets Manager holds `CONTACT_EMAIL`; CAPTCHA is WAF/edge (Plan 2b), so **no Turnstile and no secret-based CAPTCHA here**.

**Prereq:** `cd backend && npm run build` must have produced `backend/dist/ingest/index.mjs` and `backend/dist/notifier/index.mjs` before `tofu apply` (the archive data sources read them). Task 8 enforces this.

---

## File Structure

All new files live in `terraform/` alongside the existing `main.tf`/`variables.tf`/`outputs.tf`/`backend.tf`. New AWS resources are split by responsibility so each file stays focused:

```
terraform/
  providers_aws.tf      # aws provider (us-east-1) + add aws/archive to required_providers; locals
  variables.tf          # (modify) add aws_region, from_email
  dynamodb.tf           # contacts table (+GSI), rate-limits table (+TTL)
  sqs.tf                # notifications queue + DLQ + redrive
  s3_messages.tf        # private messages bucket (SSE, block-public-access)
  secrets.tf            # Secrets Manager: CONTACT_EMAIL
  ses.tf                # SES domain identity + DKIM + Cloudflare DKIM CNAMEs + recipient identity
  iam.tf                # ingest + notifier execution roles & least-privilege policies
  lambda.tf             # archive zips, ingest fn + Function URL, notifier fn + SQS event-source mapping
  outputs.tf            # (modify) add backend outputs
```

`main.tf` (Cloudflare Pages/Worker/DNS) is **left untouched** in this plan — its removal and the DNS cutover are Plan 2b.

**Local naming:** a single `locals.name_prefix = "portfolio-contact"` (in `providers_aws.tf`) prefixes resource names for consistency.

---

## Task 1: AWS provider, archive provider, variables

**Files:**
- Create: `terraform/providers_aws.tf`
- Modify: `terraform/variables.tf`

- [ ] **Step 1: Create `terraform/providers_aws.tf`**

```hcl
# AWS provider + supporting providers for the contact backend (Plan 2a).
# ACM/CloudFront/WAF in Plan 2b also require us-east-1, so a single region keeps things simple.
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.70"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.6"
    }
  }
}

provider "aws" {
  region = var.aws_region
  default_tags {
    tags = {
      Project   = "portfolio-contact"
      ManagedBy = "opentofu"
    }
  }
}

locals {
  name_prefix = "portfolio-contact"
  # Derive the From address from the domain — no email literal hardcoded.
  from_email = "noreply@${var.domain_name}"
}
```

> Note: the existing `main.tf` already has a `terraform { required_providers { cloudflare = ... } }` block. OpenTofu merges `required_providers` across files, so declaring `aws`/`archive` in a second `terraform` block here is valid and additive.

- [ ] **Step 2: Add variables to `terraform/variables.tf`**

Append:
```hcl
variable "aws_region" {
  description = "AWS region for all contact backend resources"
  type        = string
  default     = "us-east-1"
}
```

> The SES From address is derived as `local.from_email = "noreply@${var.domain_name}"` (set in `providers_aws.tf`), so there's no email literal to maintain or to leak into git.

- [ ] **Step 3: Init and validate**

Run:
```bash
cd terraform && tofu init -backend=false && tofu validate
```
Expected: providers install (aws, archive, cloudflare); `Success! The configuration is valid.`

> `-backend=false` matches the existing CI "validate" pattern (see `backend.tf`); a real `apply` (Task 10) configures the GitLab backend with `GITLAB_ACCESS_TOKEN`.

- [ ] **Step 4: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/providers_aws.tf terraform/variables.tf terraform/.terraform.lock.hcl
git commit -m "infra(2a): add aws provider (us-east-1) + archive provider + vars"
```

---

## Task 2: DynamoDB tables

**Files:**
- Create: `terraform/dynamodb.tf`

- [ ] **Step 1: Create `terraform/dynamodb.tf`**

```hcl
# Per-person contact records: PK=EMAIL#<email>, SK=SUB#<ts>#<id>; GSI1 lists everyone (GSI1PK="ALL").
resource "aws_dynamodb_table" "contacts" {
  name         = "${local.name_prefix}-contacts"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute {
    name = "pk"
    type = "S"
  }
  attribute {
    name = "sk"
    type = "S"
  }
  attribute {
    name = "gsi1pk"
    type = "S"
  }
  attribute {
    name = "gsi1sk"
    type = "S"
  }

  global_secondary_index {
    name            = "gsi1"
    hash_key        = "gsi1pk"
    range_key       = "gsi1sk"
    projection_type = "ALL"
  }

  point_in_time_recovery {
    enabled = true
  }

  server_side_encryption {
    enabled = true
  }
}

# Rate-limit counters: PK=IP#<ip>, self-expiring via TTL on expiresAt.
resource "aws_dynamodb_table" "rate_limits" {
  name         = "${local.name_prefix}-rate-limits"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"

  attribute {
    name = "pk"
    type = "S"
  }

  ttl {
    attribute_name = "expiresAt"
    enabled        = true
  }

  server_side_encryption {
    enabled = true
  }
}
```

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/dynamodb.tf
git commit -m "infra(2a): add DynamoDB contacts (+GSI) and rate-limits (+TTL) tables"
```

---

## Task 3: SQS queue + DLQ

**Files:**
- Create: `terraform/sqs.tf`

- [ ] **Step 1: Create `terraform/sqs.tf`**

```hcl
# Dead-letter queue for notification messages that repeatedly fail processing.
resource "aws_sqs_queue" "notifications_dlq" {
  name                      = "${local.name_prefix}-notifications-dlq"
  message_retention_seconds = 1209600 # 14 days
}

# Main notification queue. Notifier Lambda drains it in batches (window set on the
# event-source mapping in lambda.tf). visibility_timeout must exceed the Lambda timeout.
resource "aws_sqs_queue" "notifications" {
  name                       = "${local.name_prefix}-notifications"
  visibility_timeout_seconds = 180 # >= notifier Lambda timeout (60s) with headroom
  message_retention_seconds  = 345600 # 4 days

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.notifications_dlq.arn
    maxReceiveCount     = 5
  })
}
```

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/sqs.tf
git commit -m "infra(2a): add SQS notifications queue + DLQ"
```

---

## Task 4: S3 messages bucket (private)

**Files:**
- Create: `terraform/s3_messages.tf`

- [ ] **Step 1: Create `terraform/s3_messages.tf`**

```hcl
# Private bucket holding raw contact message bodies (messages/{yyyy}/{mm}/{id}.json).
# Never served via CloudFront; read only by the Notifier via the SDK. (The static-site
# bucket is a separate resource in Plan 2b.)
resource "aws_s3_bucket" "messages" {
  bucket = "${local.name_prefix}-messages"
}

resource "aws_s3_bucket_public_access_block" "messages" {
  bucket                  = aws_s3_bucket.messages.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "messages" {
  bucket = aws_s3_bucket.messages.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_versioning" "messages" {
  bucket = aws_s3_bucket.messages.id
  versioning_configuration {
    status = "Enabled"
  }
}
```

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/s3_messages.tf
git commit -m "infra(2a): add private S3 messages bucket (SSE, block-public-access, versioned)"
```

---

## Task 5: Secrets Manager (CONTACT_EMAIL)

**Files:**
- Create: `terraform/secrets.tf`

- [ ] **Step 1: Create `terraform/secrets.tf`**

```hcl
# CONTACT_EMAIL stored in Secrets Manager (not a plaintext Lambda env var), per the
# project's privacy-first posture. The Notifier fetches it at runtime (cached per container).
resource "aws_secretsmanager_secret" "contact_email" {
  name        = "${local.name_prefix}-contact-email"
  description = "Recipient address for contact form digest emails"
}

resource "aws_secretsmanager_secret_version" "contact_email" {
  secret_id     = aws_secretsmanager_secret.contact_email.id
  secret_string = var.contact_email
}
```

> `var.contact_email` already exists in `variables.tf` (sensitive). Its value comes from the gitignored `terraform.tfvars`, exactly as today.

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/secrets.tf
git commit -m "infra(2a): add Secrets Manager secret for CONTACT_EMAIL"
```

---

## Task 6: SES domain identity, DKIM, recipient identity

**Files:**
- Create: `terraform/ses.tf`

- [ ] **Step 1: Create `terraform/ses.tf`**

```hcl
# Verify the sending domain and publish DKIM so digest emails deliver. DNS lives in
# Cloudflare, so the DKIM CNAMEs are created there.
resource "aws_ses_domain_identity" "main" {
  domain = var.domain_name
}

resource "aws_ses_domain_dkim" "main" {
  domain = aws_ses_domain_identity.main.domain
}

# Three DKIM CNAME records in the Cloudflare zone (DNS-only; not proxied).
resource "cloudflare_dns_record" "ses_dkim" {
  count   = 3
  zone_id = var.cloudflare_zone_id
  name    = "${aws_ses_domain_dkim.main.dkim_tokens[count.index]}._domainkey"
  type    = "CNAME"
  content = "${aws_ses_domain_dkim.main.dkim_tokens[count.index]}.dkim.amazonses.com"
  proxied = false
  ttl     = 300
}

# Recipient identity. SES stays in sandbox; both sender (domain) and recipient (your
# inbox) are verified, so no production-access request is needed. Verifying the email
# identity triggers a one-time confirmation email to CONTACT_EMAIL (manual click).
resource "aws_ses_email_identity" "recipient" {
  email = var.contact_email
}
```

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/ses.tf
git commit -m "infra(2a): add SES domain identity + DKIM (Cloudflare CNAMEs) + recipient identity"
```

---

## Task 7: IAM roles and least-privilege policies

**Files:**
- Create: `terraform/iam.tf`

- [ ] **Step 1: Create `terraform/iam.tf`**

```hcl
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

data "aws_iam_policy_document" "ingest" {
  statement {
    sid       = "PutMessageBody"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.messages.arn}/messages/*"]
  }
  statement {
    sid       = "WriteContact"
    actions   = ["dynamodb:PutItem"]
    resources = [aws_dynamodb_table.contacts.arn]
  }
  statement {
    sid       = "RateLimitCounter"
    actions   = ["dynamodb:UpdateItem"]
    resources = [aws_dynamodb_table.rate_limits.arn]
  }
  statement {
    sid       = "EnqueueNotification"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.notifications.arn]
  }
}

resource "aws_iam_role_policy" "ingest" {
  name   = "${local.name_prefix}-ingest"
  role   = aws_iam_role.ingest.id
  policy = data.aws_iam_policy_document.ingest.json
}

# --- Notifier Lambda role ---
resource "aws_iam_role" "notifier" {
  name               = "${local.name_prefix}-notifier"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

resource "aws_iam_role_policy_attachment" "notifier_logs" {
  role       = aws_iam_role.notifier.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

# Scoped to exactly what the notifier handler does: drain SQS, read message bodies from
# S3, fetch CONTACT_EMAIL from Secrets Manager, and send via SES. (It does NOT read DynamoDB.)
data "aws_iam_policy_document" "notifier" {
  statement {
    sid       = "ConsumeQueue"
    actions   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
    resources = [aws_sqs_queue.notifications.arn]
  }
  statement {
    sid       = "ReadMessageBodies"
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.messages.arn}/messages/*"]
  }
  statement {
    sid       = "ReadContactEmailSecret"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.contact_email.arn]
  }
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
}

resource "aws_iam_role_policy" "notifier" {
  name   = "${local.name_prefix}-notifier"
  role   = aws_iam_role.notifier.id
  policy = data.aws_iam_policy_document.notifier.json
}
```

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/iam.tf
git commit -m "infra(2a): add least-privilege IAM roles for ingest and notifier Lambdas"
```

---

## Task 8: Lambda packaging + ingest function + Function URL

**Files:**
- Create: `terraform/lambda.tf`

- [ ] **Step 1: Ensure the bundles exist**

Run:
```bash
cd backend && npm run build && ls -la dist/ingest/index.mjs dist/notifier/index.mjs
```
Expected: both `.mjs` files present. (Terraform's `archive_file` reads them at plan time; if missing, plan fails.)

- [ ] **Step 2: Create `terraform/lambda.tf` (ingest portion)**

```hcl
# Zip each bundle. The bundle file is index.mjs at the zip root; ESM handler = "index.handler".
data "archive_file" "ingest" {
  type        = "zip"
  source_file = "${path.module}/../backend/dist/ingest/index.mjs"
  output_path = "${path.module}/.build/ingest.zip"
}

data "archive_file" "notifier" {
  type        = "zip"
  source_file = "${path.module}/../backend/dist/notifier/index.mjs"
  output_path = "${path.module}/.build/notifier.zip"
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
      MESSAGES_BUCKET        = aws_s3_bucket.messages.id
      CONTACTS_TABLE         = aws_dynamodb_table.contacts.name
      RATE_LIMIT_TABLE       = aws_dynamodb_table.rate_limits.name
      NOTIFICATION_QUEUE_URL = aws_sqs_queue.notifications.url
    }
  }
}

# Function URL, IAM-locked. In Plan 2b a CloudFront Origin Access Control will sign
# requests to it; until then the pipeline is exercised via `aws lambda invoke` (Task 10).
resource "aws_lambda_function_url" "ingest" {
  function_name      = aws_lambda_function.ingest.function_name
  authorization_type = "AWS_IAM"
}
```

- [ ] **Step 3: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 4: Format & commit**

```bash
cd terraform && tofu fmt
echo ".build/" >> terraform/.gitignore  # create the file if absent
git add terraform/lambda.tf terraform/.gitignore
git commit -m "infra(2a): add Lambda packaging + ingest function and IAM-locked Function URL"
```

---

## Task 9: Notifier function + SQS event-source mapping

**Files:**
- Modify: `terraform/lambda.tf`

- [ ] **Step 1: Append the notifier function and mapping to `terraform/lambda.tf`**

```hcl
resource "aws_lambda_function" "notifier" {
  function_name    = "${local.name_prefix}-notifier"
  role             = aws_iam_role.notifier.arn
  runtime          = "nodejs20.x"
  handler          = "index.handler"
  filename         = data.archive_file.notifier.output_path
  source_code_hash = data.archive_file.notifier.output_base64sha256
  timeout          = 60
  memory_size      = 256

  environment {
    variables = {
      FROM_EMAIL               = local.from_email
      CONTACT_EMAIL_SECRET_ARN = aws_secretsmanager_secret.contact_email.arn
    }
  }
}

# Batch bursts into one digest: collect up to 10 messages or 300s, whichever first.
# ReportBatchItemFailures lets the handler re-queue only failed records.
resource "aws_lambda_event_source_mapping" "notifier" {
  event_source_arn                   = aws_sqs_queue.notifications.arn
  function_name                      = aws_lambda_function.notifier.arn
  batch_size                         = 10
  maximum_batching_window_in_seconds = 300
  function_response_types            = ["ReportBatchItemFailures"]
}
```

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/lambda.tf
git commit -m "infra(2a): add notifier Lambda + SQS event-source mapping (batch 10/300s, partial failures)"
```

---

## Task 10: Outputs, plan review, apply, end-to-end smoke test

**Files:**
- Modify: `terraform/outputs.tf`

- [ ] **Step 1: Append backend outputs to `terraform/outputs.tf`**

```hcl
output "ingest_function_url" {
  description = "Lambda Function URL for the ingest handler (IAM-auth; fronted by CloudFront in Plan 2b)"
  value       = aws_lambda_function_url.ingest.function_url
}

output "ingest_function_name" {
  description = "Ingest Lambda function name (for aws lambda invoke testing)"
  value       = aws_lambda_function.ingest.function_name
}

output "notifications_queue_url" {
  description = "SQS notifications queue URL"
  value       = aws_sqs_queue.notifications.url
}

output "messages_bucket" {
  description = "S3 bucket holding contact message bodies"
  value       = aws_s3_bucket.messages.id
}

output "contacts_table" {
  description = "DynamoDB contacts table name"
  value       = aws_dynamodb_table.contacts.name
}
```

- [ ] **Step 2: Full validate + plan review**

Run:
```bash
cd terraform && tofu validate && tofu plan -out=2a.plan
```
Expected: a plan that **creates** the new AWS + DKIM-CNAME resources and **changes nothing** about the existing Cloudflare Pages/Worker/DNS resources (those are out of scope for 2a). Read the plan and confirm: no `destroy` actions. (A real plan requires the GitLab backend; run `tofu init` with `GITLAB_ACCESS_TOKEN` set, or apply from CI.)

- [ ] **Step 3: Apply**

```bash
cd terraform && tofu apply 2a.plan
```
Expected: resources created. Then **manually confirm the SES recipient verification email** delivered to `CONTACT_EMAIL` (click the link), and confirm SES DKIM shows `Verified` once the Cloudflare CNAMEs propagate (minutes).

- [ ] **Step 4: End-to-end smoke test (no CDN needed)**

```bash
# 1. Invoke the ingest Lambda directly with a Function-URL-shaped event (valid submission;
#    formTimestamp far in the past so the time-trap passes).
cd terraform
FN=$(tofu output -raw ingest_function_name)
cat > /tmp/ingest-event.json <<'JSON'
{ "headers": { "x-forwarded-for": "203.0.113.9", "user-agent": "smoke-test" },
  "body": "{\"name\":\"Smoke Test\",\"email\":\"YOUR_TEST_INBOX\",\"message\":\"hello from the smoke test\",\"website\":\"\",\"formTimestamp\":0}" }
# ^ Replace YOUR_TEST_INBOX with a real address you control (placeholder kept literal so the
#   repo's pre-commit secrets-check does not flag this doc).
JSON
aws lambda invoke --function-name "$FN" --payload fileb:///tmp/ingest-event.json /tmp/ingest-out.json
cat /tmp/ingest-out.json   # expect {"statusCode":200,...}

# 2. Confirm the message landed in S3 and the record in DynamoDB.
aws s3 ls "s3://$(tofu output -raw messages_bucket)/messages/" --recursive
aws dynamodb scan --table-name "$(tofu output -raw contacts_table)" --max-items 5

# 3. The SQS message should trigger the notifier within the 300s window; verify a digest
#    email arrives at CONTACT_EMAIL, and check the notifier logs.
aws logs tail "/aws/lambda/portfolio-contact-notifier" --since 10m
```
Expected: `statusCode 200`; one object under `messages/`; one item in `contacts`; a digest email at `CONTACT_EMAIL` within ~5 min; notifier logs show one batch processed with no failures.

- [ ] **Step 5: Commit**

```bash
cd terraform && tofu fmt
git add terraform/outputs.tf
git commit -m "infra(2a): add backend outputs; full plan applied and pipeline smoke-tested"
```

---

## Self-Review

**Spec coverage (against `2026-06-02-lambda-contact-pipeline-design.md` §4–§7):**
- DynamoDB `contacts` (PK/SK/GSI1) + `rate-limits` (TTL) → Task 2 ✓
- SQS notifications + DLQ + redrive → Task 3 ✓
- Private S3 messages bucket (SSE, BPA) → Task 4 ✓
- Secrets Manager `CONTACT_EMAIL` → Task 5 ✓ (consumed by notifier env `CONTACT_EMAIL_SECRET_ARN`, Task 9)
- SES domain+DKIM (Cloudflare CNAMEs) + recipient identity, sandbox-OK → Task 6 ✓
- Least-privilege IAM matching the *actual* handler behavior (notifier reads S3 not DynamoDB) → Task 7 ✓
- Ingest Lambda env wired to bucket/tables/queue; Function URL `AWS_IAM` → Tasks 8 ✓
- Notifier Lambda + SQS event-source mapping (batch 10 / **300s** window / ReportBatchItemFailures) → Task 9 ✓
- All in **us-east-1**; Lambda env var names match Plan 1 code exactly (`MESSAGES_BUCKET`, `CONTACTS_TABLE`, `RATE_LIMIT_TABLE`, `NOTIFICATION_QUEUE_URL`, `FROM_EMAIL`, `CONTACT_EMAIL_SECRET_ARN`) ✓

**Deferred to Plan 2b (correctly NOT here):** CloudFront distribution + OAC + `/api/*` behavior + `AllViewerExceptHostHeader`, the **WAF WebACL (CAPTCHA + rate rule)** (requires CloudFront, CLOUDFRONT scope = us-east-1), ACM cert, the static-site S3 bucket, the Cloudflare DNS retarget (grey-cloud) + removal of Pages/Worker resources, the frontend WAF-CAPTCHA JS SDK swap, and CI/OIDC deploy. The Function URL stays IAM-locked and is verified via `aws lambda invoke` until CloudFront fronts it.

**Placeholder scan:** none — every task has complete HCL and exact commands.

**Consistency checks:**
- Env var names in `lambda.tf` (Tasks 8/9) exactly match the `process.env.*` reads in Plan 1's `handler.ts` entrypoints (`MESSAGES_BUCKET`, `CONTACTS_TABLE`, `RATE_LIMIT_TABLE`, `NOTIFICATION_QUEUE_URL`; `FROM_EMAIL`, `CONTACT_EMAIL_SECRET_ARN`).
- IAM resource ARNs reference the exact tables/bucket/queue/secret created in Tasks 2–7.
- SQS `visibility_timeout_seconds` (180) > notifier `timeout` (60); DLQ `maxReceiveCount` set.
- Notifier IAM omits DynamoDB (the handler loads bodies from S3, not Dynamo) — least privilege matches real code.

**Known manual steps (not automatable in tofu):** SES recipient email confirmation click; waiting for DKIM CNAME propagation. Both called out in Task 10 Step 3.

**Open follow-up for 2b:** the ingest Function URL currently has no resource policy granting CloudFront invoke — that `aws_lambda_permission` (principal `cloudfront.amazonaws.com`, OAC source-ARN condition) is added in 2b when the distribution exists.
