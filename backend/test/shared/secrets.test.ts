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
    expect(await getSecret(client, 'arn:contact-email')).toBe('s3cr3t');
  });

  it('caches by ARN — second call does not hit the API', async () => {
    smMock.on(GetSecretValueCommand).resolves({ SecretString: 's3cr3t' });
    const client = new SecretsManagerClient({});
    await getSecret(client, 'arn:contact-email');
    await getSecret(client, 'arn:contact-email');
    expect(smMock.commandCalls(GetSecretValueCommand)).toHaveLength(1);
  });

  it('throws when the secret has no string value', async () => {
    smMock.on(GetSecretValueCommand).resolves({});
    const client = new SecretsManagerClient({});
    await expect(getSecret(client, 'arn:empty')).rejects.toThrow(/no SecretString/);
  });
});
