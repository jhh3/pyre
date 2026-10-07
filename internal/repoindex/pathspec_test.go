package repoindex

import (
	"reflect"
	"testing"
)

func TestSplitTarget(t *testing.T) {
	for _, tc := range []struct {
		line, target string
		rest         []string
		open         bool
	}{
		{"", "", nil, true},
		{"studio", "studio", nil, true},
		{"studio ", "studio", nil, false},
		{"  api+web @server codex fix it", "api+web", []string{"@server", "codex", "fix", "it"}, false},
		{`~/My\ Docs/ codex`, "~/My Docs/", []string{"codex"}, false},
		{`~/My\ Do`, "~/My Do", nil, true},
		{`"~/My Docs" shell don't`, "~/My Docs", []string{"shell", "don't"}, false},
		{`~/"My Docs"/sub`, "~/My Docs/sub", nil, true},
		{`'~/it"s here'`, `~/it"s here`, nil, true},
		{`"~/unterminated quote`, "~/unterminated quote", nil, true},
		{`"a \"b\""`, `a "b"`, nil, true},
		{`~/trailing\`, `~/trailing\`, nil, true},
	} {
		target, rest, open := SplitTarget(tc.line)
		if target != tc.target || !reflect.DeepEqual(rest, tc.rest) || open != tc.open {
			t.Errorf("SplitTarget(%q) = %q %q %v; want %q %q %v", tc.line, target, rest, open, tc.target, tc.rest, tc.open)
		}
	}
}

func TestQuoteTargetRoundTrip(t *testing.T) {
	for _, s := range []string{"studio", "~/My Docs", `~/a'b"c\d e`, "/tmp/x"} {
		q := QuoteTarget(s)
		if got, rest, open := SplitTarget(q); got != s || len(rest) != 0 || !open {
			t.Errorf("QuoteTarget(%q) = %q reads back as %q %q %v", s, q, got, rest, open)
		}
	}
}
