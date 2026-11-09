package claude

import (
	"encoding/json"

	"github.com/jhh3/pyre/internal/killguard"
)

// killHost loads the process table for killguard; tests replace it with a
// fake table.
var killHost = killguard.LoadHost

// killCheck refuses a shell command that would signal the processes every
// session on this machine depends on (systemd --user, PID 1, pyred, the
// sentinel, the hub, the tmux server, this harness), or stop their units
// (iss_…). It returns the PreToolUse deny answer,
// or nil to let the call run. It fails open: a panic lets the call run.
func killCheck(rec *hookRecord, p map[string]any) (out []byte) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	ti, _ := p["tool_input"].(map[string]any)
	cmd := str(ti, "command")
	if cmd == "" || (rec.ToolName != "Bash" && rec.ToolName != "Monitor") {
		return nil
	}
	b := killguard.Check(cmd, killHost)
	if b == nil {
		return nil
	}
	rec.Blocked = b.Reason
	out, _ = json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": b.Reason,
	}})
	return out
}
