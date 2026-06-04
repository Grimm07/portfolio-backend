package validation_test

import (
	"strings"
	"testing"

	"github.com/Grimm07/portfolio-backend/backend/internal/validation"
)

func TestIsHoneypotTripped(t *testing.T) {
	tests := []struct {
		name    string
		website string
		want    bool
	}{
		{"empty string", "", false},
		{"whitespace only", "   ", false},
		{"filled", "http://spam", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validation.IsHoneypotTripped(tc.website); got != tc.want {
				t.Errorf("IsHoneypotTripped(%q) = %v, want %v", tc.website, got, tc.want)
			}
		})
	}
}

func TestIsTooFast(t *testing.T) {
	const now = int64(1_000_000)
	tests := []struct {
		name          string
		formTimestamp int64
		want          bool
	}{
		{
			name:          "just under minimum — rejected",
			formTimestamp: now - (validation.MinFormTimeMs - 1),
			want:          true,
		},
		{
			name:          "exactly at minimum — allowed",
			formTimestamp: now - validation.MinFormTimeMs,
			want:          false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validation.IsTooFast(tc.formTimestamp, now); got != tc.want {
				t.Errorf("IsTooFast(%d, %d) = %v, want %v", tc.formTimestamp, now, got, tc.want)
			}
		})
	}
}

func TestIsValidEmail(t *testing.T) {
	oversized := strings.Repeat("x", 255) + "@b.co"
	tests := []struct {
		name  string
		email string
		want  bool
	}{
		{"normal address", "a@b.co", true},
		{"no at-sign", "nope", false},
		{"missing TLD", "a@b", false},
		{"oversized local part", oversized, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validation.IsValidEmail(tc.email); got != tc.want {
				t.Errorf("IsValidEmail(%q) = %v, want %v", tc.email, got, tc.want)
			}
		})
	}
}

func TestIsValidMessage(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    bool
	}{
		{"valid non-empty", "hello there", true},
		{"empty string", "", false},
		{"over MaxMessage", strings.Repeat("a", validation.MaxMessage+1), false},
		{"exactly MaxMessage", strings.Repeat("a", validation.MaxMessage), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validation.IsValidMessage(tc.message); got != tc.want {
				t.Errorf("IsValidMessage(%q...) = %v, want %v", tc.message[:min(len(tc.message), 20)], got, tc.want)
			}
		})
	}
}

func TestSanitizeName(t *testing.T) {
	t.Run("strips CR LF tab and control chars", func(t *testing.T) {
		cases := []struct{ in, want string }{
			{"Alice\nBcc: evil@x.com", "AliceBcc: evil@x.com"},
			{"A\r\nB\tC", "ABC"},
		}
		for _, c := range cases {
			if got := validation.SanitizeName(c.in); got != c.want {
				t.Errorf("SanitizeName(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	})
	t.Run("caps at MaxName runes", func(t *testing.T) {
		result := validation.SanitizeName(strings.Repeat("a", 500))
		runes := []rune(result)
		if len(runes) != validation.MaxName {
			t.Errorf("SanitizeName(500×a) rune length = %d, want %d", len(runes), validation.MaxName)
		}
	})
}

// min is a local helper for older Go toolchains (pre-1.21 builtin).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
