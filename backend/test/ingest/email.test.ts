import { describe, it, expect, beforeEach } from 'vitest';
import { mockClient } from 'aws-sdk-client-mock';
import { SESClient, SendEmailCommand } from '@aws-sdk/client-ses';
import { sendContactEmail } from '../../src/ingest/email';

const ses = mockClient(SESClient);

beforeEach(() => {
  ses.reset();
  ses.on(SendEmailCommand).resolves({ MessageId: 'm1' });
});

describe('sendContactEmail', () => {
  it('sends one SES email with From, single To, and submitter Reply-To', async () => {
    await sendContactEmail(new SESClient({}), {
      from: 'noreply@trystan-tbm.dev',
      to: 'owner@example.com',
      replyTo: 'alice@b.co',
      name: 'Alice',
      email: 'alice@b.co',
      message: 'hello there',
      ip: '1.2.3.4',
      createdAt: '2026-06-03T00:00:00.000Z',
    });

    const calls = ses.commandCalls(SendEmailCommand);
    expect(calls).toHaveLength(1);
    const input = calls[0].args[0].input;
    expect(input.Source).toBe('noreply@trystan-tbm.dev');
    expect(input.Destination?.ToAddresses).toEqual(['owner@example.com']);
    expect(input.ReplyToAddresses).toEqual(['alice@b.co']);
    expect(input.Message?.Subject?.Data).toContain('Alice');
    expect(input.Message?.Body?.Text?.Data).toContain('hello there');
    expect(input.Message?.Body?.Text?.Data).toContain('1.2.3.4');
    expect(input.Message?.Body?.Text?.Data).toContain('2026-06-03T00:00:00.000Z');
  });
});
