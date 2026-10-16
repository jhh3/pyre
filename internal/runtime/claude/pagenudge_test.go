package claude

import (
	"encoding/json"
	"testing"
	"time"
)

func pageRun(t *testing.T) string {
	t.Helper()
	dir := nudgeRun(t, true)
	t.Setenv(envProjectID, "prj_synthpage")
	return dir
}

func wantPageNudge(t *testing.T, out []byte) {
	t.Helper()
	var d map[string]string
	if json.Unmarshal(out, &d) != nil || d["decision"] != "block" || d["reason"] != PageNudgeText {
		t.Fatalf("want page nudge, got %q", out)
	}
}

func workTurn(t *testing.T, dir, prompt string) {
	t.Helper()
	hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": prompt})
	hookJSON(t, dir, "PreToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": "/tmp/synth.go"}})
}

func TestPageNudgeLeadOncePerTurnAndThrottled(t *testing.T) {
	dir := pageRun(t)
	workTurn(t, dir, "launch: build the synthetic prototype")
	wantPageNudge(t, stopHook(t, dir, "Built the synthetic prototype.", false))
	// The continued turn stops normally.
	if out := stopHook(t, dir, "Page checked.", true); out != nil {
		t.Fatalf("second stop in the turn: %q", out)
	}
	// Next turn (a Pyre message): reminded at most every 3 hours.
	workTurn(t, dir, "[pyre-msg:msg_synthnext] synthetic follow-up")
	if out := stopHook(t, dir, "Done.", false); out != nil {
		t.Fatalf("throttled: %q", out)
	}
	recs, _, _ := readEvents(dir, 0)
	if out := pageNudge(dir, hookRecord{Event: "Stop"}, map[string]any{}, time.Now().Add(pageStaleAfter+time.Minute)); out == nil {
		t.Fatalf("after 3h: no nudge (%d records)", len(recs))
	}
}

func TestPageNudgeSkips(t *testing.T) {
	// Updating the page this turn (MCP or CLI) satisfies it.
	for _, post := range []map[string]any{
		{"tool_name": "mcp__pyre__update_project_page", "tool_input": map[string]any{"content": "# synthetic"}},
		{"tool_name": "Bash", "tool_input": map[string]any{"command": "pyre project page set --file /tmp/synth-page.md"}},
	} {
		dir := pageRun(t)
		workTurn(t, dir, "launch: synthetic")
		hookJSON(t, dir, "PostToolUse", post)
		if out := stopHook(t, dir, "Done.", false); out != nil {
			t.Fatalf("after a page update (%v): %q", post["tool_name"], out)
		}
	}
	// No work this turn (only pyre tools).
	dir := pageRun(t)
	hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "launch: synthetic"})
	hookJSON(t, dir, "PreToolUse", map[string]any{"tool_name": "mcp__pyre__inbox"})
	if out := stopHook(t, dir, "Nothing to do.", false); out != nil {
		t.Fatalf("no work: %q", out)
	}
	// Not in a project; a child session.
	dir = pageRun(t)
	t.Setenv(envProjectID, "")
	workTurn(t, dir, "launch: synthetic")
	if out := stopHook(t, dir, "Done.", false); out != nil {
		t.Fatalf("no project: %q", out)
	}
	dir = pageRun(t)
	t.Setenv("PYRE_PARENT_SESSION_ID", "ses_synthparent")
	workTurn(t, dir, "launch: synthetic")
	if out := stopHook(t, dir, "Done.", false); out != nil {
		t.Fatalf("child: %q", out)
	}
}

func TestPageNudgeYieldsToAskNudge(t *testing.T) {
	dir := pageRun(t)
	workTurn(t, dir, "launch: synthetic")
	wantNudge(t, stopHook(t, dir, nudgeQuestion, false))
	if out := stopHook(t, dir, "Filed the ask.", true); out != nil {
		t.Fatalf("second nudge in the turn: %q", out)
	}
}
