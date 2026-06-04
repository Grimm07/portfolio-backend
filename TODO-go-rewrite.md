# TODO: rewrite the contact Lambda from Node/TypeScript to Go

**Status:** not started. The Lambda is currently TypeScript on `nodejs20.x`. This document specs the
port so it can be picked up as a standalone piece of work. **Behaviour must stay identical** — same
request/response contract, same validation rules, same SES email, same security properties.

## Why

Consolidate the backend on a single compiled language: smaller cold starts, a single static
`bootstrap` binary instead of a bundled JS + AWS SDK v3 tree, and no npm supply-chain surface at
runtime.

## Target runtime

| | Now (Node) | After (Go) |
|---|---|---|
| Runtime | `nodejs20.x` | `provided.al2023` (custom runtime) |
| Handler | `index.handler` | `bootstrap` |
| Artifact | `dist/ingest/index.mjs` (esbuild bundle, zipped) | `bootstrap` (static binary, zipped) |
| Arch | x86_64 (default) | `arm64` (cheaper; set `architectures = ["arm64"]`) |
| SDK | `@aws-sdk/client-ses`, `@aws-sdk/client-secrets-manager` | `aws-lambda-go` + `aws-sdk-go-v2` (ses, secretsmanager) |

## Behaviour to preserve (port 1:1)

From `backend/src/`:

1. **Origin verification** (`handler.ts` `originVerified`): constant-time compare of the
   `x-origin-verify` header against `ORIGIN_VERIFY_SECRET`. Fail closed on missing secret, missing
   header, or length mismatch. Use `crypto/subtle.ConstantTimeCompare` (guard length first, since it
   leaks on differing lengths otherwise). Reject → `403 {"error":"Forbidden"}`.
2. **Event shape**: API Gateway HTTP API payload format 2.0. Use
   `events.APIGatewayV2HTTPRequest` / `events.APIGatewayV2HTTPResponse` from `aws-lambda-go/events`.
   Honour `IsBase64Encoded` on the body. **Header keys arrive lowercased** in v2 — preserve the
   current lowercase lookups (`x-origin-verify`, `cloudfront-viewer-address`, `x-forwarded-for`).
3. **Body parsing**: JSON → struct. Non-object / invalid JSON → `400 {"error":"Invalid request"}`.
4. **Honeypot** (`validation.ts` `isHoneypotTripped`): non-empty trimmed `website` → silently
   return `200 {"ok":true}` and send nothing.
5. **Time-trap** (`isTooFast`, `MIN_FORM_TIME_MS = 3000`): missing/non-numeric `formTimestamp`, or
   `now - formTimestamp < 3000ms` → `400 {"error":"Submission too fast"}`. `formTimestamp` is ms
   epoch; `now()` is injectable for tests.
6. **Field validation** (`validation.ts`):
   - email: `len <= 254` and matches `^[^\s@]+@[^\s@]+\.[^\s@]+$` → else `400 {"error":"Invalid email"}`.
   - name: non-empty after trim → else `400 {"error":"Invalid name"}`.
   - message: string, `0 < len <= 5000` → else `400 {"error":"Invalid message"}`.
   - `sanitizeName`: strip `[\r\n\t\x00-\x1f\x7f]`, cap at `MAX_NAME = 200`.
   - lowercase the email before use.
7. **Client IP** (`ip.ts` `extractClientIp`): prefer `cloudfront-viewer-address` with the port
   stripped (cut after the **last** colon — correct for unbracketed IPv6 too); else first
   `x-forwarded-for` entry; else `"unknown"`.
8. **Secret fetch** (`secrets.ts`): read recipient from Secrets Manager by ARN
   (`CONTACT_EMAIL_SECRET_ARN`), with a process-lifetime cache. Empty `SecretString` → error.
9. **SES send** (`email.ts`): `SendEmail` with `Source = FROM_EMAIL`, `ToAddresses = [recipient]`,
   `ReplyToAddresses = [submitter]`, subject `Portfolio: new contact from <name>`, and the exact
   text body (From / When / IP / blank / message). Keep `createdAt` as RFC3339 / ISO-8601.
10. **Error logging** (`errorLabel`): **never log the error object/message** (CodeQL
    `js/clear-text-logging` — and the Go equivalent). Map the AWS error to a fixed literal label
    (`MessageRejected`, `AccessDeniedException`, `ResourceNotFoundException`, `ThrottlingException`,
    `other`) via `errors.As` on the smithy/api error types, and log only that. Failure → `500
    {"error":"Failed to send message"}`.
11. **Success** → `200 {"ok":true}`. All responses set `Content-Type: application/json`.

Keep the same dependency-injection seam the TS code uses (`IngestDeps`: env, clients, `now()`) so
the handler stays unit-testable without real AWS clients — in Go, an interface per client + a clock
func.

## Suggested Go layout

```
backend/
  go.mod
  cmd/ingest/main.go          # lambda.Start(makeHandler(realDeps))
  internal/ingest/handler.go  # HandleIngest(ctx, event, deps) — the port of handler.ts
  internal/ingest/email.go
  internal/ingest/ip.go
  internal/validation/validation.go
  internal/secrets/secrets.go
  internal/ingest/*_test.go   # port every case in backend/test/**
```
(Decide whether to keep the Node tree side-by-side during the port or replace it. The current
`backend/{src,test}` and `package.json` can be deleted once Go reaches parity.)

## Build & package

```bash
cd backend
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap ./cmd/ingest
zip ingest.zip bootstrap
```

## Terraform changes (`terraform/lambda.tf`)

- `runtime = "provided.al2023"`, `handler = "bootstrap"`, add `architectures = ["arm64"]`.
- `data "archive_file" "ingest"` → zip the `bootstrap` binary instead of `index.mjs`
  (`source_file = "${path.module}/../backend/bootstrap"`), or switch to a `data.archive_file` over
  the built binary. Keep `source_code_hash`.
- Env vars (`FROM_EMAIL`, `CONTACT_EMAIL_SECRET_ARN`, `ORIGIN_VERIFY_SECRET`) are unchanged.
- IAM, SES, SSM, permissions: **no change** — same role and grants.

## CI / Deploy changes

- `.github/workflows/ci.yml`: replace the Node `test-backend` job with `actions/setup-go` +
  `go vet ./...` + `go test ./...`. Keep the `terraform` job as-is.
- `.github/workflows/deploy.yml`: replace `npm ci && npm run build` with the `go build` +
  zip step above; everything else (OIDC, `tofu apply`) is unchanged.

## Acceptance

- Every case in `backend/test/**/*.test.ts` has a passing Go equivalent.
- `tofu plan` shows only the runtime/handler/artifact diff on the function (no IAM/SES/SSM churn).
- A dev deploy + a real contact submission produces the same SES email as today.
