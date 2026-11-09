package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/killguard"
)

// The PreToolUse hook denies a Bash call that would signal systemd --user,
// records why, and leaves other calls alone. The process table is fake: no
// test signals or inspects a real process.
func TestHookDeniesKillOfUserManager(t *testing.T) {
	old := killHost
	t.Cleanup(func() { killHost = old })
	killHost = func() killguard.Host {
		return killguard.Host{Protected: killguard.Protect([]killguard.Proc{
			{PID: 1, Comm: "systemd"}, {PID: 1432, PPID: 1, Comm: "systemd", Cmdline: "/usr/lib/systemd/systemd --user"},
		}, 0)}
	}
	dir := t.TempDir()
	ctx := context.Background()
	in := func(tool, cmd string) string {
		b, _ := json.Marshal(map[string]any{"tool_name": tool, "tool_use_id": "toolu_k", "tool_input": map[string]any{"command": cmd}})
		return string(b)
	}

	out, code := RunHook(ctx, "PreToolUse", strings.NewReader(in("Bash", "kill -9 1432")), dir)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var ans struct {
		HookSpecificOutput map[string]string `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &ans); err != nil {
		t.Fatalf("answer %q: %v", out, err)
	}
	h := ans.HookSpecificOutput
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "deny" || !strings.Contains(h["permissionDecisionReason"], "systemd --user") {
		t.Fatalf("answer %v", h)
	}
	ev, _ := os.ReadFile(filepath.Join(dir, fileEvents))
	if !strings.Contains(string(ev), `"blocked":"Pyre refused this command`) {
		t.Fatalf("event log lacks the refusal: %s", ev)
	}

	for _, ok := range []string{in("Bash", "kill 4000"), in("Bash", "go test ./..."), in("Read", "kill 1432")} {
		if out, _ := RunHook(ctx, "PreToolUse", strings.NewReader(ok), dir); len(out) != 0 {
			t.Fatalf("%s answered %s", ok, out)
		}
	}

	// A refused call opens no in-flight tool.
	rs := newRunState()
	rs.applyHook(hookRecord{Event: "PreToolUse", ToolName: "Bash", ToolUseID: "toolu_k", Blocked: "refused"})
	if rs.inflight.Len() != 0 {
		t.Fatal("refused call left a tool in flight")
	}
}
