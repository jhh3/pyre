package agy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// agy writes each conversation's steps to
// <agy-home>/brain/<conversation>/.system_generated/logs/transcript_full.jsonl
// (one JSON object per step, appended as the step happens; validated on
// 1.4.2). The hook payload's transcriptPath names the file.
//
//	{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE",
//	 "created_at":"2026-10-18T16:45:26Z","content":"<USER_REQUEST>\n...\n</USER_REQUEST>\n<ADDITIONAL_METADATA>..."}
//	{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","content":"...","tool_calls":[{"name":"run_command","args":{...}}]}
type transcriptRecord struct {
	StepIndex int64     `json:"step_index"`
	Source    string    `json:"source"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Content   string    `json:"content"`
	ToolCalls []struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"tool_calls"`
}

// TranscriptFormat names the transcript format for the search indexer and
// title scanner.
const TranscriptFormat = "agy"

// transcriptRel is the transcript path under a conversation's brain dir.
var transcriptRel = filepath.Join(".system_generated", "logs", "transcript_full.jsonl")

// transcriptPathFor is where agy keeps conversation id's transcript.
func transcriptPathFor(agyHome, id string) string {
	if agyHome == "" || !safeConversationID(id) {
		return ""
	}
	return filepath.Join(agyHome, "brain", id, transcriptRel)
}

var conversationRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func safeConversationID(id string) bool { return conversationRe.MatchString(id) }

// markerRe matches "[pyre-msg:<id>]" and the dated header the delivery
// engine renders, "[pyre-msg:<id> · sent 3:05 PM PT (8h ago)]" (as the
// Claude adapter does).
var markerRe = regexp.MustCompile(`\[pyre-msg:([A-Za-z0-9_.:\-]{1,200})(?:\]| · [^\]\n]{0,80}\])`)

// userRequest is the text the person (or Pyre) entered: the USER_REQUEST
// part of a USER_INPUT step, without agy's added metadata.
func userRequest(content string) string {
	s := content
	if i := strings.Index(s, "<USER_REQUEST>"); i >= 0 {
		s = s[i+len("<USER_REQUEST>"):]
		if j := strings.Index(s, "</USER_REQUEST>"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}

// entryFor converts one transcript step to a sessions.read entry (ok=false:
// not shown, e.g. tool output and system notes).
func entryFor(r transcriptRecord, seq int64, runID string) (contracts.TranscriptEntry, bool) {
	e := contracts.TranscriptEntry{Seq: seq, RunID: runID, At: r.CreatedAt}
	switch {
	case r.Type == "USER_INPUT":
		e.Kind, e.Text = "user", userRequest(r.Content)
		if m := markerRe.FindStringSubmatch(e.Text); m != nil {
			e.Kind, e.MessageID = "pyre_message", m[1]
		}
	case r.Type == "PLANNER_RESPONSE" && strings.TrimSpace(r.Content) != "":
		e.Kind, e.Text = "assistant", strings.TrimSpace(r.Content)
	case r.Type == "PLANNER_RESPONSE" && len(r.ToolCalls) > 0:
		var parts []string
		for _, tc := range r.ToolCalls {
			parts = append(parts, toolSummary(tc.Name, tc.Args))
		}
		e.Kind, e.Text = "tool", strings.Join(parts, "; ")
	default:
		return e, false
	}
	if r := []rune(e.Text); len(r) > 8000 {
		e.Text, e.Truncated = string(r[:8000]), true
	}
	return e, e.Text != ""
}

// toolSummary is "name: what" from agy's own toolSummary/toolAction or the
// command line, never the full arguments.
func toolSummary(name string, args json.RawMessage) string {
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	for _, k := range []string{"toolSummary", "toolAction", "CommandLine", "AbsolutePath"} {
		if v, ok := a[k].(string); ok && strings.TrimSpace(v) != "" {
			return name + ": " + clip(oneLine(v), 160)
		}
	}
	return name
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// readTranscript returns the entries of path (every step line advances Seq,
// shown or not, so a cursor is a line number).
func readTranscript(path, runID string) []contracts.TranscriptEntry {
	var out []contracts.TranscriptEntry
	var seq int64
	_, _ = readJSONLFrom(path, 0, func(line []byte, _ int64) {
		var r transcriptRecord
		if json.Unmarshal(line, &r) != nil {
			return
		}
		seq++
		if e, ok := entryFor(r, seq, runID); ok {
			out = append(out, e)
		}
	})
	return out
}

// conversationOnNode reports whether agy has conversation id in agyHome.
func conversationOnNode(agyHome, id string) bool {
	if agyHome == "" || !safeConversationID(id) {
		return false
	}
	_, err := os.Stat(filepath.Join(agyHome, "conversations", id+".db"))
	return err == nil
}
