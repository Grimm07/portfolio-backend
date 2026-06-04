# Portfolio Contact API — Phase 3 (API Gateway cutover) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Switch the portfolio contact Lambda from the (now-deleted) CloudFront-OAC → Lambda Function URL path to being invoked by the infra-owned API Gateway, validating the `x-origin-verify` shared secret so requests that skip WAF are rejected.

**Architecture:** The infra repo's `make dev-apply` already stood up an HTTP API (`POST /api/contact`, `AWS_PROXY`, payload format 2.0) pointed at this Lambda's ARN, repointed CloudFront `/api/*` at it with an injected `x-origin-verify` header, deleted the old Lambda-URL OAC, and published `/portfolio/<env>/api-execution-arn` + `/portfolio/<env>/origin-verify-secret` to SSM. This plan makes the portfolio side match: (1) the handler adapts to the API Gateway v2 event and enforces `x-origin-verify`; (2) terraform grants API Gateway `lambda:InvokeFunction` scoped to the execution ARN, injects the secret as an env var, and deletes the Function URL + its SSM param. The frontend is **already** wired (`deploy.yml` reads the WAF params from SSM and builds with `VITE_WAF_*`; `Contact.tsx` posts same-origin to `/api/contact`).

**Tech Stack:** TypeScript Lambda (Node 20, ESM, esbuild bundle), Vitest + `aws-sdk-client-mock`, OpenTofu/Terraform (`terraform/`), deployed by `.github/workflows/deploy.yml` via OIDC (`portfolio-deploy` role).

**Prerequisite:** Infra **phase 2 must be applied for the target env** (dev: DONE 2026-06-04; prod: blocked until `make prod-apply`). The new terraform `data.aws_ssm_parameter` lookups have no default and fail loudly if `/portfolio/<env>/api-execution-arn` or `/portfolio/<env>/origin-verify-secret` is absent.

---

## File Structure

**Backend handler (one responsibility: validate + process a contact submission):**
- Modify: `backend/src/ingest/handler.ts` — add `ORIGIN_VERIFY_SECRET` to `IngestEnv`, a timing-safe `x-origin-verify` check, API Gateway v2 event typing + base64 body decode.
- Modify: `backend/test/ingest/handler.test.ts` — header in the fixture, env value, new 403 + base64 cases, fix raw-event tests.

**Backend terraform (each file one concern; follow existing per-file split):**
- Modify: `terraform/lambda.tf` — delete `aws_lambda_function_url.ingest`; add SSM data source + `ORIGIN_VERIFY_SECRET` env var.
- Modify: `terraform/permissions.tf` — replace the CloudFront-URL invoke grant with an API Gateway invoke grant scoped to the execution ARN.
- Modify: `terraform/ssm.tf` — delete the `ingest-function-url` param (retired).
- Modify: `terraform/outputs.tf` — delete the two Function-URL outputs.

**No frontend code changes** — verified already wired (Task 7 is a read-only confirmation).

---

### Task 1: Enforce `x-origin-verify` in the handler (fail closed)

**Files:**
- Modify: `backend/src/ingest/handler.ts`
- Test: `backend/test/ingest/handler.test.ts`

- [ ] **Step 1: Update the test fixture + env so every existing test carries the header**

In `backend/test/ingest/handler.test.ts`, add a shared secret + header. Replace the `ENV` const and `event()` helper (lines ~11–23) with:

```ts
const ORIGIN_SECRET = 'origin-secret-value';

const ENV = {
  FROM_EMAIL: 'noreply@trystan-tbm.dev',
  CONTACT_EMAIL_SECRET_ARN: 'arn:aws:secretsmanager:us-east-1:111:secret:contact-email',
  ORIGIN_VERIFY_SECRET: ORIGIN_SECRET,
};

// Headers a real CloudFront→API-Gateway request carries, including the origin-verify secret.
const baseHeaders = (extra: Record<string, string> = {}) => ({
  'x-forwarded-for': '1.2.3.4',
  'user-agent': 'UA',
  'x-origin-verify': ORIGIN_SECRET,
  ...extra,
});

// A valid submission: honeypot empty, old-enough timestamp. No turnstileToken (WAF handles CAPTCHA).
function event(overrides: Record<string, unknown> = {}) {
  const body = JSON.stringify({
    name: 'Alice', email: 'a@b.co', message: 'hello there',
    website: '', formTimestamp: 0, ...overrides,
  });
  return { headers: baseHeaders(), body } as never;
}
```

Then fix the two raw-event tests so they still exercise *their* path (not the new 403 gate). Replace the body of the `'returns 400 on malformed JSON'` test (line ~68) and the `'returns 400 when body is JSON null'` test (line ~74) with:

```ts
  it('returns 400 on malformed JSON', async () => {
    const bad = { headers: baseHeaders(), body: '{not json' } as never;
    const res = await handleIngest(bad, deps());
    expect(res.statusCode).toBe(400);
  });

  it('returns 400 when body is JSON null (non-object body)', async () => {
    const bad = { headers: baseHeaders(), body: 'null' } as never;
    const res = await handleIngest(bad, deps());
    expect(res.statusCode).toBe(400);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(0);
  });
```

- [ ] **Step 2: Add the failing 403 tests**

Add these two tests inside the `describe('handleIngest', …)` block in `backend/test/ingest/handler.test.ts`:

```ts
  it('rejects a request missing x-origin-verify with 403 and sends nothing', async () => {
    const noHeader = { headers: { 'x-forwarded-for': '1.2.3.4' }, body: event().body } as never;
    const res = await handleIngest(noHeader, deps());
    expect(res.statusCode).toBe(403);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(0);
  });

  it('rejects a wrong x-origin-verify with 403 and sends nothing', async () => {
    const wrong = { headers: baseHeaders({ 'x-origin-verify': 'nope' }), body: event().body } as never;
    const res = await handleIngest(wrong, deps());
    expect(res.statusCode).toBe(403);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(0);
  });
```

- [ ] **Step 3: Run the tests to verify the new ones fail**

Run: `cd backend && npm test -- test/ingest/handler.test.ts`
Expected: FAIL — the two 403 tests get `200`/`400` (no origin check exists yet); other tests still pass.

- [ ] **Step 4: Add `ORIGIN_VERIFY_SECRET` to the env type and a timing-safe check**

In `backend/src/ingest/handler.ts`, add the crypto import at the top (after the existing imports, line ~11):

```ts
import { timingSafeEqual } from 'node:crypto';
```

Extend `IngestEnv` (line ~13) to include the secret:

```ts
export interface IngestEnv {
  FROM_EMAIL: string;                 // verified SES identity (noreply@<domain>)
  CONTACT_EMAIL_SECRET_ARN: string;   // Secrets Manager ARN holding the recipient address
  ORIGIN_VERIFY_SECRET: string;       // shared secret CloudFront injects as x-origin-verify
}
```

Add this helper just above `handleIngest` (after the `json()` helper, line ~36):

```ts
// Constant-time comparison of the x-origin-verify header against the expected secret.
// Fails closed: a missing expected secret, a missing header, or any length mismatch -> false.
function originVerified(headerValue: string | undefined, expected: string): boolean {
  if (!expected || typeof headerValue !== 'string') return false;
  const a = Buffer.from(headerValue);
  const b = Buffer.from(expected);
  return a.length === b.length && timingSafeEqual(a, b);
}
```

As the **first** statement inside `handleIngest`, before `const ts = now();` (line ~39), add the gate:

```ts
  // Reject anything that didn't come through CloudFront (and thus skipped WAF).
  if (!originVerified(event.headers['x-origin-verify'], deps.env.ORIGIN_VERIFY_SECRET)) {
    return json(403, { error: 'Forbidden' });
  }
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd backend && npm test -- test/ingest/handler.test.ts`
Expected: PASS — all tests green, including the two 403 cases.

- [ ] **Step 6: Commit**

```bash
git add backend/src/ingest/handler.ts backend/test/ingest/handler.test.ts
git commit -m "feat(ingest): enforce x-origin-verify shared secret (reject WAF-bypassing requests)"
```

---

### Task 2: Adapt the handler to the API Gateway v2 event (typing + base64 body)

**Files:**
- Modify: `backend/src/ingest/handler.ts`
- Test: `backend/test/ingest/handler.test.ts`

- [ ] **Step 1: Add a failing base64-body test**

API Gateway HTTP APIs may deliver the body base64-encoded (`isBase64Encoded: true`). Add this test to `backend/test/ingest/handler.test.ts`:

```ts
  it('decodes a base64-encoded body (API Gateway v2) and returns 200', async () => {
    const raw = JSON.stringify({ name: 'Alice', email: 'a@b.co', message: 'hello there', website: '', formTimestamp: 0 });
    const b64 = { headers: baseHeaders(), body: Buffer.from(raw, 'utf8').toString('base64'), isBase64Encoded: true } as never;
    const res = await handleIngest(b64, deps());
    expect(res.statusCode).toBe(200);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(1);
  });
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd backend && npm test -- test/ingest/handler.test.ts`
Expected: FAIL — the handler feeds the base64 string straight to `JSON.parse`, returning `400` instead of `200`.

- [ ] **Step 3: Rename the event types to API Gateway v2 and decode the body**

In `backend/src/ingest/handler.ts`, replace the `FunctionUrlEvent` / `FunctionUrlResult` interfaces (lines ~24–32) with API Gateway v2 shapes (only the fields we use):

```ts
// AWS API Gateway HTTP API (payload format 2.0). Response uses the simple
// { statusCode, headers, body } form, which v2 proxy integrations accept as-is.
interface ApiGatewayV2Event {
  headers: Record<string, string | undefined>;
  body?: string;
  isBase64Encoded?: boolean;
}
interface ApiGatewayV2Result {
  statusCode: number;
  headers: Record<string, string>;
  body: string;
}
```

Update the two references to the old result type: `json()`'s return type (line ~34) and `handleIngest`'s signature/return (line ~38) become `ApiGatewayV2Result`, and `handleIngest`'s `event` param becomes `ApiGatewayV2Event`:

```ts
function json(statusCode: number, payload: unknown): ApiGatewayV2Result {
  return { statusCode, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) };
}

export async function handleIngest(event: ApiGatewayV2Event, deps: IngestDeps): Promise<ApiGatewayV2Result> {
```

Replace the JSON-parse block (lines ~44–49) so it decodes base64 first:

```ts
  let parsed: unknown;
  try {
    const rawBody = event.isBase64Encoded && event.body
      ? Buffer.from(event.body, 'base64').toString('utf8')
      : (event.body ?? '');
    parsed = JSON.parse(rawBody);
  } catch {
    return json(400, { error: 'Invalid request' });
  }
```

Update the Lambda entrypoint at the bottom of the file: the `env` literal (line ~86) gains the secret, and the exported `handler` param type (line ~92) becomes `ApiGatewayV2Event`:

```ts
const env: IngestEnv = {
  FROM_EMAIL: process.env.FROM_EMAIL!,
  CONTACT_EMAIL_SECRET_ARN: process.env.CONTACT_EMAIL_SECRET_ARN!,
  ORIGIN_VERIFY_SECRET: process.env.ORIGIN_VERIFY_SECRET!,
};
const clients = { ses: new SES({}), secrets: new SM({}) };

export const handler = (event: ApiGatewayV2Event): Promise<ApiGatewayV2Result> =>
  handleIngest(event, { env, clients, now: () => Date.now() });
```

- [ ] **Step 4: Run the full backend suite + typecheck**

Run: `cd backend && npm test && npm run typecheck`
Expected: PASS — all tests green, `tsc --noEmit` clean.

- [ ] **Step 5: Commit**

```bash
git add backend/src/ingest/handler.ts backend/test/ingest/handler.test.ts
git commit -m "refactor(ingest): adapt handler to API Gateway v2 event (base64 body, v2 types)"
```

---

### Task 3: Inject the origin-verify secret as a Lambda env var; delete the Function URL

**Files:**
- Modify: `terraform/lambda.tf`

- [ ] **Step 1: Add the SSM data source and `ORIGIN_VERIFY_SECRET` env var**

In `terraform/lambda.tf`, add this data source above `resource "aws_lambda_function" "ingest"` (after the `archive_file`, line ~6):

```hcl
# Origin-verify shared secret, published by the infra repo in phase 2 (SecureString).
# No default — a missing param fails the apply loudly (infra phase 2 must run first).
data "aws_ssm_parameter" "origin_verify" {
  name            = "/portfolio/${var.environment}/origin-verify-secret"
  with_decryption = true
}
```

Add the env var to the function's `environment.variables` block (line ~19), alongside the existing two:

```hcl
  environment {
    variables = {
      FROM_EMAIL               = local.from_email
      CONTACT_EMAIL_SECRET_ARN = aws_secretsmanager_secret.contact_email.arn
      ORIGIN_VERIFY_SECRET     = data.aws_ssm_parameter.origin_verify.value
    }
  }
```

- [ ] **Step 2: Delete the Function URL resource**

Remove the entire `resource "aws_lambda_function_url" "ingest"` block (lines ~26–32) **and** its preceding comment block (lines ~26–28). The Function URL is no longer an origin — API Gateway invokes the function directly.

- [ ] **Step 3: Validate the terraform**

Run: `cd terraform && terraform fmt && terraform validate`
Expected: `Success! The configuration is valid.` (validate does not need credentials; it will only flag references to the now-removed `aws_lambda_function_url.ingest` — Task 4 and Task 5 remove those.)

> Note: `validate` may report errors until Tasks 4 & 5 also land (they reference `aws_lambda_function_url.ingest`). If so, complete Tasks 4–5 before re-running validate. Commit at the end of Task 5.

---

### Task 4: Grant API Gateway invoke permission (scoped to the execution ARN)

**Files:**
- Modify: `terraform/permissions.tf`

- [ ] **Step 1: Replace the whole file contents**

The old file grants CloudFront permission to invoke the Function URL. Replace the entire contents of `terraform/permissions.tf` with the API Gateway grant:

```hcl
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
```

This removes `data.aws_ssm_parameter.cf_arn` and `aws_lambda_permission.cf_invoke_url` (both obsolete once the Function URL is gone).

---

### Task 5: Retire the `ingest-function-url` SSM param and outputs

**Files:**
- Modify: `terraform/ssm.tf`
- Modify: `terraform/outputs.tf`

- [ ] **Step 1: Delete the Function-URL SSM param**

In `terraform/ssm.tf`, remove the `resource "aws_ssm_parameter" "ingest_function_url"` block (lines ~1–8, including its leading comment). Keep `aws_ssm_parameter.ingest_function_arn` — that is the live contract the infra repo reads.

- [ ] **Step 2: Delete the Function-URL outputs**

In `terraform/outputs.tf`, remove the `ingest_function_url` output (lines ~2–5) and the `ingest_function_url_ssm_param` output (lines ~12–15). Keep `ingest_function_name` and `ingest_function_arn_ssm_param`.

- [ ] **Step 3: Format and validate the full terraform (Tasks 3–5 together)**

Run: `cd terraform && terraform fmt && terraform validate`
Expected: `Success! The configuration is valid.` — no dangling references to `aws_lambda_function_url.ingest`.

- [ ] **Step 4: Commit Tasks 3–5 together (terraform only compiles as a set)**

```bash
git add terraform/lambda.tf terraform/permissions.tf terraform/ssm.tf terraform/outputs.tf
git commit -m "feat(terraform): API Gateway invoke grant + origin-verify env; retire Function URL"
```

---

### Task 6: Verify the deploy role can read the new SSM params (pre-flight)

**Files:** none (read-only check)

- [ ] **Step 1: Confirm both new params are readable in dev with the deploy path's perms**

The portfolio `deploy.yml` already reads `/portfolio/<env>/waf-api-key` (SecureString) after OIDC, so `ssm:GetParameter` + `kms:Decrypt` on `/portfolio/<env>/*` are granted. Confirm the two new params resolve:

Run:
```bash
aws ssm get-parameter --name /portfolio/dev/api-execution-arn \
  --profile shadowspire-dev --region us-east-1 --query Parameter.Value --output text
aws ssm get-parameter --name /portfolio/dev/origin-verify-secret --with-decryption \
  --profile shadowspire-dev --region us-east-1 --query Parameter.Type --output text
```
Expected: an `arn:aws:execute-api:us-east-1:176355979099:<api-id>` value, and `SecureString`. If either errors with `ParameterNotFound`, infra phase 2 has not been applied for that env — stop and apply it first.

---

### Task 7: Confirm the frontend is already wired (no change expected)

**Files:** none (read-only check)

- [ ] **Step 1: Verify the contact form posts same-origin and the build embeds the WAF SDK**

Run:
```bash
grep -n "api/contact\|VITE_WAF_INTEGRATION_URL\|VITE_WAF_API_KEY" src/components/Contact.tsx
grep -n "VITE_WAF_INTEGRATION_URL\|waf-integration-url\|waf-api-key" .github/workflows/deploy.yml
```
Expected: `Contact.tsx` posts to `/api/contact` and reads `import.meta.env.VITE_WAF_*`; `deploy.yml` reads both WAF params from SSM and passes them as build env. No code change required — the frontend already targets `/api/contact`, which CloudFront routes to the API Gateway.

---

### Task 8: Deploy to dev and verify end-to-end

**Files:** none (deploy + manual verification)

- [ ] **Step 1: Open a PR to deploy to dev**

`deploy.yml` runs on `pull_request` → DEV (account 176355979099): builds the Lambda bundle, `tofu apply` (which updates the Lambda env, swaps the invoke permission, destroys the Function URL + its param), reads WAF params, builds the site, syncs to S3, invalidates CloudFront.

```bash
git push -u origin HEAD
gh pr create --title "Contact API phase 3: API Gateway cutover + x-origin-verify" \
  --body "Adapts the ingest Lambda to the API Gateway v2 event, enforces x-origin-verify, swaps the Lambda invoke grant to API Gateway, and retires the Function URL. Depends on infra phase 2 (dev applied 2026-06-04)."
```

Watch the DEV deploy job succeed (esp. the `tofu apply` step — confirm the Function URL is destroyed and `aws_lambda_permission.apigw_invoke` is created):

Run: `gh run watch`
Expected: green; apply summary shows `aws_lambda_function_url.ingest` destroyed and `aws_lambda_permission.apigw_invoke` created.

- [ ] **Step 2: Verify the happy path in a real browser (CAPTCHA can't be solved headless)**

Open `https://dev.trystan-tbm.dev`, go to the contact form, fill it in, solve the WAF CAPTCHA, submit.
Expected: HTTP 200, the form shows success, and the contact email arrives via SES.

- [ ] **Step 3: Verify the WAF-bypass guard rejects a direct API-Gateway call**

A request straight to the API Gateway (skipping CloudFront, so no `x-origin-verify`) must be rejected by the handler:

Run:
```bash
API_ID=$(aws ssm get-parameter --name /portfolio/dev/api-execution-arn \
  --profile shadowspire-dev --region us-east-1 --query Parameter.Value --output text | awk -F: '{print $6}')
curl -s -o /dev/null -w "%{http_code}\n" -X POST \
  "https://${API_ID}.execute-api.us-east-1.amazonaws.com/api/contact" \
  -H 'content-type: application/json' \
  -d '{"name":"x","email":"a@b.co","message":"hello there","website":"","formTimestamp":0}'
```
Expected: `403` (handler rejects the missing `x-origin-verify`). A `200` here is a security failure — the origin-verify gate is not effective.

- [ ] **Step 4: Merge to ship dev; prod follows after infra `make prod-apply`**

Merging to `main` triggers the PROD deploy job — **do not merge until `/portfolio/prod/api-execution-arn` and `/portfolio/prod/origin-verify-secret` exist** (i.e. infra `make prod-apply` has run for prod). Otherwise the prod `tofu apply` fails on the SSM data lookup (by design).

```bash
gh pr merge --squash
```

---

## Notes / out of scope
- **Admin auth (Entra JWT):** the authorizer is scaffolded but unattached; admin routes are a separate effort.
- **Doc refresh:** `terraform/README.md`, `DEPLOYMENT_CHECKLIST.md`, and `outputs.tf` comments still mention the Function URL/OAC; update opportunistically (not required for the cutover to work).
- **Secret handling choice:** `ORIGIN_VERIFY_SECRET` is injected as a Lambda env var read from the SecureString SSM param at apply time (mirrors `FROM_EMAIL`). It lands in Lambda env + TF state; acceptable for a CloudFront↔origin shared secret. Rotating it = infra re-applies `random_password.origin_verify` (stable unless tainted) → portfolio re-deploy picks up the new value.
