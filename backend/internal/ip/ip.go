// Package ip ports the ingest/ip.ts client-IP extraction logic to Go.
package ip

import (
	"strings"
)

// stripPort removes the port suffix from value by cutting after the last colon.
// This is correct for both IPv4 ("198.51.100.10:46532" → "198.51.100.10") and
// unbracketed IPv6 as sent by CloudFront ("2001:db8::1:46532" → "2001:db8::1").
// If no colon is present, value is returned unchanged.
func stripPort(value string) string {
	i := strings.LastIndex(value, ":")
	if i == -1 {
		return value
	}
	return value[:i]
}

// ExtractClientIP reads the client IP from the request headers map.
// Header lookup treats an empty string as absent (mirrors JS truthiness).
//
// Priority:
//  1. cloudfront-viewer-address — CloudFront injects "<ip>:<port>"; IPv6 is
//     unbracketed, so we strip the last colon-delimited segment to obtain the
//     address.
//  2. x-forwarded-for — the first comma-separated entry is the originating IP;
//     no port is present in this header.
//  3. Falls back to "unknown".
func ExtractClientIP(headers map[string]string) string {
	if v := headers["cloudfront-viewer-address"]; v != "" {
		return stripPort(strings.TrimSpace(v))
	}
	if v := headers["x-forwarded-for"]; v != "" {
		parts := strings.SplitN(v, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	return "unknown"
}
