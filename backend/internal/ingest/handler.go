// Package ingest ports the portfolio contact-form ingest Lambda handler from
// backend/src/ingest/handler.ts. It validates a submission and dispatches a
// single SES notification, encoding all failures as HTTP responses.
package ingest

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	smithy "github.com/aws/smithy-go"

	"github.com/Grimm07/portfolio-backend/backend/internal/email"
	"github.com/Grimm07/portfolio-backend/backend/internal/ip"
	"github.com/Grimm07/portfolio-backend/backend/internal/secrets"
	"github.com/Grimm07/portfolio-backend/backend/internal/validation"
)

// Env holds the runtime configuration for the ingest handler.
type Env struct {
	FromEmail             string // verified SES identity (noreply@<domain>)
	ContactEmailSecretARN string // Secrets Manager ARN holding the recipient address
	OriginVerifySecret    string // shared secret CloudFront injects as x-origin-verify
}

// Deps bundles the handler's collaborators so they can be faked in tests.
type Deps struct {
	Env     Env
	SES     email.SESAPI
	Secrets secrets.Client
	Now     func() time.Time
}

// HandleIngest processes one API Gateway v2 (HTTP API, payload format 2.0)
// request. It always returns a nil error — failures are encoded as HTTP
// responses, mirroring the TypeScript original which always resolves.
func HandleIngest(ctx context.Context, event events.APIGatewayV2HTTPRequest, deps Deps) (events.APIGatewayV2HTTPResponse, error) {
	// Reject anything that didn't come through CloudFront (and thus skipped WAF).
	if !originVerified(event.Headers["x-origin-verify"], deps.Env.OriginVerifySecret) {
		return jsonResponse(403, map[string]any{"error": "Forbidden"})
	}

	// Snapshot now() once — reused for the time-trap and createdAt.
	ts := deps.Now()
	nowMs := ts.UnixMilli()

	// Parse the body (base64-decoding first when API Gateway flags it).
	var bodyBytes []byte
	if event.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(event.Body)
		if err != nil {
			return jsonResponse(400, map[string]any{"error": "Invalid request"})
		}
		bodyBytes = decoded
	} else {
		bodyBytes = []byte(event.Body)
	}

	var parsed any
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return jsonResponse(400, map[string]any{"error": "Invalid request"})
	}

	// Reject anything that isn't a JSON object (null, numbers, arrays, strings).
	m, ok := parsed.(map[string]any)
	if !ok {
		return jsonResponse(400, map[string]any{"error": "Invalid request"})
	}

	// Honeypot — silently accept (200) so bots get no signal, but send nothing.
	if w, ok := m["website"].(string); ok && validation.IsHoneypotTripped(w) {
		return jsonResponse(200, map[string]any{"ok": true})
	}

	// Time-trap. JSON numbers decode to float64; a non-number fails closed.
	ft, ok := m["formTimestamp"].(float64)
	if !ok || validation.IsTooFast(int64(ft), nowMs) {
		return jsonResponse(400, map[string]any{"error": "Submission too fast"})
	}

	// Field validation.
	e, ok := m["email"].(string)
	if !ok || !validation.IsValidEmail(e) {
		return jsonResponse(400, map[string]any{"error": "Invalid email"})
	}
	n, ok := m["name"].(string)
	if !ok || strings.TrimSpace(n) == "" {
		return jsonResponse(400, map[string]any{"error": "Invalid name"})
	}
	msg, ok := m["message"].(string)
	if !ok || !validation.IsValidMessage(msg) {
		return jsonResponse(400, map[string]any{"error": "Invalid message"})
	}

	// NOTE: CAPTCHA + rate-limiting are enforced by AWS WAF at the edge before the
	// request reaches this Lambda. There is no token check and no per-IP counter here.

	name := validation.SanitizeName(n)
	addr := strings.ToLower(e)
	clientIP := ip.ExtractClientIP(event.Headers)
	createdAt := ts.UTC().Format("2006-01-02T15:04:05.000Z07:00")

	to, err := secrets.GetSecret(ctx, deps.Secrets, deps.Env.ContactEmailSecretARN)
	if err == nil {
		err = email.SendContactEmail(ctx, deps.SES, email.ContactEmailArgs{
			From:      deps.Env.FromEmail,
			To:        to,
			ReplyTo:   addr,
			Name:      name,
			Email:     addr,
			Message:   msg,
			IP:        clientIP,
			CreatedAt: createdAt,
		})
	}
	if err != nil {
		// Log only a fixed, non-sensitive label (never the error message/object,
		// which can carry env-sourced data) — enough to tell e.g. an
		// unverified-recipient MessageRejected apart.
		log.Printf("contact ingest: failed to send message %s", errorLabel(err))
		return jsonResponse(500, map[string]any{"error": "Failed to send message"})
	}

	return jsonResponse(200, map[string]any{"ok": true})
}

// originVerified constant-time-compares the x-origin-verify header against the
// expected secret. Fails closed: a missing expected secret or any length
// mismatch returns false.
func originVerified(headerValue, expected string) bool {
	if expected == "" {
		return false
	}
	if len(headerValue) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(headerValue), []byte(expected)) == 1
}

// jsonResponse builds an API Gateway v2 JSON response. It always returns a nil
// error so callers can return it directly from HandleIngest.
func jsonResponse(statusCode int, payload any) (events.APIGatewayV2HTTPResponse, error) {
	b, _ := json.Marshal(payload)
	return events.APIGatewayV2HTTPResponse{
		StatusCode: statusCode,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(b),
	}, nil
}

// errorLabel maps a caught error to a NON-sensitive, fixed-literal label for
// logging. It never returns the error message, which can embed env-sourced data
// (the secret ARN, the recipient address, the origin-verify secret).
func errorLabel(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "MessageRejected", "AccessDeniedException", "ResourceNotFoundException", "ThrottlingException":
			return apiErr.ErrorCode()
		}
	}
	return "other"
}
