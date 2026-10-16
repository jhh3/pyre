package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/agentguide"
	"github.com/jhh3/pyre/internal/contracts"
)

const nudgeQuestion = "Rendered the stems.\n\nStill waiting on you: should I build the voice-match score in the DAW?"

func nudgeRun(t *testing.T, initialPrompt bool) string {
	t.Helper()
	t.Setenv(agentguide.EnvParentSessionID, "")
	t.Setenv(envProjectID, "") // no page nudge (pagenudge_test.go)
	dir := t.TempDir()
	b, _ := json.Marshal(runMeta{RunID: "run_nudge", InitialPrompt: initialPrompt})
	if err := os.WriteFile(filepath.Join(dir, fileRun), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func hookJSON(t *testing.T, dir, ev string, p map[string]any) []byte {
	t.Helper()
	b, _ := json.Marshal(p)
	out, _ := RunHook(context.Background(), ev, strings.NewReader(string(b)), dir)
	return out
}

func stopHook(t *testing.T, dir, msg string, active bool) []byte {
	return hookJSON(t, dir, "Stop", map[string]any{"last_assistant_message": msg, "stop_hook_active": active})
}

func wantNudge(t *testing.T, out []byte) {
	t.Helper()
	var d map[string]string
	if json.Unmarshal(out, &d) != nil || d["decision"] != "block" || d["reason"] != AskNudgeText {
		t.Fatalf("want ask nudge, got %q", out)
	}
}

func TestAskNudgeTopLevelOncePerTurn(t *testing.T) {
	dir := nudgeRun(t, true)
	hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "launch: build the prototype"})
	wantNudge(t, stopHook(t, dir, nudgeQuestion, false))
	// The run continues (not waiting for input) after the block.
	recs, _, _ := readEvents(dir, 0)
	rs := newRunState()
	for _, r := range recs {
		rs.applyHook(r)
	}
	if st, why := rs.derive(); st != contracts.RunRunning {
		t.Fatalf("after nudge: %s (%s)", st, why)
	}
	// The agent ignores it and stops again: no second block this turn.
	if out := stopHook(t, dir, nudgeQuestion, true); out != nil {
		t.Fatalf("second stop in the same turn blocked: %s", out)
	}
	// Even without stop_hook_active the nudge record holds.
	if out := stopHook(t, dir, nudgeQuestion, false); out != nil {
		t.Fatalf("third stop in the same turn blocked: %s", out)
	}
	// A new turn (a Pyre message) may be nudged again.
	hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "[pyre-msg:msg_n1] status?"})
	wantNudge(t, stopHook(t, dir, nudgeQuestion, false))
}

func TestAskNudgeSkips(t *testing.T) {
	t.Run("no question", func(t *testing.T) {
		dir := nudgeRun(t, true)
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "go"})
		if out := stopHook(t, dir, "Nothing needs you. The dashboard found no new problems.", false); out != nil {
			t.Fatalf("blocked: %s", out)
		}
	})
	t.Run("child session", func(t *testing.T) {
		dir := nudgeRun(t, true)
		t.Setenv(agentguide.EnvParentSessionID, "ses_parent")
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "go"})
		if out := stopHook(t, dir, nudgeQuestion, false); out != nil {
			t.Fatalf("child blocked: %s", out)
		}
	})
	t.Run("ask filed this turn", func(t *testing.T) {
		dir := nudgeRun(t, true)
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "go"})
		hookJSON(t, dir, "PreToolUse", map[string]any{"tool_name": "mcp__pyre__ask_owner", "tool_use_id": "toolu_a", "tool_input": map[string]any{"topic_key": "voice-match"}})
		hookJSON(t, dir, "PostToolUse", map[string]any{"tool_name": "mcp__pyre__ask_owner", "tool_use_id": "toolu_a"})
		if out := stopHook(t, dir, nudgeQuestion, false); out != nil {
			t.Fatalf("blocked after ask_owner: %s", out)
		}
	})
	t.Run("ask filed with the CLI", func(t *testing.T) {
		dir := nudgeRun(t, true)
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "go"})
		hookJSON(t, dir, "PreToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": "toolu_b", "tool_input": map[string]any{"command": "pyre  ask new --topic voice --title T"}})
		if out := stopHook(t, dir, nudgeQuestion, false); out != nil {
			t.Fatalf("blocked after pyre ask new: %s", out)
		}
	})
	t.Run("ask in an earlier turn does not count", func(t *testing.T) {
		dir := nudgeRun(t, true)
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "go"})
		hookJSON(t, dir, "PreToolUse", map[string]any{"tool_name": "mcp__pyre__ask_owner", "tool_use_id": "toolu_a"})
		stopHook(t, dir, "Filed the ask.", false)
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "[pyre-msg:msg_2] next"})
		wantNudge(t, stopHook(t, dir, nudgeQuestion, false))
	})
	t.Run("owner typed this turn", func(t *testing.T) {
		dir := nudgeRun(t, true)
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "launch prompt"})
		stopHook(t, dir, "Done.", false)
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "which layout looks better?"})
		if out := stopHook(t, dir, nudgeQuestion, false); out != nil {
			t.Fatalf("blocked while the owner is in the chat: %s", out)
		}
	})
	t.Run("owner typed first prompt of a run without a launch prompt", func(t *testing.T) {
		dir := nudgeRun(t, false)
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "hi"})
		if out := stopHook(t, dir, nudgeQuestion, false); out != nil {
			t.Fatalf("blocked while the owner is in the chat: %s", out)
		}
	})
	t.Run("launch prompt is not the owner typing", func(t *testing.T) {
		dir := nudgeRun(t, true)
		hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "launch prompt"})
		wantNudge(t, stopHook(t, dir, nudgeQuestion, false))
	})
}

func TestAskNudgeOwnerTypedStale(t *testing.T) {
	dir := nudgeRun(t, true)
	old := time.Now().Add(-2 * time.Hour).UTC()
	for _, r := range []hookRecord{
		{ID: "h1", At: old, Event: "UserPromptSubmit"},
		{ID: "h2", At: old, Event: "Stop"},
		{ID: "h3", At: old, Event: "UserPromptSubmit"},
	} {
		if err := appendJSONL(filepath.Join(dir, fileEvents), r); err != nil {
			t.Fatal(err)
		}
	}
	// The owner typed two hours ago and left during a long turn.
	wantNudge(t, stopHook(t, dir, nudgeQuestion, false))
}

func TestAskNudgeTranscriptFallback(t *testing.T) {
	dir := nudgeRun(t, true)
	tp := filepath.Join(dir, "t.jsonl")
	var lines []string
	for _, r := range []map[string]any{
		{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": nudgeQuestion}}}},
		{"type": "assistant", "isSidechain": true, "message": map[string]any{"role": "assistant", "content": "subagent done"}},
	} {
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(tp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastAssistantText(tp); got != nudgeQuestion {
		t.Fatalf("lastAssistantText = %q", got)
	}
	hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "launch"})
	wantNudge(t, hookJSON(t, dir, "Stop", map[string]any{"transcript_path": tp}))
}
