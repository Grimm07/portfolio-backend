// backend/src/ingest/handler.ts
import type { SESClient } from '@aws-sdk/client-ses';
import type { SecretsManagerClient } from '@aws-sdk/client-secrets-manager';
import { SESClient as SES } from '@aws-sdk/client-ses';
import { SecretsManagerClient as SM } from '@aws-sdk/client-secrets-manager';
import { timingSafeEqual } from 'node:crypto';

import type { ContactSubmission } from '../shared/types';
import { isHoneypotTripped, isTooFast, isValidEmail, isValidMessage, sanitizeName } from '../shared/validation';
import { extractClientIp } from './ip';
import { getSecret } from '../shared/secrets';
import { sendContactEmail } from './email';

export interface IngestEnv {
  FROM_EMAIL: string;                 // verified SES identity (noreply@<domain>)
  CONTACT_EMAIL_SECRET_ARN: string;   // Secrets Manager ARN holding the recipient address
  ORIGIN_VERIFY_SECRET: string;       // shared secret CloudFront injects as x-origin-verify
}

export interface IngestDeps {
  env: IngestEnv;
  clients: { ses: SESClient; secrets: SecretsManagerClient };
  now: () => number;
}

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

// Constant-time comparison of the x-origin-verify header against the expected secret.
// Fails closed: a missing expected secret, a missing header, or any length mismatch -> false.
function originVerified(headerValue: string | undefined, expected: string): boolean {
  if (!expected || typeof headerValue !== 'string') return false;
  const a = Buffer.from(headerValue);
  const b = Buffer.from(expected);
  return a.length === b.length && timingSafeEqual(a, b);
}

function json(statusCode: number, payload: unknown): ApiGatewayV2Result {
  return { statusCode, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) };
}

// Maps a caught error to a NON-sensitive, fixed-literal label for logging. Never returns the
// error message or object — those can embed env-sourced data (the secret ARN, the recipient
// address, the origin-verify secret). Each branch returns a constant, so nothing tainted is
// logged, while the common failure modes stay distinguishable in CloudWatch.
function errorLabel(err: unknown): string {
  switch (err instanceof Error ? err.name : '') {
    case 'MessageRejected':           return 'MessageRejected';           // SES: recipient unverified / sandbox
    case 'AccessDeniedException':     return 'AccessDeniedException';     // IAM: SES or Secrets denied
    case 'ResourceNotFoundException': return 'ResourceNotFoundException'; // secret / identity missing
    case 'ThrottlingException':       return 'ThrottlingException';
    default:                          return 'other';
  }
}

export async function handleIngest(event: ApiGatewayV2Event, deps: IngestDeps): Promise<ApiGatewayV2Result> {
  const { env, clients, now } = deps;

  // Reject anything that didn't come through CloudFront (and thus skipped WAF).
  if (!originVerified(event.headers['x-origin-verify'], deps.env.ORIGIN_VERIFY_SECRET)) {
    return json(403, { error: 'Forbidden' });
  }

  // Snapshot now() once — reused for time-trap and createdAt.
  const ts = now();

  let parsed: unknown;
  try {
    const rawBody = event.isBase64Encoded && event.body
      ? Buffer.from(event.body, 'base64').toString('utf8')
      : (event.body ?? '');
    parsed = JSON.parse(rawBody);
  } catch {
    return json(400, { error: 'Invalid request' });
  }
  if (typeof parsed !== 'object' || parsed === null) return json(400, { error: 'Invalid request' });
  const sub = parsed as Partial<ContactSubmission>;

  // Honeypot — silently accept (200) so bots get no signal, but send nothing.
  if (isHoneypotTripped(sub)) return json(200, { ok: true });

  // Time-trap.
  if (typeof sub.formTimestamp !== 'number' || isTooFast(sub.formTimestamp, ts)) {
    return json(400, { error: 'Submission too fast' });
  }

  // Field validation.
  if (typeof sub.email !== 'string' || !isValidEmail(sub.email)) return json(400, { error: 'Invalid email' });
  if (typeof sub.name !== 'string' || sub.name.trim().length === 0) return json(400, { error: 'Invalid name' });
  if (!isValidMessage(sub.message)) return json(400, { error: 'Invalid message' });

  // NOTE: CAPTCHA + rate-limiting are enforced by AWS WAF at the edge before the request
  // reaches this Lambda. There is no token check and no per-IP counter here.

  const name = sanitizeName(sub.name);
  const email = sub.email.toLowerCase();
  const message: string = sub.message as string;
  const ip = extractClientIp(event.headers);
  const createdAt = new Date(ts).toISOString();

  try {
    const to = await getSecret(clients.secrets, env.CONTACT_EMAIL_SECRET_ARN);
    await sendContactEmail(clients.ses, { from: env.FROM_EMAIL, to, replyTo: email, name, email, message, ip, createdAt });
  } catch (err) {
    // Log only a fixed, non-sensitive label (never the error message/object, which can carry
    // env-sourced data) — enough to tell e.g. an unverified-recipient MessageRejected apart.
    console.error('contact ingest: failed to send message', errorLabel(err));
    return json(500, { error: 'Failed to send message' });
  }

  return json(200, { ok: true });
}

// --- Lambda entrypoint (constructs real clients once per container) ---
const env: IngestEnv = {
  FROM_EMAIL: process.env.FROM_EMAIL!,
  CONTACT_EMAIL_SECRET_ARN: process.env.CONTACT_EMAIL_SECRET_ARN!,
  ORIGIN_VERIFY_SECRET: process.env.ORIGIN_VERIFY_SECRET!,
};
const clients = { ses: new SES({}), secrets: new SM({}) };

export const handler = (event: ApiGatewayV2Event): Promise<ApiGatewayV2Result> =>
  handleIngest(event, { env, clients, now: () => Date.now() });
