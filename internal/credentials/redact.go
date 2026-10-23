package credentials

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// minKnownLen is the shortest known secret value the Redactor will replace;
// shorter values would cause destructive false positives in ordinary text.
const minKnownLen = 6

// tokenPatterns match common credential shapes, each with its
// replacement. They are applied to diagnostic and agent-written text (tool
// stderr, error messages, issue reports), not to provider payloads. Order
// matters: whole blocks and URLs first, then token shapes, then the
// name=value and header forms. Distinct prefixes carry no leading \b so a
// token glued to an identifier (MY_ghp_…) is still caught.
var tokenPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private keys, through END or the end of the text.
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), Redacted},
	// URL userinfo: scheme://user:password@host keeps scheme and host.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}" + Redacted + "@"},
	{regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`), Redacted},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), Redacted},
	{regexp.MustCompile(`xox[abprse]-[A-Za-z0-9-]{10,}`), Redacted}, // Slack bot/user/app tokens
	{regexp.MustCompile(`xapp-[A-Za-z0-9-]{10,}`), Redacted},
	// Anthropic, OpenAI keys; sk- must start a token (not "di-sk-…", "ask-…").
	{regexp.MustCompile(`(^|[^A-Za-z0-9_-])sk-(?:ant-|proj-|svcacct-)?[A-Za-z0-9_-]{20,}`), "${1}" + Redacted},
	{regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{10,}`), Redacted},
	{regexp.MustCompile(`lin_(?:api|oauth)_[A-Za-z0-9]{20,}`), Redacted},
	{regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), Redacted}, // AWS access key IDs
	{regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), Redacted},     // Google API keys
	{regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{20,}`), Redacted},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), Redacted}, // JWT
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{12,}`), "$1 " + Redacted},
	// NAME=value / "name": "value" where the name is a secret word or ends
	// in one after _ . or - (ANTHROPIC_API_KEY=…, GITHUB_TOKEN: …,
	// db_password=…, "client_secret": …). Not bare key/credentials, nor
	// counts like input_tokens: topic_key, sort_key, idempotency_key stay.
	{regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.-])((?:[A-Za-z0-9_.-]*[_.-])?(?:api[_-]?key|apikey|secret|token|password|passwd|access[_-]?key|private[_-]?key|secret[_-]?key))(["']?\s*[:=]\s*["']?)[^\s"',;]{6,}`), "${1}${2}${3}" + Redacted},
}

// RedactPatterns replaces credential-shaped substrings in diagnostic text.
func RedactPatterns(s string) string {
	for _, p := range tokenPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// Redactor replaces known secret values (and credential-shaped patterns) in
// text before it reaches logs, errors, messages, or reports. It is safe for
// concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values [][]byte
}

// NewRedactor returns a redactor seeded with known secrets.
func NewRedactor(secrets ...Secret) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		r.Add(s)
	}
	return r
}

// Add registers a known secret value.
func (r *Redactor) Add(s Secret) {
	if s.Len() < minKnownLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, s.Bytes())
	// Longest first so a value containing another is replaced whole.
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

// String redacts known values and patterns.
func (r *Redactor) String(s string) string {
	if r != nil {
		r.mu.RLock()
		for _, v := range r.values {
			s = strings.ReplaceAll(s, string(v), Redacted)
		}
		r.mu.RUnlock()
	}
	return RedactPatterns(s)
}

// Bytes redacts known values only (no pattern rewriting), suitable for
// structured payloads where pattern rewriting could corrupt data.
func (r *Redactor) Bytes(b []byte) []byte {
	if r == nil {
		return b
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.values {
		b = bytes.ReplaceAll(b, v, []byte(Redacted))
	}
	return b
}

// Contains reports whether text contains any known value.
func (r *Redactor) Contains(text []byte) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.values {
		if bytes.Contains(text, v) {
			return true
		}
	}
	return false
}

// EnvNames returns the sorted variable names of env (never values), for
// manifests and diagnostics.
func EnvNames(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Bounded truncates diagnostic text to n bytes on a rune boundary.
func Bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// Diagnostic prepares untrusted tool output for inclusion in an error:
// trimmed, redacted, single-line, and bounded.
func Diagnostic(r *Redactor, raw []byte) string {
	s := strings.TrimSpace(string(raw))
	s = strings.Join(strings.Fields(s), " ")
	return Bounded(r.String(s), 300)
}
