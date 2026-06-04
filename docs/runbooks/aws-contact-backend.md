# Runbook — AWS Contact Backend

Operational guide for the portfolio contact backend: a single AWS Lambda
(`portfolio-contact-ingest`) behind an `AWS_IAM` Function URL, fronted by CloudFront (OAC),
emailing via Amazon SES. Region is **us-east-1** for everything.

> Architecture overview lives in `CLAUDE.md`; the historical migration plan is in
> `docs/superpowers/plans/2026-06-03-aws-cutover-reconciliation.md`.

---

## Environments (separate AWS accounts)

| env  | account        | deploy role (OIDC)                                  | site bucket                          | CloudFront dist | host(s)                                  |
|------|----------------|----------------------------------------------------|--------------------------------------|-----------------|------------------------------------------|
| dev  | `176355979099` | `arn:aws:iam::176355979099:role/portfolio-deploy`  | `shadowspire-dev-site-176355979099`  | `EGFCTGJJEER89` | `dev.trystan-tbm.dev`                    |
| prod | `681053994223` | `arn:aws:iam::681053994223:role/portfolio-deploy`  | `shadowspire-prod-site-681053994223` | `E229NB0LSTX2V8`| `trystan-tbm.dev`, `www.trystan-tbm.dev` |

The S3 site bucket, CloudFront distribution, ACM cert, and AWS WAF are **owned by the separate
infra repo** (the shadowspire landing zone). This repo owns only the contact backend + the SSM
handshake, and (in CI) syncs the built site to the infra-owned bucket and invalidates the dist.

Terraform state is a **per-env, per-account S3 backend**, selected at init:

```
dev  -> bucket shadowspire-dev-state-176355979099  + lock shadowspire-dev-tf-lock
prod -> bucket shadowspire-prod-state-681053994223 + lock shadowspire-prod-tf-lock
key  = portfolio/terraform.tfstate
```

---

## The SSM handshake

The contact backend and the infra repo coordinate through SSM Parameter Store (per env):

**This repo PUBLISHES:**
- `/portfolio/<env>/ingest-function-url` — the Lambda Function URL (CloudFront `/api/*` origin)
- `/portfolio/<env>/ingest-function-arn` — the Lambda ARN

**This repo READS (published by infra):**
- `/portfolio/<env>/cloudfront-distribution-arn` — used by `permissions.tf` to scope the OAC invoke grant
- `/portfolio/<env>/waf-integration-url` — frontend `VITE_WAF_INTEGRATION_URL` (CAPTCHA SDK)
- `/portfolio/<env>/waf-api-key` — frontend `VITE_WAF_API_KEY` (CAPTCHA widget)

**Dependency order:** this repo applies first (publishes function-url/arn) → infra wires CloudFront's
`/api/*` origin to the Function URL and publishes `cloudfront-distribution-arn` → this repo's
`permissions.tf` apply grants CloudFront OAC `lambda:InvokeFunctionUrl`.

Inspect a param:

```bash
aws ssm get-parameter --profile shadowspire-dev --region us-east-1 \
  --name /portfolio/dev/ingest-function-url --query Parameter.Value --output text
```

---

## Deploying

### Normal path — GitHub Actions (OIDC)

`.github/workflows/deploy.yml` authenticates to AWS via OIDC (no long-lived keys), then per env:

1. Builds the Lambda bundle (`cd backend && npm run build`).
2. `tofu init -reconfigure -backend-config=backend-<env>.hcl` then
   `tofu apply -auto-approve -var environment=<env> ...`.
3. Builds the frontend and `aws s3 sync`s it to the env's site bucket.
4. Invalidates the CloudFront distribution.

The `production` GitHub Environment should require reviewers; the prod job pauses for approval.

### Break-glass / first-time — local apply

> `tofu` is a user-local install. Run `export PATH="$HOME/.local/bin:$PATH"` first.
> **Always build the Lambda bundle before applying** — the archive references `backend/dist`.

```bash
export PATH="$HOME/.local/bin:$PATH"
cd backend && npm run build && cd ../terraform

# dev
tofu init -reconfigure -backend-config=backend-dev.hcl
tofu apply -var environment=dev

# prod (only after dev is verified)
tofu init -reconfigure -backend-config=backend-prod.hcl
tofu apply -var environment=prod
```

Validate-only without credentials: `tofu init -backend=false && tofu validate`.

---

## Verifying a deploy

Per host (`dev.trystan-tbm.dev`, then `trystan-tbm.dev`):

```bash
# 1. Site served from S3 via CloudFront over HTTPS (expect 200 + CloudFront headers)
curl -sI https://<host>/ | grep -iE 'HTTP|server|x-cache|x-amz-cf-id'

# 2. /api/contact with NO solved token -> WAF CAPTCHA challenge at the edge (NOT 200)
curl -s -i -X POST https://<host>/api/contact -H 'content-type: application/json' \
  -d '{"name":"x","email":"x@y.co","message":"edge probe message","website":"","formTimestamp":0}' | head -20

# 3. The Function URL is NOT directly invocable (expect 403 — AWS_IAM, no SigV4)
curl -s -o /dev/null -w '%{http_code}\n' -X POST "<function-url>" \
  -H 'content-type: application/json' -d '{}'
```

4. **Real form submit** (human step): open the site, submit the contact form, solve the CAPTCHA,
   confirm the email is received. The CAPTCHA solve cannot be automated, so this is the one manual
   acceptance test. Confirm a Lambda invocation in CloudWatch if needed.

Direct Lambda invoke (bypasses the URL auth — useful to isolate the SES path from the edge):

```bash
aws lambda invoke --profile shadowspire-dev --region us-east-1 \
  --function-name portfolio-contact-ingest --cli-binary-format raw-in-base64-out \
  --payload '{"headers":{"x-forwarded-for":"1.2.3.4"},"body":"{\"name\":\"Smoke\",\"email\":\"REDACTED\",\"message\":\"smoke test\",\"website\":\"\",\"formTimestamp\":0}"}' \
  /tmp/out.json && cat /tmp/out.json   # expect {"statusCode":200,...}
```

---

## SES notes

- The recipient address lives in **AWS Secrets Manager** (read at runtime via
  `CONTACT_EMAIL_SECRET_ARN`) — never in code, env, or git. Set it via the `contact_email` tfvar.
- The recipient **identity must be verified** in SES (one-time), and the domain **DKIM CNAMEs**
  must resolve. The DKIM CNAMEs are managed in the Cloudflare DNS zone (`ses.tf`).
- The account may be in the **SES sandbox** — that only allows sending to verified identities.
  Request SES production access **only** if you ever need to email recipients beyond the verified
  inbox.

---

## Rollback

- **Bad frontend build:** re-run the deploy from a known-good commit (re-sync + invalidate), or
  `aws s3 sync` a previous `dist/` and invalidate the distribution.
- **Bad Lambda:** revert the offending commit and re-apply, or roll back to a prior version via the
  console. The handler is stateless (no datastore), so there is nothing to migrate back.
- **Total contact-path outage:** the form degrades to "submit disabled" if the WAF CAPTCHA cannot
  render; the rest of the static site is unaffected (served straight from S3/CloudFront).

---

## Outstanding manual items (one-time, human)

- [ ] **SES:** confirm the recipient identity is verified and the domain DKIM CNAMEs resolve in
      both accounts; request SES production access only if recipients beyond the verified inbox are needed.
- [ ] **GitHub Environments:** `dev` and `production` exist with names matching the OIDC subjects;
      `production` requires reviewers.
- [ ] **GitHub secrets** (per environment): `CONTACT_EMAIL`, `CLOUDFLARE_API_TOKEN`,
      `CLOUDFLARE_ZONE_ID` (the last two only for the SES DKIM CNAMEs). Without `CONTACT_EMAIL`, a CI
      `tofu apply` would revert the recipient.
- [ ] **Cloudflare Pages git integration:** disconnect it in the Cloudflare dashboard so it stops
      auto-building on pushes (the site is served from CloudFront now).
- [ ] **Old secrets:** delete unused Turnstile / MailChannels / CF-Pages secrets; keep
      `CLOUDFLARE_API_TOKEN` / `CLOUDFLARE_ZONE_ID` (still needed for SES DKIM).
