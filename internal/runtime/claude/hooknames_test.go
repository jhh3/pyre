package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// claudeValidEvents is the "Valid events" list printed by Claude Code
// 2.1.289 in its Settings Warning for an unknown hook name (captured
// 2026-10-04). Every registered name must be in it.
var claudeValidEvents = strings.Fields(`PreToolUse PostToolUse PostToolUseFailure PostToolBatch
	Notification UserPromptSubmit UserPromptExpansion SessionStart SessionEnd Stop StopFailure
	SubagentStart SubagentStop PreCompact PostCompact PreModelSwitch PostModelSwitch
	PermissionRequest PermissionDenied Setup TeammateIdle TaskCreated TaskCompleted Elicitation
	ElicitationResult ConfigChange WorktreeCreate WorktreeRemove InstructionsLoaded CwdChanged
	FileChanged DirectoryAdded MessageDisplay`)

func TestHookEventNamesAreValid(t *testing.T) {
	valid := map[string]bool{}
	for _, e := range claudeValidEvents {
		valid[e] = true
	}
	for _, e := range HookEvents {
		if !valid[e] {
			t.Errorf("hook event %q is not a Claude Code 2.1.289 event (it would open a Settings Warning dialog)", e)
		}
	}
	for _, want := range []string{"PostToolUseFailure", "PermissionDenied", "PostCompact"} {
		found := false
		for _, e := range HookEvents {
			found = found || e == want
		}
		if !found {
			t.Errorf("%s is not registered", want)
		}
	}
	dir := t.TempDir()
	p, err := writeHookSettings(dir, []string{"pyred", "hook", "claude"}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var s struct {
		Hooks map[string][]struct{ Matcher string }
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"PostToolUseFailure", "PermissionDenied"} {
		if s.Hooks[e][0].Matcher != "*" {
			t.Errorf("%s must match every tool", e)
		}
	}
	if s.Hooks["PostCompact"][0].Matcher != "" {
		t.Error("PostCompact takes no tool matcher")
	}
}

// A PermissionRequest after a FAILED earlier call of the same tool+input
// must not be correlated to it.
func TestCorrelateSkipsFailedToolUse(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	RunHook(ctx, "PreToolUse", strings.NewReader(`{"tool_name":"Bash","tool_use_id":"toolu_old","tool_input":{"command":"x"}}`), dir)
	RunHook(ctx, "PostToolUseFailure", strings.NewReader(`{"tool_name":"Bash","tool_use_id":"toolu_old","tool_input":{"command":"x"},"error":"exit 1"}`), dir)
	if got := correlateToolUse(dir, "Bash", `{"command":"x"}`); got != "" {
		t.Fatalf("correlated to a finished (failed) call: %q", got)
	}
	RunHook(ctx, "PreToolUse", strings.NewReader(`{"tool_name":"Bash","tool_use_id":"toolu_new","tool_input":{"command":"x"}}`), dir)
	if got := correlateToolUse(dir, "Bash", `{"command":"x"}`); got != "toolu_new" {
		t.Fatalf("correlate %q", got)
	}
}

func TestApprovalClassificationWithNewEvents(t *testing.T) {
	a := &Adapter{o: Options{Now: time.Now}}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, dirAnswers), 0o700)
	h := contracts.RunHandle{RunID: "run_x", SessionID: "ses_x", StateDir: dir}
	t0 := time.Now().Add(-time.Minute)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	perm := hookRecord{Event: "PermissionRequest", At: at(1), RequestID: "r1", ToolUseID: "tu1", ToolName: "Bash", HookPID: os.Getpid()}
	mk := func(evs ...hookRecord) (*runState, []contracts.Approval) {
		rs := newRunState()
		rs.applyHook(hookRecord{Event: "SessionStart", At: t0})
		for _, e := range evs {
			rs.applyHook(e)
		}
		s, _ := rs.derive()
		return rs, a.evalApprovals(h, rs, s)
	}
	// Allowed through Pyre, then the command failed: still effective (it ran).
	rs, aps := mk(perm, hookRecord{Event: evApprovalAnswered, At: at(2), RequestID: "r1", Behavior: "allow"},
		hookRecord{Event: "PostToolUseFailure", At: at(3), ToolUseID: "tu1", ToolName: "Bash", Payload: json.RawMessage(`{"error":"exit 1"}`)})
	if aps[0].Resolution == nil || aps[0].Resolution.Effect != "effective" || aps[0].Resolution.OptionID != "allow" {
		t.Fatalf("allow+failure: %+v", aps[0].Resolution)
	}
	if !strings.Contains(rs.reason, "tool failed: Bash") {
		t.Fatalf("state reason %q", rs.reason)
	}
	// Auto mode denied it before anyone answered: resolved natively as deny.
	_, aps = mk(perm, hookRecord{Event: "PermissionDenied", At: at(2), ToolUseID: "tu1", ToolName: "Bash", Payload: json.RawMessage(`{"reason":"classifier"}`)})
	if aps[0].State != contracts.ApprovalResolved || aps[0].Resolution.Source != "native" || aps[0].Resolution.OptionID != "deny" {
		t.Fatalf("auto-mode denial: %+v", aps[0])
	}
	// PostCompact only updates activity.
	rs, _ = mk(hookRecord{Event: "PostCompact", At: at(2), Payload: json.RawMessage(`{"compaction_type":"auto"}`)})
	if rs.activity != "context compacted (auto)" {
		t.Fatalf("activity %q", rs.activity)
	}
}
