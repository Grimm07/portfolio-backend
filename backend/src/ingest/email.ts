// backend/src/ingest/email.ts
import { SESClient, SendEmailCommand } from '@aws-sdk/client-ses';

export interface ContactEmailArgs {
  from: string;       // verified SES identity, e.g. noreply@trystan-tbm.dev
  to: string;         // recipient (from Secrets Manager)
  replyTo: string;    // submitter's email, so a reply goes straight to them
  name: string;
  email: string;
  message: string;
  ip: string;
  createdAt: string;  // ISO-8601
}

export async function sendContactEmail(ses: SESClient, a: ContactEmailArgs): Promise<void> {
  const subject = `Portfolio: new contact from ${a.name}`;
  const body =
    `From: ${a.name} <${a.email}>\n` +
    `When: ${a.createdAt}\n` +
    `IP:   ${a.ip}\n` +
    `\n${a.message}\n`;

  await ses.send(
    new SendEmailCommand({
      Source: a.from,
      Destination: { ToAddresses: [a.to] },
      ReplyToAddresses: [a.replyTo],
      Message: {
        Subject: { Data: subject },
        Body: { Text: { Data: body } },
      },
    }),
  );
}
