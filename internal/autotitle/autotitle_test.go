package autotitle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestFromPrompt(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"fix the synthetic webhook retry bug. Then run the tests and report back", "Fix the synthetic webhook retry bug"},
		{"[pyre-msg:msg_01SYNTH] from owner:cli_synth (owner):\nplease refactor the synthetic billing module into smaller files", "Please refactor the synthetic billing module"},
		{"<system-reminder>synthetic context</system-reminder>\nwhy does the synthetic build fail?", "Why does the synthetic build fail"},
		{"/clear", ""},
		{"<command-name>/model</command-name>", ""},
		{"## Task\n\n**Add** dark mode to the synthetic settings page", "Add dark mode to the synthetic"},
		{"rotate the synthetic webhook secret and redeploy it", "Rotate the synthetic webhook secret"},
		{"look at https://example.invalid/x and summarize", "Look at and summarize"},
		{"   ", ""},
	} {
		if got := FromPrompt(tc.in); got != tc.want {
			t.Errorf("FromPrompt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l)
	}
}

// Synthetic Claude 2.1.29x transcript: the first prompt names the session
// after the first turn; Claude's ai-title replaces it; a custom-title (the
// person ran /rename in Claude) beats both. Reads are incremental and a
// partial last line is re-read once complete.
func TestClaudeTranscriptTitles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "synthetic-session.jsonl")
	write(t, p,
		`{"type":"permission-mode","permissionMode":"default","sessionId":"synthetic"}`+"\n",
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>synthetic</local-command-caveat>"}}`+"\n",
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"add retry backoff to the synthetic uploader please"}]}}`+"\n",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"synthetic reply"}]}}`+"\n",
		`{"type":"user","message":{"role":"user","content":"second synthetic prompt"}}`+"\n")
	s := NewScanner(FormatClaude)
	s.Feed([]string{p})
	if title, src := s.Best(); title != "Add retry backoff to the synthetic" || src != SourcePrompt {
		t.Fatalf("after first turn: %q %q", title, src)
	}
	write(t, p, `{"type":"ai-title","aiTitle":"Synthetic uploader retry backoff","sessionId":"synthetic"}`) // no newline yet
	s.Feed([]string{p})
	if _, src := s.Best(); src != SourcePrompt {
		t.Fatal("partial line consumed")
	}
	write(t, p, "\n")
	s.Feed([]string{p})
	if title, src := s.Best(); title != "Synthetic uploader retry backoff" || src != SourceNative {
		t.Fatalf("ai-title: %q %q", title, src)
	}
	write(t, p, `{"type":"custom-title","customTitle":"My synthetic name","sessionId":"synthetic"}`+"\n",
		`{"type":"ai-title","aiTitle":"Later synthetic title","sessionId":"synthetic"}`+"\n")
	s.Feed([]string{p})
	if title, src := s.Best(); title != "My synthetic name" || src != SourceCustom {
		t.Fatalf("custom-title: %q %q", title, src)
	}
}

func TestClaudeOversizedLineSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.jsonl")
	write(t, p, `{"type":"user","message":{"role":"user","content":"`+strings.Repeat("x", maxLine+10)+`"}}`+"\n",
		`{"type":"ai-title","aiTitle":"Synthetic after big line"}`+"\n")
	s := NewScanner(FormatClaude)
	s.Feed([]string{p})
	if title, _ := s.Best(); title != "Synthetic after big line" {
		t.Fatalf("got %q", title)
	}
}

// Synthetic Codex rollout: preamble response_items are ignored, the first
// user_message names the session, then the thread name (rollout event or
// $CODEX_HOME/session_index.jsonl) replaces it.
func TestCodexTranscriptTitles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "05")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "rollout-2026-10-05T00-00-00-synthetic.jsonl")
	write(t, p,
		`{"type":"session_meta","payload":{"id":"thr_synthetic","cwd":"/synthetic"}}`+"\n",
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>synthetic</environment_context>"}]}}`+"\n",
		`{"type":"event_msg","payload":{"type":"user_message","message":"Port the synthetic parser to Go and add tests"}}`+"\n")
	s := NewScanner(FormatCodex)
	s.Feed([]string{p})
	if title, src := s.Best(); title != "Port the synthetic parser to Go" || src != SourcePrompt {
		t.Fatalf("first prompt: %q %q", title, src)
	}
	write(t, filepath.Join(home, "session_index.jsonl"),
		`{"id":"thr_other","thread_name":"Not this one","updated_at":"2026-10-05T00:00:00Z"}`+"\n",
		`{"id":"thr_synthetic","thread_name":"Synthetic parser port","updated_at":"2026-10-05T00:00:01Z"}`+"\n")
	s.Feed([]string{p})
	if title, src := s.Best(); title != "Synthetic parser port" || src != SourceNative {
		t.Fatalf("session_index thread name: %q %q", title, src)
	}
	s2 := NewScanner(FormatCodex)
	p2 := filepath.Join(t.TempDir(), "rollout.jsonl")
	write(t, p2, `{"type":"event_msg","payload":{"type":"thread_name_updated","thread_name":"Synthetic from event"}}`+"\n")
	s2.Feed([]string{p2})
	if title, _ := s2.Best(); title != "Synthetic from event" {
		t.Fatalf("thread_name_updated: %q", title)
	}
}

func TestRenameRules(t *testing.T) {
	owner := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth"}
	self := contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_a"}
	auto := contracts.Session{SessionID: "ses_a", Tags: []string{"x", TagAuto}}
	if err := AllowRename(self, auto, "n"); err != nil {
		t.Fatal(err)
	}
	if err := AllowRename(self, contracts.Session{SessionID: "ses_b", Tags: []string{TagAuto}}, "n"); err != nil {
		t.Fatalf("an agent may rename any session: %v", err)
	}
	if err := AllowRename(self, contracts.Session{SessionID: "ses_b", Tags: []string{TagAgent}}, "n"); err != nil {
		t.Fatalf("over another agent's title: %v", err)
	}
	if err := AllowRename(self, contracts.Session{SessionID: "ses_b"}, "n"); err != nil {
		t.Fatalf("over the owner's title of another session: %v", err)
	}
	if err := AllowRename(self, contracts.Session{SessionID: "ses_a"}, "n"); err != nil {
		t.Fatalf("over owner title: %v", err)
	}
	if err := AllowRename(owner, contracts.Session{SessionID: "ses_b"}, " "); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("empty: %v", err)
	}
	if got := RenameTags(owner, auto.Tags); len(got) != 1 || got[0] != "x" {
		t.Fatalf("owner tags %v", got)
	}
	if got := RenameTags(self, auto.Tags); len(got) != 2 || got[1] != TagAgent {
		t.Fatalf("agent tags %v", got)
	}
	if got := StartTags("", contracts.HarnessShell, nil); len(got) != 0 {
		t.Fatalf("shell %v", got)
	}
	if got := StartTags("", contracts.HarnessCodex, nil); len(got) != 1 {
		t.Fatalf("codex %v", got)
	}
}

func TestValidIcon(t *testing.T) {
	for _, ok := range []string{"🚀", "🐛", "👍🏽", "👩‍💻", "🇺🇸", "1️⃣", "♥", ""} {
		if _, err := ValidIcon(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"a", "ab", "1", "#", "🚀🚀", "🚀x", "\x07", "  x "} {
		if _, err := ValidIcon(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
