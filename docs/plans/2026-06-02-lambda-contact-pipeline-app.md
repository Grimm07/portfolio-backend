# Lambda Contact Pipeline — Application Code Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the testable application code (two AWS Lambda handlers + a shared library) for the contact-ingestion pipeline, with all AWS interactions mocked so it is fully unit/integration-tested before any infrastructure exists.

**Architecture:** An **Ingest** handler (CloudFront `/api/*` → Function URL) validates a submission, enforces a DynamoDB rate limit, writes the message body to S3 and a per-person record to DynamoDB, then enqueues an SQS message and returns 200. A **Notifier** handler consumes SQS in batches and sends one aggregated SES digest email. All AWS access goes through thin, individually-tested modules; AWS SDK v3 clients are injected so tests mock them with `aws-sdk-client-mock`.

**Tech Stack:** TypeScript, AWS SDK v3 (`@aws-sdk/client-s3`, `client-dynamodb` + `lib-dynamodb`, `client-sqs`, `client-ses`, `client-secrets-manager`), esbuild (bundling, matching the Worker's toolchain), Vitest + `aws-sdk-client-mock` (testing).

**Scope:** Application code only. OpenTofu infrastructure, IAM, CloudFront/WAF wiring, CI/CD, and cutover are a separate plan (`2026-06-02-lambda-contact-pipeline-infra.md`, to be written). This plan references the design spec `docs/superpowers/specs/2026-06-02-lambda-contact-pipeline-design.md`.

---

## File Structure

```
backend/
  package.json                 # standalone package: deps, scripts (test, build, typecheck)
  tsconfig.json                # strict TS, matches repo conventions
  vitest.config.ts             # node environment
  src/
    shared/
      types.ts                 # ContactSubmission, ContactRecord, env interfaces
      validation.ts            # honeypot, time-trap, email, sanitize (ports + fixes worker logic)
      secrets.ts               # getSecret() with per-container cache (used by the Notifier for CONTACT_EMAIL)
    ingest/
      ip.ts                    # extractClientIp() from XFF / CloudFront-Viewer-Address
      rateLimit.ts             # checkRateLimit() via DynamoDB conditional update
      # (no turnstile.ts — CAPTCHA is enforced by AWS WAF at the edge, not in the Lambda)
      store.ts                 # putMessageBody() (S3), putContactRecord() (DynamoDB)
      enqueue.ts               # enqueueNotification() (SQS)
      handler.ts               # Function URL handler wiring all of the above
    notifier/
      digest.ts                # composeDigest() — N records -> one email body
      email.ts                 # sendDigestEmail() via SES
      handler.ts               # SQS batch handler with ReportBatchItemFailures
  test/
    shared/validation.test.ts
    shared/secrets.test.ts
    ingest/ip.test.ts
    ingest/turnstile.test.ts
    ingest/rateLimit.test.ts
    ingest/store.test.ts
    ingest/enqueue.test.ts
    ingest/handler.test.ts
    notifier/digest.test.ts
    notifier/email.test.ts
    notifier/handler.test.ts
```

**Design rule:** each AWS interaction lives in its own module and receives its SDK client as an argument (dependency injection). Handlers construct the real clients once at module scope (warm-container reuse) and pass them in. This keeps every unit independently testable and keeps the handlers thin.

---

## Task 0: Scaffold the backend package

**Files:**
- Create: `backend/package.json`
- Create: `backend/tsconfig.json`
- Create: `backend/vitest.config.ts`

- [ ] **Step 1: Create `backend/package.json`**

```json
{
  "name": "portfolio-contact-backend",
  "version": "0.0.1",
  "private": true,
  "type": "module",
  "scripts": {
    "test": "vitest run",
    "test:watch": "vitest",
    "typecheck": "tsc --noEmit",
    "build:ingest": "esbuild src/ingest/handler.ts --bundle --platform=node --target=node20 --format=esm --outfile=dist/ingest/index.mjs --banner:js=\"import{createRequire}from'module';const require=createRequire(import.meta.url);\"",
    "build:notifier": "esbuild src/notifier/handler.ts --bundle --platform=node --target=node20 --format=esm --outfile=dist/notifier/index.mjs --banner:js=\"import{createRequire}from'module';const require=createRequire(import.meta.url);\"",
    "build": "npm run build:ingest && npm run build:notifier"
  },
  "dependencies": {
    "@aws-sdk/client-dynamodb": "^3.700.0",
    "@aws-sdk/lib-dynamodb": "^3.700.0",
    "@aws-sdk/client-s3": "^3.700.0",
    "@aws-sdk/client-sqs": "^3.700.0",
    "@aws-sdk/client-ses": "^3.700.0",
    "@aws-sdk/client-secrets-manager": "^3.700.0"
  },
  "devDependencies": {
    "@types/aws-lambda": "^8.10.145",
    "@types/node": "^22.0.0",
    "aws-sdk-client-mock": "^4.1.0",
    "esbuild": "^0.27.2",
    "typescript": "~5.9.3",
    "vitest": "^4.0.18"
  }
}
```

> Note: the AWS SDK v3 packages are available in the Lambda Node 20 runtime, but bundling them guarantees version pinning; that's a build-plan concern handled in the infra plan. The `createRequire` banner avoids ESM/CJS interop errors from bundled SDK internals.

- [ ] **Step 2: Create `backend/tsconfig.json`**

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "bundler",
    "lib": ["ES2022"],
    "strict": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "esModuleInterop": true,
    "skipLibCheck": true,
    "types": ["node"],
    "outDir": "dist"
  },
  "include": ["src", "test"]
}
```

- [ ] **Step 3: Create `backend/vitest.config.ts`**

```ts
import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    environment: 'node',
    include: ['test/**/*.test.ts'],
  },
});
```

- [ ] **Step 4: Install dependencies**

Run: `cd backend && npm install`
Expected: `node_modules` created, no errors.

- [ ] **Step 5: Commit**

```bash
git add backend/package.json backend/tsconfig.json backend/vitest.config.ts backend/package-lock.json
git commit -m "scaffold contact backend package (TS, vitest, aws-sdk v3)"
```

---

## Task 1: Shared types

**Files:**
- Create: `backend/src/shared/types.ts`

- [ ] **Step 1: Create `backend/src/shared/types.ts`**

```ts
/** Raw JSON body posted by the contact form. */
export interface ContactSubmission {
  name: string;
  email: string;
  message: string;
  website: string;        // honeypot — must be empty
  formTimestamp: number;  // ms epoch when the form was rendered
  // (no turnstileToken — AWS WAF CAPTCHA validates aws-waf-token at the edge, not in this body)
}

/** Message persisted to S3 (the full body). */
export interface StoredMessage {
  id: string;
  name: string;
  email: string;
  message: string;
  createdAt: string;      // ISO-8601
  ip: string;             // raw client IP (per spec §7)
  userAgent: string;
}

/** Pointer + metadata persisted to DynamoDB (no message body). */
export interface ContactRecord {
  pk: string;             // EMAIL#<lowercased email>
  sk: string;             // SUB#<ISO timestamp>#<id>
  gsi1pk: string;         // "ALL"
  gsi1sk: string;         // createdAt
  id: string;
  name: string;
  email: string;
  ip: string;
  userAgent: string;
  createdAt: string;
  s3Bucket: string;
  s3Key: string;
  status: 'new';
}

/** Minimal message placed on SQS for the notifier. */
export interface NotificationMessage {
  id: string;
  email: string;
  s3Bucket: string;
  s3Key: string;
}
```

- [ ] **Step 2: Typecheck**

Run: `cd backend && npm run typecheck`
Expected: PASS (no errors).

- [ ] **Step 3: Commit**

```bash
git add backend/src/shared/types.ts
git commit -m "add shared contact pipeline types"
```

---

## Task 2: Validation (port + fix Worker logic)

Ports honeypot / time-trap / email checks from `worker/src/index.ts` and folds in the bug-review fixes (strip ALL control chars from name, hard length caps).

**Files:**
- Create: `backend/src/shared/validation.ts`
- Test: `backend/test/shared/validation.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect } from 'vitest';
import {
  isHoneypotTripped,
  isTooFast,
  isValidEmail,
  sanitizeName,
  MIN_FORM_TIME_MS,
} from '../../src/shared/validation';

describe('isHoneypotTripped', () => {
  it('passes when website is empty', () => {
    expect(isHoneypotTripped({ website: '' })).toBe(false);
    expect(isHoneypotTripped({ website: '   ' })).toBe(false);
  });
  it('trips when website is filled', () => {
    expect(isHoneypotTripped({ website: 'http://spam' })).toBe(true);
  });
});

describe('isTooFast', () => {
  it('rejects submissions faster than the minimum', () => {
    const now = 1_000_000;
    expect(isTooFast(now - (MIN_FORM_TIME_MS - 1), now)).toBe(true);
  });
  it('allows submissions at or past the minimum', () => {
    const now = 1_000_000;
    expect(isTooFast(now - MIN_FORM_TIME_MS, now)).toBe(false);
  });
});

describe('isValidEmail', () => {
  it('accepts a normal address', () => {
    expect(isValidEmail('a@b.co')).toBe(true);
  });
  it('rejects malformed or oversized addresses', () => {
    expect(isValidEmail('nope')).toBe(false);
    expect(isValidEmail('a@b')).toBe(false);
    expect(isValidEmail('x'.repeat(255) + '@b.co')).toBe(false);
  });
});

describe('sanitizeName', () => {
  it('strips CR, LF, tabs and control chars (header-injection fix)', () => {
    expect(sanitizeName('Alice\nBcc: evil@x.com')).toBe('AliceBcc: evil@x.com');
    expect(sanitizeName('A\r\nB\tC')).toBe('ABC');
  });
  it('caps length at 200 chars', () => {
    expect(sanitizeName('a'.repeat(500)).length).toBe(200);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/shared/validation.test.ts`
Expected: FAIL — cannot find module `../../src/shared/validation`.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/shared/validation.ts
export const MIN_FORM_TIME_MS = 3000;
export const MAX_NAME = 200;
export const MAX_MESSAGE = 5000;
export const MAX_EMAIL = 254;

export function isHoneypotTripped(s: { website?: string }): boolean {
  return !!s.website && s.website.trim().length > 0;
}

export function isTooFast(formTimestamp: number, now: number): boolean {
  return now - formTimestamp < MIN_FORM_TIME_MS;
}

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
export function isValidEmail(email: string): boolean {
  return typeof email === 'string' && email.length <= MAX_EMAIL && EMAIL_RE.test(email);
}

/** Strip all control characters and cap length (fixes Worker header-injection bug). */
export function sanitizeName(name: string): string {
  // eslint-disable-next-line no-control-regex
  return name.replace(/[\r\n\t\x00-\x1f\x7f]/g, '').slice(0, MAX_NAME);
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/shared/validation.test.ts`
Expected: PASS (all cases green).

- [ ] **Step 5: Commit**

```bash
git add backend/src/shared/validation.ts backend/test/shared/validation.test.ts
git commit -m "add contact validation (port worker logic + header-injection fix)"
```

---

## Task 3: Secrets cache

Fetches a secret string from Secrets Manager once per container and caches it. **Consumer: the Notifier Lambda, for `CONTACT_EMAIL`** (the Ingest Lambda holds no secrets after the CAPTCHA move). The module itself is a generic `getSecret` and is unchanged by that move.

**Files:**
- Create: `backend/src/shared/secrets.ts`
- Test: `backend/test/shared/secrets.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect, beforeEach } from 'vitest';
import { mockClient } from 'aws-sdk-client-mock';
import { SecretsManagerClient, GetSecretValueCommand } from '@aws-sdk/client-secrets-manager';
import { getSecret, __clearSecretCache } from '../../src/shared/secrets';

const smMock = mockClient(SecretsManagerClient);

describe('getSecret', () => {
  beforeEach(() => {
    smMock.reset();
    __clearSecretCache();
  });

  it('fetches the secret value', async () => {
    smMock.on(GetSecretValueCommand).resolves({ SecretString: 's3cr3t' });
    const client = new SecretsManagerClient({});
    expect(await getSecret(client, 'arn:turnstile')).toBe('s3cr3t');
  });

  it('caches by ARN — second call does not hit the API', async () => {
    smMock.on(GetSecretValueCommand).resolves({ SecretString: 's3cr3t' });
    const client = new SecretsManagerClient({});
    await getSecret(client, 'arn:turnstile');
    await getSecret(client, 'arn:turnstile');
    expect(smMock.commandCalls(GetSecretValueCommand)).toHaveLength(1);
  });

  it('throws when the secret has no string value', async () => {
    smMock.on(GetSecretValueCommand).resolves({});
    const client = new SecretsManagerClient({});
    await expect(getSecret(client, 'arn:empty')).rejects.toThrow(/no SecretString/);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/shared/secrets.test.ts`
Expected: FAIL — cannot find module `../../src/shared/secrets`.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/shared/secrets.ts
import { SecretsManagerClient, GetSecretValueCommand } from '@aws-sdk/client-secrets-manager';

const cache = new Map<string, string>();

export function __clearSecretCache(): void {
  cache.clear();
}

export async function getSecret(client: SecretsManagerClient, secretId: string): Promise<string> {
  const cached = cache.get(secretId);
  if (cached !== undefined) return cached;
  const res = await client.send(new GetSecretValueCommand({ SecretId: secretId }));
  if (!res.SecretString) throw new Error(`Secret ${secretId} has no SecretString`);
  cache.set(secretId, res.SecretString);
  return res.SecretString;
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/shared/secrets.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/src/shared/secrets.ts backend/test/shared/secrets.test.ts
git commit -m "add per-container Secrets Manager cache"
```

---

## Task 4: Client IP extraction

**Files:**
- Create: `backend/src/ingest/ip.ts`
- Test: `backend/test/ingest/ip.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect } from 'vitest';
import { extractClientIp } from '../../src/ingest/ip';

describe('extractClientIp', () => {
  it('prefers CloudFront-Viewer-Address, stripping the port (IPv4)', () => {
    expect(extractClientIp({ 'cloudfront-viewer-address': '198.51.100.10:46532' }))
      .toBe('198.51.100.10');
  });
  it('handles IPv6 CloudFront-Viewer-Address (port after last colon)', () => {
    expect(extractClientIp({ 'cloudfront-viewer-address': '2001:db8::1:46532' }))
      .toBe('2001:db8::1');
  });
  it('falls back to the first X-Forwarded-For entry', () => {
    expect(extractClientIp({ 'x-forwarded-for': '203.0.113.7, 70.41.3.18, 150.172.238.178' }))
      .toBe('203.0.113.7');
  });
  it('returns "unknown" when no IP headers present', () => {
    expect(extractClientIp({})).toBe('unknown');
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/ingest/ip.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/ingest/ip.ts
type Headers = Record<string, string | undefined>;

/** CloudFront-Viewer-Address is "<ip>:<port>"; the port is after the LAST colon. */
function stripPort(value: string): string {
  const i = value.lastIndexOf(':');
  return i === -1 ? value : value.slice(0, i);
}

export function extractClientIp(headers: Headers): string {
  const viewer = headers['cloudfront-viewer-address'];
  if (viewer) return stripPort(viewer.trim());

  const xff = headers['x-forwarded-for'];
  if (xff) return xff.split(',')[0].trim();

  return 'unknown';
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/ingest/ip.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/src/ingest/ip.ts backend/test/ingest/ip.test.ts
git commit -m "add client IP extraction (CloudFront viewer addr / XFF)"
```

---

## Task 5: ~~Turnstile verification~~ — REMOVED

**Removed by the "use AWS CAPTCHA" decision.** CAPTCHA is no longer verified in the Lambda; AWS WAF enforces it at the edge on the `/api/*` POST rule (the frontend obtains an `aws-waf-token` via the WAF CAPTCHA JS SDK; WAF validates it before the request reaches the Lambda). There is no `turnstile.ts` and no in-Lambda token verification. WAF CAPTCHA configuration lives in the infrastructure plan; the frontend widget swap is tracked separately. Skip directly to Task 6.

---

## Task 6: DynamoDB rate limit

Conditional counter: at most 3 per IP per rolling hour; the item self-expires via DynamoDB TTL.

**Files:**
- Create: `backend/src/ingest/rateLimit.ts`
- Test: `backend/test/ingest/rateLimit.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect, beforeEach } from 'vitest';
import { mockClient } from 'aws-sdk-client-mock';
import { DynamoDBDocumentClient, UpdateCommand } from '@aws-sdk/lib-dynamodb';
import { checkRateLimit, RATE_LIMIT_MAX } from '../../src/ingest/rateLimit';

const ddbMock = mockClient(DynamoDBDocumentClient);

describe('checkRateLimit', () => {
  beforeEach(() => ddbMock.reset());

  it('allows when the conditional update succeeds', async () => {
    ddbMock.on(UpdateCommand).resolves({});
    const doc = DynamoDBDocumentClient.from({} as never);
    expect(await checkRateLimit(doc, 'rate-limits', '1.2.3.4', 1000)).toBe(true);
  });

  it('denies when the condition fails (limit reached)', async () => {
    const err = new Error('limit');
    err.name = 'ConditionalCheckFailedException';
    ddbMock.on(UpdateCommand).rejects(err);
    const doc = DynamoDBDocumentClient.from({} as never);
    expect(await checkRateLimit(doc, 'rate-limits', '1.2.3.4', 1000)).toBe(false);
  });

  it('re-throws unexpected errors', async () => {
    ddbMock.on(UpdateCommand).rejects(new Error('boom'));
    const doc = DynamoDBDocumentClient.from({} as never);
    await expect(checkRateLimit(doc, 'rate-limits', '1.2.3.4', 1000)).rejects.toThrow('boom');
  });

  it('exposes the limit as 3', () => {
    expect(RATE_LIMIT_MAX).toBe(3);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/ingest/rateLimit.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/ingest/rateLimit.ts
import { DynamoDBDocumentClient, UpdateCommand } from '@aws-sdk/lib-dynamodb';

export const RATE_LIMIT_MAX = 3;
export const RATE_LIMIT_WINDOW_S = 3600;

/**
 * Returns true if the request is allowed. Atomically increments a per-IP counter
 * with a condition that blocks the (MAX+1)th request inside the window. The item
 * carries a TTL so DynamoDB evicts it after the window, resetting the counter.
 */
export async function checkRateLimit(
  doc: DynamoDBDocumentClient,
  table: string,
  ip: string,
  nowSec: number,
): Promise<boolean> {
  try {
    await doc.send(
      new UpdateCommand({
        TableName: table,
        Key: { pk: `IP#${ip}` },
        UpdateExpression:
          'SET #c = if_not_exists(#c, :zero) + :one, expiresAt = if_not_exists(expiresAt, :exp)',
        ConditionExpression: 'attribute_not_exists(#c) OR #c < :max',
        ExpressionAttributeNames: { '#c': 'count' },
        ExpressionAttributeValues: {
          ':zero': 0,
          ':one': 1,
          ':max': RATE_LIMIT_MAX,
          ':exp': nowSec + RATE_LIMIT_WINDOW_S,
        },
      }),
    );
    return true;
  } catch (e) {
    if (e instanceof Error && e.name === 'ConditionalCheckFailedException') return false;
    throw e;
  }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/ingest/rateLimit.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/src/ingest/rateLimit.ts backend/test/ingest/rateLimit.test.ts
git commit -m "add DynamoDB conditional rate limiter (3/hr/IP, TTL reset)"
```

---

## Task 7: Persistence (S3 + DynamoDB)

**Files:**
- Create: `backend/src/ingest/store.ts`
- Test: `backend/test/ingest/store.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect, beforeEach } from 'vitest';
import { mockClient } from 'aws-sdk-client-mock';
import { S3Client, PutObjectCommand } from '@aws-sdk/client-s3';
import { DynamoDBDocumentClient, PutCommand } from '@aws-sdk/lib-dynamodb';
import { putMessageBody, putContactRecord, messageKey } from '../../src/ingest/store';
import type { StoredMessage, ContactRecord } from '../../src/shared/types';

const s3Mock = mockClient(S3Client);
const ddbMock = mockClient(DynamoDBDocumentClient);

const msg: StoredMessage = {
  id: 'abc', name: 'Alice', email: 'a@b.co', message: 'hi',
  createdAt: '2026-06-02T12:00:00.000Z', ip: '1.2.3.4', userAgent: 'UA',
};

describe('messageKey', () => {
  it('partitions by year/month from createdAt', () => {
    expect(messageKey(msg)).toBe('messages/2026/06/abc.json');
  });
});

describe('putMessageBody', () => {
  beforeEach(() => s3Mock.reset());
  it('writes the JSON body to the computed key', async () => {
    s3Mock.on(PutObjectCommand).resolves({});
    const s3 = new S3Client({});
    const key = await putMessageBody(s3, 'bucket', msg);
    expect(key).toBe('messages/2026/06/abc.json');
    const call = s3Mock.commandCalls(PutObjectCommand)[0].args[0].input;
    expect(call.Bucket).toBe('bucket');
    expect(call.Key).toBe('messages/2026/06/abc.json');
    expect(JSON.parse(call.Body as string)).toMatchObject({ id: 'abc', email: 'a@b.co' });
  });
});

describe('putContactRecord', () => {
  beforeEach(() => ddbMock.reset());
  it('writes the per-person record', async () => {
    ddbMock.on(PutCommand).resolves({});
    const doc = DynamoDBDocumentClient.from({} as never);
    const rec: ContactRecord = {
      pk: 'EMAIL#a@b.co', sk: 'SUB#2026-06-02T12:00:00.000Z#abc',
      gsi1pk: 'ALL', gsi1sk: '2026-06-02T12:00:00.000Z',
      id: 'abc', name: 'Alice', email: 'a@b.co', ip: '1.2.3.4', userAgent: 'UA',
      createdAt: '2026-06-02T12:00:00.000Z', s3Bucket: 'bucket',
      s3Key: 'messages/2026/06/abc.json', status: 'new',
    };
    await putContactRecord(doc, 'contacts', rec);
    const call = ddbMock.commandCalls(PutCommand)[0].args[0].input;
    expect(call.TableName).toBe('contacts');
    expect((call.Item as ContactRecord).pk).toBe('EMAIL#a@b.co');
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/ingest/store.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/ingest/store.ts
import { S3Client, PutObjectCommand } from '@aws-sdk/client-s3';
import { DynamoDBDocumentClient, PutCommand } from '@aws-sdk/lib-dynamodb';
import type { StoredMessage, ContactRecord } from '../shared/types';

/** messages/{yyyy}/{mm}/{id}.json from the message's createdAt. */
export function messageKey(msg: StoredMessage): string {
  const d = new Date(msg.createdAt);
  const yyyy = d.getUTCFullYear();
  const mm = String(d.getUTCMonth() + 1).padStart(2, '0');
  return `messages/${yyyy}/${mm}/${msg.id}.json`;
}

export async function putMessageBody(
  s3: S3Client,
  bucket: string,
  msg: StoredMessage,
): Promise<string> {
  const key = messageKey(msg);
  await s3.send(
    new PutObjectCommand({
      Bucket: bucket,
      Key: key,
      Body: JSON.stringify(msg),
      ContentType: 'application/json',
    }),
  );
  return key;
}

export async function putContactRecord(
  doc: DynamoDBDocumentClient,
  table: string,
  record: ContactRecord,
): Promise<void> {
  await doc.send(new PutCommand({ TableName: table, Item: record }));
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/ingest/store.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/src/ingest/store.ts backend/test/ingest/store.test.ts
git commit -m "add S3 message + DynamoDB contact persistence"
```

---

## Task 8: SQS enqueue

**Files:**
- Create: `backend/src/ingest/enqueue.ts`
- Test: `backend/test/ingest/enqueue.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect, beforeEach } from 'vitest';
import { mockClient } from 'aws-sdk-client-mock';
import { SQSClient, SendMessageCommand } from '@aws-sdk/client-sqs';
import { enqueueNotification } from '../../src/ingest/enqueue';
import type { NotificationMessage } from '../../src/shared/types';

const sqsMock = mockClient(SQSClient);

describe('enqueueNotification', () => {
  beforeEach(() => sqsMock.reset());
  it('sends the notification message JSON to the queue', async () => {
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'm1' });
    const sqs = new SQSClient({});
    const note: NotificationMessage = {
      id: 'abc', email: 'a@b.co', s3Bucket: 'bucket', s3Key: 'messages/2026/06/abc.json',
    };
    await enqueueNotification(sqs, 'https://sqs/q', note);
    const call = sqsMock.commandCalls(SendMessageCommand)[0].args[0].input;
    expect(call.QueueUrl).toBe('https://sqs/q');
    expect(JSON.parse(call.MessageBody as string)).toEqual(note);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/ingest/enqueue.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/ingest/enqueue.ts
import { SQSClient, SendMessageCommand } from '@aws-sdk/client-sqs';
import type { NotificationMessage } from '../shared/types';

export async function enqueueNotification(
  sqs: SQSClient,
  queueUrl: string,
  note: NotificationMessage,
): Promise<void> {
  await sqs.send(new SendMessageCommand({ QueueUrl: queueUrl, MessageBody: JSON.stringify(note) }));
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/ingest/enqueue.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/src/ingest/enqueue.ts backend/test/ingest/enqueue.test.ts
git commit -m "add SQS notification enqueue"
```

---

## Task 9: Ingest handler (orchestration)

Wires validation → rate limit → persist → enqueue behind a Lambda Function URL. Pure orchestration over the tested modules; AWS clients and a `now`/`uuid` injector are passed in so it is testable without real time/IDs.

**Files:**
- Create: `backend/src/ingest/handler.ts`
- Test: `backend/test/ingest/handler.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect, beforeEach } from 'vitest';
import { mockClient } from 'aws-sdk-client-mock';
import { S3Client, PutObjectCommand } from '@aws-sdk/client-s3';
import { DynamoDBDocumentClient, UpdateCommand, PutCommand } from '@aws-sdk/lib-dynamodb';
import { SQSClient, SendMessageCommand } from '@aws-sdk/client-sqs';
import { handleIngest } from '../../src/ingest/handler';

const s3 = mockClient(S3Client);
const ddb = mockClient(DynamoDBDocumentClient);
const sqs = mockClient(SQSClient);

// CAPTCHA is enforced by AWS WAF at the edge — the Lambda does NOT verify a token,
// so there is no Secrets Manager client and no fetch stub here.
const ENV = {
  MESSAGES_BUCKET: 'bucket', CONTACTS_TABLE: 'contacts', RATE_LIMIT_TABLE: 'rate-limits',
  NOTIFICATION_QUEUE_URL: 'https://sqs/q',
};

// A valid submission: honeypot empty, old enough timestamp. No turnstileToken (WAF handles CAPTCHA).
function event(overrides: Record<string, unknown> = {}) {
  const body = JSON.stringify({
    name: 'Alice', email: 'a@b.co', message: 'hello there',
    website: '', formTimestamp: 0, ...overrides,
  });
  return {
    headers: { 'x-forwarded-for': '1.2.3.4', 'user-agent': 'UA' },
    body,
  } as never;
}

const deps = () => ({
  env: ENV,
  clients: {
    s3: new S3Client({}), doc: DynamoDBDocumentClient.from({} as never), sqs: new SQSClient({}),
  },
  now: () => 10_000, // 10s > MIN_FORM_TIME_MS past formTimestamp=0
  uuid: () => 'fixed-id',
});

beforeEach(() => {
  s3.reset(); ddb.reset(); sqs.reset();
  ddb.on(UpdateCommand).resolves({});
  ddb.on(PutCommand).resolves({});
  s3.on(PutObjectCommand).resolves({});
  sqs.on(SendMessageCommand).resolves({ MessageId: 'm1' });
});

describe('handleIngest', () => {
  it('persists, enqueues, and returns 200 for a valid submission', async () => {
    const res = await handleIngest(event(), deps());
    expect(res.statusCode).toBe(200);
    expect(s3.commandCalls(PutObjectCommand)).toHaveLength(1);
    expect(ddb.commandCalls(PutCommand)).toHaveLength(1);     // contact record
    expect(sqs.commandCalls(SendMessageCommand)).toHaveLength(1);
  });

  it('rejects a tripped honeypot with 200 but no persistence (silent)', async () => {
    const res = await handleIngest(event({ website: 'spam' }), deps());
    expect(res.statusCode).toBe(200);
    expect(s3.commandCalls(PutObjectCommand)).toHaveLength(0);
    expect(sqs.commandCalls(SendMessageCommand)).toHaveLength(0);
  });

  it('rejects too-fast submissions with 400', async () => {
    const d = deps(); d.now = () => 1000; // 1s after formTimestamp=0
    const res = await handleIngest(event(), d);
    expect(res.statusCode).toBe(400);
    expect(s3.commandCalls(PutObjectCommand)).toHaveLength(0);
  });

  it('rejects invalid email with 400', async () => {
    const res = await handleIngest(event({ email: 'nope' }), deps());
    expect(res.statusCode).toBe(400);
  });

  it('returns 429 when rate limited', async () => {
    const err = new Error('limit'); err.name = 'ConditionalCheckFailedException';
    ddb.on(UpdateCommand).rejects(err);
    const res = await handleIngest(event(), deps());
    expect(res.statusCode).toBe(429);
    expect(s3.commandCalls(PutObjectCommand)).toHaveLength(0);
  });

  it('returns 400 on malformed JSON', async () => {
    const bad = { headers: {}, body: '{not json' } as never;
    const res = await handleIngest(bad, deps());
    expect(res.statusCode).toBe(400);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/ingest/handler.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/ingest/handler.ts
import type { S3Client } from '@aws-sdk/client-s3';
import type { DynamoDBDocumentClient } from '@aws-sdk/lib-dynamodb';
import type { SQSClient } from '@aws-sdk/client-sqs';
import { S3Client as S3 } from '@aws-sdk/client-s3';
import { DynamoDBClient } from '@aws-sdk/client-dynamodb';
import { DynamoDBDocumentClient as Doc } from '@aws-sdk/lib-dynamodb';
import { SQSClient as SQS } from '@aws-sdk/client-sqs';

import type { ContactSubmission, StoredMessage, ContactRecord, NotificationMessage } from '../shared/types';
import { isHoneypotTripped, isTooFast, isValidEmail, sanitizeName, MAX_MESSAGE } from '../shared/validation';
import { extractClientIp } from './ip';
import { checkRateLimit } from './rateLimit';
import { putMessageBody, putContactRecord } from './store';
import { enqueueNotification } from './enqueue';

export interface IngestEnv {
  MESSAGES_BUCKET: string;
  CONTACTS_TABLE: string;
  RATE_LIMIT_TABLE: string;
  NOTIFICATION_QUEUE_URL: string;
}

export interface IngestDeps {
  env: IngestEnv;
  clients: { s3: S3Client; doc: DynamoDBDocumentClient; sqs: SQSClient };
  now: () => number;
  uuid: () => string;
}

interface FunctionUrlEvent {
  headers: Record<string, string | undefined>;
  body?: string;
}
interface FunctionUrlResult {
  statusCode: number;
  headers: Record<string, string>;
  body: string;
}

function json(statusCode: number, payload: unknown): FunctionUrlResult {
  return { statusCode, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) };
}

export async function handleIngest(event: FunctionUrlEvent, deps: IngestDeps): Promise<FunctionUrlResult> {
  const { env, clients, now, uuid } = deps;

  let sub: ContactSubmission;
  try {
    sub = JSON.parse(event.body ?? '') as ContactSubmission;
  } catch {
    return json(400, { error: 'Invalid request' });
  }

  // Layer 2: honeypot — silently accept (200) so bots get no signal, but persist nothing.
  if (isHoneypotTripped(sub)) return json(200, { ok: true });

  // Layer 3: time-trap.
  if (typeof sub.formTimestamp !== 'number' || isTooFast(sub.formTimestamp, now())) {
    return json(400, { error: 'Submission too fast' });
  }

  // Layer 5: email shape (cheap) before the network call.
  if (!isValidEmail(sub.email)) return json(400, { error: 'Invalid email' });
  if (!sub.message || sub.message.length > MAX_MESSAGE) return json(400, { error: 'Invalid message' });

  const ip = extractClientIp(event.headers);

  // NOTE: CAPTCHA (human verification) is enforced by AWS WAF at the edge before the
  // request reaches this Lambda — there is no token check here.

  // Layer 1: rate limit (after cheap checks, before writes).
  const nowSec = Math.floor(now() / 1000);
  if (!(await checkRateLimit(clients.doc, env.RATE_LIMIT_TABLE, ip, nowSec))) {
    return json(429, { error: 'Too many requests' });
  }

  // Persist.
  const id = uuid();
  const createdAt = new Date(now()).toISOString();
  const name = sanitizeName(sub.name);
  const email = sub.email.toLowerCase();

  const message: StoredMessage = { id, name, email, message: sub.message, createdAt, ip, userAgent: event.headers['user-agent'] ?? '' };
  const s3Key = await putMessageBody(clients.s3, env.MESSAGES_BUCKET, message);

  const record: ContactRecord = {
    pk: `EMAIL#${email}`, sk: `SUB#${createdAt}#${id}`, gsi1pk: 'ALL', gsi1sk: createdAt,
    id, name, email, ip, userAgent: message.userAgent, createdAt,
    s3Bucket: env.MESSAGES_BUCKET, s3Key, status: 'new',
  };
  await putContactRecord(clients.doc, env.CONTACTS_TABLE, record);

  const note: NotificationMessage = { id, email, s3Bucket: env.MESSAGES_BUCKET, s3Key };
  await enqueueNotification(clients.sqs, env.NOTIFICATION_QUEUE_URL, note);

  return json(200, { ok: true });
}

// --- Lambda entrypoint (constructs real clients once per container) ---
const env: IngestEnv = {
  MESSAGES_BUCKET: process.env.MESSAGES_BUCKET!,
  CONTACTS_TABLE: process.env.CONTACTS_TABLE!,
  RATE_LIMIT_TABLE: process.env.RATE_LIMIT_TABLE!,
  NOTIFICATION_QUEUE_URL: process.env.NOTIFICATION_QUEUE_URL!,
};
const clients = {
  s3: new S3({}),
  doc: Doc.from(new DynamoDBClient({})),
  sqs: new SQS({}),
};

export const handler = (event: FunctionUrlEvent): Promise<FunctionUrlResult> =>
  handleIngest(event, { env, clients, now: () => Date.now(), uuid: () => crypto.randomUUID() });
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/ingest/handler.test.ts`
Expected: PASS (all 6 cases).

- [ ] **Step 5: Commit**

```bash
git add backend/src/ingest/handler.ts backend/test/ingest/handler.test.ts
git commit -m "add ingest handler orchestration (validate -> persist -> enqueue)"
```

---

## Task 10: Digest composition

**Files:**
- Create: `backend/src/notifier/digest.ts`
- Test: `backend/test/notifier/digest.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect } from 'vitest';
import { composeDigest } from '../../src/notifier/digest';
import type { StoredMessage } from '../../src/shared/types';

const m = (id: string, email: string): StoredMessage => ({
  id, name: `N-${id}`, email, message: `msg-${id}`,
  createdAt: '2026-06-02T12:00:00.000Z', ip: '1.2.3.4', userAgent: 'UA',
});

describe('composeDigest', () => {
  it('summarizes the count in the subject', () => {
    const { subject } = composeDigest([m('a', 'a@x.co'), m('b', 'b@x.co')]);
    expect(subject).toBe('Portfolio: 2 new contact submissions');
  });
  it('uses singular subject for one message', () => {
    expect(composeDigest([m('a', 'a@x.co')]).subject).toBe('Portfolio: 1 new contact submission');
  });
  it('includes each sender name, email, and message in the body', () => {
    const { body } = composeDigest([m('a', 'a@x.co'), m('b', 'b@x.co')]);
    expect(body).toContain('N-a');
    expect(body).toContain('a@x.co');
    expect(body).toContain('msg-a');
    expect(body).toContain('N-b');
    expect(body).toContain('msg-b');
  });
  it('lists the first sender as Reply-To target', () => {
    const { replyTo } = composeDigest([m('a', 'first@x.co'), m('b', 'b@x.co')]);
    expect(replyTo).toBe('first@x.co');
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/notifier/digest.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/notifier/digest.ts
import type { StoredMessage } from '../shared/types';

export interface Digest {
  subject: string;
  body: string;
  replyTo: string;
}

export function composeDigest(messages: StoredMessage[]): Digest {
  const n = messages.length;
  const subject = `Portfolio: ${n} new contact submission${n === 1 ? '' : 's'}`;

  const body = messages
    .map(
      (m, i) =>
        `#${i + 1} — ${m.name} <${m.email}> at ${m.createdAt} (ip ${m.ip})\n${m.message}\n`,
    )
    .join('\n----------------------------------------\n');

  return { subject, body, replyTo: messages[0].email };
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/notifier/digest.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/src/notifier/digest.ts backend/test/notifier/digest.test.ts
git commit -m "add digest email composition"
```

---

## Task 11: SES send

**Files:**
- Create: `backend/src/notifier/email.ts`
- Test: `backend/test/notifier/email.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect, beforeEach } from 'vitest';
import { mockClient } from 'aws-sdk-client-mock';
import { SESClient, SendEmailCommand } from '@aws-sdk/client-ses';
import { sendDigestEmail } from '../../src/notifier/email';

const sesMock = mockClient(SESClient);

describe('sendDigestEmail', () => {
  beforeEach(() => sesMock.reset());
  it('sends with From, To, Reply-To, subject and body', async () => {
    sesMock.on(SendEmailCommand).resolves({ MessageId: 'm1' });
    const ses = new SESClient({});
    await sendDigestEmail(ses, {
      from: 'noreply@example.com', to: 'me@example.com',
      digest: { subject: 'Subj', body: 'Body', replyTo: 'sender@x.co' },
    });
    const input = sesMock.commandCalls(SendEmailCommand)[0].args[0].input;
    expect(input.Source).toBe('noreply@example.com');
    expect(input.Destination?.ToAddresses).toEqual(['me@example.com']);
    expect(input.ReplyToAddresses).toEqual(['sender@x.co']);
    expect(input.Message?.Subject?.Data).toBe('Subj');
    expect(input.Message?.Body?.Text?.Data).toBe('Body');
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/notifier/email.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/notifier/email.ts
import { SESClient, SendEmailCommand } from '@aws-sdk/client-ses';
import type { Digest } from './digest';

export interface SendArgs {
  from: string;
  to: string;
  digest: Digest;
}

export async function sendDigestEmail(ses: SESClient, args: SendArgs): Promise<void> {
  await ses.send(
    new SendEmailCommand({
      Source: args.from,
      Destination: { ToAddresses: [args.to] },
      ReplyToAddresses: [args.digest.replyTo],
      Message: {
        Subject: { Data: args.digest.subject },
        Body: { Text: { Data: args.digest.body } },
      },
    }),
  );
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/notifier/email.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/src/notifier/email.ts backend/test/notifier/email.test.ts
git commit -m "add SES digest email send"
```

---

## Task 12: Notifier handler (SQS batch)

Consumes an SQS batch, loads each message body from S3, composes one digest, sends it, and reports partial failures so only failed records re-queue.

**Files:**
- Create: `backend/src/notifier/handler.ts`
- Test: `backend/test/notifier/handler.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect, beforeEach } from 'vitest';
import { mockClient } from 'aws-sdk-client-mock';
import { S3Client, GetObjectCommand } from '@aws-sdk/client-s3';
import { SESClient, SendEmailCommand } from '@aws-sdk/client-ses';
import { handleNotify } from '../../src/notifier/handler';
import type { StoredMessage } from '../../src/shared/types';

const s3 = mockClient(S3Client);
const ses = mockClient(SESClient);

const ENV = { CONTACT_EMAIL: 'me@example.com', FROM_EMAIL: 'noreply@example.com' };

function record(id: string, key: string) {
  return {
    messageId: id,
    body: JSON.stringify({ id, email: `${id}@x.co`, s3Bucket: 'bucket', s3Key: key }),
  };
}
function stored(id: string): StoredMessage {
  return { id, name: `N-${id}`, email: `${id}@x.co`, message: `msg-${id}`,
    createdAt: '2026-06-02T12:00:00.000Z', ip: '1.2.3.4', userAgent: 'UA' };
}
// aws-sdk-client-mock returns a body with transformToString() for GetObject.
function s3Body(obj: unknown) {
  return { Body: { transformToString: async () => JSON.stringify(obj) } } as never;
}

const deps = () => ({ env: ENV, clients: { s3: new S3Client({}), ses: new SESClient({}) } });

beforeEach(() => {
  s3.reset(); ses.reset();
  ses.on(SendEmailCommand).resolves({ MessageId: 'm1' });
});

describe('handleNotify', () => {
  it('sends ONE digest for a batch and reports no failures', async () => {
    s3.on(GetObjectCommand, { Key: 'k-a' }).resolves(s3Body(stored('a')));
    s3.on(GetObjectCommand, { Key: 'k-b' }).resolves(s3Body(stored('b')));
    const res = await handleNotify({ Records: [record('a', 'k-a'), record('b', 'k-b')] } as never, deps());
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(1);
    expect(res.batchItemFailures).toEqual([]);
  });

  it('reports a record whose S3 fetch fails, still sends the rest', async () => {
    s3.on(GetObjectCommand, { Key: 'k-a' }).resolves(s3Body(stored('a')));
    s3.on(GetObjectCommand, { Key: 'k-bad' }).rejects(new Error('missing'));
    const res = await handleNotify({ Records: [record('a', 'k-a'), record('bad', 'k-bad')] } as never, deps());
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(1); // digest of the 1 good record
    expect(res.batchItemFailures).toEqual([{ itemIdentifier: 'bad' }]);
  });

  it('reports ALL records as failures when the SES send throws', async () => {
    s3.on(GetObjectCommand, { Key: 'k-a' }).resolves(s3Body(stored('a')));
    ses.on(SendEmailCommand).rejects(new Error('ses down'));
    const res = await handleNotify({ Records: [record('a', 'k-a')] } as never, deps());
    expect(res.batchItemFailures).toEqual([{ itemIdentifier: 'a' }]);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend && npx vitest run test/notifier/handler.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Write minimal implementation**

```ts
// backend/src/notifier/handler.ts
import type { S3Client } from '@aws-sdk/client-s3';
import type { SESClient } from '@aws-sdk/client-ses';
import { S3Client as S3, GetObjectCommand } from '@aws-sdk/client-s3';
import { SESClient as SES } from '@aws-sdk/client-ses';
import type { StoredMessage, NotificationMessage } from '../shared/types';
import { composeDigest } from './digest';
import { sendDigestEmail } from './email';

export interface NotifyEnv { CONTACT_EMAIL: string; FROM_EMAIL: string; }
export interface NotifyDeps { env: NotifyEnv; clients: { s3: S3Client; ses: SESClient }; }

interface SQSRecord { messageId: string; body: string; }
interface SQSEvent { Records: SQSRecord[]; }
interface BatchResponse { batchItemFailures: { itemIdentifier: string }[]; }

async function loadMessage(s3: S3Client, note: NotificationMessage): Promise<StoredMessage> {
  const res = await s3.send(new GetObjectCommand({ Bucket: note.s3Bucket, Key: note.s3Key }));
  const text = await (res.Body as { transformToString: () => Promise<string> }).transformToString();
  return JSON.parse(text) as StoredMessage;
}

export async function handleNotify(event: SQSEvent, deps: NotifyDeps): Promise<BatchResponse> {
  const { clients, env } = deps;
  const failures: { itemIdentifier: string }[] = [];
  const loaded: { messageId: string; msg: StoredMessage }[] = [];

  // Load each record's body; a record that fails to load is an individual failure.
  for (const r of event.Records) {
    try {
      const note = JSON.parse(r.body) as NotificationMessage;
      loaded.push({ messageId: r.messageId, msg: await loadMessage(clients.s3, note) });
    } catch {
      failures.push({ itemIdentifier: r.messageId });
    }
  }

  if (loaded.length === 0) return { batchItemFailures: failures };

  // One digest for the whole batch. If the send fails, every loaded record must retry.
  try {
    const digest = composeDigest(loaded.map((l) => l.msg));
    await sendDigestEmail(clients.ses, { from: env.FROM_EMAIL, to: env.CONTACT_EMAIL, digest });
  } catch {
    for (const l of loaded) failures.push({ itemIdentifier: l.messageId });
  }

  return { batchItemFailures: failures };
}

// --- Lambda entrypoint ---
// CONTACT_EMAIL is resolved from Secrets Manager (cached per container by getSecret);
// FROM_EMAIL (noreply@<domain>) is non-sensitive and stays an env var.
import { SecretsManagerClient } from '@aws-sdk/client-secrets-manager';
import { getSecret } from '../shared/secrets';

const FROM_EMAIL = process.env.FROM_EMAIL!;
const CONTACT_EMAIL_SECRET_ARN = process.env.CONTACT_EMAIL_SECRET_ARN!;
const clients = { s3: new S3({}), ses: new SES({}) };
const secrets = new SecretsManagerClient({});

export const handler = async (event: SQSEvent): Promise<BatchResponse> => {
  const contactEmail = await getSecret(secrets, CONTACT_EMAIL_SECRET_ARN);
  return handleNotify(event, { env: { CONTACT_EMAIL: contactEmail, FROM_EMAIL }, clients });
};
```

> Note: `handleNotify` still takes a resolved `CONTACT_EMAIL` string, so the unit test in Step 1 needs no Secrets Manager mock — only the entrypoint touches Secrets Manager.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend && npx vitest run test/notifier/handler.test.ts`
Expected: PASS (all 3 cases).

- [ ] **Step 5: Commit**

```bash
git add backend/src/notifier/handler.ts backend/test/notifier/handler.test.ts
git commit -m "add notifier SQS batch handler (one digest, partial-failure aware)"
```

---

## Task 13: Full suite + build smoke test

**Files:**
- Modify: none (verification task)

- [ ] **Step 1: Run the whole test suite**

Run: `cd backend && npm test`
Expected: all test files PASS, no failures.

- [ ] **Step 2: Typecheck**

Run: `cd backend && npm run typecheck`
Expected: PASS, no errors.

- [ ] **Step 3: Build both Lambda bundles**

Run: `cd backend && npm run build`
Expected: `dist/ingest/index.mjs` and `dist/notifier/index.mjs` produced, no errors.

- [ ] **Step 4: Add `backend/dist/` to gitignore**

Add to `backend/.gitignore`:
```
dist/
node_modules/
```

- [ ] **Step 5: Commit**

```bash
git add backend/.gitignore
git commit -m "ignore backend build output; full suite green"
```

---

## Self-Review

**Spec coverage (against `2026-06-02-lambda-contact-pipeline-design.md`):**
- §5 ingest validation (honeypot/time-trap/email) → Tasks 2, 9 ✓
- §3/§5 CAPTCHA — enforced by AWS WAF at the edge, **not** application code → no app task (infra plan) ✓
- §5/§7 rate limiting (DynamoDB conditional, 3/hr) → Task 6, wired in Task 9 ✓
- §5 S3 body + §6 DynamoDB per-person record (PK=EMAIL#, SK=SUB#, GSI1 ALL) → Tasks 1, 7, 9 ✓
- §5 SQS enqueue → Tasks 8, 9 ✓
- §3/§5 batched SES digest + ReportBatchItemFailures → Tasks 10, 11, 12 ✓
- §7 raw IP stored → Tasks 1 (`ip` field), 4, 9 ✓
- §3/§7 Secrets Manager (`CONTACT_EMAIL` only, cached) → Tasks 3, 12 (notifier entrypoint) ✓
- Client IP from XFF/viewer-address → Task 4 ✓
- Bug-review fixes (header-injection name sanitize, email caps) → Task 2 ✓

**Deferred to the infra plan (correctly NOT in this plan):** CloudFront `/api/*` behavior + OAC, Function URL + IAM, `AllViewerExceptHostHeader`, **WAF CAPTCHA action + rate rule**, SES domain/DKIM verification, Secrets Manager *resource* creation (`CONTACT_EMAIL`), SQS `BatchSize`/`MaximumBatchingWindowInSeconds` (300s) + DLQ wiring, DynamoDB table/TTL/GSI definitions, IAM roles, CI/OIDC. The application code is written to consume these via env vars and injected clients. **Frontend WAF CAPTCHA JS SDK swap is tracked separately from this backend plan.**

**Placeholder scan:** none — every code step contains complete implementation and tests.

**Type consistency:** `StoredMessage`, `ContactRecord`, `NotificationMessage`, `Digest`, `IngestEnv`/`IngestDeps`, `NotifyEnv`/`NotifyDeps` are defined once and used consistently. `composeDigest` returns `{subject, body, replyTo}` and is consumed identically in Tasks 11–12. `checkRateLimit`/`putMessageBody`/`putContactRecord`/`enqueueNotification`/`extractClientIp` signatures match their call sites in the Task 9 ingest handler (which no longer references Turnstile or Secrets Manager). `getSecret` is consumed only by the Task 12 notifier entrypoint. Notifier `handleNotify` returns `{batchItemFailures}` matching the SQS partial-batch-response contract.

**Note on `crypto.randomUUID()`:** available as a Node global in the Lambda Node 20 runtime; injected via `deps.uuid` in tests so no real randomness is needed under test.
