// Package secrets ports the shared/secrets.ts Secrets Manager fetch + cache logic to Go.
package secrets

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// Client is a narrow interface satisfied by *secretsmanager.Client.
// Defining it here keeps the package testable without importing the AWS mock SDK.
type Client interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

var (
	mu    sync.Mutex
	cache = make(map[string]string)
)

// ClearCache empties the package-level secret cache.
// Mirrors __clearSecretCache in secrets.ts; intended for tests.
func ClearCache() {
	mu.Lock()
	defer mu.Unlock()
	cache = make(map[string]string)
}

// GetSecret returns the plaintext value of the named secret.
// Results are cached in memory; subsequent calls with the same secretID are
// served from the cache without a network round-trip.
func GetSecret(ctx context.Context, client Client, secretID string) (string, error) {
	mu.Lock()
	if v, ok := cache[secretID]; ok {
		mu.Unlock()
		return v, nil
	}
	mu.Unlock()

	out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretID),
	})
	if err != nil {
		return "", err
	}
	if out.SecretString == nil {
		return "", fmt.Errorf("secret %s has no SecretString", secretID)
	}

	mu.Lock()
	cache[secretID] = *out.SecretString
	mu.Unlock()

	return *out.SecretString, nil
}
