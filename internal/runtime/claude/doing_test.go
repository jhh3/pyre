package claude

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestDoingNowFollowsToolCalls(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	rs := newRunState()
	hook := func(ev, tool, id string, input map[string]any) {
		at = at.Add(time.Second)
		p, _ := json.Marshal(map[string]any{"tool_input": input})
		rs.applyHook(hookRecord{ID: "h" + id, At: at, Event: ev, ToolName: tool, ToolUseID: id, Payload: p})
	}
	hook("SessionStart", "", "", nil)
	hook("UserPromptSubmit", "", "", nil)
	if got := rs.doingNow(); got != "thinking" {
		t.Fatalf("after prompt = %q", got)
	}
	hook("PreToolUse", "Bash", "t1", map[string]any{"command": "go test ./..."})
	if got := rs.doingNow(); got != "running tests" {
		t.Fatalf("bash test = %q", got)
	}
	hook("PostToolUse", "Bash", "t1", map[string]any{"command": "go test ./..."})
	hook("PreToolUse", "Read", "t2", map[string]any{"file_path": "/synthetic/a.go"})
	hook("PreToolUse", "Read", "t3", map[string]any{"file_path": "/synthetic/b.go"})
	if got := rs.doingNow(); got != "reading 2 files" {
		t.Fatalf("parallel reads = %q", got)
	}
	hook("PostToolUse", "Read", "t2", nil)
	hook("PostToolUse", "Read", "t3", nil)
	hook("PreToolUse", "Edit", "t4", map[string]any{"file_path": "/synthetic/src/useCanvas.ts"})
	if got := rs.doingNow(); got != "editing useCanvas.ts" {
		t.Fatalf("edit = %q", got)
	}
	hook("Stop", "", "", nil)
	if rs.state != contracts.RunWaitingInput || rs.inflight.Len() != 0 {
		t.Fatalf("after Stop state=%s inflight=%d", rs.state, rs.inflight.Len())
	}
}

// Claude Code 2.1.292 runs internal agents after Stop whose tool events carry
// an agent_id with no SubagentStart (observed: PreToolUse ScheduleWakeup with
// no PostToolUse). They must not make an idle session look running.
func TestHiddenAgentToolAfterStopKeepsIdle(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	rs := newRunState()
	hook := func(ev, tool, id, agent string) {
		at = at.Add(time.Second)
		m := map[string]any{"tool_input": map[string]any{"stop": true}}
		if agent != "" {
			m["agent_id"] = agent
		}
		p, _ := json.Marshal(m)
		rs.applyHook(hookRecord{ID: "h" + id + ev, At: at, Event: ev, ToolName: tool, ToolUseID: id, Payload: p})
	}
	hook("SessionStart", "", "", "")
	hook("UserPromptSubmit", "", "", "")
	hook("Stop", "", "", "")
	hook("PreToolUse", "ScheduleWakeup", "t1", "synthetic-agent")
	if rs.state != contracts.RunWaitingInput || rs.inflight.Len() != 0 {
		t.Fatalf("hidden agent tool flipped idle run: state=%s reason=%q inflight=%d", rs.state, rs.reason, rs.inflight.Len())
	}
	hook("PostToolUse", "ScheduleWakeup", "t1", "synthetic-agent")
	if rs.state != contracts.RunWaitingInput {
		t.Fatalf("hidden agent PostToolUse flipped idle run: %s %q", rs.state, rs.reason)
	}
	// An announced subagent inside a turn still counts.
	hook("UserPromptSubmit", "", "", "")
	at = at.Add(time.Second)
	sp, _ := json.Marshal(map[string]any{"agent_id": "synthetic-sub", "agent_type": "general-purpose"})
	rs.applyHook(hookRecord{ID: "hs", At: at, Event: "SubagentStart", Payload: sp})
	hook("PreToolUse", "Read", "t2", "synthetic-sub")
	if rs.state != contracts.RunRunning || rs.inflight.Len() != 1 {
		t.Fatalf("announced subagent tool: state=%s inflight=%d", rs.state, rs.inflight.Len())
	}
}

// iss_…: a background subagent the turn started
// (announced by SubagentStart) keeps running tools after Stop. Its tool
// events must not reopen the turn, or the hub sees the parent "mid-turn"
// and holds its children's reports.
func TestBackgroundSubagentToolAfterStopKeepsTurnEnded(t *testing.T) {
	at := time.Date(2026, 11, 3, 20, 11, 0, 0, time.UTC)
	rs := newRunState()
	apply := func(ev, tool, id string, m map[string]any) {
		at = at.Add(time.Second)
		p, _ := json.Marshal(m)
		rs.applyHook(hookRecord{ID: "h" + id + ev, At: at, Event: ev, ToolName: tool, ToolUseID: id, Payload: p})
	}
	sub := map[string]any{"agent_id": "synthetic-bg", "agent_type": "implementer"}
	apply("SessionStart", "", "", nil)
	apply("UserPromptSubmit", "", "", nil)
	apply("SubagentStart", "", "", sub)
	apply("PreToolUse", "Bash", "t1", sub)
	if rs.state != contracts.RunRunning {
		t.Fatalf("subagent tool inside the turn: state=%s", rs.state)
	}
	apply("PostToolUse", "Bash", "t1", sub)
	apply("Stop", "", "", map[string]any{"background_tasks": []any{map[string]any{"type": "agent", "status": "running", "description": "live check"}}})
	if rs.state != contracts.RunWaitingBackground {
		t.Fatalf("after Stop: state=%s %q", rs.state, rs.reason)
	}
	for i, ev := range []string{"PreToolUse", "PostToolUse", "PreToolUse", "PostToolUseFailure", "PermissionDenied"} {
		apply(ev, "Bash", fmt.Sprintf("t%d", 2+i/2), sub)
		if rs.state != contracts.RunWaitingBackground {
			t.Fatalf("background subagent %s reopened the turn: state=%s %q", ev, rs.state, rs.reason)
		}
	}
	// The main thread's own tool call (no agent_id) is a new turn.
	apply("PreToolUse", "Bash", "t9", nil)
	if rs.state != contracts.RunRunning {
		t.Fatalf("main-thread tool after Stop: state=%s", rs.state)
	}
}
