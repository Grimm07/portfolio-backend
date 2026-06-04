package email

import (
	"context"
	"strings"
	"testing"

	ses "github.com/aws/aws-sdk-go-v2/service/ses"
)

// fakeSES is a hand-written fake that satisfies SESAPI and captures the most
// recent SendEmailInput along with the total call count.
type fakeSES struct {
	callCount int
	lastInput *ses.SendEmailInput
}

func (f *fakeSES) SendEmail(_ context.Context, params *ses.SendEmailInput, _ ...func(*ses.Options)) (*ses.SendEmailOutput, error) {
	f.callCount++
	f.lastInput = params
	return &ses.SendEmailOutput{}, nil
}

func TestSendContactEmail(t *testing.T) {
	fake := &fakeSES{}
	args := ContactEmailArgs{
		From:      "noreply@trystan-tbm.dev",
		To:        "owner@example.com",
		ReplyTo:   "alice@b.co",
		Name:      "Alice",
		Email:     "alice@b.co",
		Message:   "hello there",
		IP:        "1.2.3.4",
		CreatedAt: "2026-06-03T00:00:00.000Z",
	}

	if err := SendContactEmail(context.Background(), fake, args); err != nil {
		t.Fatalf("SendContactEmail returned unexpected error: %v", err)
	}

	// SendEmail must be called exactly once.
	if fake.callCount != 1 {
		t.Fatalf("expected SendEmail called 1 time, got %d", fake.callCount)
	}

	input := fake.lastInput

	// Source
	if input.Source == nil || *input.Source != "noreply@trystan-tbm.dev" {
		t.Errorf("Source: got %v, want %q", input.Source, "noreply@trystan-tbm.dev")
	}

	// Destination.ToAddresses
	if len(input.Destination.ToAddresses) != 1 || input.Destination.ToAddresses[0] != "owner@example.com" {
		t.Errorf("Destination.ToAddresses: got %v, want [owner@example.com]", input.Destination.ToAddresses)
	}

	// ReplyToAddresses
	if len(input.ReplyToAddresses) != 1 || input.ReplyToAddresses[0] != "alice@b.co" {
		t.Errorf("ReplyToAddresses: got %v, want [alice@b.co]", input.ReplyToAddresses)
	}

	// Subject contains the sender name.
	subjectData := *input.Message.Subject.Data
	if !strings.Contains(subjectData, "Alice") {
		t.Errorf("Subject %q does not contain %q", subjectData, "Alice")
	}

	// Body contains message, IP, and timestamp.
	bodyData := *input.Message.Body.Text.Data
	for _, want := range []string{"hello there", "1.2.3.4", "2026-06-03T00:00:00.000Z"} {
		if !strings.Contains(bodyData, want) {
			t.Errorf("body %q does not contain %q", bodyData, want)
		}
	}
}
