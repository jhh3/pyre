package configuration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Setting keys are "<harness>.<path>" where path is a dotted path into the
// harness's own settings document: "claude.permissions.defaultMode" is
// settings.json {"permissions":{"defaultMode":...}}, "codex.model" is
// config.toml model. Segments containing '.', '"', whitespace or nothing are
// written in double quotes: claude.enabledPlugins."a.b@m".

// SplitKey parses a setting key into segments.
func SplitKey(key string) ([]string, error) {
	var segs []string
	i := 0
	for i <= len(key) {
		if i == len(key) {
			return nil, fmt.Errorf("key %q: trailing dot", key)
		}
		var seg string
		if key[i] == '"' {
			j := i + 1
			var b strings.Builder
			for ; j < len(key) && key[j] != '"'; j++ {
				if key[j] == '\\' && j+1 < len(key) {
					j++
				}
				b.WriteByte(key[j])
			}
			if j >= len(key) {
				return nil, fmt.Errorf("key %q: unterminated quote", key)
			}
			seg = b.String()
			i = j + 1
		} else {
			j := strings.IndexByte(key[i:], '.')
			if j < 0 {
				j = len(key) - i
			}
			seg = key[i : i+j]
			if seg == "" {
				return nil, fmt.Errorf("key %q: empty segment", key)
			}
			i += j
		}
		segs = append(segs, seg)
		if i == len(key) {
			break
		}
		if key[i] != '.' {
			return nil, fmt.Errorf("key %q: expected '.' after quoted segment", key)
		}
		i++
	}
	return segs, nil
}

// JoinKey renders segments as a setting key.
func JoinKey(segs ...string) string {
	var b strings.Builder
	for i, s := range segs {
		if i > 0 {
			b.WriteByte('.')
		}
		if needsQuote(s) {
			b.WriteByte('"')
			b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s))
			b.WriteByte('"')
		} else {
			b.WriteString(s)
		}
	}
	return b.String()
}

func needsQuote(s string) bool {
	return s == "" || strings.ContainsAny(s, ". \t\"\\")
}

// harnessOf returns the harness a key belongs to and the remaining path.
func harnessOf(key string) (contracts.Harness, []string, error) {
	segs, err := SplitKey(key)
	if err != nil {
		return "", nil, err
	}
	if len(segs) < 2 {
		return "", nil, fmt.Errorf("key %q: want <claude|codex>.<path>", key)
	}
	switch contracts.Harness(segs[0]) {
	case contracts.HarnessClaude, contracts.HarnessCodex:
		return contracts.Harness(segs[0]), segs[1:], nil
	}
	return "", nil, fmt.Errorf("key %q: unknown harness %q (want claude or codex)", key, segs[0])
}

// normalize converts decoded JSON/TOML values into a canonical JSON-compatible
// form (maps, slices, string, bool, int64, float64, nil).
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = normalize(x)
		}
		return m
	case []map[string]any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = normalize(x)
		}
		return s
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = normalize(x)
		}
		return s
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t)
		}
		return t
	case time.Time:
		return t.Format(time.RFC3339Nano)
	case *omap:
		return t.plain()
	}
	return v
}

// ParseValue interprets a session/CLI override string: JSON literals
// (true, 3, ["a"], {"k":1}) decode as JSON, anything else is a string.
func ParseValue(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
	switch t[0] {
	case '{', '[', '"', 't', 'f', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		dec := json.NewDecoder(strings.NewReader(t))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) == nil && !dec.More() {
			return normalize(v)
		}
	}
	return s
}

// renderValue renders a value for ResolvedValue.Value.
func renderValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// canon returns a canonical encoding used for equality and digests.
func canon(v any) string {
	b, _ := json.Marshal(v) // encoding/json sorts map keys
	return string(b)
}

func equalValues(a, b any) bool { return canon(a) == canon(b) }

// flatten turns a nested document into leaf keys under prefix. Arrays are
// leaves. skip reports subtrees handled elsewhere (hooks, MCP, plugins) or
// harness-internal state.
func flatten(prefix []string, v any, skip func([]string) bool, out map[string]any) {
	if skip != nil && len(prefix) > 1 && skip(prefix[1:]) {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		out[JoinKey(prefix...)] = v
		return
	}
	if len(m) == 0 && len(prefix) > 1 {
		out[JoinKey(prefix...)] = map[string]any{}
		return
	}
	for k, x := range m {
		p := append(append([]string(nil), prefix...), k)
		flatten(p, x, skip, out)
	}
}

// setPath sets segs in a nested plain map, creating intermediate maps.
func setPath(m map[string]any, segs []string, v any) error {
	for i, s := range segs {
		if i == len(segs)-1 {
			m[s] = v
			return nil
		}
		next, ok := m[s].(map[string]any)
		if !ok {
			if _, exists := m[s]; exists {
				return fmt.Errorf("%s is not a table", JoinKey(segs[:i+1]...))
			}
			next = map[string]any{}
			m[s] = next
		}
		m = next
	}
	return nil
}

// --- secret detection and redaction ---------------------------------------

// Redacted replaces sensitive values in ResolvedValue and diffs.
const Redacted = "[redacted]"

var (
	secretKeyRe   = regexp.MustCompile(`(?i)(token|secret|passw(or)?d|api[_-]?key|apikey|credential|bearer|cookie|private[_-]?key|session[_-]?key|access[_-]?key|auth[_-]?header|^authorization$)`)
	secretValueRe = regexp.MustCompile(`^(sk-[A-Za-z0-9_-]{8,}|sk-ant-[A-Za-z0-9_-]+|gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|xox[abprs]-[A-Za-z0-9-]{8,}|AKIA[0-9A-Z]{12,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\..*|(?i:bearer)\s+\S{8,}|glpat-[A-Za-z0-9_-]{16,}|-----BEGIN [A-Z ]*PRIVATE KEY-----.*)$`)
	opaqueTokenRe = regexp.MustCompile(`^[A-Za-z0-9_\-+/=]{40,}$`)
)

// SensitiveKey reports whether a key path names secret material.
func SensitiveKey(key string) bool {
	segs, err := SplitKey(key)
	if err != nil {
		segs = []string{key}
	}
	for _, s := range segs {
		if secretKeyRe.MatchString(s) {
			return true
		}
	}
	return false
}

// SensitiveValue reports whether a value looks like a credential.
func SensitiveValue(v any) bool {
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		return secretValueRe.MatchString(s) || (opaqueTokenRe.MatchString(s) && hasDigitAndLetter(s))
	case []any:
		for _, x := range t {
			if SensitiveValue(x) {
				return true
			}
		}
	case map[string]any:
		for k, x := range t {
			if secretKeyRe.MatchString(k) || SensitiveValue(x) {
				return true
			}
		}
	}
	return false
}

func hasDigitAndLetter(s string) bool {
	var d, l bool
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			d = true
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			l = true
		}
	}
	return d && l
}

// redactValue renders v for display, redacting sensitive content.
func redactValue(key string, v any) string {
	if SensitiveKey(key) || SensitiveValue(v) {
		return Redacted
	}
	return renderValue(v)
}

// --- TOML rendering for Codex -c overlays ----------------------------------

// tomlValue renders v as an inline TOML value.
func tomlValue(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", fmt.Errorf("TOML has no null")
	case string:
		return tomlString(t), nil
	case bool:
		return strconv.FormatBool(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case int:
		return strconv.Itoa(t), nil
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			s, err := tomlValue(x)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case []string:
		a := make([]any, len(t))
		for i, s := range t {
			a[i] = s
		}
		return tomlValue(a)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			s, err := tomlValue(t[k])
			if err != nil {
				return "", err
			}
			parts = append(parts, tomlKey(k)+" = "+s)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, s := range t {
			m[k] = s
		}
		return tomlValue(m)
	}
	return "", fmt.Errorf("unsupported TOML value %T", v)
}

var bareKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKey(k string) string {
	if bareKeyRe.MatchString(k) {
		return k
	}
	return tomlString(k)
}

// tomlString renders a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// codexOverridePath renders a -c key. Codex splits the key on '.', so a
// segment containing '.' cannot be addressed; ok=false reports that.
func codexOverridePath(segs []string) (string, bool) {
	for _, s := range segs {
		if s == "" || strings.ContainsAny(s, ".= \t\"") {
			return "", false
		}
	}
	return strings.Join(segs, "."), true
}

// prettyJSON encodes v with two-space indentation and a trailing newline.
func prettyJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return buf.Bytes()
}
