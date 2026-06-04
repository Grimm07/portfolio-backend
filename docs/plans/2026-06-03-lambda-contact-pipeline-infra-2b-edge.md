# Lambda Contact Pipeline — Infrastructure Plan 2b: AWS Edge Resources

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the AWS **edge** in front of both the static site and the ingest Lambda — ACM certificate, a private static-site S3 bucket with OAC, a CLOUDFRONT-scoped WAF WebACL (managed common rules + rate-based rule + CAPTCHA on `POST /api/*`) and its CAPTCHA API key, the CloudFront distribution (S3 default behavior + `/api/*` → Lambda Function URL behavior), and the `aws_lambda_permission` that finally lets CloudFront invoke the IAM-locked ingest Function URL — **without** cutting prod DNS over yet, so it is verifiable at the raw CloudFront domain before go-live.

**Architecture:** Extends the existing `terraform/` config (OpenTofu, GitLab HTTP state, `cloudflare` v5 provider, `aws` provider in **us-east-1** from Plan 2a) with the edge resource set. CloudFront, ACM, and the CLOUDFRONT-scoped WAF WebACL **must** all live in **us-east-1** — the 2a provider is already there, so no provider alias is needed. The distribution fronts two origins: the new static-site S3 bucket (default behavior, OAC `origin_type = s3`) and the `aws_lambda_function_url.ingest` from 2a (`/api/*` behavior, OAC `origin_type = lambda`, `AllViewerExceptHostHeader` origin-request policy, caching disabled). ACM DNS-validates via `cloudflare_dns_record` CNAMEs (DNS-only). Cloudflare resources in `main.tf` are **not** touched here — the prod DNS retarget and Pages/Worker removal are Plan 2c.

**Tech Stack:** OpenTofu (`tofu`), AWS provider `~> 5.70` (pinned in `main.tf`'s single `required_providers` block — see the 2a correction note below), existing `cloudflare` provider `~> 5.16` (for ACM-validation CNAMEs). All edge resources in **us-east-1**.

**Decisions carried in:**
- **Environment strategy (from the meta-plan):** prod is the default/live env; `var.environment` (default `"prod"`) drives both resource names (`local.name_prefix = "portfolio-contact-${local.env}"`) and the served hostname (`local.site_domain`). State is separated per-env at `tofu init` via partial backend-config (prod keeps the existing `.../state/portfolio` address — no state migration; dev uses `.../state/portfolio-dev`). This is **Task 1** and is a prerequisite for Plans 2c and 3.
- **Free retrofit:** 2a was never applied (no AWS creds), so re-keying every 2a resource name to include `${local.env}` costs nothing now. Task 1 does exactly that by changing `local.name_prefix`.
- **No prod DNS here:** the distribution is built and aliased, but the prod root/`www` records still point at Cloudflare Pages. Flipping them (grey-cloud CNAME → `cloudfront_domain_name`) is **Plan 2c**. Verification in this plan uses the raw `*.cloudfront.net` domain.

**Prereq:** Plan 2a's files exist in `terraform/` (notably `providers_aws.tf` with `local.name_prefix`/`local.from_email`, and `lambda.tf` with `aws_lambda_function.ingest` + `aws_lambda_function_url.ingest`). 2a need not be *applied* — but its HCL must be present for references to resolve at `validate`.

> **2a correction inherited here:** OpenTofu permits only ONE `required_providers` block per module. Plan 2a's `providers_aws.tf` was reconciled so the `aws`/`archive`/`cloudflare` version pins all live in **`main.tf`**'s single `required_providers` block; `providers_aws.tf` holds only the `provider "aws"` block and the `locals`. This plan does **not** add another `required_providers` block — CloudFront/ACM/WAF/S3 are all `hashicorp/aws` resources already covered by the existing pin.

---

## File Structure

All new files live in `terraform/` alongside the existing config. New edge resources are split by responsibility so each file stays focused:

```
terraform/
  variables.tf          # (modify) add var.environment
  providers_aws.tf      # (modify) env-aware locals (name_prefix, env, site_domain)
  acm.tf                # ACM cert (us-east-1) + Cloudflare DNS-validation CNAMEs + validation
  s3_site.tf            # private static-site bucket (SSE, BPA, versioned) + S3 OAC + bucket policy
  waf.tf                # CLOUDFRONT WebACL (common rules + rate rule + CAPTCHA /api/*) + wafv2 api key
  cloudfront.tf         # distribution: S3 default behavior + /api/* Lambda-URL behavior + Lambda OAC
  lambda.tf             # (modify) append aws_lambda_permission for cloudfront.amazonaws.com
  outputs.tf            # (modify) add the interface-contract edge outputs
```

`main.tf` (Cloudflare Pages/Worker/DNS) is **left untouched** in this plan — its removal and the DNS cutover are Plan 2c.

**Local naming:** extends 2a's prefix — `${local.name_prefix}-site` (bucket), `${local.name_prefix}-waf` (WebACL), distribution `comment = local.name_prefix`, where `name_prefix = "portfolio-contact-${local.env}"`.

---

## Task 1: Environment scaffolding (retrofit name_prefix + site_domain)

This re-keys **every 2a resource name** to include `${local.env}` (e.g. `portfolio-contact-prod-contacts`). Free because 2a is unapplied. Prerequisite for Plans 2c and 3.

**Files:**
- Modify: `terraform/variables.tf`
- Modify: `terraform/providers_aws.tf`

- [ ] **Step 1: Add `variable "environment"` to `terraform/variables.tf`**

Append:
```hcl
variable "environment" {
  description = "Deployment environment (drives resource names + state). prod is the live/default env."
  type        = string
  default     = "prod"

  validation {
    condition     = contains(["prod", "dev"], var.environment)
    error_message = "environment must be \"prod\" or \"dev\"."
  }
}
```

- [ ] **Step 2: Replace the static `locals` in `terraform/providers_aws.tf` with env-aware locals**

Current (2a) block:
```hcl
locals {
  name_prefix = "portfolio-contact"
  # Derive the From address from the domain — no email literal hardcoded.
  from_email = "noreply@${var.domain_name}"
}
```

Replace with:
```hcl
locals {
  env         = var.environment                       # "prod" | "dev"
  name_prefix = "portfolio-contact-${local.env}"      # re-keys all 2a resource names
  # prod serves the apex domain; non-prod serves "<env>.<domain>" (e.g. dev.trystan-tbm.dev)
  site_domain = local.env == "prod" ? var.domain_name : "${local.env}.${var.domain_name}"
  # Derive the From address from the domain — no email literal hardcoded.
  from_email = "noreply@${var.domain_name}"
}
```

> **Effect:** all 2a `${local.name_prefix}-…` names now embed the env (`portfolio-contact-prod-contacts`, `portfolio-contact-prod-notifications`, `portfolio-contact-prod-ingest`, …). Because 2a is unapplied, this is a pure rename with no state move. `local.site_domain` is consumed by the ACM cert (Task 2), WAF `token_domains` (Task 4), and CloudFront `aliases` (Task 5). The `Project` default tag in `provider "aws"` stays the literal `portfolio-contact` (cross-env grouping); resource-name uniqueness comes from `name_prefix`.

- [ ] **Step 3: Validate**

Run:
```bash
cd terraform && tofu init -backend=false && tofu validate
```
Expected: providers already installed; `Success! The configuration is valid.` (The rename touches only string interpolation, so all 2a references still resolve.)

- [ ] **Step 4: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/variables.tf terraform/providers_aws.tf
git commit -m "infra(2b): add var.environment + env-aware name_prefix/site_domain locals"
```

---

## Task 2: ACM certificate (us-east-1) + Cloudflare DNS validation

**Files:**
- Create: `terraform/acm.tf`

- [ ] **Step 1: Create `terraform/acm.tf`**

```hcl
# Public TLS cert for CloudFront. CloudFront REQUIRES the cert in us-east-1 — the 2a
# aws provider is already us-east-1, so no alias is needed. DNS-validated via Cloudflare.
# prod => apex + www; dev => dev.<domain> + www.dev.<domain> (both from local.site_domain).
resource "aws_acm_certificate" "site" {
  domain_name               = local.site_domain
  subject_alternative_names = ["www.${local.site_domain}"]
  validation_method         = "DNS"

  lifecycle {
    create_before_destroy = true
  }

  tags = {
    Name = "${local.name_prefix}-cert"
  }
}

# One DNS-only (grey-cloud) CNAME per domain_validation_option. distinct() guards against
# duplicate validation records when the apex and a SAN share a validation CNAME.
resource "cloudflare_dns_record" "acm_validation" {
  for_each = {
    for dvo in aws_acm_certificate.site.domain_validation_options : dvo.domain_name => {
      name   = dvo.resource_record_name
      type   = dvo.resource_record_type
      record = dvo.resource_record_value
    }
  }

  zone_id = var.cloudflare_zone_id
  # Cloudflare appends the zone automatically; strip the trailing dot from the FQDN ACM gives.
  name    = trimsuffix(each.value.name, ".")
  type    = each.value.type
  content = trimsuffix(each.value.record, ".")
  proxied = false
  ttl     = 60
}

# Blocks until ACM observes the CNAMEs and marks the cert ISSUED.
resource "aws_acm_certificate_validation" "site" {
  certificate_arn         = aws_acm_certificate.site.arn
  validation_record_fqdns = [for r in cloudflare_dns_record.acm_validation : r.name]
}
```

> **Why `for_each` (not `count`):** `domain_validation_options` is a set whose ordering isn't index-stable; keying by `domain_name` (per the current AWS-provider docs' recommended pattern) avoids spurious diffs if the apex/SAN order changes. The meta-plan said "count over domain_validation_options" — `for_each` is the current, drift-safe equivalent and is the form the provider docs now recommend; noted here as a deliberate, equivalent substitution.
> **Assumption (context7-unverified detail):** `cloudflare_dns_record.name` accepts the FQDN with the zone suffix present; we `trimsuffix` the trailing dot. If the v5 Cloudflare provider rejects the embedded zone suffix, strip it to the record-relative name instead. The CNAME content is the ACM-provided target with its trailing dot removed.

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/acm.tf
git commit -m "infra(2b): add ACM cert (us-east-1) with Cloudflare DNS validation CNAMEs"
```

---

## Task 3: Static-site S3 bucket + OAC + bucket policy

**Files:**
- Create: `terraform/s3_site.tf`

- [ ] **Step 1: Create `terraform/s3_site.tf`**

```hcl
# Static SPA origin. Private (Block-Public-Access), SSE-S3, versioned. Served ONLY through
# CloudFront via OAC — never public. Plan 2c uploads dist/ here with `aws s3 sync`.
# (Distinct from 2a's `…-messages` bucket, which holds private submission bodies and is
# never fronted by CloudFront.)
resource "aws_s3_bucket" "site" {
  bucket = "${local.name_prefix}-site"
}

resource "aws_s3_bucket_public_access_block" "site" {
  bucket                  = aws_s3_bucket.site.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "site" {
  bucket = aws_s3_bucket.site.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_versioning" "site" {
  bucket = aws_s3_bucket.site.id
  versioning_configuration {
    status = "Enabled"
  }
}

# Origin Access Control for the S3 origin (SigV4-signed origin requests). Replaces the
# legacy Origin Access Identity. signing_behavior = "always" so every origin request is signed.
resource "aws_cloudfront_origin_access_control" "site" {
  name                              = "${local.name_prefix}-site-oac"
  description                       = "OAC for the static-site S3 origin"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

# Bucket policy: allow ONLY this distribution (via the CloudFront service principal +
# SourceArn = the distribution ARN) to GetObject. Defined here, references the distribution
# created in Task 5 — OpenTofu resolves the cross-file dependency at plan time.
data "aws_iam_policy_document" "site_oac" {
  statement {
    sid     = "AllowCloudFrontOACRead"
    effect  = "Allow"
    actions = ["s3:GetObject"]
    principals {
      type        = "Service"
      identifiers = ["cloudfront.amazonaws.com"]
    }
    resources = ["${aws_s3_bucket.site.arn}/*"]
    condition {
      test     = "StringEquals"
      variable = "AWS:SourceArn"
      values   = [aws_cloudfront_distribution.site.arn]
    }
  }
}

resource "aws_s3_bucket_policy" "site" {
  bucket = aws_s3_bucket.site.id
  policy = data.aws_iam_policy_document.site_oac.json
}
```

> **Note:** `aws_s3_bucket_policy.site` references `aws_cloudfront_distribution.site.arn` (Task 5). This is a forward reference across files — valid in OpenTofu (single module, file order irrelevant). The OAC→S3 trust uses the distribution ARN as `AWS:SourceArn`, which is the current OAC pattern (replaces the old `iam_arn`/OAI form).

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid. (Will resolve once Task 5's distribution exists; if validating Task 3 in isolation before Task 5, temporarily comment the `aws_s3_bucket_policy`/`data` block, or author Tasks 3–5 before the first `validate`. Recommended: run `validate` after Task 5.)

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/s3_site.tf
git commit -m "infra(2b): add private static-site S3 bucket + OAC + OAC bucket policy"
```

---

## Task 4: WAF WebACL (CLOUDFRONT) + CAPTCHA API key

**Files:**
- Create: `terraform/waf.tf`

- [ ] **Step 1: Create `terraform/waf.tf`**

```hcl
# Edge WebACL. scope = CLOUDFRONT REQUIRES the provider region to be us-east-1 (it is, from 2a).
# Three rules, ascending priority:
#   1. AWS managed common rule set (broad OWASP-ish coverage), action inherited from the group.
#   2. Rate-based rule: volumetric per-IP throttle across ALL requests.
#   3. CAPTCHA action scoped to POST requests whose URI path starts with "/api/" — the contact
#      submission. WAF issues/validates the aws-waf-token; the Lambda never sees a CAPTCHA token.
# default_action = allow {} so the static site (GET /) is unchallenged.
resource "aws_wafv2_web_acl" "edge" {
  name        = "${local.name_prefix}-waf"
  description = "Edge WebACL for ${local.site_domain}: managed rules + rate limit + /api CAPTCHA"
  scope       = "CLOUDFRONT"

  default_action {
    allow {}
  }

  # token_domains lets the WAF CAPTCHA JS SDK (Plan 2c frontend) obtain tokens valid for the
  # site host and its www alias.
  token_domains = [local.site_domain, "www.${local.site_domain}"]

  # --- Rule 1: AWS managed common rule set ---
  rule {
    name     = "common-rule-set"
    priority = 1

    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        name        = "AWSManagedRulesCommonRuleSet"
        vendor_name = "AWS"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name_prefix}-common"
      sampled_requests_enabled   = true
    }
  }

  # --- Rule 2: rate-based per-IP throttle ---
  rule {
    name     = "rate-limit"
    priority = 2

    action {
      block {}
    }

    statement {
      rate_based_statement {
        limit              = 2000 # requests per 5-min sliding window per IP
        aggregate_key_type = "IP"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name_prefix}-rate"
      sampled_requests_enabled   = true
    }
  }

  # --- Rule 3: CAPTCHA on POST /api/* ---
  rule {
    name     = "captcha-api-post"
    priority = 3

    action {
      captcha {}
    }

    statement {
      and_statement {
        statement {
          byte_match_statement {
            search_string         = "/api/"
            positional_constraint = "STARTS_WITH"
            field_to_match {
              uri_path {}
            }
            text_transformation {
              priority = 0
              type     = "NONE"
            }
          }
        }
        statement {
          byte_match_statement {
            search_string         = "POST"
            positional_constraint = "EXACTLY"
            field_to_match {
              method {}
            }
            text_transformation {
              priority = 0
              type     = "NONE"
            }
          }
        }
      }
    }

    # How long a solved CAPTCHA stays valid before re-challenge.
    captcha_config {
      immunity_time_property {
        immunity_time = 300
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name_prefix}-captcha"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${local.name_prefix}-waf"
    sampled_requests_enabled   = true
  }
}

# API key the frontend WAF CAPTCHA JS SDK uses to request CAPTCHA tokens for the allowed
# domains. scope must match the WebACL (CLOUDFRONT). The token itself is exported as `api_key`.
resource "aws_wafv2_api_key" "captcha" {
  scope         = "CLOUDFRONT"
  token_domains = [local.site_domain, "www.${local.site_domain}"]
}
```

> **CAPTCHA targeting:** the meta-plan scopes CAPTCHA to "POST on URI path starting `/api/`". Implemented as an `and_statement` of two `byte_match_statement`s — one on `uri_path` (`STARTS_WITH "/api/"`) and one on the HTTP `method` (`EXACTLY "POST"`). Matching the method via `field_to_match { method {} }` + `EXACTLY "POST"` is the documented WAFv2 pattern.
> **Assumption (context7-unverified detail):** `aws_wafv2_api_key` exports its key via the `api_key` attribute and accepts `scope = "CLOUDFRONT"` + `token_domains`. context7 returned the `REGIONAL` example only; the `CLOUDFRONT` scope value and the `api_key` exported attribute are taken from the AWS WAFv2 API (`CreateAPIKey` / `APIKey`). If the provider names the attribute differently (e.g. `key`), adjust the Task 7 output reference accordingly.
> **Rate limit:** `limit = 2000`/5-min/IP is a conservative volumetric floor (WAFv2 minimum is 100); the precise 3/hr/IP contact limit is still enforced in the ingest Lambda's DynamoDB counter (2a) — this is the belt-and-suspenders edge layer from the design §7.

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/waf.tf
git commit -m "infra(2b): add CLOUDFRONT WAF WebACL (common+rate+/api CAPTCHA) + wafv2 api key"
```

---

## Task 5: CloudFront distribution (S3 default + /api/* Lambda-URL behavior)

**Files:**
- Create: `terraform/cloudfront.tf`

- [ ] **Step 1: Create `terraform/cloudfront.tf`**

```hcl
# Managed policies (data sources, by canonical name — avoids hardcoding IDs where possible):
#   - origin-request "AllViewerExceptHostHeader": forwards all viewer headers EXCEPT Host, so
#     the Lambda Function URL accepts the request (it rejects a mismatched Host). ID is the
#     well-known b689b0a8-53d0-40ab-baf2-68738e2966ac.
#   - cache policy "Managed-CachingDisabled": no caching for the dynamic /api/* origin.
data "aws_cloudfront_origin_request_policy" "all_viewer_except_host" {
  name = "Managed-AllViewerExceptHostHeader"
}

data "aws_cloudfront_cache_policy" "caching_disabled" {
  name = "Managed-CachingDisabled"
}

data "aws_cloudfront_cache_policy" "caching_optimized" {
  name = "Managed-CachingOptimized"
}

# OAC for the Lambda Function URL origin (SigV4-signed origin requests to the IAM-locked URL).
resource "aws_cloudfront_origin_access_control" "lambda" {
  name                              = "${local.name_prefix}-lambda-oac"
  description                       = "OAC for the ingest Lambda Function URL origin"
  origin_access_control_origin_type = "lambda"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

locals {
  s3_origin_id     = "${local.name_prefix}-s3-site"
  lambda_origin_id = "${local.name_prefix}-lambda-ingest"
  # Function URL is "https://<id>.lambda-url.<region>.on.aws/" — CloudFront origins want the
  # bare host, so strip the scheme and any trailing slash.
  lambda_origin_host = replace(replace(aws_lambda_function_url.ingest.function_url, "https://", ""), "/", "")
  # prod serves apex + www; non-prod just the env host.
  site_aliases = local.env == "prod" ? [local.site_domain, "www.${local.site_domain}"] : [local.site_domain]
}

resource "aws_cloudfront_distribution" "site" {
  enabled             = true
  is_ipv6_enabled     = true
  comment             = local.name_prefix
  default_root_object = "index.html"
  price_class         = "PriceClass_100"
  aliases             = local.site_aliases
  web_acl_id          = aws_wafv2_web_acl.edge.arn

  # --- Origin A: static-site S3 bucket (OAC) ---
  origin {
    domain_name              = aws_s3_bucket.site.bucket_regional_domain_name
    origin_id                = local.s3_origin_id
    origin_access_control_id = aws_cloudfront_origin_access_control.site.id
  }

  # --- Origin B: ingest Lambda Function URL (custom origin over HTTPS, OAC lambda) ---
  origin {
    domain_name              = local.lambda_origin_host
    origin_id                = local.lambda_origin_id
    origin_access_control_id = aws_cloudfront_origin_access_control.lambda.id

    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "https-only" # Function URL is HTTPS-only
      origin_ssl_protocols   = ["TLSv1.2"]
    }
  }

  # Default behavior → static SPA from S3.
  default_cache_behavior {
    target_origin_id       = local.s3_origin_id
    allowed_methods        = ["GET", "HEAD", "OPTIONS"]
    cached_methods         = ["GET", "HEAD"]
    viewer_protocol_policy = "redirect-to-https"
    compress               = true
    cache_policy_id        = data.aws_cloudfront_cache_policy.caching_optimized.id
  }

  # /api/* behavior → Lambda Function URL origin. All methods, no caching, forward all viewer
  # headers except Host (so the Function URL accepts the request), and let WAF challenge POSTs.
  ordered_cache_behavior {
    path_pattern             = "/api/*"
    target_origin_id         = local.lambda_origin_id
    allowed_methods          = ["GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"]
    cached_methods           = ["GET", "HEAD"]
    viewer_protocol_policy   = "redirect-to-https"
    compress                 = true
    cache_policy_id          = data.aws_cloudfront_cache_policy.caching_disabled.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer_except_host.id
  }

  restrictions {
    geo_restriction {
      restriction_type = "none"
    }
  }

  viewer_certificate {
    acm_certificate_arn      = aws_acm_certificate_validation.site.certificate_arn
    ssl_support_method       = "sni-only"
    minimum_protocol_version = "TLSv1.2_2021"
  }

  tags = {
    Name = local.name_prefix
  }
}
```

> **Lambda-URL origin host derivation:** `aws_lambda_function_url.ingest.function_url` is `https://<id>.lambda-url.<region>.on.aws/`. The nested `replace()` strips `https://` and the trailing `/`, leaving the bare host CloudFront's `origin.domain_name` expects.
> **`AllViewerExceptHostHeader`:** referenced by managed name via the data source (canonical ID `b689b0a8-53d0-40ab-baf2-68738e2966ac`). Forwarding all viewer headers *except* Host is what lets the IAM-locked Function URL accept the OAC-signed request — a mismatched Host would otherwise be rejected.
> **Cert reference:** uses `aws_acm_certificate_validation.site.certificate_arn` (not the raw cert ARN) so the distribution waits for the cert to be ISSUED before creating.
> **Assumption (context7-unverified detail):** context7 confirmed `origin_access_control_origin_type = "s3"` and `signing_protocol = "sigv4"`, but did not explicitly return the `"lambda"` origin type value. `"lambda"` is the documented OAC origin type for Function URL origins; if the provider rejects it on `tofu validate`/`plan`, fall back to a plain `custom_origin_config` Lambda origin **without** OAC and instead allow the public Function URL — but that weakens the lockdown, so prefer fixing the OAC value. Flagged for the executor to confirm at first `plan`.

- [ ] **Step 2: Validate (Tasks 3–5 together)**

Run: `cd terraform && tofu validate`
Expected: valid. This is the first point at which the Task 3 bucket policy's forward reference to `aws_cloudfront_distribution.site.arn` resolves.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/cloudfront.tf
git commit -m "infra(2b): add CloudFront distribution (S3 default + /api Lambda-URL behavior, WAF)"
```

---

## Task 6: Lambda permission for CloudFront invoke

Closes the open follow-up from 2a's Self-Review: the ingest Function URL is `AWS_IAM`-locked with no caller. This grants CloudFront (the specific distribution) permission to invoke it via OAC SigV4.

**Files:**
- Modify: `terraform/lambda.tf`

- [ ] **Step 1: Append to `terraform/lambda.tf`**

```hcl
# Let CloudFront (this distribution only) invoke the IAM-locked ingest Function URL via OAC.
# Without this, the OAC-signed origin requests from Task 5 would be denied (403).
# action is the Function-URL-specific InvokeFunctionUrl; source_arn scopes it to our distribution.
resource "aws_lambda_permission" "cloudfront_invoke_ingest" {
  statement_id           = "AllowCloudFrontInvoke"
  action                 = "lambda:InvokeFunctionUrl"
  function_name          = aws_lambda_function.ingest.function_name
  principal              = "cloudfront.amazonaws.com"
  source_arn             = aws_cloudfront_distribution.site.arn
  function_url_auth_type = "AWS_IAM"
}
```

> **`function_url_auth_type = "AWS_IAM"`** is required on the permission so it applies to the IAM-locked Function URL (matches `aws_lambda_function_url.ingest.authorization_type = "AWS_IAM"` from 2a). The `action` is `lambda:InvokeFunctionUrl` (the Function-URL invoke action), not `lambda:InvokeFunction`.

- [ ] **Step 2: Validate**

Run: `cd terraform && tofu validate`
Expected: valid.

- [ ] **Step 3: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/lambda.tf
git commit -m "infra(2b): grant CloudFront permission to invoke the IAM-locked ingest Function URL"
```

---

## Task 7: Outputs (interface contract) + plan review

**Files:**
- Modify: `terraform/outputs.tf`

- [ ] **Step 1: Append the edge outputs to `terraform/outputs.tf`**

These match the meta-plan Interface Contract exactly; Plans 2c and 3 consume them by name.

```hcl
output "cloudfront_distribution_id" {
  description = "CloudFront distribution ID (2c: cache invalidation; 3: RUM linkage)"
  value       = aws_cloudfront_distribution.site.id
}

output "cloudfront_domain_name" {
  description = "CloudFront distribution domain (*.cloudfront.net) — 2c retargets DNS to this; verify here pre-cutover"
  value       = aws_cloudfront_distribution.site.domain_name
}

output "cloudfront_arn" {
  description = "CloudFront distribution ARN"
  value       = aws_cloudfront_distribution.site.arn
}

output "site_bucket" {
  description = "Static-site S3 bucket name (2c: aws s3 sync target)"
  value       = aws_s3_bucket.site.id
}

output "acm_certificate_arn" {
  description = "ACM certificate ARN backing the distribution (internal to 2b)"
  value       = aws_acm_certificate_validation.site.certificate_arn
}

output "waf_web_acl_arn" {
  description = "Edge WAF WebACL ARN (3: optional RUM<->WAF link)"
  value       = aws_wafv2_web_acl.edge.arn
}

output "waf_captcha_api_key" {
  description = "WAF CAPTCHA API key for the frontend JS SDK (2c)"
  value       = aws_wafv2_api_key.captcha.api_key
  sensitive   = true
}

output "waf_captcha_integration_url" {
  description = "WAF CAPTCHA JS SDK integration URL for the frontend (2c)"
  value       = "https://${aws_wafv2_web_acl.edge.name}.${aws_wafv2_web_acl.edge.id}.sdk.awswaf.com/${aws_wafv2_web_acl.edge.id}/${aws_wafv2_api_key.captcha.api_key}/jsapi.js"
}
```

> **`waf_captcha_integration_url`** is the per-WebACL CAPTCHA JS SDK URL the frontend loads to call `AwsWafIntegration.getToken()`. The exact host/path shape is published by AWS in the WAF console's "application integration" page. The interpolation above follows the documented `…sdk.awswaf.com/<web-acl-id>/<api-key>/jsapi.js` form.
> **Assumption (context7-unverified detail):** the precise integration-URL string is **not** a Terraform-exported attribute — it is constructed here from the WebACL id/name + API key. If AWS changes the SDK URL format, 2c can instead read it from the WAF console and pass it as a `VITE_WAF_*` build var directly. Flagged so 2c does not treat this as authoritative without a console check. (`waf_web_acl.id` is the WebACL's GUID; `.name` is its friendly name — both used above.)

- [ ] **Step 2: Full validate + plan review**

Run:
```bash
cd terraform && tofu validate && tofu plan -out=2b.plan
```
Expected: a plan that **creates** the new edge resources (ACM cert + validation, validation CNAMEs, site bucket + BPA/SSE/versioning/policy, 2 OACs, WAF WebACL + api key, CloudFront distribution, Lambda permission) and **changes nothing** about the existing Cloudflare Pages/Worker/DNS resources in `main.tf`. Confirm **no `destroy`** actions against live Cloudflare resources. (A real `plan` requires the GitLab backend — `tofu init` with `GITLAB_ACCESS_TOKEN`, per-env address; or apply from CI. The 2a backend resources also show as creates because 2a is unapplied.)

- [ ] **Step 3: Apply (prod) + verify at the CloudFront domain**

```bash
cd terraform
# prod state keeps the existing address (no migration); CI passes per-env backend-config.
tofu apply 2b.plan
CF_DOMAIN=$(tofu output -raw cloudfront_domain_name)
echo "CloudFront domain: $CF_DOMAIN"

# Static site: serves (placeholder until 2c uploads dist/). 403/404 from S3 is expected
# until the bucket has an index.html — the point is CloudFront + cert + OAC are wired.
curl -sI "https://$CF_DOMAIN/" | head -n 20

# /api/* path reaches the Lambda origin through CloudFront -> OAC -> Function URL.
# A GET (no CAPTCHA rule) should reach the Lambda; a POST triggers the WAF CAPTCHA challenge
# (405 from the Lambda for a GET, or the contact handler's response — both prove the wiring).
curl -si -X POST "https://$CF_DOMAIN/api/contact" \
  -H 'content-type: application/json' \
  --data '{"name":"edge wiring test","email":"PLACEHOLDER_INBOX","message":"hi","website":"","formTimestamp":0}' \
  | head -n 30
# ^ Without a valid aws-waf-token, WAF returns a 405/CAPTCHA interstitial — that is SUCCESS
#   (proves the CAPTCHA rule is enforcing at the edge). Full end-to-end submission is verified
#   in 2c once the frontend obtains a token via the JS SDK.
```
Expected: `curl` to the apex path returns CloudFront headers (`x-cache`, `via: …cloudfront`); the `/api/contact` POST is intercepted by the WAF CAPTCHA action (no valid token) — demonstrating the edge enforces CAPTCHA before the Lambda. The cert shows ISSUED; ACM validation CNAMEs resolved.

- [ ] **Step 4: Commit**

```bash
cd terraform && tofu fmt
git add terraform/outputs.tf
git commit -m "infra(2b): add edge outputs (cloudfront/site_bucket/acm/waf + captcha key+url); plan applied"
```

---

## Dev vs prod notes

- **Single config, env-driven.** `var.environment` (Task 1) is the only switch. `prod` (default) → `site_domain = trystan-tbm.dev`, aliases `[apex, www]`, state at `.../state/portfolio`. `dev` → `site_domain = dev.trystan-tbm.dev`, alias `[dev.trystan-tbm.dev]` only (no `www.dev`), state at `.../state/portfolio-dev`.
- **State separation at init** (CI passes the right address):
  ```bash
  # prod (keeps the existing address — no state migration)
  tofu init -backend-config="address=https://gitlab.com/api/v4/projects/77949451/terraform/state/portfolio" \
            -backend-config="lock_address=https://gitlab.com/api/v4/projects/77949451/terraform/state/portfolio/lock" ...
  # dev
  tofu init -backend-config="address=https://gitlab.com/api/v4/projects/77949451/terraform/state/portfolio-dev" \
            -backend-config="lock_address=https://gitlab.com/api/v4/projects/77949451/terraform/state/portfolio-dev/lock" ...
  ```
- **Dev differs only by `site_domain` + state** — same resources, env-suffixed names (`portfolio-contact-dev-site`, `…-dev-waf`, …). Dev is applied on demand and destroyed when idle (no always-on dev stack).
- **No prod DNS wiring here.** The distribution is aliased to `site_domain`, but the prod Cloudflare root/`www` records still point at Pages. For **dev**, the ACM validation CNAMEs are created (DNS-only) but pointing `dev.trystan-tbm.dev` at the dev distribution is still a 2c-style DNS step — out of scope here; verify dev at its raw `*.cloudfront.net`. **Prod DNS cutover + Pages/Worker removal = Plan 2c.**

---

## Verification

- `tofu plan` shows **creates only** for edge + 2a resources; **no destroys** against the existing Cloudflare Pages/Worker/DNS in `main.tf`.
- ACM cert reaches **ISSUED** (validation CNAMEs resolved in Cloudflare, DNS-only).
- `curl -I https://<cloudfront_domain_name>/` returns CloudFront response headers (`via: …cloudfront.net`), proving distribution + cert + S3-OAC origin are wired (content is a placeholder until 2c uploads `dist/`).
- `curl -X POST https://<cloudfront_domain_name>/api/contact` (no `aws-waf-token`) is intercepted by the WAF CAPTCHA action — proving `/api/*` → Lambda-URL origin + OAC + the CAPTCHA rule all enforce at the edge. The IAM-locked Function URL now accepts CloudFront's signed requests (Task 6 permission).
- `tofu output` exposes the full interface-contract set, with `waf_captcha_api_key` marked `sensitive`.

---

## Self-Review

**Spec coverage (against the meta-plan Plan 2b scope + Interface Contract, and design §2/§3/§7):**
- Env scaffolding — `var.environment` + env-aware `name_prefix`/`site_domain`; re-keys all 2a names (free, 2a unapplied) → Task 1 ✓
- ACM cert (us-east-1) for `site_domain` + `www`, DNS-validated via Cloudflare CNAMEs (proxied=false) + `aws_acm_certificate_validation` → Task 2 ✓
- Static-site bucket `${name_prefix}-site` (private, SSE AES256, full BPA, versioned) + S3 OAC (sigv4) + OAC bucket policy (SourceArn = distribution) → Task 3 ✓
- WAF WebACL scope=CLOUDFRONT (us-east-1): AWSManagedRulesCommonRuleSet + rate-based rule + CAPTCHA on POST `/api/*`; `aws_wafv2_api_key` (CLOUDFRONT, token_domains=[site_domain, www]) → Task 4 ✓
- CloudFront: S3 default behavior (OAC, redirect-to-https, compress); `/api/*` → Lambda Function URL origin (custom HTTPS origin, OAC lambda, AllViewerExceptHostHeader, CachingDisabled, all methods); aliases = site_domain (+www prod); cert from Task 2; `web_acl_id` = WAF ARN → Task 5 ✓
- `aws_lambda_permission` principal `cloudfront.amazonaws.com`, source_arn = distribution ARN — closes 2a's open follow-up → Task 6 ✓
- Outputs: `cloudfront_distribution_id`/`_domain_name`/`_arn`, `site_bucket`, `acm_certificate_arn`, `waf_web_acl_arn`, `waf_captcha_api_key` (sensitive), `waf_captcha_integration_url` → Task 7 ✓

**context7-flagged assumptions (verify at first real `plan`/`apply`):**
1. OAC `origin_access_control_origin_type = "lambda"` for the Function URL origin — context7 confirmed `"s3"` + `sigv4` but did not echo the `"lambda"` literal; it is the documented value. Fallback noted in Task 5.
2. `aws_wafv2_api_key` `scope = "CLOUDFRONT"` + exported `api_key` attribute — context7 returned only the REGIONAL example; CLOUDFRONT + `api_key` taken from the WAFv2 API. Fallback (attribute name) noted in Task 4.
3. `waf_captcha_integration_url` is **constructed**, not a TF-exported attribute — the `…sdk.awswaf.com/<acl-id>/<api-key>/jsapi.js` shape should be confirmed against the WAF console; 2c can override with a console-read value. Noted in Task 7.
4. `cloudflare_dns_record.name` accepting the zone-suffixed validation FQDN (we `trimsuffix` the trailing dot) — noted in Task 2; strip to record-relative if the v5 provider rejects it.
5. `for_each` over `domain_validation_options` (drift-safe) substituted for the meta-plan's literal "count" — equivalent, current-docs-recommended; noted in Task 2.

**Deferred to Plan 2c (correctly NOT here):** the prod DNS retarget (flip Cloudflare root/`www` to DNS-only CNAME → `cloudfront_domain_name`); the frontend WAF-CAPTCHA JS SDK swap (consuming `waf_captcha_api_key` + `waf_captcha_integration_url`, replacing `@marsidev/react-turnstile`/`VITE_TURNSTILE_*`); the `aws s3 sync dist/ s3://<site_bucket>` upload + CloudFront invalidation (replacing `wrangler pages deploy`); removal of the Cloudflare Pages/Worker/route resources + `turnstile_*` vars/outputs from `main.tf`; CI OIDC + per-env backend-config + gated apply.

**Deferred to Plan 3 (observability):** X-Ray active tracing on both Lambdas; CloudWatch RUM (Cognito guest pool, `aws_rum_app_monitor` for `site_domain`, snippet in `index.html`); optional RUM↔WAF link via `waf_web_acl_arn`; dashboards/alarms (Lambda errors/throttles, SQS DLQ depth, SES bounce rate).

**Consistency checks:**
- All edge resources are `hashicorp/aws` in us-east-1 — covered by the existing `main.tf` `required_providers` pin; no new `required_providers`/alias added (respects the 2a single-block correction).
- `web_acl_id` on the distribution = `aws_wafv2_web_acl.edge.arn` (CloudFront wants the ACL **ARN**, not id).
- Distribution cert reference = `aws_acm_certificate_validation.site.certificate_arn` (waits for ISSUED).
- S3 bucket policy `AWS:SourceArn` = `aws_cloudfront_distribution.site.arn`; Lambda permission `source_arn` = same — both scope access to exactly this distribution.
- Lambda permission `action = lambda:InvokeFunctionUrl` + `function_url_auth_type = "AWS_IAM"` matches 2a's `authorization_type = "AWS_IAM"` Function URL.
- `token_domains` on both the WebACL and the api key = `[site_domain, www.site_domain]`, matching the CloudFront `aliases`.
- No email/phone/secret literals introduced (passes the repo's pre-commit secrets check); `PLACEHOLDER_INBOX` kept literal in the verify curl.

**Known manual/CI steps (not automatable in tofu here):** per-env `tofu init` backend-config (CI); confirming the two context7-flagged OAC/api-key assumptions at first real `plan`; reading the authoritative WAF CAPTCHA integration URL from the console for 2c if the constructed form differs.
