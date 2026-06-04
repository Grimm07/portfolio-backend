# Lambda Contact Pipeline — Plan 2c: Cutover (DNS + frontend CAPTCHA swap + retire Cloudflare + CI)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make AWS the live site + backend and retire the Cloudflare Worker/Pages. Cut the frontend over from Cloudflare Turnstile to the **AWS WAF CAPTCHA JavaScript SDK**, flip DNS from proxied→`pages.dev` to DNS-only→CloudFront, delete the Cloudflare app resources from terraform, and rewrite the CI deploy job to authenticate to AWS via GitHub OIDC, `tofu apply` the AWS edge, and ship the static site via `aws s3 sync` + CloudFront invalidation.

**This is the go-live, highest-risk plan.** Sequence it so that **rollback is a single DNS flip** (re-point root/`www` back to `proxied`→`pages.dev`). Do NOT delete Cloudflare Pages/Worker resources (Task 4) or `wrangler delete` the Worker (Task 6) until the new AWS edge is verified live on prod DNS.

**Depends on:** Plan **2b** (edge infra) — this plan consumes 2b's interface-contract outputs **by exact name**:

| 2b output (interface contract) | Consumed here for |
|---|---|
| `cloudfront_domain_name` | DNS retarget (Task 3), CloudFront invalidation target |
| `cloudfront_distribution_id` | CloudFront invalidation (Tasks 2, 5) |
| `site_bucket` (`portfolio-contact-${env}-site`) | frontend `s3 sync` (Tasks 2, 5) |
| `waf_captcha_integration_url` | frontend WAF CAPTCHA script URL → `VITE_WAF_INTEGRATION_URL` (Task 1) |
| `waf_captcha_api_key` (`aws_wafv2_api_key`, sensitive) | frontend WAF CAPTCHA api key → `VITE_WAF_API_KEY` (Task 1) |

**Environment strategy (carried from the meta-plan):** prod is the default/live env (`var.environment = "prod"`). `local.site_domain` is `var.domain_name` for prod and `dev.${var.domain_name}` for dev. State is separated per-env at `tofu init` via partial backend-config (`prod` → `.../state/portfolio`, `dev` → `.../state/portfolio-dev`). Cloudflare app resources are **prod-only** and are removed here; **dev has no Pages/Worker to remove** (dev was always AWS-only under `dev.trystan-tbm.dev`).

**Tech Stack:** React 19 + Vite (frontend), AWS WAF CAPTCHA JS SDK (browser), OpenTofu (`tofu`) with the existing `aws`/`archive`/`cloudflare` providers, GitHub Actions with `aws-actions/configure-aws-credentials@v6` (OIDC) + AWS CLI.

> ⚠️ **context7-unverified detail (flagged, not invented):** context7 confirmed the WAF CAPTCHA **API-key model** (`aws_wafv2_api_key`, up to 5 token domains, key required for the CAPTCHA JS integration — see `CreateAPIKeyCommand`/`GetDecryptedAPIKeyCommand` in the WAFV2 client docs). It did **not** return the exact browser method surface. The names used below — the global `AwsWafIntegration` object, `AwsWafIntegration.getToken()`, `AwsWafIntegration.fetch()`, and the injected `x-aws-waf-token` request header/cookie — follow the documented AWS WAF Developer Guide "WAF client application integration" pattern but were **not** machine-verified here. **Confirm against the live WAF console "Application integration" page (it emits the exact integration-URL `<script>` snippet, the api-key value, and the SDK method names for your distribution) before executing Task 1**, and reconcile any naming drift (e.g. header `aws-waf-token` vs `x-aws-waf-token`). The CAPTCHA enforcement itself is at the edge (WAF rule on `POST /api/*` from 2b), so the Lambda contract is unaffected regardless.

---

## Sequencing & risk warnings (read before starting)

This plan's tasks are authored in dependency order, but **execution ordering is load-bearing**:

1. **Task 1 (frontend swap)** and **Task 2 (upload path)** can be authored/merged anytime — they don't go live until deployed.
2. **2b must be applied and verified at the CloudFront domain** (per 2b's verify step) **before Task 3 (DNS retarget)**. The site must already be uploaded to `site_bucket` (Task 5's deploy step, or a manual `s3 sync`) so CloudFront serves real content the instant DNS flips.
3. **Task 3 (DNS flip) is the actual go-live.** After it, verify end-to-end (Self-Review checklist) **before** Task 4.
4. **Task 4 (remove Cloudflare app resources)** and **Task 6 (decommission Worker)** are destructive — do them **only after** prod is confirmed healthy on AWS. Keep `worker/` in-repo for one release as a rollback artifact (per the deploy-path memory).
5. **Rollback at any point before Task 4:** re-flip the two `cloudflare_dns_record`s back to `proxied = true`, `ttl = 1`, content `${project}.pages.dev`, and `tofu apply`. Pages + Worker still exist, so the old site/backend resume serving within a DNS TTL. Keep TTL low (300s) during the cutover window to make rollback fast.

---

## Task 1: Frontend CAPTCHA swap (Turnstile → AWS WAF CAPTCHA JS SDK)

**Files:**
- Modify: `package.json` (remove `@marsidev/react-turnstile`)
- Modify: `src/components/Contact.tsx`
- Modify: `src/components/__tests__/Contact.test.tsx`
- Modify: `vite.config.ts` (drop turnstile from `noSideEffects`)
- Modify: `.env.example`

**Why:** WAF enforces CAPTCHA at the edge on `POST /api/*` (2b). The browser must hold a valid WAF token so the edge challenge passes silently; the SDK injects that token on the same-origin `POST /api/contact` (same origin because both the site and `/api/*` are served by the one CloudFront distribution). The Lambda no longer verifies any CAPTCHA token — **keep the honeypot, time-trap, and the rest of the `/api/contact` payload unchanged**; just drop the `turnstileToken` field.

- [ ] **Step 1: Remove the Turnstile dependency from `package.json`**

Delete this line from `dependencies`:
```json
    "@marsidev/react-turnstile": "^1.4.2",
```

> Run `npm install` after editing to regenerate `package-lock.json` (per the lockfile-regen memory) — do this in Step 6 alongside the verify build.

- [ ] **Step 2: Drop the turnstile entry from `vite.config.ts` `noSideEffects`**

In `vite.config.ts`, the `moduleSideEffects` list currently is:
```ts
          const noSideEffects = [
            /node_modules\/@marsidev\/react-turnstile/,
            /node_modules\/mermaid/,
          ];
```
Change it to remove the now-absent package (the WAF SDK is loaded via a runtime `<script>`, not bundled, so it needs no tree-shaking entry):
```ts
          const noSideEffects = [
            /node_modules\/mermaid/,
          ];
```

- [ ] **Step 3: Rewrite the CAPTCHA portion of `src/components/Contact.tsx`**

The component currently imports the `Turnstile` React component, keeps a `turnstileToken` in state + `turnstileRef`, gates submit on the token, sends `turnstileToken` in the POST body, and renders `<Turnstile … />`. Replace all of that with the WAF integration-script loader.

> **Before writing**, confirm the exact SDK surface from the WAF console "Application integration" page (see the flagged note above). The code below assumes the documented pattern: loading the integration `<script src={VITE_WAF_INTEGRATION_URL}>` defines a global `window.AwsWafIntegration` whose `getToken()` returns a Promise<string> for the current WAF token. We attach the token explicitly as the `x-aws-waf-token` header on our existing `fetch('/api/contact', …)` (equivalently, `AwsWafIntegration.fetch('/api/contact', …)` wraps `fetch` and injects it for you — pick one; the explicit-header form keeps the rest of the existing fetch logic intact). The api key (`VITE_WAF_API_KEY`) is passed to the SDK as required by the console snippet — most integrations embed it in the script URL/snippet; if the console snippet wires it differently, follow the console.

**3a. Remove the import (line 2):**
```diff
-import { Turnstile, type TurnstileInstance } from '@marsidev/react-turnstile';
```

**3b. Replace the token/ref state. Remove these (lines ~64, ~71):**
```diff
-  const [turnstileToken, setTurnstileToken] = useState<string>('');
   ...
-  const turnstileRef = useRef<TurnstileInstance>(null);
```
and add a "SDK ready" flag plus the integration-URL/api-key reads near the top of the component:
```ts
  const [wafReady, setWafReady] = useState(false);
  const wafIntegrationUrl = import.meta.env.VITE_WAF_INTEGRATION_URL as string | undefined;
  const wafApiKey = import.meta.env.VITE_WAF_API_KEY as string | undefined;
```
Also drop `useRef` from the React import on line 1 if it is no longer used elsewhere (it is currently only used for `turnstileRef`):
```diff
-import { useState, useEffect, useRef } from 'react';
+import { useState, useEffect } from 'react';
```

**3c. Add an effect that injects the WAF integration script once** (place alongside the existing effects, after the theme observer effect ~line 85):
```ts
  // Load the AWS WAF CAPTCHA integration script (defines window.AwsWafIntegration).
  useEffect(() => {
    if (!wafIntegrationUrl) return;
    // Avoid double-injection (StrictMode / re-mounts).
    const existing = document.querySelector<HTMLScriptElement>('script[data-aws-waf]');
    if (existing) {
      setWafReady(true);
      return;
    }
    const script = document.createElement('script');
    script.src = wafIntegrationUrl;
    script.defer = true;
    script.dataset.awsWaf = 'true';
    script.onload = () => setWafReady(true);
    script.onerror = () =>
      setSubmissionState({
        status: 'error',
        message: 'Security check failed to load. Please reload the page.',
      });
    document.head.appendChild(script);
  }, [wafIntegrationUrl]);
```

**3d. In the success-reset effect (~lines 103–108), remove the turnstile reset/clear:**
```diff
-        setTurnstileToken('');
         setFormTimestamp(Date.now());
         setSubmissionState({ status: 'idle', message: '' });
-        if (turnstileRef.current) {
-          turnstileRef.current.reset();
-        }
```

**3e. Update the submit gate (line ~172).** WAF tokens are obtained at submit time, so the button no longer waits on a token — it waits on the SDK being ready:
```diff
-  const canSubmit = isFormValid && turnstileToken && submissionState.status !== 'loading';
+  const canSubmit = isFormValid && wafReady && submissionState.status !== 'loading';
```

**3f. Update `handleSubmit` (lines ~182–202).** Remove the `!turnstileToken` guard, fetch a WAF token, attach it as a header, and drop `turnstileToken` from the body:
```diff
-    if (!isFormValid || !turnstileToken) {
+    if (!isFormValid) {
       return;
     }

     setSubmissionState({ status: 'loading', message: '' });

     try {
-      const response = await fetch('/api/contact', {
+      // Obtain a fresh WAF token; the edge WAF rule on POST /api/* validates it.
+      const wafToken: string =
+        typeof window !== 'undefined' && window.AwsWafIntegration
+          ? await window.AwsWafIntegration.getToken()
+          : '';
+      const response = await fetch('/api/contact', {
         method: 'POST',
         headers: {
           'Content-Type': 'application/json',
+          'x-aws-waf-token': wafToken,
         },
         body: JSON.stringify({
           name: formData.name,
           email: formData.email,
           message: formData.message,
-          turnstileToken,
           timestamp: formTimestamp,
           website: formData.website || undefined,
         }),
       });
```

**3g. Remove the two error-path `turnstileRef.current?.reset()` + `setTurnstileToken('')` blocks** (in the JSON-parse-error path ~lines 221–222, the non-OK path ~lines 249–253, and the catch path ~lines 272–273). There is no per-attempt token to reset anymore; the SDK manages token lifecycle.

**3h. Replace the rendered CAPTCHA block (lines ~277, ~393–420).** Remove `const turnstileSiteKey = import.meta.env.VITE_TURNSTILE_SITE_KEY;` and the `<Turnstile … />` JSX. Replace the `{/* Turnstile CAPTCHA */}` block with a minimal status/fallback (WAF CAPTCHA renders its own challenge overlay at the edge when needed, so there is no inline widget to mount):
```tsx
            {/* AWS WAF CAPTCHA — token acquired at submit time; the edge presents a
                challenge overlay only when WAF decides one is needed. */}
            {!wafIntegrationUrl && import.meta.env.PROD ? (
              <div className="p-4 bg-yellow-500/10 border border-yellow-500/50 rounded-lg text-center">
                <p className="text-sm text-yellow-500">Contact form temporarily unavailable. Please reach out via LinkedIn.</p>
              </div>
            ) : null}
```

**3i. Add a module-level type declaration for the global** (top of `Contact.tsx`, after imports), so `window.AwsWafIntegration` typechecks under strict mode:
```ts
declare global {
  interface Window {
    AwsWafIntegration?: {
      getToken: () => Promise<string>;
      fetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
    };
  }
}
```

- [ ] **Step 4: Update `src/components/__tests__/Contact.test.tsx`**

The test file mocks `@marsidev/react-turnstile` and clicks a `turnstile-success` button to set the token, and sets `VITE_TURNSTILE_SITE_KEY` in `beforeEach`. Rework:

**4a. Remove the `vi.mock('@marsidev/react-turnstile', …)` block (lines ~7–35)** and the `mockReset` const — there is no Turnstile component to mock.

**4b. In `beforeEach` (line ~45)**, replace the Turnstile env var and stub the WAF global + script:
```diff
-    import.meta.env.VITE_TURNSTILE_SITE_KEY = 'test-site-key';
+    import.meta.env.VITE_WAF_INTEGRATION_URL = 'https://waf.test/integration.js';
+    import.meta.env.VITE_WAF_API_KEY = 'test-waf-api-key';
+    // Stub the SDK the integration script would define.
+    window.AwsWafIntegration = {
+      getToken: vi.fn(async () => 'mock-waf-token'),
+      fetch: vi.fn(),
+    };
```
Because the component injects the script via `document.createElement('script')` and waits for `onload` to set `wafReady`, the cleanest test seam is to **pre-mark the script as present and force-ready**. Add to `beforeEach`, before each render path needs it:
```ts
    // Pretend the integration script already loaded so `wafReady` is true.
    const s = document.createElement('script');
    s.dataset.awsWaf = 'true';
    document.head.appendChild(s);
```
> Note: the component's effect short-circuits to `setWafReady(true)` when it finds an existing `script[data-aws-waf]`. If a test still observes `wafReady === false` timing, expose readiness deterministically (e.g. allow `VITE_WAF_INTEGRATION_URL` empty to default `wafReady` true in test) — but prefer the existing-script seam first.

**4c. Replace every `screen.getByTestId('turnstile-success')` click** (in "enables submit button…", "submits form successfully", "handles form submission error", "silently rejects…honeypot") — there is no widget to click. Submit becomes enabled once the form is valid and `wafReady` is true, so just remove the verify-button clicks and assert on the now-enabled submit button. Example for "enables submit button…":
```diff
-    // Verify Turnstile
-    const verifyButton = screen.getByTestId('turnstile-success');
-    await user.click(verifyButton);
-
     await waitFor(() => {
       const submitButton = screen.getByRole('button', { name: /send message/i });
       expect(submitButton).not.toBeDisabled();
     }, { timeout: 3000 });
```

**4d. Update the success-submit assertion (lines ~164–169)** to assert the WAF token header instead of the bare content-type object, and confirm `getToken` was called:
```diff
     expect(globalThis.fetch).toHaveBeenCalledWith('/api/contact', expect.objectContaining({
       method: 'POST',
-      headers: {
-        'Content-Type': 'application/json',
-      },
+      headers: expect.objectContaining({
+        'Content-Type': 'application/json',
+        'x-aws-waf-token': 'mock-waf-token',
+      }),
     }));
+    expect(window.AwsWafIntegration!.getToken).toHaveBeenCalled();
```

**4e. The honeypot test** stays meaningful: fill the honeypot, submit, assert `fetch` not called. Just delete its Turnstile click.

- [ ] **Step 5: Update `.env.example`**

Replace the Turnstile frontend block (lines ~8–13) with the WAF block (keep everything PUBLIC-safe — neither value is a secret in the same sense, but the api key is treated as sensitive in CI; see Task 5):
```bash
# -----------------------------------------------------------------------------
# AWS WAF CAPTCHA (Frontend)
# -----------------------------------------------------------------------------
# From the WAF console "Application integration" page for the prod distribution
# (these come from the 2b outputs waf_captcha_integration_url + waf_captcha_api_key).
# The integration URL is public; the API key is scoped to your token domains.
VITE_WAF_INTEGRATION_URL=https://<integration-id>.<region>.captcha-sdk.awswaf.com/<integration-id>/jsapi.js
VITE_WAF_API_KEY=your-waf-captcha-api-key
```
> Leave the Cloudflare API / Worker sections in `.env.example` for now if `worker/` is retained one release (Task 6); they are removed when the Worker is fully decommissioned. (Optional: prune the dead `CLOUDFLARE_TURNSTILE_*` references here too.)

- [ ] **Step 6: Verify (build + tests)**

```bash
npm install            # regenerate package-lock.json after removing the dep
npx tsc --noEmit       # strict-mode typecheck (covers the new window.AwsWafIntegration decl)
npm run build          # tsc -b && vite build — must succeed with no @marsidev import
npm test -- --run      # Contact.test.tsx green with the WAF stubs
```
Expected: build succeeds (no `@marsidev/react-turnstile` resolution), all Contact tests pass, no `VITE_TURNSTILE_SITE_KEY` references remain (`grep -r VITE_TURNSTILE_SITE_KEY src .env.example` returns nothing).

- [ ] **Step 7: Commit**

```bash
git add package.json package-lock.json src/components/Contact.tsx \
        src/components/__tests__/Contact.test.tsx vite.config.ts .env.example
git commit -m "feat(2c): swap Turnstile for AWS WAF CAPTCHA JS SDK in contact form"
```

---

## Task 2: Static-site upload path (s3 sync + CloudFront invalidation)

**Files:** none committed here (this task defines the deploy commands used in Task 5's CI; capture them as a documented, runnable sequence). If a helper script is desired, create `scripts/deploy-frontend.sh`; otherwise inline in CI.

**Why:** The frontend no longer ships via `wrangler pages deploy`. It is uploaded to the env-aware `site_bucket` (2b output `portfolio-contact-${env}-site`) behind CloudFront, then the CDN cache is invalidated so visitors get the new build immediately.

- [ ] **Step 1: Define the upload + invalidation commands**

The canonical sequence (env-aware; `SITE_BUCKET` and `DISTRIBUTION_ID` come from `tofu output` or CI secrets/vars):
```bash
# Build output is in ./dist (from `npm run build`).
# SITE_BUCKET      = 2b output `site_bucket`            (portfolio-contact-${env}-site)
# DISTRIBUTION_ID  = 2b output `cloudfront_distribution_id`
aws s3 sync dist/ "s3://${SITE_BUCKET}" --delete
aws cloudfront create-invalidation \
  --distribution-id "${DISTRIBUTION_ID}" \
  --paths "/*"
```
Notes:
- `--delete` removes stale objects so the bucket mirrors `dist/` exactly (matches Pages "atomic" semantics closely enough; index.html + hashed assets).
- `--paths "/*"` invalidates everything; acceptable for a small site. (Optional optimization: invalidate only `/` and `/index.html` since hashed asset filenames change per build — but `/*` is simplest and the free-tier invalidation allowance covers it.)
- These values are read in CI from `tofu output -raw site_bucket` / `tofu output -raw cloudfront_distribution_id` right after the apply step (so dev vs prod resolve automatically from the active state), or from environment-scoped secrets.

- [ ] **Step 2 (optional): Create `scripts/deploy-frontend.sh`**

If you prefer a single invocable artifact over inline CI YAML:
```bash
#!/usr/bin/env bash
set -euo pipefail
: "${SITE_BUCKET:?set SITE_BUCKET}"
: "${DISTRIBUTION_ID:?set DISTRIBUTION_ID}"
aws s3 sync dist/ "s3://${SITE_BUCKET}" --delete
aws cloudfront create-invalidation --distribution-id "${DISTRIBUTION_ID}" --paths "/*"
```
```bash
chmod +x scripts/deploy-frontend.sh
git add scripts/deploy-frontend.sh
git commit -m "feat(2c): add s3 sync + CloudFront invalidation deploy helper"
```
> If you keep it inline in CI instead (Task 5), skip this commit — Task 5 carries the same commands.

---

## Task 3: DNS retarget (proxied→pages.dev  ⇒  DNS-only→CloudFront)

**Files:**
- Modify: `terraform/main.tf` (the two `cloudflare_dns_record` resources `pages_root` + `pages_www`)

**Why:** This is the **go-live flip**. Cloudflare stays name-service only (grey cloud); traffic for the apex + `www` resolves to the CloudFront distribution, which terminates TLS (ACM cert from 2b) and routes `/api/*` to the ingest Lambda + everything else to the S3 site.

> **Reference the CloudFront domain by the 2b output, not a literal.** Since `main.tf` and the AWS edge live in the **same state**, reference the resource/output directly. Use a local that reads the 2b output value. If 2b exposes it as `output "cloudfront_domain_name"`, reference the underlying resource attribute (e.g. `aws_cloudfront_distribution.site.domain_name`) — confirm the exact resource name 2b used. Below uses a local indirection so this plan stays decoupled from 2b's internal resource name.

- [ ] **Step 1: Add a local for the CloudFront alias target** (top of `main.tf` or in `providers_aws.tf` locals; pick one and keep it consistent). Reference 2b's distribution resource:
```hcl
locals {
  # 2b created the distribution; reference its domain for the DNS CNAME target.
  # Adjust the resource address to match 2b's actual resource name if different.
  cloudfront_target = aws_cloudfront_distribution.site.domain_name
}
```
> If 2b named the resource differently (e.g. `aws_cloudfront_distribution.cdn`), use that address. Do **not** hardcode the `dxxxx.cloudfront.net` value — it changes per distribution.

- [ ] **Step 2: Retarget `cloudflare_dns_record.pages_root`** — replace its body (lines ~45–66) with a DNS-only CNAME to CloudFront:
```hcl
resource "cloudflare_dns_record" "pages_root" {
  zone_id = var.cloudflare_zone_id
  name    = "@"
  type    = "CNAME"
  content = local.cloudfront_target

  # DNS-only (grey cloud): Cloudflare is name-service only; CloudFront serves + terminates TLS.
  proxied = false
  ttl     = 300
}
```

- [ ] **Step 3: Retarget `cloudflare_dns_record.pages_www`** — replace its body (lines ~71–92) likewise:
```hcl
resource "cloudflare_dns_record" "pages_www" {
  zone_id = var.cloudflare_zone_id
  name    = "www"
  type    = "CNAME"
  content = local.cloudfront_target

  proxied = false
  ttl     = 300
}
```

> **Apex CNAME note:** Cloudflare supports CNAME at the zone apex via CNAME flattening even when DNS-only, so `name = "@"` → CloudFront is valid. The ACM cert from 2b must already cover both `site_domain` (apex) and `www`, and the CloudFront distribution must list both as aliases — verify before applying or TLS will fail on the flipped names.

- [ ] **Step 4: Validate**

```bash
cd terraform && tofu validate
```
Expected: valid. (A real `plan` requires the AWS edge from 2b to exist in state; run it during the go-live apply, confirming the only DNS changes are the two record `content`/`proxied`/`ttl` updates — **in-place updates, not destroy/recreate**.)

- [ ] **Step 5: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/main.tf terraform/providers_aws.tf
git commit -m "infra(2c): retarget apex + www DNS from proxied pages.dev to DNS-only CloudFront"
```

---

## Task 4: Remove Cloudflare app resources from terraform

**Files:**
- Modify: `terraform/main.tf` (delete Pages + Worker resources)
- Modify: `terraform/variables.tf` (delete `turnstile_site_key`, `turnstile_secret_key`)
- Modify: `terraform/outputs.tf` (delete `worker_url`, `turnstile_site_key`, and `pages_url`)

> **Do this only after Task 3 is live and the Self-Review end-to-end check passes.** `tofu apply` of these deletions will **destroy** the Cloudflare Pages project, Pages domain, Worker, worker version, worker deployment, and worker route — that is the point, but it removes the rollback target. Confirm AWS is healthy first.

**Keep:** the `cloudflare` provider block, `var.cloudflare_api_token` / `_account_id` / `_zone_id`, the two `cloudflare_dns_record`s (now pointing at CloudFront), and the SES DKIM `cloudflare_dns_record.ses_dkim` records from 2a. Cloudflare remains the DNS authority.

- [ ] **Step 1: Delete the Pages + Worker resources from `main.tf`**

Remove these resource blocks entirely:
- `resource "cloudflare_pages_project" "portfolio"` (lines ~26–30)
- `resource "cloudflare_pages_domain" "portfolio"` (lines ~35–40)
- `resource "cloudflare_worker" "contact_form"` (lines ~105–108)
- `resource "cloudflare_worker_version" "contact_form"` (lines ~111–142)
- `resource "cloudflare_workers_deployment" "contact_form"` (lines ~145–154)
- `resource "cloudflare_workers_route" "contact_form"` (lines ~159–169)

After this, `main.tf` should contain only: the `terraform`/provider blocks and the two `cloudflare_dns_record` resources (`pages_root`, `pages_www`). The `pages_root`/`pages_www` `content` already references `local.cloudfront_target` (Task 3), so the now-deleted `cloudflare_pages_project.portfolio.name` is no longer referenced — confirm no dangling references remain (`grep -n "cloudflare_pages_project\|cloudflare_worker" terraform/`).

> Consider renaming the resources `pages_root`/`pages_www` to `site_root`/`site_www` for clarity. **Skip the rename** to avoid a destroy/recreate of live DNS records (a rename re-keys them in state → delete+create → brief NXDOMAIN). Leave the resource names as-is; only their content changed in Task 3.

- [ ] **Step 2: Delete the now-dead variables from `variables.tf`**

Remove:
```hcl
variable "turnstile_site_key" { ... }   # lines ~17–20
variable "turnstile_secret_key" { ... } # lines ~22–26
```
Keep `var.contact_email` (still used by SES/secrets in 2a) and all `cloudflare_*` + `aws_region` + `domain_name` vars.

- [ ] **Step 3: Delete the now-dead outputs from `outputs.tf`**

Remove:
- `output "worker_url"` (lines ~16–19) — Worker is gone.
- `output "turnstile_site_key"` (lines ~21–25) — Turnstile is gone.
- `output "pages_url"` (lines ~1–4) — Pages project is gone (its reference `cloudflare_pages_project.portfolio.name` no longer exists, so this output would fail to evaluate).

Keep `custom_domain_url` / `www_domain_url` (they reference only `var.domain_name`) and all the 2a backend outputs. **Note:** any 2b-added outputs (`cloudfront_domain_name`, `site_bucket`, `cloudfront_distribution_id`, etc.) stay — CI reads them in Task 5.

- [ ] **Step 4: Validate**

```bash
cd terraform && tofu validate
```
Expected: valid, no references to deleted resources/vars/outputs.

- [ ] **Step 5: Format & commit**

```bash
cd terraform && tofu fmt
git add terraform/main.tf terraform/variables.tf terraform/outputs.tf
git commit -m "infra(2c): remove Cloudflare Pages/Worker resources + turnstile vars/outputs (keep DNS + provider)"
```

> **Apply note:** when this is applied (Task 5 CI on prod), `tofu plan` will show **destroys** of the 6 Cloudflare app resources and **no change** to the DNS records or SES CNAMEs. Read the plan and confirm only those 6 destroys before approving.

---

## Task 5: CI/CD rewrite — AWS OIDC + per-env tofu + s3 deploy

**Files:**
- Modify: `.github/workflows/ci.yml`

**Why:** The deploy job must (a) auth to AWS via GitHub OIDC (no static keys), (b) `tofu init` with the per-env GitLab backend address, (c) `tofu apply` (which now also creates/updates the AWS edge), (d) ship the frontend via `s3 sync` + invalidation instead of `wrangler pages deploy`. Build-job env swaps `CLOUDFLARE_TURNSTILE_SITE_KEY` → `VITE_WAF_*`. The `environment: production` approval gate is kept for prod; a `workflow_dispatch` input drives an on-demand `dev` apply.

> **OIDC pattern (verified via context7, `aws-actions/configure-aws-credentials`):** requires `permissions: id-token: write` on the job and `role-to-assume: <role-arn>` + `aws-region`. The role's trust policy must trust GitHub's OIDC provider scoped to this repo. New secret: **`AWS_DEPLOY_ROLE_ARN`** (the IAM role to assume). One-time AWS setup (outside this plan): create the GitHub OIDC identity provider + the deploy role with a trust policy on `repo:Grimm07/portfolio:*` and a permissions policy covering tofu's AWS actions + `s3:PutObject`/`s3:DeleteObject` on `site_bucket` + `cloudfront:CreateInvalidation`.

- [ ] **Step 1: Add `workflow_dispatch` with an `environment` input** (top of `ci.yml`, in `on:`):
```yaml
on:
  push:
    branches:
      - main
  pull_request:
    branches:
      - main
  workflow_dispatch:
    inputs:
      environment:
        description: "Target environment for a manual apply"
        type: choice
        options:
          - dev
        default: dev
```
> Prod is only ever deployed by `push` to `main` (auto, gated by `environment: production`). `workflow_dispatch` is **dev-only** here — selecting `dev` runs the deploy job against `dev.trystan-tbm.dev` state. (Keeping prod off manual dispatch avoids accidental out-of-band prod applies.)

- [ ] **Step 2: Swap the build-job frontend env** (lines ~64–67):
```diff
       - name: Build frontend
         env:
-          VITE_TURNSTILE_SITE_KEY: ${{ secrets.CLOUDFLARE_TURNSTILE_SITE_KEY }}
+          VITE_WAF_INTEGRATION_URL: ${{ vars.VITE_WAF_INTEGRATION_URL }}
+          VITE_WAF_API_KEY: ${{ secrets.WAF_CAPTCHA_API_KEY }}
         run: npm run build
```
> `VITE_WAF_INTEGRATION_URL` is non-secret (a public URL) → use a repo/environment **variable** (`vars.`). `VITE_WAF_API_KEY` is treated as sensitive → repo/environment **secret** `WAF_CAPTCHA_API_KEY`. Both are embedded statically into the bundle at build time (Vite `VITE_*` semantics — per the memory note), so they must be present in the **build** job, and the build artifact is env-specific. (For a dev build, supply the dev distribution's values via the `dev` environment's vars/secrets.)

- [ ] **Step 3: Decide the worker build's fate in CI.** For the **interim release** that retains `worker/` as rollback (Task 6), leave the worker build/upload steps in place but note they are slated for removal. The straightforward path: **drop the worker build + upload** from the build job and the worker download + verify from deploy (Task 6 covers this). If keeping one release, leave them and remove in Task 6. Pick one and be consistent. This plan's Task 6 assumes worker steps are removed there.

- [ ] **Step 4: Rewrite the `deploy` job.** Replace the whole `deploy:` job (lines ~148–221) with the OIDC + per-env tofu + s3 deploy version:
```yaml
  deploy:
    name: Deploy
    runs-on: ubuntu-latest
    # Prod auto-deploys on push to main; dev deploys on manual dispatch.
    if: >-
      (github.event_name == 'push' && github.ref == 'refs/heads/main') ||
      github.event_name == 'workflow_dispatch'
    needs: [validate, build, terraform]
    # Prod uses the gated 'production' environment; dispatch targets 'dev'.
    environment: ${{ (github.event_name == 'workflow_dispatch' && inputs.environment) || 'production' }}
    permissions:
      contents: read
      id-token: write   # required for AWS OIDC
    env:
      TF_ENV: ${{ (github.event_name == 'workflow_dispatch' && inputs.environment) || 'prod' }}
    steps:
      - name: Checkout code
        uses: actions/checkout@v6

      - name: Download frontend build
        uses: actions/download-artifact@v4
        with:
          name: frontend-dist
          path: dist/

      - name: Configure AWS credentials (OIDC)
        uses: aws-actions/configure-aws-credentials@v6
        with:
          role-to-assume: ${{ secrets.AWS_DEPLOY_ROLE_ARN }}
          aws-region: us-east-1

      - name: Setup OpenTofu
        uses: opentofu/setup-opentofu@v1

      - name: Tofu Init (per-env backend)
        working-directory: terraform
        env:
          TF_HTTP_USERNAME: gitlab-ci-token
          TF_HTTP_PASSWORD: ${{ secrets.GITLAB_ACCESS_TOKEN }}
        run: |
          # prod keeps the existing address (no state migration); dev uses portfolio-dev.
          if [ "$TF_ENV" = "prod" ]; then STATE="portfolio"; else STATE="portfolio-${TF_ENV}"; fi
          tofu init \
            -backend-config="address=${{ secrets.TF_STATE_BASE_URL }}/${STATE}" \
            -backend-config="lock_address=${{ secrets.TF_STATE_BASE_URL }}/${STATE}/lock" \
            -backend-config="unlock_address=${{ secrets.TF_STATE_BASE_URL }}/${STATE}/lock"

      - name: Tofu Apply
        working-directory: terraform
        env:
          TF_HTTP_USERNAME: gitlab-ci-token
          TF_HTTP_PASSWORD: ${{ secrets.GITLAB_ACCESS_TOKEN }}
          TF_VAR_environment: ${{ env.TF_ENV }}
          TF_VAR_cloudflare_api_token: ${{ secrets.CLOUDFLARE_API_TOKEN }}
          TF_VAR_cloudflare_account_id: ${{ secrets.CLOUDFLARE_ACCOUNT_ID }}
          TF_VAR_cloudflare_zone_id: ${{ secrets.CLOUDFLARE_ZONE_ID }}
          TF_VAR_contact_email: ${{ secrets.CONTACT_EMAIL }}
        run: tofu apply -auto-approve

      - name: Read tofu outputs
        id: tf
        working-directory: terraform
        run: |
          echo "site_bucket=$(tofu output -raw site_bucket)" >> "$GITHUB_OUTPUT"
          echo "distribution_id=$(tofu output -raw cloudfront_distribution_id)" >> "$GITHUB_OUTPUT"

      - name: Deploy frontend to S3 + invalidate CloudFront
        env:
          SITE_BUCKET: ${{ steps.tf.outputs.site_bucket }}
          DISTRIBUTION_ID: ${{ steps.tf.outputs.distribution_id }}
        run: |
          aws s3 sync dist/ "s3://${SITE_BUCKET}" --delete
          aws cloudfront create-invalidation --distribution-id "${DISTRIBUTION_ID}" --paths "/*"
```

Key changes vs the old job:
- **Removed:** `wrangler-action`, all `TF_VAR_turnstile_*`, worker artifact download/verify, static AWS keys.
- **Added:** OIDC `configure-aws-credentials`, `permissions.id-token: write`, per-env `tofu init` backend-config (prod keeps `.../state/portfolio`; dev uses `.../state/portfolio-dev` — matches the meta-plan), `TF_VAR_environment`, and the `s3 sync` + invalidation step reading live `tofu output`s.
- **Kept:** `environment` gate (prod → `production` approval; dispatch → `dev`), `GITLAB_ACCESS_TOKEN` for the HTTP backend.

> The `terraform` **validate** job (lines ~100–143) can stay on `terraform`/`hashicorp/setup-terraform` for fmt/validate/tflint, but consider switching it to `opentofu/setup-opentofu` + `tofu` for parity. Not required for cutover; flagged. Drop its worker artifact download once `worker/` leaves CI (Task 6).

- [ ] **Step 5: Verify (lint the workflow + dry-read)**

```bash
# YAML sanity (no apply here):
python -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml')); print('yaml ok')"
# Confirm no Turnstile / wrangler residue in the deploy path:
grep -n "turnstile\|wrangler\|pages deploy" .github/workflows/ci.yml || echo "clean"
```
Expected: YAML parses; no `turnstile`/`wrangler`/`pages deploy` references remain.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci(2c): AWS OIDC auth + per-env tofu apply + s3 sync/invalidation; drop wrangler + turnstile"
```

**New CI secrets/vars required (document in the PR description):**
| Name | Kind | Purpose |
|---|---|---|
| `AWS_DEPLOY_ROLE_ARN` | secret | IAM role assumed via OIDC |
| `TF_STATE_BASE_URL` | secret | GitLab HTTP state base (e.g. `https://gitlab.example.com/api/v4/projects/<id>/terraform/state`) |
| `WAF_CAPTCHA_API_KEY` | secret (build + per-env) | `VITE_WAF_API_KEY` at build time (= 2b `waf_captcha_api_key`) |
| `VITE_WAF_INTEGRATION_URL` | variable (build + per-env) | `VITE_WAF_INTEGRATION_URL` at build time (= 2b `waf_captcha_integration_url`) |
| `GITLAB_ACCESS_TOKEN` | secret (existing) | HTTP backend auth |
| `CLOUDFLARE_API_TOKEN` / `_ACCOUNT_ID` / `_ZONE_ID` | secret (existing) | DNS + provider auth |
| `CONTACT_EMAIL` | secret (existing) | SES recipient |
**Retired secrets:** `CLOUDFLARE_TURNSTILE_SITE_KEY`, `CLOUDFLARE_TURNSTILE_SECRET_KEY`.

---

## Task 6: Decommission the Worker

**Files:**
- Modify: `.github/workflows/ci.yml` (drop worker build/upload from build job, if not already done in Task 5 Step 3)
- (No code deletion of `worker/` yet — retained one release as rollback.)

> **Only after prod is verified live on AWS (Self-Review passes).** Task 4's `tofu apply` already removed the `cloudflare_worker*`/`cloudflare_workers_route` resources, so the Worker is no longer routed. This task cleans up CI and (optionally) the Worker deployment itself.

- [ ] **Step 1: Remove the worker build steps from the `build` job** (lines ~69–80 and the worker upload lines ~90–95):
```diff
-      # Build worker
-      - name: Install worker dependencies
-        working-directory: worker
-        run: npm ci
-      - name: Worker type check
-        working-directory: worker
-        run: npm run typecheck
-      - name: Build worker
-        working-directory: worker
-        run: npm run build
       ...
-      - name: Upload worker build
-        uses: actions/upload-artifact@v4
-        with:
-          name: worker-dist
-          path: worker/dist/
-          retention-days: 1
```
And drop the worker artifact download from the `terraform` validate job (lines ~108–113) — terraform no longer references `worker/dist/index.js` after Task 4.

- [ ] **Step 2: Retire the Worker deployment.** Since Task 4's apply already deleted the Cloudflare Worker resource and its route, no separate `wrangler delete` is strictly required — the route is gone (requests to `/api/*` now hit CloudFront → Lambda). If the Worker script object lingers in the Cloudflare account (e.g. it was created outside this state), remove it explicitly:
```bash
cd worker && npx wrangler delete --name portfolio-contact-worker
```
> Confirm via the Cloudflare dashboard that `portfolio-contact-worker` and its `trystan-tbm.dev/api/*` route are gone.

- [ ] **Step 3: Retain `worker/` in-repo for one release.** Do **not** `git rm worker/` yet (per the deploy-path memory: the Worker was the live deploy path; keep it as a rollback artifact until the next release confirms AWS-only is stable). Add a note to `worker/wrangler.toml` or the PR that `worker/` is deprecated and slated for removal next release.

- [ ] **Step 4: Verify CI is worker-free**

```bash
grep -n "worker" .github/workflows/ci.yml || echo "no worker refs in CI"
python -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); print('yaml ok')"
```
Expected: no `worker` references in the workflow; YAML parses.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/ci.yml worker/wrangler.toml
git commit -m "ci(2c): drop worker from CI build; retire Cloudflare Worker (worker/ kept one release as rollback)"
```

---

## Rollback (keep visible during the cutover window)

**Before Task 4 is applied** (Pages + Worker still exist):
1. Revert the two `cloudflare_dns_record`s to the pre-cutover state — `proxied = true`, `ttl = 1`, `content = "${cloudflare_pages_project.portfolio.name}.pages.dev"` (or the literal `trystan-portfolio.pages.dev` if the project resource is mid-removal).
2. `cd terraform && tofu apply` (with the Cloudflare provider vars). DNS reverts within the 300s TTL set during cutover; the old Pages site + Worker `/api/*` resume serving.
3. The frontend build still posts to `/api/contact` — under the reverted proxied path that hits the **Worker** again. If Task 1 (Turnstile removal) already shipped to prod, the Worker's server-side Turnstile verification will reject submissions (no token). Mitigation: keep the **previous frontend build** (with Turnstile) deployable, or accept that rollback restores the Worker but contact submissions need the prior frontend artifact. For a fast rollback that preserves contact, re-deploy the last pre-2c Pages build via `wrangler pages deploy`.

**After Task 4 is applied** (Cloudflare app resources destroyed): rollback is no longer a single DNS flip — you'd have to re-create the Pages project + Worker. **Therefore: do not apply Task 4 until prod is confirmed healthy on AWS for a sustained window.**

---

## Dev vs prod notes

- **Prod** (`TF_ENV=prod`, default): the real cutover. Apex + `www` flip to the prod CloudFront. Cloudflare Pages/Worker removed (Task 4). Deployed by `push` to `main`, gated by `environment: production`.
- **Dev** (`TF_ENV=dev`, manual `workflow_dispatch`): targets `dev.trystan-tbm.dev` (= `local.site_domain` for dev). **No Pages/Worker exist in dev** — Task 3's DNS records and Task 4's removals are prod-only concerns; dev only ever had the AWS stack. The dev build uses the **dev** distribution's `VITE_WAF_*` values (dev environment vars/secrets). Dev state is `.../state/portfolio-dev`.
- The Cloudflare DKIM CNAMEs (2a) and the apex/`www` records live in the **prod** Cloudflare zone regardless of env; dev does not duplicate Cloudflare resources (meta-plan decision).

---

## Self-Review

**Spec coverage (against meta-plan Plan 2c, tasks 1–6):**
- Frontend CAPTCHA swap: remove `@marsidev/react-turnstile`, load WAF integration script from `VITE_WAF_INTEGRATION_URL`, attach token on `POST /api/contact`, replace `VITE_TURNSTILE_SITE_KEY` with `VITE_WAF_INTEGRATION_URL` + `VITE_WAF_API_KEY`, update test + `.env.example` + `vite.config.ts`; honeypot/time-trap/payload otherwise unchanged; Lambda no longer verifies CAPTCHA → Task 1 ✓
- Static-site upload path (`s3 sync --delete` + `create-invalidation`), env-aware bucket via 2b `site_bucket` → Task 2 ✓
- DNS retarget to DNS-only (`proxied=false`, `ttl=300`) CNAME→`cloudfront_domain_name` (via local referencing 2b distribution) → Task 3 ✓
- Remove Cloudflare Pages/Worker resources + dead `turnstile_*` vars + `worker_url`/`turnstile_site_key`(+`pages_url`) outputs; keep provider + zone + DNS + SES CNAMEs → Task 4 ✓
- CI: AWS OIDC (`id-token: write`, `configure-aws-credentials` `role-to-assume`), per-env `tofu init` backend-config (prod→`portfolio`, dev→`portfolio-dev`), `tofu apply` (now creates AWS edge), `s3 sync`+invalidation replacing `wrangler pages deploy`, drop `CLOUDFLARE_TURNSTILE_*`, add `VITE_WAF_*`, keep `environment: production` gate, add `workflow_dispatch` dev input → Task 5 ✓
- Decommission: Worker removed via Task 4 apply (or `wrangler delete`), worker dropped from CI build, `worker/` retained one release as rollback → Task 6 ✓
- Prominent **Rollback** (re-flip DNS to proxied→pages.dev) + sequencing warnings + dev/prod notes → present ✓

**Interface-contract consumption (by exact name):** `cloudfront_domain_name`, `cloudfront_distribution_id`, `site_bucket`, `waf_captcha_integration_url`, `waf_captcha_api_key` — all referenced ✓. Honors prod-default `var.environment` + per-env state ✓.

**End-to-end verification (run after Task 3 go-live, before Task 4):**
1. `dig +short trystan-tbm.dev` and `dig +short www.trystan-tbm.dev` → CloudFront domain (CNAME-flattened apex), **no** orange-cloud Cloudflare IPs.
2. Load `https://trystan-tbm.dev` → site served by CloudFront (check `via:`/`x-cache:` response headers; valid ACM TLS for apex + `www`).
3. Submit the contact form with valid data → the WAF CAPTCHA challenge appears when WAF requires it; on success the `POST /api/contact` carries `x-aws-waf-token` and returns 200.
4. Trace the pipeline: object under `s3://portfolio-contact-prod-messages/messages/`, item in `portfolio-contact-prod-contacts` (DynamoDB), message through the `…-prod-notifications` SQS queue, and a **digest email** delivered to `CONTACT_EMAIL` via SES within the batch window (~5 min).
5. Confirm the old Worker route is gone: `curl -i https://trystan-tbm.dev/api/contact` is served by CloudFront/Lambda (not the Worker); the Cloudflare dashboard shows no `…/api/*` worker route.
6. Negative: submit with the honeypot `website` filled → `fetch` not called (client-side), and a malformed payload → Lambda still rejects (time-trap/validation unchanged).

**Ordering guardrails embedded:** 2b applied+verified → upload site → Task 3 flip → verify → Task 4 destroy → Task 6 decommission. Rollback is a single DNS flip **only before Task 4**.

**context7-flagged (unverified) assumptions:**
- The browser SDK surface (`window.AwsWafIntegration.getToken()` / `.fetch()`) and the exact injected header name (`x-aws-waf-token` vs `aws-waf-token`) were **not** machine-verified — confirm from the WAF console "Application integration" snippet for the prod distribution before executing Task 1. The api-key model and its role in the JS integration **were** confirmed via context7 (WAFV2 `CreateAPIKeyCommand`/`GetDecryptedAPIKeyCommand`).
- The AWS OIDC pattern (`permissions: id-token: write` + `aws-actions/configure-aws-credentials@v6` `role-to-assume`) **was** verified via context7.
- 2b's exact CloudFront distribution **resource name** (`aws_cloudfront_distribution.site` vs other) is assumed — reconcile with 2b before Task 3 Step 1. Prefer referencing a 2b **output** if one exposes the domain directly.

**Placeholder scan:** the only intentional placeholders are the `<integration-id>` / `<region>` in `.env.example` (illustrative, replaced from 2b outputs) and the IAM role ARN (a CI secret). No secrets, emails, or phone numbers are committed (honors the project's privacy rules).
