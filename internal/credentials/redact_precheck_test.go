package credentials

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
)

// redactEvery is RedactPatterns without the prechecks: every pattern runs.
func redactEvery(s string) string {
	for _, p := range tokenPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// The prechecks only skip patterns that cannot match: on random text built
// from the patterns' own pieces the result equals running every pattern.
func TestRedactPrecheckSkipsOnlyNonMatches(t *testing.T) {
	pieces := []string{
		"sk-", "ant-", "proj-", "ghp_", "gho_", "github_pat_", "xoxb-", "xapp-", "_live_", "sk_test_", "lin_api_", "AKIA", "ASIA", "AIza", "ya29.",
		"eyJ", "eyJabcdefghijk.", "abcdefghijk.", "Bearer", "bearer", "BASIC", "Token", "token", "TOKEN", "key", "KEY", "api_key", "ApiKey", "secret", "Secret", "password", "PASSWD",
		"https://", "user:pw@", "@", "://", "-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----",
		"=", ":", " ", "  ", "\n", "\t", "\r", "\"", "'", "_", "-", ".", ",", ";", "/", "+", "~",
		"0123456789", "abcdefghij", "ABCDEFGHIJKLMNOP", "x", "Q", "é", "İ", "K", "ſ", "日本", "input_tokens", "topic_key",
	}
	r := rand.New(rand.NewSource(1))
	matched := make([]int, len(tokenPatterns))
	for i := 0; i < 40000; i++ {
		var b strings.Builder
		for n := 1 + r.Intn(24); n > 0; n-- {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		s := b.String()
		if got, want := RedactPatterns(s), redactEvery(s); got != want {
			t.Fatalf("%q: prechecked %q, every pattern %q", s, got, want)
		}
		lower := asciiLower(s)
		for k, p := range tokenPatterns {
			if !p.re.MatchString(s) {
				continue
			}
			matched[k]++
			if !strings.ContainsAny(s, foldRunes) && !p.may(lower) {
				t.Fatalf("pattern %d matches %q but its precheck says it cannot", k, s)
			}
		}
	}
	for k, n := range matched {
		if n < 20 {
			t.Errorf("pattern %d matched only %d random texts: the test does not exercise it", k, n)
		}
	}
}

// foldRunes lists every non-ASCII rune that case folding equates with an
// ASCII letter (the only way ASCII lowercasing could hide a match).
func TestRedactFoldRunes(t *testing.T) {
	var got []rune
	for r := rune(0x80); r <= unicode.MaxRune; r++ {
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < 0x80 {
				got = append(got, r)
				break
			}
		}
	}
	if string(got) != foldRunes {
		t.Fatalf("runes folding to ASCII: %q, foldRunes %q", string(got), foldRunes)
	}
}

// Transcript text without a secret-shaped substring is returned byte for
// byte; secrets of each kind are masked, and masking twice changes nothing.
func TestRedactTranscriptOrdinaryText(t *testing.T) {
	for _, plain := range []string{
		"",
		"I ran `go test ./internal/daemon/ -run TestExpandFull` and it passed in 0.8s.\n\nNext: the token budget, the key ordering and the password policy doc.",
		"## Summary\n\n- tail reads: unchanged\n- full reads: 256 KB per entry\n\n```go\nfunc f(key string) error { return nil }\n```\n",
		`{"input_tokens": 123456, "output_tokens": 789, "topic_key": "inbox-asks-dedupe", "cache_key": null}`,
		"see https://example.invalid/synth/pull/77 and mail synth@example.invalid; ssh git@github.com:jhh3/x.git",
		"task-runner and ask-owner and disk-cleanup: risk-assessment-for-the-rollout-plan",
		"Unicode: héllo wörld 日本語 İstanbul — “quotes” and emoji 🔥\r\nCRLF line\ttab",
		"Bearer of bad news; basic idea; token ring",
		"session ses_01JSYNTH0000000000000000AB run run_01JSYNTH; pyre read ses_x --full; python3 py_compile",
		"command: sed -n 1,80p internal/credentials/redact.go (exit 0)",
	} {
		if out := RedactTranscript(plain); out != plain {
			t.Errorf("ordinary text changed:\n%q\n->\n%q", plain, out)
		}
	}
	for _, secret := range []string{
		"export ANTHROPIC_API_KEY=sk-ant-api03-SYNTHETICsyntheticSYNTHETIC00",
		"curl -H 'Authorization: Bearer synthetic.bearer.value123'",
		"PYRE_SESSION_TOKEN=pys_synthetic0123456789abcdef",
		"token pyn_synthetic0123456789abcdef in the log",
		"GITHUB_TOKEN: ghp_SYNTHETICsyntheticSYNTHETIC1234",
	} {
		out := RedactTranscript(secret)
		if !strings.Contains(out, Redacted) || strings.Contains(out, "synthetic0123456789") || strings.Contains(out, "SYNTHETICsynthetic") || strings.Contains(out, "bearer.value123") {
			t.Errorf("%q -> %q", secret, out)
		}
		if again := RedactTranscript(out); again != out {
			t.Errorf("redacting twice changed %q to %q", out, again)
		}
	}
}
