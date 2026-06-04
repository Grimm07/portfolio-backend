package ip

import (
	"testing"
)

func TestExtractClientIP(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "cloudfront IPv4 with port",
			headers: map[string]string{"cloudfront-viewer-address": "198.51.100.10:46532"},
			want:    "198.51.100.10",
		},
		{
			name:    "cloudfront unbracketed IPv6 with port",
			headers: map[string]string{"cloudfront-viewer-address": "2001:db8::1:46532"},
			want:    "2001:db8::1",
		},
		{
			name:    "x-forwarded-for multiple entries",
			headers: map[string]string{"x-forwarded-for": "203.0.113.7, 70.41.3.18, 150.172.238.178"},
			want:    "203.0.113.7",
		},
		{
			name:    "no relevant headers",
			headers: map[string]string{},
			want:    "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractClientIP(tc.headers)
			if got != tc.want {
				t.Errorf("ExtractClientIP(%v) = %q; want %q", tc.headers, got, tc.want)
			}
		})
	}
}
