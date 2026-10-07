package search

import (
	"strings"
	"unicode"

	"github.com/jhh3/pyre/internal/contracts"
)

// MaxQueryLen bounds a query (bytes).
const MaxQueryLen = 500

// Terms splits a user query into words and "quoted phrases". Terms without
// a letter or digit are dropped.
func Terms(q string) []string {
	if len(q) > MaxQueryLen {
		q = q[:MaxQueryLen]
	}
	var out []string
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t != "" && hasWord(t) {
			out = append(out, t)
		}
	}
	for {
		q = strings.TrimSpace(q)
		if q == "" {
			return out
		}
		if q[0] == '"' {
			end := strings.IndexByte(q[1:], '"')
			if end < 0 {
				add(strings.Trim(q, `"`))
				return out
			}
			add(q[1 : end+1])
			q = q[end+2:]
			continue
		}
		i := strings.IndexFunc(q, unicode.IsSpace)
		if i < 0 {
			add(strings.Trim(q, `"`))
			return out
		}
		add(strings.Trim(q[:i], `"`))
		q = q[i:]
	}
}

func hasWord(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// FTSQuery builds an FTS5 MATCH expression: every term must match (implicit
// AND); each term is quoted (so no user input is FTS syntax), and a final
// single word also matches as a prefix ("webh" finds "webhook").
func FTSQuery(q string) string {
	terms := Terms(q)
	parts := make([]string, 0, len(terms))
	for i, t := range terms {
		s := `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		if i == len(terms)-1 && !strings.ContainsFunc(t, unicode.IsSpace) && len([]rune(t)) >= 3 && !strings.HasSuffix(q, " ") && !strings.HasSuffix(q, `"`) {
			s += "*"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// Matches reports whether text contains every term (case-insensitive).
// Used where no FTS index exists (hub titles, the mock backend).
func Matches(text string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	lt := strings.ToLower(text)
	for _, t := range terms {
		if !strings.Contains(lt, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

// Snippet cuts a window of about width runes around the first term match
// and marks every term occurrence with contracts.SearchMarkOpen/Close.
func Snippet(text string, terms []string, width int) string {
	text = strings.Join(strings.Fields(text), " ")
	rs := []rune(text)
	lower := []rune(strings.ToLower(text))
	if len(lower) != len(rs) {
		lower = rs // case folding changed lengths; match case-sensitively
	}
	first := -1
	for _, t := range terms {
		if i := runeIndex(lower, []rune(strings.ToLower(t))); i >= 0 && (first < 0 || i < first) {
			first = i
		}
	}
	start, end := 0, len(rs)
	if width > 0 && len(rs) > width {
		if first > width/3 {
			start = first - width/3
		}
		end = min(len(rs), start+width)
		if end-start < width {
			start = max(0, end-width)
		}
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	for i := start; i < end; {
		hit := 0
		for _, t := range terms {
			tr := []rune(strings.ToLower(t))
			if len(tr) > 0 && i+len(tr) <= len(lower) && string(lower[i:i+len(tr)]) == string(tr) && len(tr) > hit {
				hit = len(tr)
			}
		}
		if hit > 0 {
			b.WriteString(contracts.SearchMarkOpen)
			b.WriteString(string(rs[i:min(i+hit, len(rs))]))
			b.WriteString(contracts.SearchMarkClose)
			i += hit
			continue
		}
		b.WriteRune(rs[i])
		i++
	}
	if end < len(rs) {
		b.WriteString("…")
	}
	return b.String()
}

func runeIndex(s, sub []rune) int {
	if len(sub) == 0 {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if string(s[i:i+len(sub)]) == string(sub) {
			return i
		}
	}
	return -1
}

// MarkedTerms returns the distinct marked substrings of a snippet, longest
// first (readers highlight and jump with them).
func MarkedTerms(snippet string) []string {
	var out []string
	seen := map[string]bool{}
	for {
		i := strings.Index(snippet, contracts.SearchMarkOpen)
		if i < 0 {
			break
		}
		snippet = snippet[i+len(contracts.SearchMarkOpen):]
		j := strings.Index(snippet, contracts.SearchMarkClose)
		if j < 0 {
			break
		}
		t := snippet[:j]
		snippet = snippet[j+len(contracts.SearchMarkClose):]
		if k := strings.ToLower(t); t != "" && !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j]) > len(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// PlainSnippet removes the match markers.
func PlainSnippet(s string) string {
	return strings.NewReplacer(contracts.SearchMarkOpen, "", contracts.SearchMarkClose, "").Replace(s)
}
