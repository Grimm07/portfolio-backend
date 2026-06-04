# portfolio-backend

Contact backend for [trystan-tbm.dev](https://trystan-tbm.dev) — the `portfolio-contact-ingest`
AWS Lambda and the OpenTofu that provisions it. Extracted from the
[`portfolio`](https://github.com/Grimm07/portfolio) repo, which now owns only the frontend +
static-site deploy.

> **Language migration pending.** The Lambda is currently TypeScript/Node (`nodejs20.x`). A planned
> rewrite to Go (`provided.al2023`) is specced in [`TODO-go-rewrite.md`](./TODO-go-rewrite.md) but
> **not yet started** — everything here is still Node.

## Architecture

A single `portfolio-contact-ingest` Lambda invoked by an infra-owned **API Gateway** (HTTP API,
payload format 2.0), fronted by CloudFront which injects an `x-origin-verify` secret header.

- `backend/src/ingest/handler.ts` — verify `x-origin-verify` (reject WAF-bypassing requests) →
  parse (API Gateway v2 event) → honeypot (`website`) → time-trap → field validation → SES send.
- `backend/src/ingest/email.ts` — sends one Amazon SES email per submission (`Reply-To` = submitter).
- `backend/src/ingest/ip.ts` — extracts client IP for the email body.
- `backend/src/shared/secrets.ts` — reads the recipient address from Secrets Manager (never hardcoded).
- `backend/src/shared/validation.ts`, `backend/src/shared/types.ts` — shared validation + the
  `ContactSubmission` shape.

**CAPTCHA + rate-limiting are enforced by AWS WAF at the edge**, before the request reaches the
Lambda — there is no token check or per-IP counter in the handler.

## Layout

```
backend/      Lambda source (Node + TypeScript) + Vitest suite
terraform/    OpenTofu — ingest Lambda, IAM, SES/DKIM, contact-email secret, SSM handshake
docs/         backend runbook (docs/runbooks), design spec (docs/specs), contact-pipeline plans
.github/      CI (tests + tofu validate) and Deploy (build Lambda + tofu apply) workflows
```

## Development

```bash
# Lambda (from backend/)
cd backend
npm ci
npm test          # Vitest suite (handler, email, validation, ip, secrets)
npm run typecheck # tsc --noEmit
npm run build     # esbuild -> dist/ingest/index.mjs  (required before tofu apply)

# Infrastructure (from terraform/) — OpenTofu, AWS-only, per-env state
# `tofu` is a user-local install (~/.local/bin), not on the system PATH:
export PATH="$HOME/.local/bin:$PATH"
cd terraform
tofu init -reconfigure -backend-config=backend-dev.hcl   # dev (or backend-prod.hcl)
tofu validate
tofu apply -var environment=dev
```

**IMPORTANT**: build the Lambda bundle before applying — the archive references `backend/dist`:

```bash
cd backend && npm run build
cd ../terraform && tofu apply -var environment=dev
```

## Deployment

`.github/workflows/deploy.yml` builds the Lambda bundle and runs `tofu apply` (per-env
backend-config) via GitHub OIDC. PRs deploy **dev**; pushes to `main` deploy **prod**.

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

Required GitHub **secrets** (per environment): `CONTACT_EMAIL`, `CLOUDFLARE_API_TOKEN`,
`CLOUDFLARE_ZONE_ID`. The `production` environment should require reviewers and restrict to `main`.

## Operations

See [`docs/runbooks/aws-contact-backend.md`](./docs/runbooks/aws-contact-backend.md). Notable
gotchas:

- **SES is in sandbox (intentional)**: both the sending domain AND the recipient inbox must be
  verified or `SendEmail` throws `MessageRejected` → the form returns 500. The recipient identity
  needs a one-time manual click on the AWS verification email. Dev and prod verify separately.
- **Never log a caught error object/message** in the Lambda — CodeQL `js/clear-text-logging` fails
  the PR. Log a fixed-literal label instead (see `errorLabel()` in `handler.ts`).

## Security

- No email addresses, phone numbers, or secrets in code. The recipient address lives in **AWS
  Secrets Manager**, read at runtime via `CONTACT_EMAIL_SECRET_ARN`.
- `terraform/terraform.tfvars` holds sensitive values and is gitignored.
