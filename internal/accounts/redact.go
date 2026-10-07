package accounts

import (
	"regexp"
	"strings"
)

// secret holds a credential value read from a profile store for the duration
// of one request. Every formatting path prints a placeholder, so a secret
// accidentally passed to a logger, error or JSON encoder never leaks.
type secret struct{ v string }

func (s secret) String() string               { return "[redacted]" }
func (s secret) GoString() string             { return "[redacted]" }
func (s secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (s secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }
func (s secret) empty() bool                  { return s.v == "" }

// reveal returns the raw value. Call it only where the value is placed
// directly into an outgoing Authorization header.
func (s secret) reveal() string { return s.v }

var (
	// JWTs, bearer values and long opaque token-looking runs.
	reJWT    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_.-]*`)
	reBearer = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',;]+`)
	reKV     = regexp.MustCompile(`(?i)("?(?:access_?token|refresh_?token|id_?token|accessToken|refreshToken|api_?key|authorization)"?\s*[:=]\s*)"?[^\s"',;}]+"?`)
	reOpaque = regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9_-]+|sk-[A-Za-z0-9_-]{16,}|pyre-synthetic-[A-Za-z0-9_-]+)\b`)
)

// redact scrubs credential-shaped substrings from text that may have come
// from a provider response, a subprocess or a file. It is a second line of
// defense: code paths never intentionally put credentials in messages.
func redact(s string) string {
	if s == "" {
		return s
	}
	s = reJWT.ReplaceAllString(s, "[redacted-jwt]")
	s = reBearer.ReplaceAllString(s, "${1}[redacted]")
	s = reKV.ReplaceAllString(s, "${1}[redacted]")
	s = reOpaque.ReplaceAllString(s, "[redacted]")
	return s
}

// truncate bounds diagnostic text.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
