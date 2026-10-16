package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/agentguide"
)

// Page nudge (W-web-inbox): a project's lead keeps the project page (the
// owner's main view of the project in the web app) current. When a
// top-level session in a project did work this turn and has not updated
// the page itself (update_project_page, `pyre project page set`) or been
// reminded in pageStaleAfter, the Stop hook blocks the stop once with
// PageNudgeText. The hook has no hub connection, so "updated" is this
// session's own evidence in events.jsonl: a lead whose page another session
// keeps current is reminded at most every pageStaleAfter and told to check
// first. Never when the owner typed this turn's prompt (he is in the chat),
// never in a turn a Stop hook already continued (stop_hook_active: at most
// one nudge per turn, the ask nudge first), never for a session with a
// parent.

// PageNudgeText is the Stop-hook block reason.
const PageNudgeText = "You lead this project and did work this turn, but you have not updated its page (the owner's main view " +
	"of the project in the web app) in 3 hours. Check it with mcp__pyre__get_project_page; if it is missing or stale, " +
	"rewrite it with mcp__pyre__update_project_page (goal, current status, results with links, open decisions, what's next). " +
	"If it is current, just end your turn."

// evPageNudged records that the Stop hook blocked a stop with PageNudgeText.
const evPageNudged = "PyreStopPageNudge"

// envProjectID is the session's project at launch (set by pyred).
const envProjectID = "PYRE_PROJECT_ID"

// pageStaleAfter is how long after the session's last page update (or
// reminder) it is reminded again.
const pageStaleAfter = 3 * time.Hour

func pageNudge(stateDir string, rec hookRecord, p map[string]any, now time.Time) []byte {
	if os.Getenv(agentguide.EnvParentSessionID) != "" || os.Getenv(envProjectID) == "" || p["stop_hook_active"] == true {
		return nil
	}
	recs, first, err := tailEvents(stateDir)
	if err != nil {
		return nil
	}
	start := -1
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Event == "UserPromptSubmit" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil // no turn start in view
	}
	worked := false
	for _, r := range recs[start+1:] {
		switch {
		case r.Event == evAskNudged || r.Event == evPageNudged || r.Event == evStopInjected:
			return nil // one nudge per turn
		case r.Event == "PreToolUse" && r.ToolName != "" && !strings.HasPrefix(r.ToolName, "mcp__pyre__"):
			worked = true
		}
	}
	if !worked {
		return nil
	}
	for _, r := range recs {
		if (r.Event == evPageNudged || updatedPage(r)) && now.Sub(r.At) < pageStaleAfter {
			return nil
		}
	}
	if ownerTyped(stateDir, recs, start, first, now) {
		return nil
	}
	events := filepath.Join(stateDir, fileEvents)
	_ = appendJSONL(events, rec)
	_ = appendJSONL(events, hookRecord{ID: newEventID(), At: now.UTC(), Event: evPageNudged})
	out, _ := json.Marshal(map[string]any{"decision": "block", "reason": PageNudgeText})
	return out
}

// updatedPage reports a tool call in r that wrote the project page.
func updatedPage(r hookRecord) bool {
	if r.Event != "PostToolUse" {
		return false
	}
	if r.ToolName == "mcp__pyre__update_project_page" {
		return true
	}
	if r.ToolName != "Bash" {
		return false
	}
	ti, _ := r.payload()["tool_input"].(map[string]any)
	cmd := " " + strings.Join(strings.Fields(str(ti, "command")), " ") + " "
	return strings.Contains(cmd, " project page set ") || strings.Contains(cmd, " projects page set ")
}
