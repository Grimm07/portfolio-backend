# Design: Contact backend → AWS Lambda ingestion pipeline

- **Date:** 2026-06-02
- **Status:** Approved design (pre-implementation)
- **Supersedes decision:** "keep the Worker on Cloudflare for now" — the contact backend moves off Cloudflare Workers to AWS.
- **Related:** Cloudflare→hybrid CDN migration (Cloudflare DNS + AWS CloudFront/S3). This backend work shares that migration's CloudFront distribution and AWS/Cloudflare OpenTofu providers.
- **Observability:** Full AWS-native — CloudWatch RUM (browser) + AWS X-Ray via ADOT (Lambdas) + CloudWatch. No third-party APM. Detailed tracing design deferred (see §8).

## 1. Goal & scope

Replace the Cloudflare Worker contact-form handler (`worker/src/index.ts`) with an AWS serverless **contact-ingestion pipeline** that:

1. Accepts contact submissions behind CloudFront (same-origin `/api/*`), preserving the existing frontend contract.
2. Runs the security layers (honeypot, time-trap, email validation, rate limiting) — with rate limiting now *actually* correct. **Human-verification (CAPTCHA) is enforced upstream by AWS WAF at the edge, not in the Lambda** (replaces Cloudflare Turnstile).
3. **Persists every submission**: message body in S3, a per-person contact record + S3 pointer in DynamoDB.
4. **Batches bursts** of submissions into a single notification email via SQS + SES (async; never blocks the user response).
5. Manages secrets via **AWS Secrets Manager**.

Out of scope (separate follow-ups): the static-hosting/CDN cutover itself, and the OpenTelemetry tracing rebase.

## 2. Architecture overview

Two decoupled paths. **Ingest** is synchronous (browser-facing, fast, durable). **Notify** is asynchronous (batched email).

```
                       ┌──────── CloudFront (trystan-tbm.dev, ACM) ────────┐
 Browser ─ GET / ─────►│ behavior *      → S3 origin (OAC)  → static SPA     │
 Browser ─ POST /api/*►│ behavior /api/* → Lambda origin (OAC, no cache)    │
                       └───────────────────────┬───────────────────────────┘
                                               ▼
                              ┌─ INGEST Lambda (Node, Function URL/IAM) ─┐
   WAF: CAPTCHA action (/api/* │ honeypot · time-trap · email validate    │
   POST) + rate rule (per-IP)  │ · DynamoDB rate-counter (3/hr)           │
        ▲ aws-waf-token        │ (no CAPTCHA here — WAF already enforced)  │
                              │ ① S3 PutObject  body                      │
                              │ ② Dynamo PutItem  record + s3 pointer     │
                              │ ③ SQS SendMessage  {id, email}            │
                              │ ④ 200 → browser                           │
                              └───────────────────────┬──────────────────┘
                                                      ▼
                                  SQS (BatchWindow 300s, BatchSize N) + DLQ
                                                      ▼
                              ┌─ NOTIFIER Lambda (batch handler) ─┐
                              │ load records → compose ONE digest │──► SES ──► your inbox
                              │ ReportBatchItemFailures → DLQ      │
                              └────────────────────────────────────┘

 Secrets Manager: CONTACT_EMAIL  (fetched by Notifier, cached per container)
 Frontend: AWS WAF CAPTCHA JS SDK renders the puzzle, obtains aws-waf-token (replaces Turnstile widget)
 SES auth: via Lambda IAM role (no stored credential)
```

## 3. Decisions (locked)

| Decision | Choice | Rationale |
|---|---|---|
| API fronting | **CloudFront `/api/*` → Lambda Function URL** (IAM auth + OAC, `AllViewerExceptHostHeader` origin request policy, caching disabled) | Same-origin → no CORS, no frontend change, one domain/cert. Function URL locked so it can't be hit directly bypassing CloudFront/WAF. |
| Email provider | **Amazon SES** | AWS-native, IAM auth (no stored credential), ~free at this volume. Both sender (domain) and recipient (your inbox) are verifiable → stays in **sandbox**, no production-access ticket. |
| Email delivery | **Async via SQS, batched** | Ingest returns 200 immediately; SES hiccups never fail a submission. |
| Batch window | **~300s (5 min) max**, `ReportBatchItemFailures` | Maximize burst collapsing into one email; 5-min notification latency acceptable. |
| Contact data model | **Per-person, with history** (single-table) | Matches "record of everyone who contacted us"; query a person's history or list everyone. |
| Message storage | **Body in S3, pointer in DynamoDB** | Keeps Dynamo items small; bodies can be long. |
| Rate limiting | **WAF rate rule (edge) + DynamoDB precise counter (3/hr/IP)** | Belt-and-suspenders; finally a correct, shared limit (in-memory Map never worked across isolates). |
| CAPTCHA | **AWS WAF CAPTCHA** on the `/api/*` POST rule, via the **WAF CAPTCHA JS SDK** in the form | Replaces Cloudflare Turnstile. Enforced at the edge; the Lambda never verifies a token. |
| Secrets | **AWS Secrets Manager** (+ cache), `CONTACT_EMAIL` only | Notifier fetches it; kept out of env/git per privacy posture. (Turnstile secret removed with CAPTCHA move.) |
| Client IP | **`X-Forwarded-For` / `CloudFront-Viewer-Address`**, **stored raw** | Replaces `CF-Connecting-IP`. Raw (un-hashed) IP retained for abuse investigation. |
| Observability | **Full AWS-native**: CloudWatch RUM (browser) + X-Ray/ADOT (Lambdas) + CloudWatch | No third-party APM; native end-to-end trace map. |
| IaC | **OpenTofu** (extends the Cloudflare+AWS config from the CDN migration) | One tool for Cloudflare DNS + AWS resources; HCL-compatible, same provider ecosystem. |
| Retention | **Keep indefinitely**, encrypted at rest | User wants a durable record; TTL/lifecycle can be added later. |

## 4. Components

| Component | Responsibility | Key IAM (least privilege) |
|---|---|---|
| **Ingest Lambda** (`backend/ingest/`) | Validation (ported from `worker/src/index.ts`, minus CAPTCHA), persist S3 + Dynamo, rate-counter, enqueue SQS, return 200. IP from XFF/viewer-address. No Secrets Manager access. | `s3:PutObject` (messages/*), `dynamodb:PutItem/UpdateItem` (contacts, rate-limits), `sqs:SendMessage` |
| **Notifier Lambda** (`backend/notifier/`) | SQS batch consumer; aggregate the window's submissions into one SES digest. Partial-failure aware. Reads `CONTACT_EMAIL` from Secrets Manager. | `sqs:ReceiveMessage/DeleteMessage`, `s3:GetObject` (messages/*), `dynamodb:GetItem/Query`, `ses:SendEmail`, `secretsmanager:GetSecretValue` (CONTACT_EMAIL) |
| **S3 bucket** `…-contact-messages` | Private (Block-Public-Access + OAC), SSE-S3. Object per submission. | — |
| **DynamoDB** `contacts` | Per-person contact records + S3 pointers. On-demand, PITR on, SSE on. | — |
| **DynamoDB** `rate-limits` | IP→count, TTL-expiring (1h). Conditional counter. | — |
| **SQS** `contact-notifications` + **DLQ** | Decouple email from ingest; DLQ for poison messages. | — |
| **SES** | Domain identity (DKIM) + verified recipient; sends digest. | — |
| **WAF WebACL** (CloudFront scope) | Rate-based rule for volumetric protection. | — |
| **Secrets Manager** | `CONTACT_EMAIL` (Notifier). Value injected out-of-band (not committed). | — |
| **WAF CAPTCHA** | CAPTCHA action on the `/api/*` POST rule; frontend uses the WAF CAPTCHA JS SDK to obtain `aws-waf-token`. | — |

## 5. Data flow

### Ingest (synchronous, per submission)
1. CloudFront `/api/*` → Ingest Lambda (Function URL).
2. Extract client IP from `X-Forwarded-For` / `CloudFront-Viewer-Address`.
3. Validate: honeypot (`website` empty) → time-trap (≥3s) → email format. (CAPTCHA already enforced by WAF before this point — the request wouldn't reach the Lambda without a valid `aws-waf-token`.)
4. Rate limit: conditional update on `rate-limits` (`PK=IP#<raw-ip>`, TTL=now+1h); reject if >3 in window.
5. Generate `id` (UUID v4).
6. `PutObject` body → S3 `messages/{yyyy}/{mm}/{id}.json`.
7. `PutItem` → `contacts` (record + `s3Key`).
8. `SendMessage` → SQS `{id, email}`.
9. Return `200 {ok:true}` (or the existing error shapes on failure — **no frontend contract change**).

Persist-before-enqueue: if SQS send fails after S3+Dynamo writes, the record still exists and is reconcilable; email failure never affects the user's 200.

### Notify (asynchronous, per batch)
1. SQS delivers up to `BatchSize` records collected over ≤300s.
2. Notifier loads each `{id,email}` (+ Dynamo/S3 record as needed).
3. Compose **one** digest email ("N new contacts in the last 5 min" + each name / email / message).
4. `ses:SendEmail` with `From: noreply@<site-domain>`, `Reply-To:` the contact's email.
5. Per-item failures → `ReportBatchItemFailures` re-queues only failures; repeated failures → DLQ; CloudWatch alarm on DLQ depth.

## 6. Data model (per-person, with history)

### `contacts` table (single-table)
```
PK  = EMAIL#<lowercased email>
SK  = SUB#<ISO-8601 timestamp>#<id>     ← one item per submission
attrs: name, ip, userAgent, createdAt, s3Bucket, s3Key, status
GSI1: GSI1PK = "ALL", GSI1SK = createdAt   ← list everyone / recent contacts
```
- A person's full history: `Query PK = EMAIL#…`.
- All/recent contacts: `Query GSI1 where GSI1PK="ALL"` ordered by `createdAt`.
- Message body is **not** in Dynamo — only the `s3Key` pointer.

### S3 layout
```
messages/{yyyy}/{mm}/{id}.json  =  { name, email, message, createdAt, meta }
```
- SSE-S3, Block-Public-Access. This bucket is **never** served through CloudFront (it holds private submission data); the Notifier reads bodies via the AWS SDK only. (Distinct from the static-site bucket, which uses CloudFront + OAC.)

### `rate-limits` table
```
PK = IP#<raw-ip>
attrs: count, windowStart
TTL = expiresAt (now + 1h)   ← DynamoDB TTL auto-evicts
```

## 7. Security & privacy

- **Edge (WAF):** rate-based rule on the CloudFront WebACL throttles volumetric floods before Lambda.
- **Precise (DynamoDB):** conditional-update counter enforces 3/hour/IP — a correct, shared limit.
- **Function URL lockdown:** `AuthType=AWS_IAM` + CloudFront OAC (SigV4-signed origin requests) so the raw `*.lambda-url…on.aws` host can't be hit directly.
- **PII posture:** client IP stored **raw** (deliberate — retained for abuse investigation; documented trade against the project's privacy-first default). Compensating controls: S3/Dynamo encrypted at rest (default); Block-Public-Access; least-privilege IAM per Lambda; `CONTACT_EMAIL` in Secrets Manager, not env/git. (If retention of raw IPs later becomes a concern, add a DynamoDB TTL to age them out.)
- **CAPTCHA (edge):** AWS WAF CAPTCHA action on the `/api/*` POST rule rejects requests lacking a valid `aws-waf-token`; the frontend obtains the token via the WAF CAPTCHA JS SDK. Bots never reach the Lambda. Replaces Cloudflare Turnstile (and its in-Lambda verification + secret).
- **Secrets:** only `CONTACT_EMAIL` in Secrets Manager, fetched by the Notifier (cached per container). SES uses the Lambda **IAM role**, no stored credential. The Ingest Lambda holds no secrets.

## 8. Observability — full AWS-native (deferred detailed design)

No third-party APM (Honeycomb dropped). Everything lands in CloudWatch with a native end-to-end trace map:

- **Backend (Lambdas):** enable **X-Ray active tracing**; instrument with the **ADOT (AWS Distro for OpenTelemetry) Lambda layer** so the handler plus AWS SDK calls (S3 / DynamoDB / SQS / SES) appear as child segments. SQS context propagation links the Ingest segment to the Notifier segment, so a submission traces ingest → queue → digest send.
- **Browser:** **CloudWatch RUM** via the `aws-rum-web` client (page-load timing, JS errors, HTTP requests), with **X-Ray trace linking** enabled so a browser session stitches to the Lambda segments in the X-Ray trace map. RUM's web client authenticates via a **Cognito identity pool** (unauthenticated identities) — an added resource versus the dropped Worker-proxy approach, but fully AWS-native.
- **Logs/metrics:** Lambda logs to CloudWatch Logs; alarms on DLQ depth, Lambda errors, and SES bounce rate.

This removes the Worker OTLP-proxy, the Honeycomb secret, and the browser bundle's OTel SDK entirely (`aws-rum-web` replaces it). **Detailed design (Cognito pool, RUM app monitor config, ADOT layer wiring, X-Ray sampling) is deferred to its own spec** after this backend lands. The earlier Honeycomb/`otel-cf-workers` design is obsolete.

## 9. IaC & deployment

- **OpenTofu**, extending the Cloudflare (DNS/DKIM) + AWS (with `us-east-1` alias for the CloudFront ACM cert) providers introduced by the CDN migration. One tool, one `tofu apply`. HCL + provider ecosystem are Terraform-compatible, so resource definitions are unchanged from standard Terraform.
- **Build:** Lambdas bundled with esbuild (same toolchain as the Worker) → zip → `aws_lambda_function`.
- **Secrets:** `aws_secretsmanager_secret` + `aws_secretsmanager_secret_version`; values supplied out-of-band (consistent with the existing gitignored-tfvars / env-secret pattern).
- **CI** (`.github/workflows/ci.yml`): add an AWS **OIDC** role (`id-token: write`); deploy = build Lambdas + `tofu apply` + frontend `aws s3 sync` + CloudFront invalidation. The old `VITE_TURNSTILE_SITE_KEY` build var is removed; the frontend instead loads the **WAF CAPTCHA JS SDK** (per-WebACL integration URL) — a frontend change tracked separately from this backend work.
- **Frontend change (separate):** replace `@marsidev/react-turnstile` + `VITE_TURNSTILE_SITE_KEY` in `src/components/Contact.tsx` with the AWS WAF CAPTCHA JS SDK (`AwsWafCaptcha.renderCaptcha` / `AwsWafIntegration.getToken`). The `aws-waf-token` it sets is validated by WAF on the `/api/*` POST; no token field is sent in the JSON body.

## 10. Error handling & resilience

- Ingest persists to S3+Dynamo **before** enqueuing; SQS-send failure leaves a reconcilable record.
- Notifier: `ReportBatchItemFailures` (partial-batch retry) + **DLQ** for poison messages; CloudWatch alarm on DLQ depth.
- SES throttling/bounce: retried via SQS visibility timeout; bounces/complaints optionally routed to an SNS topic (future).

## 11. Testing

- **Unit (vitest):** ported validation logic (reuse existing tests); rate-counter conditional logic (allow/limit/expiry); digest composition (N records → one email); IP extraction from XFF; Secrets Manager fetch+cache wrapper.
- **Integration (local):** LocalStack or AWS SDK mocks for S3/Dynamo/SQS/SES/Secrets Manager; assert ingest writes all three + returns 200; notifier composes one email per batch; partial-failure path re-queues correctly.
- **Contract:** existing `Contact.test.tsx` stays green (frontend unchanged — same `/api/contact`, same response shapes).

## 12. Cutover sequencing (rollback at each step)

1. Stand up S3/Dynamo/SQS/SES/Lambdas/WAF/Secrets in AWS (not wired to DNS). Verify via Function URL / CloudFront test domain. **Rollback:** destroy new resources; nothing user-facing changed.
2. Verify SES DKIM + recipient; send a test digest. **Rollback:** none needed.
3. Add `/api/*` behavior to CloudFront → Lambda. Test `https://<cf-domain>/api/contact` end-to-end (submission lands in S3+Dynamo, digest arrives). **Rollback:** remove the behavior.
4. Cut DNS to CloudFront (CDN migration step). Worker still exists as rollback.
5. Soak; confirm submissions + digests + rate limiting. Then decommission the Worker + MailChannels. **Rollback window closes here.**

## 13. Open items / assumptions

- IaC = **OpenTofu** (one tool with DNS/CDN work).
- Client IP = **stored raw** (not hashed), retained for abuse investigation.
- Observability = **full AWS-native** (CloudWatch RUM + X-Ray/ADOT); detailed design deferred to its own spec.
- Retention = keep indefinitely, encrypted. *(TTL/lifecycle addable later — also the escape hatch for raw-IP retention.)*
- SQS `BatchSize` exact value (e.g., 10) tuned during implementation.
