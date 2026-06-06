package ingest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	ses "github.com/aws/aws-sdk-go-v2/service/ses"
	smithy "github.com/aws/smithy-go"

	"github.com/Grimm07/portfolio-backend/backend/internal/secrets"
)

const originSecret = "test-origin-secret"

// fakeSecrets implements secrets.Client.
type fakeSecrets struct {
	out       *secretsmanager.GetSecretValueOutput
	err       error
	callCount int
}

func (f *fakeSecrets) GetSecretValue(_ context.Context, _ *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.callCount++
	return f.out, f.err
}

// fakeSES implements email.SESAPI, recording the call count and last input.
type fakeSES struct {
	callCount int
	lastInput *ses.SendEmailInput
	err       error
}

func (f *fakeSES) SendEmail(_ context.Context, params *ses.SendEmailInput, _ ...func(*ses.Options)) (*ses.SendEmailOutput, error) {
	f.callCount++
	f.lastInput = params
	if f.err != nil {
		return nil, f.err
	}
	return &ses.SendEmailOutput{}, nil
}

func newSecrets() *fakeSecrets {
	return &fakeSecrets{
		out: &secretsmanager.GetSecretValueOutput{SecretString: aws.String("owner@example.com")},
	}
}

func baseHeaders() map[string]string {
	return map[string]string{
		"x-origin-verify": originSecret,
		"x-forwarded-for": "1.2.3.4",
	}
}

// jsonBody marshals the given fields into a JSON request body.
func jsonBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(b)
}

func validFields() map[string]any {
	return map[string]any{
		"name":          "Alice",
		"email":         "a@b.co",
		"message":       "hello there",
		"website":       "",
		"formTimestamp": 0,
	}
}

func newEvent(headers map[string]string, body string, b64 bool) events.APIGatewayV2HTTPRequest {
	return events.APIGatewayV2HTTPRequest{
		Headers:         headers,
		Body:            body,
		IsBase64Encoded: b64,
	}
}

func newDeps(ses *fakeSES, sm *fakeSecrets) Deps {
	return Deps{
		Env: Env{
			FromEmail:             "noreply@trystan-tbm.dev",
			ContactEmailSecretARN: "arn:aws:secretsmanager:us-east-1:111:secret:contact",
			OriginVerifySecret:    originSecret,
		},
		SES:     ses,
		Secrets: sm,
		Now:     func() time.Time { return time.UnixMilli(10000) },
	}
}

func TestHandleIngest_valid(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{}
	smFake := newSecrets()
	res, err := HandleIngest(context.Background(), newEvent(baseHeaders(), jsonBody(t, validFields()), false), newDeps(sesFake, smFake))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if sesFake.callCount != 1 {
		t.Fatalf("SES called %d times, want 1", sesFake.callCount)
	}
	to := sesFake.lastInput.Destination.ToAddresses
	if len(to) != 1 || to[0] != "owner@example.com" {
		t.Errorf("ToAddresses = %v, want [owner@example.com]", to)
	}
	rt := sesFake.lastInput.ReplyToAddresses
	if len(rt) != 1 || rt[0] != "a@b.co" {
		t.Errorf("ReplyToAddresses = %v, want [a@b.co]", rt)
	}
}

func TestHandleIngest_honeypot(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{}
	f := validFields()
	f["website"] = "spam"
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), jsonBody(t, f), false), newDeps(sesFake, newSecrets()))
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if sesFake.callCount != 0 {
		t.Fatalf("SES called %d times, want 0", sesFake.callCount)
	}
}

func TestHandleIngest_tooFast(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{}
	deps := newDeps(sesFake, newSecrets())
	deps.Now = func() time.Time { return time.UnixMilli(1000) } // 1s after formTimestamp=0
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), jsonBody(t, validFields()), false), deps)
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if sesFake.callCount != 0 {
		t.Fatalf("SES called %d times, want 0", sesFake.callCount)
	}
}

func TestHandleIngest_invalidEmail(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{}
	f := validFields()
	f["email"] = "nope"
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), jsonBody(t, f), false), newDeps(sesFake, newSecrets()))
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if sesFake.callCount != 0 {
		t.Fatalf("SES called %d times, want 0", sesFake.callCount)
	}
}

func TestHandleIngest_malformedJSON(t *testing.T) {
	secrets.ClearCache()
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), "{not json", false), newDeps(&fakeSES{}, newSecrets()))
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestHandleIngest_jsonNull(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{}
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), "null", false), newDeps(sesFake, newSecrets()))
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if sesFake.callCount != 0 {
		t.Fatalf("SES called %d times, want 0", sesFake.callCount)
	}
}

func TestHandleIngest_messageNumber(t *testing.T) {
	secrets.ClearCache()
	body := `{"name":"Alice","email":"a@b.co","message":123,"website":"","formTimestamp":0}`
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), body, false), newDeps(&fakeSES{}, newSecrets()))
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestHandleIngest_nameNumber(t *testing.T) {
	secrets.ClearCache()
	body := `{"name":123,"email":"a@b.co","message":"hello there","website":"","formTimestamp":0}`
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), body, false), newDeps(&fakeSES{}, newSecrets()))
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestHandleIngest_base64Body(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{}
	raw := jsonBody(t, validFields())
	b64 := base64.StdEncoding.EncodeToString([]byte(raw))
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), b64, true), newDeps(sesFake, newSecrets()))
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if sesFake.callCount != 1 {
		t.Fatalf("SES called %d times, want 1", sesFake.callCount)
	}
}

func TestHandleIngest_missingOriginHeader(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{}
	headers := map[string]string{"x-forwarded-for": "1.2.3.4"}
	res, _ := HandleIngest(context.Background(), newEvent(headers, jsonBody(t, validFields()), false), newDeps(sesFake, newSecrets()))
	if res.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	if sesFake.callCount != 0 {
		t.Fatalf("SES called %d times, want 0", sesFake.callCount)
	}
}

func TestHandleIngest_wrongOriginHeader(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{}
	headers := baseHeaders()
	headers["x-origin-verify"] = "nope"
	res, _ := HandleIngest(context.Background(), newEvent(headers, jsonBody(t, validFields()), false), newDeps(sesFake, newSecrets()))
	if res.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	if sesFake.callCount != 0 {
		t.Fatalf("SES called %d times, want 0", sesFake.callCount)
	}
}

func TestHandleIngest_sesError(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{err: errors.New("SES down")}
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), jsonBody(t, validFields()), false), newDeps(sesFake, newSecrets()))
	if res.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
}

func TestHandleIngest_secretsError(t *testing.T) {
	secrets.ClearCache()
	sesFake := &fakeSES{}
	smFake := &fakeSecrets{err: errors.New("SM down")}
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), jsonBody(t, validFields()), false), newDeps(sesFake, smFake))
	if res.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	if sesFake.callCount != 0 {
		t.Fatalf("SES called %d times, want 0", sesFake.callCount)
	}
}

func TestHandleIngest_logsNonSensitiveLabel(t *testing.T) {
	secrets.ClearCache()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr) // restore default

	boom := &smithy.GenericAPIError{Code: "MessageRejected", Message: "rejected for owner+pii@example.com not verified"}
	sesFake := &fakeSES{err: boom}
	res, _ := HandleIngest(context.Background(), newEvent(baseHeaders(), jsonBody(t, validFields()), false), newDeps(sesFake, newSecrets()))
	if res.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	logged := buf.String()
	if !strings.Contains(logged, "MessageRejected") {
		t.Errorf("log %q does not contain %q", logged, "MessageRejected")
	}
	if strings.Contains(logged, "owner+pii@example.com") {
		t.Errorf("log %q leaks PII", logged)
	}
}
