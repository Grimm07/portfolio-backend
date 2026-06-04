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
