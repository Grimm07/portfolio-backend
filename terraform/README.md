# Contact Backend Infrastructure (OpenTofu / AWS)

This directory is the **OpenTofu** configuration for the portfolio's **contact backend** on AWS
(region `us-east-1`). It manages the ingest Lambda, its IAM role, SES sending identity, the
recipient secret, and the SSM handshake that wires the function into the edge.

> Tooling note: this project uses **OpenTofu** (`tofu`), **not** the `terraform` CLI. Every command
> below is `tofu ...`. See [Prerequisites](#prerequisites) for the user-local install path.

## Table of Contents

1. [Overview](#overview)
2. [Prerequisites](#prerequisites)
3. [The SSM Handshake](#the-ssm-handshake)
4. [State Backend (per-env, per-account)](#state-backend-per-env-per-account)
5. [Setup](#setup)
6. [Build + Apply Workflow](#build--apply-workflow)
7. [Outputs](#outputs)
8. [Troubleshooting](#troubleshooting)
9. [Security Notes](#security-notes)
10. [Quick Reference](#quick-reference)

---

## Overview

### What this configuration manages

| Resource | File | Notes |
|----------|------|-------|
| Ingest Lambda `portfolio-contact-ingest` + `AWS_IAM` Function URL | `lambda.tf` | Bundle is `backend/dist/ingest/index.mjs`, zipped via `archive_file` |
| Lambda IAM role + least-privilege policy | `iam.tf` | `ses:SendEmail`/`ses:SendRawEmail` (scoped by a `ses:FromAddress` condition), `secretsmanager:GetSecretValue`, plus basic Lambda logging |
| SES domain identity + DKIM | `ses.tf` | DKIM CNAMEs are created in the **Cloudflare DNS zone** — the only remaining Cloudflare use |
| SES recipient email identity | `ses.tf` | Triggers a one-time verification email to `contact_email` (manual click) |
| Contact-email secret `portfolio-contact-contact-email` | `secrets.tf` | Holds the recipient address; read by the Lambda at runtime |
| SSM publish: function URL + ARN | `ssm.tf` | `/portfolio/<env>/ingest-function-url`, `/portfolio/<env>/ingest-function-arn` |
| CloudFront-OAC invoke permission | `permissions.tf` | Reads `/portfolio/<env>/cloudfront-distribution-arn` from SSM and grants `lambda:InvokeFunctionUrl` to `cloudfront.amazonaws.com`, scoped to that distribution |

### What this configuration does NOT manage

The edge/hosting layer is owned by the separate **shadowspire** infrastructure repo (the landing
zone), not here. Do **not** create these in this directory:

- The S3 site bucket
- The CloudFront distribution
- The ACM certificate
- The AWS WAF web ACL

This config and the infra repo communicate exclusively through SSM Parameter Store (see
[The SSM Handshake](#the-ssm-handshake)).

---

## Prerequisites

### OpenTofu (`tofu`) — user-local install

The `tofu` binary is installed under `~/.local/bin` and is **not** on the system `PATH`. Export it
at the start of every shell session before running any command in this guide:

```bash
export PATH="$HOME/.local/bin:$PATH"
tofu version   # confirm it resolves
```

Required version: `>= 1.0` (see `main.tf`).

### AWS credentials (per account)

`dev` and `prod` live in **separate AWS accounts**, so you authenticate to whichever account you are
deploying to (typically via AWS SSO or a named profile):

| Env | Account ID |
|-----|------------|
| dev | `176355979099` |
| prod | `681053994223` |

Example:

```bash
aws sso login --profile portfolio-dev      # or portfolio-prod
export AWS_PROFILE=portfolio-dev
aws sts get-caller-identity                 # confirm the account matches the env you intend
```

All resources are created in `us-east-1`.

### Node.js (for the Lambda build)

The Lambda bundle is produced by the `backend/` build before each apply. Node 18+ or 20+ is required
(the Lambda runtime is `nodejs20.x`). See [Build + Apply Workflow](#build--apply-workflow).

---

## The SSM Handshake

This repo and the infra (shadowspire) repo hand off through SSM parameters under
`/portfolio/<env>/`. Understanding this is the key to applying in the right order.

**This repo PUBLISHES (other repo reads):**

- `/portfolio/<env>/ingest-function-url` — the Lambda Function URL
- `/portfolio/<env>/ingest-function-arn` — the Lambda ARN

**This repo READS (published by the infra repo):**

- `/portfolio/<env>/cloudfront-distribution-arn` — used by `permissions.tf` to scope the OAC grant
- `/portfolio/<env>/waf-integration-url` — consumed by the frontend build (`VITE_WAF_INTEGRATION_URL`)
- `/portfolio/<env>/waf-api-key` — consumed by the frontend/edge

### Dependency order

1. **This repo applies first.** It creates the Lambda + Function URL and publishes
   `ingest-function-url` / `ingest-function-arn`. On a brand-new env, `permissions.tf` will fail at
   this point because `cloudfront-distribution-arn` does not exist yet — that is expected (see
   [Troubleshooting](#troubleshooting)).
2. **The infra repo applies next.** It wires CloudFront's `/api/*` origin to the Function URL and
   publishes `cloudfront-distribution-arn`.
3. **This repo applies again.** Now `permissions.tf` finds the distribution ARN and grants
   CloudFront's OAC permission to invoke the Function URL.

---

## State Backend (per-env, per-account)

The S3 backend is **partial** (`backend.tf`): the bucket and lock table are supplied per-env at init
time, because each env's deploy role can only reach its own account's state bucket.

| Env | State bucket | Lock table | Backend config |
|-----|--------------|------------|----------------|
| dev | `shadowspire-dev-state-176355979099` | `shadowspire-dev-tf-lock` | `backend-dev.hcl` |
| prod | `shadowspire-prod-state-681053994223` | `shadowspire-prod-tf-lock` | `backend-prod.hcl` |

State key (both envs): `portfolio/terraform.tfstate`. Region: `us-east-1`. Encryption is enabled.

Select the backend at init:

```bash
tofu init -reconfigure -backend-config=backend-dev.hcl    # or backend-prod.hcl
```

> Always re-init with `-reconfigure` when switching environments so you don't accidentally apply
> `dev` changes against `prod` state (or vice versa). The env you `init` must match the
> `-var environment=` you `apply`.

---

## Setup

Create `terraform.tfvars` from the example (it is **gitignored** — it contains the recipient
address and the Cloudflare token):

```bash
export PATH="$HOME/.local/bin:$PATH"
cp terraform.tfvars.example terraform.tfvars
```

Fill in the required inputs:

```hcl
# Selects which env's SSM paths/resources this apply targets. Must be "dev" or "prod".
environment = "dev"

# Recipient address for contact emails (stored in Secrets Manager, never in plaintext code).
contact_email = "your-email@example.com"

# Cloudflare — required ONLY for the SES DKIM CNAMEs (DNS still lives in the Cloudflare zone).
cloudflare_api_token = "..."
cloudflare_zone_id   = "..."

# domain_name defaults to trystan-tbm.dev; override only if needed.
# domain_name = "trystan-tbm.dev"
```

| Variable | Required | Purpose |
|----------|----------|---------|
| `environment` | yes | `"dev"` or `"prod"` (validated); drives SSM paths and resource selection |
| `contact_email` | yes | Recipient address, stored in Secrets Manager |
| `cloudflare_api_token` | yes | DNS edit token for the SES DKIM CNAMEs only |
| `cloudflare_zone_id` | yes | Zone for the DKIM CNAMEs |
| `domain_name` | no | Defaults to `trystan-tbm.dev` |
| `aws_region` | no | Defaults to `us-east-1` |

> The Cloudflare token needs only **Zone → DNS → Edit** on the `trystan-tbm.dev` zone — nothing
> more. It exists solely to publish the three SES DKIM CNAME records.

---

## Build + Apply Workflow

### Critical: build the Lambda bundle first

The `archive_file` data source in `lambda.tf` reads `backend/dist/ingest/index.mjs`. If that file
does not exist, the apply fails. **Always build the bundle before `tofu apply`:**

```bash
export PATH="$HOME/.local/bin:$PATH"
cd backend && npm run build      # produces backend/dist/ingest/index.mjs
cd ../terraform
```

### Canonical workflow (dev)

```bash
export PATH="$HOME/.local/bin:$PATH"

# 1. Build the Lambda bundle
cd backend && npm run build && cd ../terraform

# 2. Init the dev backend
tofu init -reconfigure -backend-config=backend-dev.hcl

# 3. Validate config
tofu validate

# 4. Plan / apply against dev (must match the backend you init'd)
tofu plan  -var environment=dev
tofu apply -var environment=dev
```

### Promoting to prod

Re-init against the prod backend and apply with `environment=prod`. Make sure your AWS credentials
point at the **prod account** (`681053994223`):

```bash
export AWS_PROFILE=portfolio-prod
aws sts get-caller-identity                          # confirm 681053994223

cd backend && npm run build && cd ../terraform
tofu init -reconfigure -backend-config=backend-prod.hcl
tofu validate
tofu apply -var environment=prod
```

### Validate-only (no AWS credentials)

To syntax/type-check the configuration without touching a backend or AWS:

```bash
tofu init -backend=false
tofu validate
```

### CI vs. local applies

In normal operation, deploys run through **GitHub Actions with OIDC**
(`.github/workflows/deploy.yml`): the workflow builds the Lambda bundle and runs `tofu apply` for the
target env (using the matching per-env backend config) before syncing the site. There are no
long-lived AWS keys.

Local applies are reserved for **break-glass / first-time setup** — e.g. bootstrapping a new env, or
the initial two-phase handshake with the infra repo.

---

## Outputs

After a successful apply (`tofu output`):

| Output | Description |
|--------|-------------|
| `ingest_function_url` | The IAM-auth Lambda Function URL (fronted by CloudFront OAC) |
| `ingest_function_name` | The Lambda function name (`portfolio-contact-ingest`), handy for `aws lambda invoke` |
| `ingest_function_url_ssm_param` | SSM param name where the Function URL is published |
| `ingest_function_arn_ssm_param` | SSM param name where the Lambda ARN is published |

> The Function URL is `AWS_IAM`-authorized — calling it directly without a SigV4 signature returns
> `403`. Only CloudFront's OAC (granted in `permissions.tf`) can invoke it.

---

## Troubleshooting

### `archive_file` can't find `backend/dist/ingest/index.mjs`

The Lambda bundle hasn't been built. Run the build, then apply:

```bash
cd backend && npm run build
cd ../terraform && tofu apply -var environment=dev
```

### `permissions.tf` / data source error: SSM parameter not found

`data.aws_ssm_parameter.cf_arn` reads `/portfolio/<env>/cloudfront-distribution-arn`, which is
published by the **infra repo**. If it's missing:

- On a brand-new env, this is expected on the **first** apply — the infra repo hasn't wired
  CloudFront yet. Complete the infra repo's phase, then re-apply here (see
  [The SSM Handshake](#the-ssm-handshake)).
- If the infra phase is supposedly done, confirm the parameter exists and you're in the right
  account/region:
  ```bash
  aws ssm get-parameter --name /portfolio/dev/cloudfront-distribution-arn --region us-east-1
  ```

### Wrong account or wrong backend

Symptoms: `AccessDenied` on the state bucket, an unexpected plan (resources you didn't change show as
create/destroy), or state that doesn't match the env.

- Confirm credentials match the env's account:
  ```bash
  aws sts get-caller-identity   # dev=176355979099, prod=681053994223
  ```
- Re-init the correct backend with `-reconfigure`:
  ```bash
  tofu init -reconfigure -backend-config=backend-dev.hcl    # or backend-prod.hcl
  ```
- Ensure `-var environment=` matches the backend you init'd.

### `environment must be "dev" or "prod"`

The `environment` variable is validated. Set it in `terraform.tfvars` or pass
`-var environment=dev` / `-var environment=prod`.

### SES verification / DKIM not active

- The recipient identity (`aws_ses_email_identity.recipient`) sends a one-time confirmation email to
  `contact_email`. Click the link in that email or SES will refuse to send to it.
- DKIM CNAMEs are created in the Cloudflare zone; propagation can take a few minutes. Check status:
  ```bash
  aws ses get-identity-dkim-attributes --identities trystan-tbm.dev --region us-east-1
  ```

### Inspecting state

```bash
tofu state list
tofu state show aws_lambda_function.ingest
```

---

## Security Notes

- **No plaintext PII in code.** No email addresses or phone numbers are hardcoded anywhere in this
  configuration. The recipient address lives in **AWS Secrets Manager**
  (`portfolio-contact-contact-email`) and is read by the Lambda at runtime; the sender is derived as
  `noreply@<domain_name>`.
- **`terraform.tfvars` is gitignored.** It contains `contact_email` and the Cloudflare token — never
  commit it. Verify with `git status` before committing.
- **State may contain sensitive values.** It lives encrypted in the per-env S3 backend, scoped to
  each account. Don't copy `*.tfstate` locally or into git.
- **Least-privilege IAM.** The Lambda role can only send SES email from the verified `from_email`
  (enforced by a `ses:FromAddress` condition), read the one contact-email secret, and write its own
  logs.
- **IAM-locked Function URL.** Direct public calls return `403`; only CloudFront's OAC can invoke it.
- **No long-lived AWS keys in CI.** Deploys use GitHub Actions OIDC.

---

## Quick Reference

```bash
# Always first: put tofu on PATH (user-local install)
export PATH="$HOME/.local/bin:$PATH"

# Build the Lambda bundle (REQUIRED before apply)
cd backend && npm run build && cd ../terraform

# Init the per-env backend (dev shown; use backend-prod.hcl for prod)
tofu init -reconfigure -backend-config=backend-dev.hcl

# Validate
tofu validate

# Plan / apply (env must match the backend you init'd)
tofu plan  -var environment=dev
tofu apply -var environment=dev

# Validate only, no AWS creds
tofu init -backend=false && tofu validate

# Outputs / state
tofu output
tofu state list
```

### File structure

```
terraform/
├── main.tf                   # required_providers (cloudflare/aws/archive) + cloudflare provider
├── providers_aws.tf          # aws provider, default tags, locals (name_prefix, from_email)
├── variables.tf              # environment, contact_email, cloudflare_*, domain_name, aws_region
├── outputs.tf                # ingest function url/name + SSM param names
├── backend.tf                # partial S3 backend (bucket/lock supplied per-env at init)
├── backend-dev.hcl           # dev account backend config
├── backend-prod.hcl          # prod account backend config
├── lambda.tf                 # ingest Lambda + AWS_IAM Function URL (archive from backend/dist)
├── iam.tf                    # ingest role + least-privilege policy
├── ses.tf                    # SES domain identity + DKIM (CNAMEs in Cloudflare zone) + recipient
├── secrets.tf                # contact-email secret (recipient address)
├── ssm.tf                    # publishes ingest-function-url / ingest-function-arn
├── permissions.tf            # CloudFront OAC invoke grant (reads cloudfront-distribution-arn)
├── terraform.tfvars          # your values (NOT in git)
├── terraform.tfvars.example  # template
└── README.md                 # this file
```

---

**Last Updated:** 2026-06
