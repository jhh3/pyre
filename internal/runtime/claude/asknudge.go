package claude

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/agentguide"
	"github.com/jhh3/pyre/internal/askdetect"
)

// Ask nudge (owner request 2026-10-14): leads ended turns with decisions for
// the owner written only in their chat, which he does not read, and filed no
// asks. When a top-level session's turn ends with an owner-directed question
// (askdetect) and it filed no ask in that turn, the Stop hook blocks the stop
// once and tells it to file the ask. Not for sessions with a parent (they
// report to it), not when the owner typed this turn's prompt himself (he is
// in the chat), and never twice in one turn.

// AskNudgeText is the Stop-hook block reason.
const AskNudgeText = "Your turn ended with a question for the owner. He doesn't read chat questions. " +
	"File each open decision with mcp__pyre__ask_owner (topic_key, title, context he can decide from alone, 2–4 options each with a detail, " +
	"a default unless it spends money or is irreversible), then end your turn without repeating the questions."

// evAskNudged records that the Stop hook blocked a stop with AskNudgeText;
// the turn continues (like evStopInjected).
const evAskNudged = "PyreStopAskNudge"

const (
	// askNudgeWindow bounds how much of events.jsonl the hook reads back to
	// find the turn start (payloads are bounded, so this spans long turns).
	askNudgeWindow = 4 << 20
	// ownerTypedFresh: a prompt the owner typed this long ago or less means
	// he is in the chat, where a question is fine.
	ownerTypedFresh = 30 * time.Minute
	// transcriptTail bounds the transcript read when the Stop payload has no
	// last_assistant_message.
	transcriptTail = 512 << 10
)

// askTools are the tool calls that file an ask or otherwise put the session
// in front of the owner (flag_attention: a non-choice blocker).
var askTools = map[string]bool{"mcp__pyre__ask_owner": true, "mcp__pyre__flag_attention": true}

// askNudge returns the Stop-hook block decision when this stop should be
// turned into "file an ask", else nil. rec is the Stop record (appended,
// with the nudge marker, when it blocks).
func askNudge(stateDir string, rec hookRecord, p map[string]any, now time.Time) []byte {
	if os.Getenv(agentguide.EnvParentSessionID) != "" {
		return nil
	}
	msg := str(p, "last_assistant_message")
	if msg == "" {
		msg = lastAssistantText(str(p, "transcript_path"))
	}
	if _, ok := askdetect.OwnerQuestion(msg); !ok {
		return nil
	}
	recs, first, err := tailEvents(stateDir)
	if err != nil {
		return nil // cannot prove this is the first nudge of the turn
	}
	start := -1 // the turn's UserPromptSubmit
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Event == "UserPromptSubmit" {
			start = i
			break
		}
	}
	for _, r := range recs[start+1:] {
		if r.Event == evAskNudged || filedAsk(r) {
			return nil
		}
	}
	if start < 0 && p["stop_hook_active"] == true {
		return nil // no turn start in view; never risk a loop
	}
	if start >= 0 && ownerTyped(stateDir, recs, start, first, now) {
		return nil
	}
	events := filepath.Join(stateDir, fileEvents)
	_ = appendJSONL(events, rec)
	_ = appendJSONL(events, hookRecord{ID: newEventID(), At: now.UTC(), Event: evAskNudged})
	out, _ := json.Marshal(map[string]any{"decision": "block", "reason": AskNudgeText})
	return out
}

// filedAsk reports a tool call in r that filed an ask: the MCP tools, or the
// CLI (`pyre ask new`) through Bash.
func filedAsk(r hookRecord) bool {
	if r.Event != "PreToolUse" && r.Event != "PostToolUse" {
		return false
	}
	if askTools[r.ToolName] {
		return true
	}
	if r.ToolName != "Bash" {
		return false
	}
	ti, _ := r.payload()["tool_input"].(map[string]any)
	cmd := " " + strings.Join(strings.Fields(str(ti, "command")), " ") + " "
	return strings.Contains(cmd, "pyre ask new ") || strings.Contains(cmd, "pyre asks new ")
}

// ownerTyped reports whether the turn's prompt (recs[start]) was typed by a
// human recently: it carries no Pyre message marker and is not the launch
// prompt (the run's first prompt when it was launched with one).
func ownerTyped(stateDir string, recs []hookRecord, start int, first bool, now time.Time) bool {
	r := recs[start]
	if len(r.Markers) > 0 || now.Sub(r.At) > ownerTypedFresh {
		return false
	}
	if first {
		for _, e := range recs[:start] {
			if e.Event == "UserPromptSubmit" {
				return true // an earlier prompt was the launch prompt
			}
		}
		var m runMeta
		if b, err := os.ReadFile(filepath.Join(stateDir, fileRun)); err == nil && json.Unmarshal(b, &m) == nil && m.InitialPrompt {
			return false
		}
	}
	return true
}

// tailEvents reads the last askNudgeWindow bytes of events.jsonl; first says
// the window starts at the beginning of the file. A line cut by the window
// start fails to decode and is skipped.
func tailEvents(stateDir string) ([]hookRecord, bool, error) {
	var off int64
	if st, err := os.Stat(filepath.Join(stateDir, fileEvents)); err == nil && st.Size() > askNudgeWindow {
		off = st.Size() - askNudgeWindow
	}
	recs, _, err := readEvents(stateDir, off)
	return recs, off == 0, err
}

// lastAssistantText returns the text of the last main-thread assistant
// record in the transcript tail ("" when unreadable).
func lastAssistantText(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > transcriptTail {
		_, _ = f.Seek(st.Size()-transcriptTail, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, transcriptTail))
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var r transcriptRecord
		if json.Unmarshal([]byte(lines[i]), &r) != nil || r.Type != "assistant" || r.IsSidechain {
			continue
		}
		if t := r.text(); t != "" {
			return t
		}
	}
	return ""
}
