package diagnostics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Rule names reported in Findings.
const (
	RulePyreToken  = "pyre-token"  // pyc_/pyn_/pys_/pyt_/pye_/pyp_ secrets
	RuleBearer     = "bearer"      // Authorization: Bearer <value>
	RuleOAuth      = "oauth"       // OAuth/provider-token-shaped strings
	RuleJWT        = "jwt"         // three-segment base64url JWT
	RulePrivateKey = "private-key" // PEM private key block
	RuleEmail      = "email"       // personal data, not a credential
	RuleKnown      = "known-value" // an exact secret value registered with the scanner
)

// Severity separates credentials from personal data so callers can decide
// what a given surface may contain (e.g. an owner-chosen account label).
type Severity string

const (
	SevSecret Severity = "secret"
	SevPII    Severity = "pii"
)

type rule struct {
	name string
	sev  Severity
	re   *regexp.Regexp
	// group selects the submatch that is the sensitive value (0 = whole).
	group int
}

// Patterns are deliberately conservative: a redacted marker such as
// "pyn_[REDACTED]" or "Bearer [REDACTED]" never matches.
var defaultRules = []rule{
	{RulePyreToken, SevSecret, regexp.MustCompile(`\bpy[cnstep]_[A-Za-z0-9_\-]{20,}`), 0},
	{RuleBearer, SevSecret, regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/=\-]{12,})`), 1},
	{RuleOAuth, SevSecret, regexp.MustCompile(`\b(?:` +
		`ya29\.[A-Za-z0-9_\-]{20,}` + // Google OAuth access token
		`|sk-ant-[A-Za-z0-9_\-]{20,}` + // Anthropic API / OAuth tokens
		`|sk-(?:proj-)?[A-Za-z0-9_\-]{32,}` + // OpenAI keys
		`|gh[pousr]_[A-Za-z0-9]{20,}` + // GitHub tokens
		`|github_pat_[A-Za-z0-9_]{20,}` +
		`|xox[abprs]-[A-Za-z0-9\-]{10,}` + // Slack
		`|AKIA[0-9A-Z]{16}` + // AWS access key id
		`|(?:sk|rk)_live_[A-Za-z0-9]{10,}` +
		`)`), 0},
	{RuleOAuth, SevSecret, regexp.MustCompile(`(?i)"(?:access_token|refresh_token|id_token|client_secret|oauth_token)"\s*:\s*"([^"\s]{8,})"`), 1},
	{RuleJWT, SevSecret, regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), 0},
	{RulePrivateKey, SevSecret, regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`), 0},
	{RuleEmail, SevPII, regexp.MustCompile(`\b[A-Za-z0-9._%+\-]{1,64}@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,24}\b`), 0},
}

// redactedValue reports whether a match is itself a redaction marker.
func redactedValue(v string) bool {
	return strings.Contains(v, "REDACTED") || strings.Contains(v, "redacted")
}

// Finding is one suspected secret or personal datum. It never contains the
// full matched value: Preview keeps at most the first four characters and
// Hash identifies the value for de-duplication.
type Finding struct {
	Source   string   `json:"source"`
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Offset   int64    `json:"offset"`
	Preview  string   `json:"preview"` // e.g. "pyn_…(47 bytes)"
	Hash     string   `json:"hash"`    // sha256 prefix of the value
}

func (f Finding) String() string {
	return fmt.Sprintf("%s@%d: %s %s [%s]", f.Source, f.Offset, f.Rule, f.Preview, f.Hash)
}

// Scanner greps arbitrary bytes (logs, SQLite files including -wal/-shm,
// event payloads, artifacts, HTTP bodies) for credentials and personal data.
// The zero value is not usable; call NewScanner.
type Scanner struct {
	rules []rule
	known map[string]string // value -> label
	allow []*regexp.Regexp
	// IgnorePII drops email findings (for surfaces that may legitimately
	// carry an owner-chosen address).
	IgnorePII bool
	// MaxFileBytes bounds ScanFile/ScanDir reads (default 256 MiB).
	MaxFileBytes int64
}

// NewScanner returns a scanner with the default rules.
func NewScanner() *Scanner {
	return &Scanner{rules: defaultRules, known: map[string]string{}, MaxFileBytes: 256 << 20}
}

// AddKnown registers exact secret values (e.g. tokens minted during a test)
// that must never appear anywhere. Values shorter than 8 bytes are ignored to
// avoid false positives.
func (s *Scanner) AddKnown(label string, values ...string) *Scanner {
	for _, v := range values {
		if len(v) >= 8 {
			s.known[v] = label
		}
	}
	return s
}

// Allow suppresses findings whose matched value matches re (e.g. synthetic
// test addresses like "@example.com").
func (s *Scanner) Allow(re string) *Scanner {
	s.allow = append(s.allow, regexp.MustCompile(re))
	return s
}

func (s *Scanner) allowed(v string) bool {
	for _, re := range s.allow {
		if re.MatchString(v) {
			return true
		}
	}
	return false
}

// Preview renders a value safely for reports.
func Preview(v string) string {
	p := v
	if len(p) > 4 {
		p = p[:4]
	}
	return fmt.Sprintf("%s…(%d bytes)", p, len(v))
}

func valueHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:6])
}

// ScanBytes scans b, attributing findings to source.
func (s *Scanner) ScanBytes(source string, b []byte) []Finding {
	var out []Finding
	seen := map[string]bool{}
	add := func(rule string, sev Severity, off int, v string) {
		key := fmt.Sprintf("%s|%d", rule, off)
		if seen[key] || s.allowed(v) {
			return
		}
		seen[key] = true
		out = append(out, Finding{Source: source, Rule: rule, Severity: sev, Offset: int64(off), Preview: Preview(v), Hash: valueHash(v)})
	}
	for v, label := range s.known {
		vb := []byte(v)
		for base, n := 0, 0; n < 1000; n++ {
			j := bytes.Index(b[base:], vb)
			if j < 0 {
				break
			}
			add(RuleKnown+":"+label, SevSecret, base+j, v)
			base += j + len(vb)
		}
	}
	for _, r := range s.rules {
		if r.sev == SevPII && s.IgnorePII {
			continue
		}
		for _, m := range r.re.FindAllSubmatchIndex(b, 1000) {
			lo, hi := m[0], m[1]
			if r.group > 0 && len(m) > 2*r.group+1 && m[2*r.group] >= 0 {
				lo, hi = m[2*r.group], m[2*r.group+1]
			}
			v := string(b[lo:hi])
			if redactedValue(v) {
				continue
			}
			add(r.name, r.sev, lo, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out
}

// ScanString is ScanBytes for strings.
func (s *Scanner) ScanString(source, text string) []Finding {
	return s.ScanBytes(source, []byte(text))
}

// ScanReader scans up to MaxFileBytes of r.
func (s *Scanner) ScanReader(source string, r io.Reader) ([]Finding, error) {
	b, err := io.ReadAll(io.LimitReader(r, s.MaxFileBytes))
	if err != nil {
		return nil, err
	}
	return s.ScanBytes(source, b), nil
}

// ScanFile scans one file. A missing file yields no findings and no error,
// so callers can pass "<db>-wal" and "<db>-shm" unconditionally.
func (s *Scanner) ScanFile(path string) ([]Finding, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return s.ScanReader(path, f)
}

// ScanSQLite scans a SQLite database file plus its -wal and -shm siblings
// (uncheckpointed pages live in the WAL).
func (s *Scanner) ScanSQLite(dbPath string) ([]Finding, error) {
	var out []Finding
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"} {
		f, err := s.ScanFile(p)
		if err != nil {
			return out, err
		}
		out = append(out, f...)
	}
	return out, nil
}

// ScanDir scans every regular file under root (symlinks are not followed).
func (s *Scanner) ScanDir(root string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := s.ScanFile(path)
		if err != nil {
			return err
		}
		out = append(out, f...)
		return nil
	})
	return out, err
}

// Secrets filters findings to SevSecret.
func Secrets(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Severity == SevSecret {
			out = append(out, f)
		}
	}
	return out
}

// Redact replaces every secret-severity match in text with a marker that
// keeps only a non-secret prefix (e.g. "pyn_[REDACTED]", "Bearer
// [REDACTED]"). Email addresses are replaced by "[EMAIL]". Known values
// registered on s are replaced too.
func (s *Scanner) Redact(text string) string {
	for v := range s.known {
		text = strings.ReplaceAll(text, v, "[REDACTED]")
	}
	for _, r := range s.rules {
		r := r
		text = r.re.ReplaceAllStringFunc(text, func(m string) string {
			if redactedValue(m) {
				return m
			}
			switch r.name {
			case RulePyreToken:
				return m[:4] + "[REDACTED]"
			case RuleBearer:
				sub := r.re.FindStringSubmatchIndex(m)
				if len(sub) >= 4 && sub[2] >= 0 {
					return m[:sub[2]] + "[REDACTED]"
				}
				return "[REDACTED]"
			case RuleOAuth:
				if sub := r.re.FindStringSubmatchIndex(m); r.group > 0 && len(sub) >= 4 && sub[2] >= 0 {
					return m[:sub[2]] + "[REDACTED]" + m[sub[3]:]
				}
				return "[REDACTED]"
			case RuleEmail:
				if s.IgnorePII {
					return m
				}
				return "[EMAIL]"
			}
			return "[REDACTED]"
		})
	}
	return text
}

// defaultScanner backs the package-level Redact.
var defaultScanner = NewScanner()

// Redact applies the default rules (see Scanner.Redact).
func Redact(text string) string { return defaultScanner.Redact(text) }
