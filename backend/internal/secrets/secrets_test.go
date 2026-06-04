package secrets

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// fakeClient is a hand-written fake that implements Client.
// It returns the canned output/error and records how many times it was called.
type fakeClient struct {
	out       *secretsmanager.GetSecretValueOutput
	err       error
	callCount int
}

func (f *fakeClient) GetSecretValue(
	_ context.Context,
	_ *secretsmanager.GetSecretValueInput,
	_ ...func(*secretsmanager.Options),
) (*secretsmanager.GetSecretValueOutput, error) {
	f.callCount++
	return f.out, f.err
}

func TestGetSecret_fetch(t *testing.T) {
	ClearCache()
	fake := &fakeClient{
		out: &secretsmanager.GetSecretValueOutput{SecretString: aws.String("s3cr3t")},
	}

	got, err := GetSecret(context.Background(), fake, "my-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "s3cr3t" {
		t.Errorf("GetSecret = %q; want %q", got, "s3cr3t")
	}
}

func TestGetSecret_cacheHit(t *testing.T) {
	ClearCache()
	fake := &fakeClient{
		out: &secretsmanager.GetSecretValueOutput{SecretString: aws.String("s3cr3t")},
	}

	first, err := GetSecret(context.Background(), fake, "my-secret")
	if err != nil {
		t.Fatalf("first call unexpected error: %v", err)
	}
	second, err := GetSecret(context.Background(), fake, "my-secret")
	if err != nil {
		t.Fatalf("second call unexpected error: %v", err)
	}

	if first != "s3cr3t" || second != "s3cr3t" {
		t.Errorf("GetSecret returned (%q, %q); want both %q", first, second, "s3cr3t")
	}
	if fake.callCount != 1 {
		t.Errorf("fake called %d time(s); want 1 (second should be served from cache)", fake.callCount)
	}
}

func TestGetSecret_noSecretString(t *testing.T) {
	ClearCache()
	fake := &fakeClient{
		out: &secretsmanager.GetSecretValueOutput{}, // SecretString is nil
	}

	_, err := GetSecret(context.Background(), fake, "my-secret")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "no SecretString") {
		t.Errorf("error %q does not contain %q", err.Error(), "no SecretString")
	}
}

func TestGetSecret_clientError(t *testing.T) {
	ClearCache()
	fake := &fakeClient{
		err: fmt.Errorf("access denied"),
	}

	_, err := GetSecret(context.Background(), fake, "my-secret")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error %q does not contain %q", err.Error(), "access denied")
	}
}
