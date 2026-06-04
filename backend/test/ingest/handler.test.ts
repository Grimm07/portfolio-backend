import { describe, it, expect, beforeEach, vi } from 'vitest';
import { mockClient } from 'aws-sdk-client-mock';
import { SESClient, SendEmailCommand } from '@aws-sdk/client-ses';
import { SecretsManagerClient, GetSecretValueCommand } from '@aws-sdk/client-secrets-manager';
import { handleIngest } from '../../src/ingest/handler';
import { __clearSecretCache } from '../../src/shared/secrets';

const ses = mockClient(SESClient);
const sm = mockClient(SecretsManagerClient);

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

const deps = () => ({
  env: ENV,
  clients: { ses: new SESClient({}), secrets: new SecretsManagerClient({}) },
  now: () => 10_000, // 10s > MIN_FORM_TIME_MS past formTimestamp=0
});

beforeEach(() => {
  ses.reset();
  sm.reset();
  __clearSecretCache();
  sm.on(GetSecretValueCommand).resolves({ SecretString: 'owner@example.com' });
  ses.on(SendEmailCommand).resolves({ MessageId: 'm1' });
});

describe('handleIngest', () => {
  it('sends one email and returns 200 for a valid submission', async () => {
    const res = await handleIngest(event(), deps());
    expect(res.statusCode).toBe(200);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(1);
    const input = ses.commandCalls(SendEmailCommand)[0].args[0].input;
    expect(input.Destination?.ToAddresses).toEqual(['owner@example.com']);
    expect(input.ReplyToAddresses).toEqual(['a@b.co']);
  });

  it('rejects a tripped honeypot with 200 but sends nothing (silent)', async () => {
    const res = await handleIngest(event({ website: 'spam' }), deps());
    expect(res.statusCode).toBe(200);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(0);
  });

  it('rejects too-fast submissions with 400 and sends nothing', async () => {
    const d = deps(); d.now = () => 1000; // 1s after formTimestamp=0
    const res = await handleIngest(event(), d);
    expect(res.statusCode).toBe(400);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(0);
  });

  it('rejects invalid email with 400', async () => {
    const res = await handleIngest(event({ email: 'nope' }), deps());
    expect(res.statusCode).toBe(400);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(0);
  });

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

  it('returns 400 when message is a number (non-string field)', async () => {
    const res = await handleIngest(event({ message: 123 }), deps());
    expect(res.statusCode).toBe(400);
  });

  it('returns 400 when name is a number (non-string name)', async () => {
    const res = await handleIngest(event({ name: 123 }), deps());
    expect(res.statusCode).toBe(400);
  });

  it('decodes a base64-encoded body (API Gateway v2) and returns 200', async () => {
    const raw = JSON.stringify({ name: 'Alice', email: 'a@b.co', message: 'hello there', website: '', formTimestamp: 0 });
    const b64 = { headers: baseHeaders(), body: Buffer.from(raw, 'utf8').toString('base64'), isBase64Encoded: true } as never;
    const res = await handleIngest(b64, deps());
    expect(res.statusCode).toBe(200);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(1);
  });

  it('rejects a request missing x-origin-verify with 403 and sends nothing', async () => {
    const validBody = JSON.stringify({ name: 'Alice', email: 'a@b.co', message: 'hello there', website: '', formTimestamp: 0 });
    const noHeader = { headers: { 'x-forwarded-for': '1.2.3.4' }, body: validBody } as never;
    const res = await handleIngest(noHeader, deps());
    expect(res.statusCode).toBe(403);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(0);
  });

  it('rejects a wrong x-origin-verify with 403 and sends nothing', async () => {
    const validBody = JSON.stringify({ name: 'Alice', email: 'a@b.co', message: 'hello there', website: '', formTimestamp: 0 });
    const wrong = { headers: baseHeaders({ 'x-origin-verify': 'nope' }), body: validBody } as never;
    const res = await handleIngest(wrong, deps());
    expect(res.statusCode).toBe(403);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(0);
  });

  it('returns 500 when SES send fails', async () => {
    ses.on(SendEmailCommand).rejects(new Error('SES down'));
    const res = await handleIngest(event(), deps());
    expect(res.statusCode).toBe(500);
  });

  it('returns 500 when Secrets Manager fetch fails and sends no email', async () => {
    sm.on(GetSecretValueCommand).rejects(new Error('SM down'));
    const res = await handleIngest(event(), deps());
    expect(res.statusCode).toBe(500);
    expect(ses.commandCalls(SendEmailCommand)).toHaveLength(0);
  });

  it('logs a non-sensitive label (not the raw error) when the send path throws', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    // A realistic SES rejection whose message embeds the recipient address (PII).
    const boom = Object.assign(new Error('Email address is not verified: pibot.zeus@gmail.com'), {
      name: 'MessageRejected',
    });
    ses.on(SendEmailCommand).rejects(boom);
    const res = await handleIngest(event(), deps());
    expect(res.statusCode).toBe(500);
    // Logs the fixed label...
    expect(spy).toHaveBeenCalledWith('contact ingest: failed to send message', 'MessageRejected');
    // ...and never the raw message / address.
    const logged = spy.mock.calls.flat().join(' ');
    expect(logged).not.toContain('pibot.zeus@gmail.com');
    expect(logged).not.toContain('not verified');
    spy.mockRestore();
  });
});
