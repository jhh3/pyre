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
// stderr, error messages, issue reports, transcript reads), not to provider
// payloads. Order matters: whole blocks and URLs first, then token shapes,
// then the name=value and header forms. Distinct prefixes carry no leading
// \b so a token glued to an identifier (MY_ghp_…) is still caught.
//
// may is a cheap test on the ASCII-lowercased text that is true whenever
// the pattern can match (it may also be true when it cannot): a pattern
// whose may is false is skipped, which keeps ordinary text cheap to scan
// (every transcript read is redacted).
var tokenPatterns = []struct {
	re   *regexp.Regexp
	repl string
	may  func(lower string) bool
}{
	// PEM private keys, through END or the end of the text.
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), Redacted, has("-----begin ")},
	// URL userinfo: scheme://user:password@host keeps scheme and host.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}" + Redacted + "@", hasAll("://", "@")},
	{regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`), Redacted, has("ghp_", "gho_", "ghu_", "ghs_", "ghr_")},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), Redacted, has("github_pat_")},
	{regexp.MustCompile(`xox[abprse]-[A-Za-z0-9-]{10,}`), Redacted, has("xox")}, // Slack bot/user/app tokens
	{regexp.MustCompile(`xapp-[A-Za-z0-9-]{10,}`), Redacted, has("xapp-")},
	// Anthropic, OpenAI keys; sk- must start a token (not "di-sk-…", "ask-…").
	{regexp.MustCompile(`(^|[^A-Za-z0-9_-])sk-(?:ant-|proj-|svcacct-)?[A-Za-z0-9_-]{20,}`), "${1}" + Redacted, startsToken("sk-")},
	{regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{10,}`), Redacted, has("_live_", "_test_")},
	{regexp.MustCompile(`lin_(?:api|oauth)_[A-Za-z0-9]{20,}`), Redacted, has("lin_")},
	{regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), Redacted, has("akia", "asia")}, // AWS access key IDs
	{regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), Redacted, has("aiza")},             // Google API keys
	{regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{20,}`), Redacted, has("ya29.")},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), Redacted, has("eyj")}, // JWT
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{12,}`), "$1 " + Redacted, followedBy(startsSpacedValue, "bearer", "basic", "token")},
	// NAME=value / "name": "value" where the name is a secret word or ends
	// in one after _ . or - (ANTHROPIC_API_KEY=…, GITHUB_TOKEN: …,
	// db_password=…, "client_secret": …). Not bare key/credentials, nor
	// counts like input_tokens: topic_key, sort_key, idempotency_key stay.
	{regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.-])((?:[A-Za-z0-9_.-]*[_.-])?(?:api[_-]?key|apikey|secret|token|password|passwd|access[_-]?key|private[_-]?key|secret[_-]?key))(["']?\s*[:=]\s*["']?)[^\s"',;]{6,}`), "${1}${2}${3}" + Redacted, followedBy(startsAssign, "key", "secret", "token", "password", "passwd")},
}

// foldRunes are the non-ASCII runes the case-insensitive patterns match as
// ASCII letters (long s as s, Kelvin sign as k; TestRedactFoldRunes checks
// there are no others). ASCII lowercasing keeps them, so text holding one
// runs every pattern.
const foldRunes = "\u017f\u212a"

// RedactPatterns replaces credential-shaped substrings in diagnostic text;
// everything else is kept byte for byte.
func RedactPatterns(s string) string {
	check := !strings.ContainsAny(s, foldRunes)
	lower := asciiLower(s)
	for _, p := range tokenPatterns {
		if check && !p.may(lower) {
			continue
		}
		if out := p.re.ReplaceAllString(s, p.repl); out != s {
			s, lower = out, asciiLower(out)
		}
	}
	return s
}

// pyreTokenRe matches Pyre's own prefixed secrets (as transport.Redact).
var pyreTokenRe = regexp.MustCompile(`py[cnstep]_[A-Za-z0-9_\-]{12,}`)

// RedactTranscript masks credentials in transcript text (sessions.read
// entries and screens): RedactPatterns, and Pyre's own tokens keep only
// their prefix (pyn_[REDACTED]). Other text is kept byte for byte, and
// redacting twice gives the same text.
func RedactTranscript(s string) string {
	s = RedactPatterns(s)
	if !strings.Contains(s, "py") {
		return s
	}
	return pyreTokenRe.ReplaceAllStringFunc(s, func(m string) string { return m[:4] + Redacted })
}

// asciiLower lowercases ASCII letters only: every byte keeps its offset
// and no other rune becomes an ASCII letter.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// has: the text contains any of subs.
func has(subs ...string) func(string) bool {
	return func(l string) bool {
		for _, sub := range subs {
			if strings.Contains(l, sub) {
				return true
			}
		}
		return false
	}
}

// hasAll: the text contains every one of subs.
func hasAll(subs ...string) func(string) bool {
	return func(l string) bool {
		for _, sub := range subs {
			if !strings.Contains(l, sub) {
				return false
			}
		}
		return true
	}
}

// startsToken: sub occurs at the start of the text or after a byte other
// than [a-z0-9_-].
func startsToken(sub string) func(string) bool {
	return func(l string) bool {
		for i := 0; ; {
			j := strings.Index(l[i:], sub)
			if j < 0 {
				return false
			}
			if k := i + j; k == 0 || !isWordDash(l[k-1]) {
				return true
			}
			i += j + 1
		}
	}
}

// followedBy: one of words occurs where rest is true of the text after it.
func followedBy(rest func(string) bool, words ...string) func(string) bool {
	return func(l string) bool {
		for _, w := range words {
			for i := 0; ; {
				j := strings.Index(l[i:], w)
				if j < 0 {
					break
				}
				if rest(l[i+j+len(w):]) {
					return true
				}
				i += j + 1
			}
		}
		return false
	}
}

// startsSpace: the text starts with \s (RE2: [\t\n\f\r ]).
func startsSpace(r string) bool {
	return r != "" && (r[0] == ' ' || r[0] == '\t' || r[0] == '\n' || r[0] == '\f' || r[0] == '\r')
}

// startsSpacedValue: the text starts with \s+[A-Za-z0-9._~+/=-]{12}.
func startsSpacedValue(r string) bool {
	if !startsSpace(r) {
		return false
	}
	for startsSpace(r) {
		r = r[1:]
	}
	n := 0
	for n < len(r) && n < 12 && (isWordDash(r[n]) || strings.IndexByte("._~+/=", r[n]) >= 0) {
		n++
	}
	return n == 12
}

// startsAssign: the text starts with ["']?\s*[:=].
func startsAssign(r string) bool {
	if r != "" && (r[0] == '"' || r[0] == '\'') {
		r = r[1:]
	}
	for startsSpace(r) {
		r = r[1:]
	}
	return r != "" && (r[0] == ':' || r[0] == '=')
}

func isWordDash(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '_' || b == '-'
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
