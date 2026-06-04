# Lambda Contact Pipeline — Plan 3: AWS-native Observability (X-Ray + CloudWatch RUM)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add AWS-native end-to-end observability to the contact pipeline — **AWS X-Ray active tracing** on both Lambdas and **CloudWatch RUM** in the browser — so a single contact submission produces one connected trace **browser → CloudFront → ingest Lambda → S3/DynamoDB/SQS → notifier Lambda → SES**, and browser sessions (page-load timing, JS errors, HTTP timing) land in CloudWatch RUM. This replaces the deferred Honeycomb/OTel idea from the design spec (§8) with the AWS-native stack the spec's decision table actually locked (CloudWatch RUM + X-Ray + CloudWatch).

**Architecture:** Extends the OpenTofu config from Plan 2a (AWS provider in **us-east-1**, GitLab HTTP state, `local.name_prefix`) and Plan 2b (env strategy: `var.environment`, `local.env`, `local.name_prefix = "portfolio-contact-${local.env}"`, `local.site_domain`; plus the CloudFront distribution + WAF). X-Ray is a two-line retrofit on the existing `aws_lambda_function` resources (Plan 2a `lambda.tf`) plus a managed IAM policy on each role (Plan 2a `iam.tf`). RUM is new: a Cognito **unauthenticated (guest)** identity pool, a guest IAM role allowing `rum:PutRumEvents`, an `aws_rum_app_monitor` for `local.site_domain` with `enable_xray = true` (so browser-emitted X-Ray trace headers stitch the RUM session to the Lambda segments), and a small build-flag-gated JS snippet injected into the built SPA.

**Tech Stack:** OpenTofu (`tofu`), AWS provider `~> 5.70`, Node 20 Lambda runtime (ESM `.mjs`), `aws-rum-web` browser client (loaded via the AWS-hosted CDN snippet — no new npm dependency required), Vite (`VITE_RUM_*` build vars).

**Tracing approach (recommended — simpler path):** Enable **X-Ray active tracing** on both Lambdas and rely on the **AWS SDK v3's built-in X-Ray instrumentation**. On `nodejs20.x`, when active tracing is on, the Lambda service emits the function invocation segment, and the AWS SDK v3 clients (`@aws-sdk/client-s3`, `-dynamodb`, `-sqs`, `-ses`, `-secrets-manager`) automatically emit X-Ray **subsegments** for their calls *provided the X-Ray SDK is present and the clients are captured*. To get subsegments **without app-code changes**, the **ADOT (AWS Distro for OpenTelemetry) Lambda layer** + `AWS_LAMBDA_EXEC_WRAPPER = /opt/otel-handler` is the zero-instrumentation option. **This plan recommends the no-layer path** (active tracing only) as the default — it already yields the connected `browser → ingest → SQS → notifier → SES` trace map via service-to-service propagation, with the Lambda + downstream-AWS segments visible — and documents the **ADOT layer as an OPTIONAL enhancement** (Task 1, Step 5) for richer per-SDK-call subsegments without touching `backend/` code. SQS trace-context propagation (ingest → notifier) works automatically because X-Ray injects the trace header into the SQS message system attributes when both functions are traced. See "Tracing depth trade-off" note in Task 1.

**Decisions carried in:**
- Observability is **AWS-native only** — no Honeycomb, no OTLP proxy, no browser OTel SDK (per design spec §8, the Honeycomb design is obsolete).
- RUM monitor + Cognito pool are **per-env** (`${local.name_prefix}` already includes `${local.env}` after Plan 2b Task 1). X-Ray active tracing is free-tier-friendly and **fine to leave on in dev**.
- This plan **depends on Plan 2b** (it consumes `local.site_domain` for the RUM domain and, optionally, the CloudFront/WAF outputs) but is **independent of Plan 2c** (the DNS cutover) — RUM tolerates being configured for the prod domain before DNS flips; sessions simply won't arrive until the site serves from CloudFront.
- Dashboards/alarms (Task 3) are **OPTIONAL** and clearly marked.

**Prereq:** Plan 2b is authored/applied (or at least Plan 2b **Task 1**, the env scaffolding, is merged so `local.name_prefix`/`local.site_domain` exist). The two `aws_lambda_function` resources from Plan 2a (`aws_lambda_function.ingest`, `aws_lambda_function.notifier`) and their roles (`aws_iam_role.ingest`, `aws_iam_role.notifier`) must already exist.

---

## File Structure

New/modified files in `terraform/`, plus one frontend change:

```
terraform/
  lambda.tf        # (modify) add tracing_config { mode = "Active" } to ingest + notifier
  iam.tf           # (modify) attach AWSXRayDaemonWriteAccess to both Lambda roles
  xray.tf          # (new, small) X-Ray group + sampling rule (optional grouping) + notes
  rum.tf           # (new) Cognito guest pool + roles attachment + guest role (rum:PutRumEvents)
                   #       + aws_rum_app_monitor (enable_xray=true) for local.site_domain
  outputs.tf       # (modify) add rum_app_monitor_id, rum_identity_pool_id, rum_snippet_config
  dashboards.tf    # (new, OPTIONAL) CloudWatch dashboard + alarms (Lambda/SQS-DLQ/SES)
  variables.tf     # (modify, OPTIONAL) add alarm_email for SNS alarm notifications

index.html         # (modify) add RUM snippet placeholder injected at build time, OR
src/rum.ts         # (new) tiny module that conditionally boots aws-rum-web from VITE_RUM_* vars
src/main.tsx       # (modify) import './rum' (side-effect) before rendering
.env.example       # (modify) document VITE_RUM_* build vars
```

> **Naming:** all new AWS resources extend Plan 2b's prefix, e.g. `${local.name_prefix}-rum` (app monitor), `${local.name_prefix}-rum-identity` (Cognito pool), `${local.name_prefix}-rum-guest` (guest role). Since `local.name_prefix` already carries `${local.env}`, dev and prod get distinct resources automatically.

> **Provider version note:** `aws_rum_app_monitor`, `aws_cognito_identity_pool`, and `aws_cognito_identity_pool_roles_attachment` are all available in AWS provider `~> 5.70` (the version pinned in Plan 2a). No provider bump required.

---

## Task 1: X-Ray active tracing on both Lambdas + IAM write permissions

**Files:**
- Modify: `terraform/lambda.tf`
- Modify: `terraform/iam.tf`
- Create: `terraform/xray.tf`

- [ ] **Step 1: Add `tracing_config` to the ingest Lambda (`terraform/lambda.tf`)**

In the existing `resource "aws_lambda_function" "ingest"` block (Plan 2a), add a `tracing_config` block after `memory_size`:

```hcl
resource "aws_lambda_function" "ingest" {
  function_name    = "${local.name_prefix}-ingest"
  role             = aws_iam_role.ingest.arn
  runtime          = "nodejs20.x"
  handler          = "index.handler"
  filename         = data.archive_file.ingest.output_path
  source_code_hash = data.archive_file.ingest.output_base64sha256
  timeout          = 10
  memory_size      = 256

  # Plan 3: X-Ray active tracing. The Lambda service emits the invocation segment;
  # AWS SDK v3 calls (S3/DynamoDB/SQS) and the SQS trace header (→ notifier) propagate
  # automatically. No app-code change required for the connected trace map.
  tracing_config {
    mode = "Active"
  }

  environment {
    variables = {
      MESSAGES_BUCKET        = aws_s3_bucket.messages.id
      CONTACTS_TABLE         = aws_dynamodb_table.contacts.name
      RATE_LIMIT_TABLE       = aws_dynamodb_table.rate_limits.name
      NOTIFICATION_QUEUE_URL = aws_sqs_queue.notifications.url
    }
  }
}
```

- [ ] **Step 2: Add `tracing_config` to the notifier Lambda (`terraform/lambda.tf`)**

In the existing `resource "aws_lambda_function" "notifier"` block, add the same block after `memory_size`:

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

  # Plan 3: X-Ray active tracing. Inherits the trace context propagated through SQS from
  # the ingest Lambda, so the digest send (SES) appears in the same end-to-end trace.
  tracing_config {
    mode = "Active"
  }

  environment {
    variables = {
      FROM_EMAIL               = local.from_email
      CONTACT_EMAIL_SECRET_ARN = aws_secretsmanager_secret.contact_email.arn
    }
  }
}
```

- [ ] **Step 3: Attach X-Ray write permissions to both Lambda roles (`terraform/iam.tf`)**

Active tracing requires the function role to be able to send segments to the X-Ray daemon. The AWS-managed `AWSXRayDaemonWriteAccess` policy is the standard grant (it bundles `xray:PutTraceSegments` + `xray:PutTelemetryRecords` + the sampling-rule reads). Append to `terraform/iam.tf`, after the existing `..._logs` attachments:

```hcl
# --- X-Ray write permissions (Plan 3) ---
# AWSXRayDaemonWriteAccess grants xray:PutTraceSegments, xray:PutTelemetryRecords,
# xray:GetSamplingRules, xray:GetSamplingTargets — the minimum for active tracing.
resource "aws_iam_role_policy_attachment" "ingest_xray" {
  role       = aws_iam_role.ingest.name
  policy_arn = "arn:aws:iam::aws:policy/AWSXRayDaemonWriteAccess"
}

resource "aws_iam_role_policy_attachment" "notifier_xray" {
  role       = aws_iam_role.notifier.name
  policy_arn = "arn:aws:iam::aws:policy/AWSXRayDaemonWriteAccess"
}
```

> Alternative (if you prefer an inline least-privilege statement over the managed policy): add a statement with `actions = ["xray:PutTraceSegments", "xray:PutTelemetryRecords", "xray:GetSamplingRules", "xray:GetSamplingTargets", "xray:GetSamplingStatisticSummaries"]` and `resources = ["*"]` (X-Ray write actions do not support resource-level scoping) to each role's existing `aws_iam_role_policy`. The managed policy is the AWS-recommended default and is used above.

- [ ] **Step 4: Create `terraform/xray.tf` (X-Ray sampling + group)**

Optional but recommended grouping so the contact pipeline's traces are filterable in the X-Ray console, and an explicit sampling rule so prod doesn't sample 100% of traffic (cost/noise control). For this low-volume form, a high rate is fine.

```hcl
# Plan 3: an X-Ray group scoping the console/service-map view to this pipeline's services,
# and a sampling rule. Active tracing on the Lambdas is what actually produces segments;
# these just shape what you see and how much is sampled.

resource "aws_xray_group" "contact" {
  group_name        = "${local.name_prefix}-trace"
  filter_expression = "service(\"${local.name_prefix}-ingest\") OR service(\"${local.name_prefix}-notifier\")"
}

# Sample generously — contact volume is tiny, so capture (almost) everything for debugging.
resource "aws_xray_sampling_rule" "contact" {
  rule_name      = "${local.name_prefix}-sampling"
  priority       = 1000
  version        = 1
  reservoir_size = 1
  fixed_rate     = 1.0 # 100% — fine at this volume; lower for high-traffic services
  host           = "*"
  http_method    = "*"
  url_path       = "*"
  service_name   = "${local.name_prefix}-*"
  service_type   = "*"
  resource_arn   = "*"
}
```

> ⚠️ **context7-unverified:** the exact argument set for `aws_xray_sampling_rule` (`reservoir_size`, `fixed_rate`, `version`, `priority`, `service_name` wildcard support) and `aws_xray_group` (`filter_expression` syntax) was not re-confirmed via context7 in this pass — they match the long-stable provider schema but **verify against `tofu validate` / the provider docs before apply**. If `service_name = "${local.name_prefix}-*"` is rejected, set `service_name = "*"` and rely on the group's `filter_expression` for scoping. This whole file is **optional**; X-Ray works with active tracing alone — you may skip `xray.tf` entirely and still get the connected trace map.

- [ ] **Step 5: (OPTIONAL) ADOT layer for richer auto-instrumentation**

If, after verifying (Task 4) the trace map shows the invocation + service-to-service edges but you want **per-SDK-call subsegments** (each S3/DynamoDB/SQS/SES call as its own timed subsegment) **without editing `backend/` code**, add the ADOT layer to each function:

```hcl
# OPTIONAL — only if you want zero-code-change per-call subsegments. Look up the current
# ADOT Node.js layer ARN for us-east-1 from the aws-otel.github.io docs (region-specific,
# versioned) and pin it. AWS_LAMBDA_EXEC_WRAPPER activates auto-instrumentation.
#
#   layers = ["arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-nodejs-amd64-ver-1-x-x:N"]
#   environment.variables += { AWS_LAMBDA_EXEC_WRAPPER = "/opt/otel-handler" }
```

> **Tracing depth trade-off:** active tracing alone (Steps 1–3) already gives the **connected end-to-end trace** required by the verification section (browser → ingest → SQS → notifier → SES). The ADOT layer adds finer subsegment detail at the cost of a layer dependency (region-pinned ARN to maintain), ~cold-start latency, and slightly more config. **Recommendation: ship without ADOT;** add it later only if the per-call breakdown is needed. **This step is deliberately left as commented guidance, not active HCL,** so the default path stays simple.

- [ ] **Step 6: Validate**

Run: `cd terraform && tofu validate`
Expected: `Success! The configuration is valid.` (If `xray.tf` sampling-rule args error, apply the fallback in the Step 4 note, then re-validate.)

- [ ] **Step 7: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/lambda.tf terraform/iam.tf terraform/xray.tf
git commit -m "obs(3): enable X-Ray active tracing on both Lambdas + X-Ray IAM + group/sampling"
```

---

## Task 2: CloudWatch RUM — Cognito guest pool, guest role, app monitor, snippet

**Files:**
- Create: `terraform/rum.tf`
- Modify: `terraform/outputs.tf`

- [ ] **Step 1: Create `terraform/rum.tf` — Cognito unauthenticated (guest) identity pool**

CloudWatch RUM's web client authenticates anonymous browser sessions via a Cognito **identity pool with unauthenticated identities enabled**. The guest role it assumes is granted only `rum:PutRumEvents` for this monitor.

```hcl
# Plan 3: CloudWatch RUM. The browser RUM client authenticates as a Cognito guest
# (unauthenticated identity) and assumes a role allowed only to call rum:PutRumEvents
# for this app monitor. Per-env (name_prefix carries ${local.env}).

resource "aws_cognito_identity_pool" "rum" {
  identity_pool_name               = "${local.name_prefix}-rum-identity"
  allow_unauthenticated_identities = true
  allow_classic_flow               = false
}
```

- [ ] **Step 2: Append the guest IAM role + trust policy to `terraform/rum.tf`**

The role trusts `cognito-identity.amazonaws.com` via web identity, scoped to **this** pool and to **unauthenticated** sessions (`amr = "unauthenticated"`):

```hcl
data "aws_iam_policy_document" "rum_guest_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = ["cognito-identity.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "cognito-identity.amazonaws.com:aud"
      values   = [aws_cognito_identity_pool.rum.id]
    }
    condition {
      test     = "ForAnyValue:StringLike"
      variable = "cognito-identity.amazonaws.com:amr"
      values   = ["unauthenticated"]
    }
  }
}

resource "aws_iam_role" "rum_guest" {
  name               = "${local.name_prefix}-rum-guest"
  assume_role_policy = data.aws_iam_policy_document.rum_guest_assume.json
}

# Least privilege: only PutRumEvents, only for this monitor's ARN.
data "aws_iam_policy_document" "rum_guest" {
  statement {
    sid       = "PutRumEvents"
    effect    = "Allow"
    actions   = ["rum:PutRumEvents"]
    resources = [aws_rum_app_monitor.main.arn]
  }
}

resource "aws_iam_role_policy" "rum_guest" {
  name   = "${local.name_prefix}-rum-guest"
  role   = aws_iam_role.rum_guest.id
  policy = data.aws_iam_policy_document.rum_guest.json
}
```

> The guest role references `aws_rum_app_monitor.main.arn` and the monitor (Step 4) references this role's ARN. OpenTofu resolves the dependency graph fine as long as the monitor's `app_monitor_configuration.guest_role_arn` points at `aws_iam_role.rum_guest.arn` (a role attribute) while the policy points at the monitor's `.arn` (a monitor attribute) — no cycle, since each references a *different* attribute of the other. If a cycle is reported, scope the policy `resources` to `"arn:aws:rum:${var.aws_region}:*:appmonitor/${local.name_prefix}-rum"` (constructed string) instead of the monitor attribute, breaking the reference.

- [ ] **Step 3: Append the roles attachment to `terraform/rum.tf`**

```hcl
resource "aws_cognito_identity_pool_roles_attachment" "rum" {
  identity_pool_id = aws_cognito_identity_pool.rum.id
  roles = {
    "unauthenticated" = aws_iam_role.rum_guest.arn
  }
}
```

- [ ] **Step 4: Append the RUM app monitor to `terraform/rum.tf`**

`enable_xray = true` is the key linkage: the RUM web client adds an X-Ray trace header to sampled HTTP requests and records a browser-side X-Ray segment, so the browser session stitches to the Lambda segments in the X-Ray service map. `cw_log_enabled = true` mirrors RUM events to a CloudWatch Logs log group for debugging.

```hcl
resource "aws_rum_app_monitor" "main" {
  name           = "${local.name_prefix}-rum"
  domain         = local.site_domain
  cw_log_enabled = true

  app_monitor_configuration {
    allow_cookies       = true
    enable_xray         = true # links browser session → Lambda X-Ray segments
    session_sample_rate = 1.0  # capture all sessions at this volume
    telemetries         = ["errors", "performance", "http"]

    identity_pool_id = aws_cognito_identity_pool.rum.id
    guest_role_arn   = aws_iam_role.rum_guest.arn
  }
}
```

> ⚠️ **context7-partially-verified:** context7 confirmed `aws_rum_app_monitor` exists with `name`/`domain`, and confirmed the `app_monitor_configuration` sub-arguments `allow_cookies` and `enable_xray`. The exact spelling of `session_sample_rate`, `telemetries`, `identity_pool_id`, `guest_role_arn`, and the top-level `cw_log_enabled` matches the stable provider schema but was **not each individually echoed back** by context7 in this pass — **confirm via `tofu validate` and the provider docs for `aws_rum_app_monitor` before apply.** If `domain` is rejected for a subdomain, the provider also accepts `domain_list = [local.site_domain, "www.${local.site_domain}"]` in newer versions — check which your pinned `~> 5.70` supports.

- [ ] **Step 5: Add RUM outputs to `terraform/outputs.tf`**

The frontend snippet (Task selection of `VITE_RUM_*` build vars) needs the app monitor **id** (a GUID, distinct from name/ARN), the identity pool id, the region, and the guest role ARN.

```hcl
output "rum_app_monitor_id" {
  description = "CloudWatch RUM app monitor ID (GUID) — used by the browser snippet"
  value       = aws_rum_app_monitor.main.id
}

output "rum_identity_pool_id" {
  description = "Cognito identity pool ID for RUM guest auth"
  value       = aws_cognito_identity_pool.rum.id
}

output "rum_guest_role_arn" {
  description = "Guest IAM role ARN the RUM client assumes"
  value       = aws_iam_role.rum_guest.arn
}

# Convenience: the exact values to drop into the frontend's VITE_RUM_* build vars.
output "rum_snippet_config" {
  description = "Values for the frontend VITE_RUM_* build vars (region, monitor id, identity pool id)"
  value = {
    VITE_RUM_APP_MONITOR_ID = aws_rum_app_monitor.main.id
    VITE_RUM_IDENTITY_POOL  = aws_cognito_identity_pool.rum.id
    VITE_RUM_REGION         = var.aws_region
  }
}
```

> ⚠️ **context7-unverified:** that the `id` attribute of `aws_rum_app_monitor` is the snippet's app-monitor GUID (vs. a separate computed `app_monitor_id`). The RUM web client's `applicationId` is a GUID; `aws_rum_app_monitor.id` is the documented identifier attribute and is the GUID. **Confirm with `tofu output rum_app_monitor_id` after apply** that it's a GUID, not the name.

- [ ] **Step 6: Validate**

Run: `cd terraform && tofu validate`
Expected: valid. (If a dependency cycle is reported on the guest role ↔ monitor, apply the constructed-ARN fallback from Step 2's note.)

- [ ] **Step 7: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/rum.tf terraform/outputs.tf
git commit -m "obs(3): add CloudWatch RUM app monitor + Cognito guest pool/role + outputs"
```

---

## Task 3 (part of obs flow): Inject the RUM JS snippet into the built SPA (build-flag gated)

**Files:**
- Create: `src/rum.ts`
- Modify: `src/main.tsx`
- Modify: `index.html` (CSP only)
- Modify: `.env.example`
- Modify: `vite.config.ts` (tree-shake side-effect list)

The `aws-rum-web` client is booted from a tiny side-effect module that **only initializes when the `VITE_RUM_*` vars are present** — so local dev (vars unset) and prod (vars set by CI from the `rum_snippet_config` output) differ cleanly. Loading from the AWS-hosted CDN avoids adding an npm dependency; alternatively `npm i aws-rum-web` and import `AwsRum` directly (note in Step 1).

- [ ] **Step 1: Create `src/rum.ts`**

```ts
// CloudWatch RUM bootstrap. No-op unless VITE_RUM_* build vars are present, so dev builds
// (and any env without RUM provisioned) ship zero RUM code-path. Values come from the
// terraform `rum_snippet_config` output, injected as VITE_RUM_* at build time.
//
// Loads aws-rum-web from the AWS-hosted CDN (no npm dep). To use the bundled client instead,
// `npm i aws-rum-web`, then `import { AwsRum } from 'aws-rum-web'` and skip the loader.

const APP_ID = import.meta.env.VITE_RUM_APP_MONITOR_ID as string | undefined;
const IDENTITY_POOL = import.meta.env.VITE_RUM_IDENTITY_POOL as string | undefined;
const REGION = (import.meta.env.VITE_RUM_REGION as string | undefined) ?? 'us-east-1';

if (APP_ID && IDENTITY_POOL) {
  const config = {
    sessionSampleRate: 1,
    identityPoolId: IDENTITY_POOL,
    endpoint: `https://dataplane.rum.${REGION}.amazonaws.com`,
    telemetries: ['errors', 'performance', 'http'] as const,
    allowCookies: true,
    enableXRay: true, // mirrors the app monitor; links browser session → Lambda X-Ray
  };

  // Standard AWS snippet loader: pulls the cwr() shim + the web client bundle.
  /* eslint-disable */
  (function (n: any, i: any, v: any, r: any, s: any, c: any, x: any, z: any) {
    x = window as any;
    x['AwsRumClient'] = { q: [], n, i, v, r, c };
    x[n] = function (...args: any[]) { x[n].q.push(args); };
    z = document.createElement('script');
    z.async = true;
    z.src = s;
    document.head.insertBefore(z, document.head.getElementsByTagName('script')[0]);
  })(
    'cwr',
    APP_ID,
    '1.0.0',
    REGION,
    'https://client.rum.us-east-1.amazonaws.com/1.x/cwr.js',
    config,
    undefined,
    undefined,
  );
  /* eslint-enable */
}

export {};
```

> ⚠️ **context7-unverified (browser SDK):** the exact `cwr.js` CDN URL/version path and the loader-shim signature come from AWS's published RUM snippet and the `aws-rum-web` README, **not** from context7 (which serves the Terraform provider, not the browser client). **Verify the current snippet** at the RUM console's "JavaScript snippet" tab (the console generates the exact `applicationId`/region/`cwr.js` URL for your monitor) or the `aws-rum-web` README before shipping. The console-generated snippet is authoritative; this module mirrors its shape with the values parameterized via `VITE_RUM_*`. The cleaner, type-safe alternative is `npm i aws-rum-web` + `new AwsRum(APP_ID, '1.0.0', REGION, config)` — prefer that if adding the dep is acceptable (it removes the CDN loader and the eslint-disable).

- [ ] **Step 2: Import the bootstrap in `src/main.tsx` (side-effect, before render)**

```tsx
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import './rum' // CloudWatch RUM bootstrap (no-op unless VITE_RUM_* set)
import { App } from './App.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
```

- [ ] **Step 3: Allow the RUM endpoints in the CSP (`index.html`)**

The current `Content-Security-Policy` meta restricts `script-src`/`connect-src` to self + Cloudflare. Add the RUM CDN (`client.rum.us-east-1.amazonaws.com`) to `script-src`, the RUM dataplane + Cognito to `connect-src`. (When Plan 2c removes Turnstile, the Cloudflare entries go; keep them for now since 2c runs in parallel.)

Replace the `script-src` and `connect-src` directives:
```
script-src 'self' 'unsafe-inline' 'unsafe-eval' https://challenges.cloudflare.com https://*.cloudflare.com https://client.rum.us-east-1.amazonaws.com;
connect-src 'self' https://challenges.cloudflare.com https://*.cloudflare.com https://dataplane.rum.us-east-1.amazonaws.com https://cognito-identity.us-east-1.amazonaws.com https://sts.us-east-1.amazonaws.com;
```

> Cognito guest auth calls `cognito-identity.<region>` and (for `GetCredentialsForIdentity`) may touch `sts` — both added to `connect-src`. Adjust the region in the URLs if `var.aws_region` ever changes from `us-east-1`.

- [ ] **Step 4: Mark `src/rum.ts` / aws-rum CDN as having side effects in `vite.config.ts`**

`src/rum.ts` is imported for its side effect (booting RUM); ensure tree-shaking doesn't drop it. It's a local module imported with a bare `import './rum'`, which Vite/Rollup already treats as a side-effect import, so **no change is usually needed**. If you instead `npm i aws-rum-web`, add a guard so it is **not** marked side-effect-free (do **not** add it to the `noSideEffects` regex list — that list is for modules safe to drop). No edit required unless adding the npm package; document this in the commit.

- [ ] **Step 5: Document the build vars in `.env.example`**

Append:
```bash
# CloudWatch RUM (browser observability). Leave unset locally to disable RUM in dev.
# Values come from `tofu output rum_snippet_config` after applying Plan 3.
VITE_RUM_APP_MONITOR_ID=
VITE_RUM_IDENTITY_POOL=
VITE_RUM_REGION=us-east-1
```

> **CI wiring (handoff to Plan 2c's CI task):** the prod build job sets `VITE_RUM_APP_MONITOR_ID` / `VITE_RUM_IDENTITY_POOL` / `VITE_RUM_REGION` from the corresponding `tofu output`s (or GitHub secrets populated from them). The dev build leaves them unset → no RUM in dev preview, RUM only on the deployed dev/prod sites that have a monitor. Plan 2c owns the `ci.yml` edit; this plan only defines the contract (the three `VITE_RUM_*` names + the `rum_snippet_config` output).

- [ ] **Step 6: Typecheck + build the frontend**

Run:
```bash
npx tsc --noEmit && npm run build
```
Expected: typecheck passes; build succeeds. With `VITE_RUM_*` unset, the `if (APP_ID && IDENTITY_POOL)` guard is false → the RUM loader is dead-code-eliminated/never runs (verify the snippet isn't in `dist/` when vars are unset, or is gated behind the runtime check).

- [ ] **Step 7: Format & commit**

```bash
git add src/rum.ts src/main.tsx index.html vite.config.ts .env.example
git commit -m "obs(3): add build-flag-gated CloudWatch RUM browser snippet + CSP allowances"
```

---

## Task 4: Apply + end-to-end trace/RUM verification

**Files:** none (verification only)

- [ ] **Step 1: Plan review**

Run:
```bash
cd terraform && tofu validate && tofu plan -out=3.plan
```
Expected: a plan that **adds** `tracing_config` to the two existing Lambdas (in-place update), the two X-Ray IAM attachments, the X-Ray group/sampling rule, the Cognito pool + guest role + roles attachment, and the RUM app monitor — and **changes nothing else**. Confirm: **no destroys** of Plan 2a/2b resources. (A real plan needs the GitLab backend init with `GITLAB_ACCESS_TOKEN` + the per-env backend-config; apply from CI or with creds.)

- [ ] **Step 2: Apply**

```bash
cd terraform && tofu apply 3.plan
```
Expected: Lambdas updated to active tracing; RUM/Cognito/X-Ray resources created. Capture the outputs:
```bash
tofu output rum_snippet_config   # feed these into VITE_RUM_* for the prod build
tofu output rum_app_monitor_id   # confirm it's a GUID, not the name
```

- [ ] **Step 3: Build + deploy the frontend with RUM enabled**

Set the three `VITE_RUM_*` vars from `rum_snippet_config`, build, and deploy (via the Plan 2c `s3 sync` path, or `wrangler` for the interim prod site until 2c flips). Then load the live site in a browser.

- [ ] **Step 4: Verify the connected X-Ray trace (end-to-end)**

Submit a real contact form on the live site (through CloudFront so WAF/Lambda are exercised). Then:
- **X-Ray console → Traces / Service map:** within a minute, a trace appears showing **browser (RUM client segment) → CloudFront → `…-ingest` Lambda → subsegments for S3 PutObject / DynamoDB PutItem-UpdateItem / SQS SendMessage**. After the SQS batch window (≤300s), the **`…-notifier` Lambda → SES SendEmail** segment links into the **same trace** (via the SQS-propagated trace header). The service map shows a connected graph ingest → SQS → notifier → SES.
  - If you see the ingest segment but the notifier appears as a *separate* trace, confirm both Lambdas have `tracing_config { mode = "Active" }` and the X-Ray IAM attachment (the SQS trace header only propagates when both are traced).
  - Per-AWS-SDK-call subsegments missing? That's expected on the no-ADOT path for some SDK paths; the **invocation + service edges still connect**. Add the optional ADOT layer (Task 1 Step 5) if you need every call broken out.

- [ ] **Step 5: Verify a RUM session appears**

- **CloudWatch RUM console → your `…-rum` app monitor:** within ~1–2 min of loading the site, a **session** appears with page-load performance, and (if you triggered one) a JS error and the `/api/contact` HTTP request. The browser network tab should show successful `POST https://dataplane.rum.us-east-1.amazonaws.com/...` (200) and a prior `cognito-identity` `GetId`/`GetCredentialsForIdentity` call.
  - No sessions? Check: CSP allows the RUM CDN + dataplane + cognito-identity (Task 3 Step 3); the build actually had `VITE_RUM_*` set; the guest role policy `rum:PutRumEvents` resource matches the monitor ARN; the Cognito pool has `allow_unauthenticated_identities = true`.
  - `enable_xray` linkage: a sampled RUM session's HTTP request to `/api/contact` should carry an `X-Amzn-Trace-Id` header, and that trace id should be the one found in the X-Ray console (Step 4) — confirming the browser→Lambda stitch.

- [ ] **Step 6: Commit (verification notes only, if any tweaks were needed)**

```bash
cd terraform && tofu fmt
git add -A
git commit -m "obs(3): apply X-Ray + RUM; verified connected browser→Lambda→SES trace and RUM session"
```

---

## Task 5 (OPTIONAL): CloudWatch dashboard + alarms

> **This task is OPTIONAL.** X-Ray + RUM (Tasks 1–4) satisfy the plan's observability goal. This task adds proactive metric alarms + a single-pane dashboard. Skip if not wanted; it does not block anything.

**Files:**
- Create: `terraform/dashboards.tf`
- Modify: `terraform/variables.tf` (add `alarm_email`)
- Modify: `terraform/outputs.tf` (dashboard URL)

- [ ] **Step 1: Add an `alarm_email` variable + SNS topic for notifications (`variables.tf` + `dashboards.tf`)**

```hcl
# variables.tf
variable "alarm_email" {
  description = "Email address to receive CloudWatch alarm notifications (optional; from gitignored tfvars). Empty disables the SNS subscription."
  type        = string
  default     = ""
}
```

```hcl
# dashboards.tf — SNS topic + (conditional) email subscription
resource "aws_sns_topic" "alarms" {
  name = "${local.name_prefix}-alarms"
}

resource "aws_sns_topic_subscription" "alarms_email" {
  count     = var.alarm_email == "" ? 0 : 1
  topic_arn = aws_sns_topic.alarms.arn
  protocol  = "email"
  endpoint  = var.alarm_email # confirm the subscription via the email AWS sends
}
```

> `alarm_email` is supplied via the gitignored `terraform.tfvars` (same pattern as `contact_email`) — **no email literal in git**. Empty default → no subscription created (alarms still fire to the topic; wire it later).

- [ ] **Step 2: Add Lambda alarms (errors, throttles, duration) to `dashboards.tf`**

```hcl
locals {
  lambda_fns = {
    ingest   = aws_lambda_function.ingest.function_name
    notifier = aws_lambda_function.notifier.function_name
  }
}

resource "aws_cloudwatch_metric_alarm" "lambda_errors" {
  for_each            = local.lambda_fns
  alarm_name          = "${local.name_prefix}-${each.key}-errors"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = each.value }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "lambda_throttles" {
  for_each            = local.lambda_fns
  alarm_name          = "${local.name_prefix}-${each.key}-throttles"
  namespace           = "AWS/Lambda"
  metric_name         = "Throttles"
  dimensions          = { FunctionName = each.value }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "lambda_duration" {
  for_each            = local.lambda_fns
  alarm_name          = "${local.name_prefix}-${each.key}-duration-p99"
  namespace           = "AWS/Lambda"
  metric_name         = "Duration"
  dimensions          = { FunctionName = each.value }
  extended_statistic  = "p99"
  period              = 300
  evaluation_periods  = 3
  threshold           = 5000 # ms; ingest timeout is 10s, notifier 60s — tune per function
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
}
```

- [ ] **Step 3: Add the SQS DLQ-depth alarm to `dashboards.tf`**

```hcl
# Any visible message in the DLQ means a notification permanently failed processing.
resource "aws_cloudwatch_metric_alarm" "dlq_not_empty" {
  alarm_name          = "${local.name_prefix}-dlq-not-empty"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.notifications_dlq.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
}
```

- [ ] **Step 4: Add SES bounce/complaint-rate alarms to `dashboards.tf`**

SES publishes account-level `Reputation.BounceRate` / `Reputation.ComplaintRate` to `AWS/SES`. Alarm above AWS's review thresholds (bounce 5%, complaint 0.1%).

```hcl
resource "aws_cloudwatch_metric_alarm" "ses_bounce_rate" {
  alarm_name          = "${local.name_prefix}-ses-bounce-rate"
  namespace           = "AWS/SES"
  metric_name         = "Reputation.BounceRate"
  statistic           = "Average"
  period              = 3600
  evaluation_periods  = 1
  threshold           = 0.05 # 5% — SES enforcement threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "ses_complaint_rate" {
  alarm_name          = "${local.name_prefix}-ses-complaint-rate"
  namespace           = "AWS/SES"
  metric_name         = "Reputation.ComplaintRate"
  statistic           = "Average"
  period              = 3600
  evaluation_periods  = 1
  threshold           = 0.001 # 0.1% — SES enforcement threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
}
```

> ⚠️ **context7-unverified:** the `AWS/SES` `Reputation.BounceRate` / `Reputation.ComplaintRate` metric names are AWS-documented CloudWatch metrics (not provider schema), so context7 (provider docs) doesn't cover them. They are stable but **confirm the namespace/metric spelling in the CloudWatch console** before relying on the alarm. If the account has no SES reputation metrics yet (very low volume), these alarms sit in `INSUFFICIENT_DATA` until SES emits — `treat_missing_data = "notBreaching"` keeps that benign.

- [ ] **Step 5: Add a CloudWatch dashboard to `dashboards.tf`**

```hcl
resource "aws_cloudwatch_dashboard" "contact" {
  dashboard_name = "${local.name_prefix}"
  dashboard_body = jsonencode({
    widgets = [
      {
        type = "metric", x = 0, y = 0, width = 12, height = 6,
        properties = {
          title  = "Lambda invocations & errors",
          region = var.aws_region,
          metrics = [
            ["AWS/Lambda", "Invocations", "FunctionName", aws_lambda_function.ingest.function_name],
            ["AWS/Lambda", "Errors", "FunctionName", aws_lambda_function.ingest.function_name],
            ["AWS/Lambda", "Invocations", "FunctionName", aws_lambda_function.notifier.function_name],
            ["AWS/Lambda", "Errors", "FunctionName", aws_lambda_function.notifier.function_name],
          ]
        }
      },
      {
        type = "metric", x = 12, y = 0, width = 12, height = 6,
        properties = {
          title  = "SQS depth (main + DLQ)",
          region = var.aws_region,
          metrics = [
            ["AWS/SQS", "ApproximateNumberOfMessagesVisible", "QueueName", aws_sqs_queue.notifications.name],
            ["AWS/SQS", "ApproximateNumberOfMessagesVisible", "QueueName", aws_sqs_queue.notifications_dlq.name],
          ]
        }
      },
      {
        type = "metric", x = 0, y = 6, width = 12, height = 6,
        properties = {
          title  = "SES send / bounce / complaint",
          region = var.aws_region,
          metrics = [
            ["AWS/SES", "Send"],
            ["AWS/SES", "Reputation.BounceRate"],
            ["AWS/SES", "Reputation.ComplaintRate"],
          ]
        }
      },
    ]
  })
}
```

- [ ] **Step 6: Add dashboard output (`outputs.tf`)**

```hcl
output "dashboard_url" {
  description = "CloudWatch dashboard URL for the contact pipeline"
  value       = "https://${var.aws_region}.console.aws.amazon.com/cloudwatch/home?region=${var.aws_region}#dashboards:name=${aws_cloudwatch_dashboard.contact.dashboard_name}"
}
```

- [ ] **Step 7: Validate, format, commit**

```bash
cd terraform && tofu validate && tofu fmt
git add terraform/dashboards.tf terraform/variables.tf terraform/outputs.tf
git commit -m "obs(3): add OPTIONAL CloudWatch dashboard + Lambda/SQS-DLQ/SES alarms (SNS email)"
```

> If applied, confirm the SNS email subscription (AWS sends a confirmation email to `alarm_email` — click to activate), then force-fail a Lambda once to confirm an alarm → email fires.

---

## Dev/prod notes

- **Per-env resources:** RUM app monitor (`${local.name_prefix}-rum`), Cognito pool (`…-rum-identity`), guest role (`…-rum-guest`), and (optional) alarms/dashboard all carry `${local.env}` via `local.name_prefix` (from Plan 2b Task 1) → dev and prod get **separate** monitors/pools automatically. `domain = local.site_domain` resolves to `trystan-tbm.dev` (prod) or `dev.trystan-tbm.dev` (dev).
- **X-Ray:** active tracing is free-tier-friendly (1M traces + 1M scanned/mo free); **leave it on in dev** — no per-env gating needed. The `xray.tf` group/sampling rule is global-ish but prefixed per-env.
- **Frontend gating:** dev vs prod differ purely by whether `VITE_RUM_*` are set at build time. Local `npm run dev` (vars unset) → no RUM, clean console. The deployed dev site can point at the dev monitor by setting the dev build's `VITE_RUM_*` from the dev `tofu output rum_snippet_config`.
- **Independence from 2c:** none of this touches DNS or the Cloudflare→AWS cutover. The RUM monitor can be created for the prod domain before DNS flips; sessions just won't arrive until CloudFront serves the site. So Plan 3 is safely applied **alongside** Plan 2c.
- **Provider/region:** all in `us-east-1` (matches Plan 2a/2b). RUM, Cognito, X-Ray are all in-region; the browser snippet hardcodes the region via `VITE_RUM_REGION` and CSP URLs — keep them in sync if the region ever changes.

---

## Self-Review

**In scope (this plan):**
- X-Ray **active tracing** on both Lambdas (`lambda.tf` retrofit) + `AWSXRayDaemonWriteAccess` on both roles (`iam.tf`) → Task 1 ✓
- X-Ray group + sampling rule for console scoping/cost (`xray.tf`, optional within the task) → Task 1 ✓
- **CloudWatch RUM**: Cognito unauthenticated identity pool + roles attachment + least-privilege guest role (`rum:PutRumEvents` scoped to the monitor) + `aws_rum_app_monitor` with `enable_xray = true` (browser↔Lambda trace stitch) → Task 2 ✓
- Build-flag-gated (`VITE_RUM_*`) browser RUM snippet (`src/rum.ts`, `main.tsx`, CSP, `.env.example`) so dev/prod differ → Task 3 ✓
- New outputs: `rum_app_monitor_id`, `rum_identity_pool_id`, `rum_guest_role_arn`, `rum_snippet_config` (and optional `dashboard_url`) → Tasks 2/5 ✓
- New vars: `VITE_RUM_APP_MONITOR_ID` / `VITE_RUM_IDENTITY_POOL` / `VITE_RUM_REGION` (frontend), optional `alarm_email` (terraform) ✓
- Verification: connected end-to-end X-Ray trace (browser→ingest→SQS→notifier→SES) + a RUM session in console → Task 4 ✓
- OPTIONAL CloudWatch dashboard + Lambda/SQS-DLQ/SES alarms with SNS email → Task 5 ✓

**Recommended approach chosen:** **X-Ray active tracing + AWS SDK v3 built-in instrumentation, NO ADOT layer** (the connected trace map is produced by active tracing + service-to-service/SQS propagation alone). ADOT layer documented as an **optional** enhancement (Task 1 Step 5, commented guidance only) for richer per-SDK-call subsegments. RUM via the **AWS-hosted CDN snippet** (no npm dep), with `npm i aws-rum-web` noted as the type-safe alternative.

**Deferred / handed off (correctly NOT here):**
- The `ci.yml` edit that injects `VITE_RUM_*` into the prod build and wires AWS OIDC → **Plan 2c** (this plan defines only the var/output contract).
- The CloudFront/WAF resources themselves → **Plan 2b** (consumed by name only; RUM links to X-Ray, not directly to WAF — the meta-plan's "RUM↔WAF link" is optional and not required for the trace stitch, so it's intentionally omitted).
- Removing Turnstile CSP entries → **Plan 2c** (kept here since 2c runs in parallel; RUM CSP allowances are additive).

**context7-unverified assumptions flagged (must confirm with `tofu validate` / provider docs / AWS console before apply):**
1. `aws_xray_sampling_rule` / `aws_xray_group` exact argument set + `service_name` wildcard support (Task 1 Step 4) — `xray.tf` is optional; fallback provided.
2. `aws_rum_app_monitor.app_monitor_configuration` sub-args `session_sample_rate` / `telemetries` / `identity_pool_id` / `guest_role_arn` and top-level `cw_log_enabled` (context7 confirmed only `allow_cookies` + `enable_xray` + the resource's `name`/`domain`) (Task 2 Step 4).
3. `aws_rum_app_monitor.id` is the browser snippet's GUID `applicationId` (Task 2 Step 5) — confirm via `tofu output` post-apply.
4. The browser **RUM snippet** (`cwr.js` CDN URL, loader-shim signature) is from AWS's published snippet / `aws-rum-web` README, **not** context7 — use the RUM console's generated snippet as authoritative (Task 3 Step 1).
5. `AWS/SES` `Reputation.BounceRate` / `Reputation.ComplaintRate` metric names are AWS CloudWatch metrics, not provider schema — confirm spelling in console (Task 5 Step 4).
6. Possible guest-role ↔ app-monitor reference cycle (Task 2 Step 2) — constructed-ARN fallback provided.

**Placeholder scan:** no email/secret literals; `alarm_email`/`contact_email` come from gitignored tfvars; the ADOT layer ARN is intentionally left as a lookup (region/version-specific) rather than a guessed pin.

**Consistency checks:**
- Resource names extend Plan 2b's `${local.name_prefix}` (env-aware) — `…-rum`, `…-rum-identity`, `…-rum-guest`, `…-trace`, `…-alarms`, `…-<fn>-errors`.
- RUM `domain = local.site_domain` (Plan 2b interface output), `region = var.aws_region` (Plan 2a var).
- Guest role grants exactly `rum:PutRumEvents` on exactly the monitor ARN (least privilege, mirrors the Plan 2a IAM posture).
- X-Ray IAM uses the AWS-managed `AWSXRayDaemonWriteAccess` (inline least-privilege alternative documented).
