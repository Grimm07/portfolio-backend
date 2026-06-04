# portfolio-backend

Contact backend for [trystan-tbm.dev](https://trystan-tbm.dev) — the `portfolio-contact-ingest`
AWS Lambda and the OpenTofu that provisions it. Extracted from the
[`portfolio`](https://github.com/Grimm07/portfolio) repo, which now owns only the frontend +
static-site deploy.

## Architecture

A single `portfolio-contact-ingest` Lambda invoked by an infra-owned **API Gateway** (HTTP API,
payload format 2.0), fronted by CloudFront which injects an `x-origin-verify` secret header.

- `backend/cmd/ingest/main.go` — entrypoint; calls `lambda.Start`.
- `backend/internal/ingest/handler.go` — verify `x-origin-verify` (reject WAF-bypassing requests) →
  parse (API Gateway v2 event) → honeypot (`website`) → time-trap → field validation → SES send.
- `backend/internal/email` — sends one Amazon SES email per submission (`Reply-To` = submitter).
- `backend/internal/ip` — extracts client IP for the email body.
- `backend/internal/secrets` — reads the recipient address from Secrets Manager (never hardcoded).
- `backend/internal/validation` — shared field validation (honeypot, time-trap, email/name/message).

**CAPTCHA + rate-limiting are enforced by AWS WAF at the edge**, before the request reaches the
Lambda — there is no token check or per-IP counter in the handler.

## Layout

```
backend/      Lambda source (Go, module github.com/Grimm07/portfolio-backend/backend, go 1.22)
terraform/    OpenTofu — ingest Lambda, IAM, SES/DKIM, contact-email secret, SSM handshake
docs/         backend runbook (docs/runbooks), design spec (docs/specs), contact-pipeline plans
.github/      CI (tests + tofu validate) and Deploy (build Lambda + tofu apply) workflows
```

## Development

```bash
# Lambda (from backend/)
cd backend
go test ./...     # Go stdlib testing suite (handler, email, validation, ip, secrets)
make build        # produces backend/bootstrap (static arm64 binary, required before tofu apply)

# Infrastructure (from terraform/) — OpenTofu, AWS-only, per-env state
# `tofu` is a user-local install (~/.local/bin), not on the system PATH:
export PATH="$HOME/.local/bin:$PATH"
cd terraform
tofu init -reconfigure -backend-config=backend-dev.hcl   # dev (or backend-prod.hcl)
tofu validate
tofu apply -var environment=dev
```

**IMPORTANT**: build the Lambda binary before applying — the archive references `backend/bootstrap`:

```bash
cd backend && make build
cd ../terraform && tofu apply -var environment=dev
```

## Deployment

`.github/workflows/deploy.yml` builds the Lambda binary (`make build`) and runs `tofu apply`
(per-env backend-config) via GitHub OIDC. PRs deploy **dev**; pushes to `main` deploy **prod**.

> **⚠️ Cross-repo OIDC prerequisite.** The `portfolio-deploy` IAM role in each AWS account
> (owned by the infra/shadowspire landing zone, **not** this repo) must trust this repo's OIDC
> subject. The trust policy needs to allow:
>
> ```
> repo:Grimm07/portfolio-backend:environment:dev
> repo:Grimm07/portfolio-backend:environment:production
> ```
>
> (it previously only allowed `repo:Grimm07/portfolio:environment:*`). Until that change lands in
> the infra repo, the deploy's `Configure AWS credentials` step fails to assume the role.

Required GitHub **secrets** (per environment): `CONTACT_EMAIL`. DNS is on Amazon Route 53, so the
SES DKIM records are written via the AWS provider (no Cloudflare token) — the deploy's OIDC role must
be able to read+write the `trystan-tbm.dev` hosted zone (see the cross-account note in
`terraform/ses.tf`). The `production` environment should require reviewers and restrict to `main`.

## Operations

See [`docs/runbooks/aws-contact-backend.md`](./docs/runbooks/aws-contact-backend.md). Notable
gotchas:

- **SES is in sandbox (intentional)**: both the sending domain AND the recipient inbox must be
  verified or `SendEmail` throws `MessageRejected` → the form returns 500. The recipient identity
  needs a one-time manual click on the AWS verification email. Dev and prod verify separately.
- **Never log a caught error object/message** in the Lambda — doing so can embed env-sourced data
  (secret ARN, addresses) in logs. Log a fixed-literal label instead (see `errorLabel()` in
  `backend/internal/ingest/handler.go`, which maps `smithy.APIError.ErrorCode()` to a fixed string).

## Security

- No email addresses, phone numbers, or secrets in code. The recipient address lives in **AWS
  Secrets Manager**, read at runtime via `CONTACT_EMAIL_SECRET_ARN`.
- `terraform/terraform.tfvars` holds sensitive values and is gitignored.
