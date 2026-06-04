// Package validation ports the shared/validation.ts rules to Go.
package validation

import (
	"regexp"
	"unicode/utf8"
)

// Field-size and timing constants mirroring shared/validation.ts.
const (
	MinFormTimeMs = 3000
	MaxName       = 200
	MaxMessage    = 5000
	MaxEmail      = 254
)

// emailRe mirrors the TypeScript EMAIL_RE: /^[^\s@]+@[^\s@]+\.[^\s@]+$/
var emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// controlRe matches all C0 control characters (0x00–0x1f) and DEL (0x7f).
// This covers \r, \n, \t because they all fall below 0x20.
var controlRe = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// IsHoneypotTripped returns true when the honeypot website field contains
// non-whitespace content, indicating a bot submission.
// Mirrors TS: !!website && website.trim().length > 0
func IsHoneypotTripped(website string) bool {
	for _, r := range website {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			// whitespace — keep scanning
		default:
			return true
		}
	}
	return false
}

// IsTooFast returns true when the elapsed time since the form was rendered
// (formTimestamp, ms epoch) is less than MinFormTimeMs milliseconds.
func IsTooFast(formTimestamp, now int64) bool {
	return now-formTimestamp < MinFormTimeMs
}

// IsValidEmail returns true when email is syntactically valid and within the
// MaxEmail rune-count limit. Rune count mirrors JavaScript's string.length
// semantics (JS uses UTF-16 code units, but for ASCII-only email addresses
// the counts are equivalent; we use rune count as the closest safe analogue).
func IsValidEmail(email string) bool {
	return utf8.RuneCountInString(email) <= MaxEmail && emailRe.MatchString(email)
}

// IsValidMessage returns true when message is non-empty and at most MaxMessage
// runes long.
func IsValidMessage(message string) bool {
	n := utf8.RuneCountInString(message)
	return n > 0 && n <= MaxMessage
}

// SanitizeName strips all C0 control characters (0x00–0x1f) and DEL (0x7f)
// from name, then caps the result to MaxName runes.
func SanitizeName(name string) string {
	clean := controlRe.ReplaceAllString(name, "")
	runes := []rune(clean)
	if len(runes) > MaxName {
		runes = runes[:MaxName]
	}
	return string(runes)
}
