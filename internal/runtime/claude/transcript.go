package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// transcriptRecord is the subset of Claude's ~/.claude/projects/<slug>/<id>.jsonl
// records Pyre reads (format observed on 2.1.289).
type transcriptRecord struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype,omitempty"`
	Operation        string          `json:"operation,omitempty"` // queue-operation: enqueue | dequeue
	Content          json.RawMessage `json:"content,omitempty"`   // queue-operation enqueue content
	UUID             string          `json:"uuid,omitempty"`
	Timestamp        string          `json:"timestamp,omitempty"`
	IsMeta           bool            `json:"isMeta,omitempty"`
	IsSidechain      bool            `json:"isSidechain,omitempty"`
	IsAPIError       bool            `json:"isApiErrorMessage,omitempty"`
	Message          json.RawMessage `json:"message,omitempty"`
	DurationMs       int64           `json:"durationMs,omitempty"`
	ToolUseResult    json.RawMessage `json:"toolUseResult,omitempty"`
	SourceToolAssist string          `json:"sourceToolAssistantUUID,omitempty"`
}

type tMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type tBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (r transcriptRecord) at() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
	return t
}

// blocks decodes message content as text blocks; a string content becomes a
// single text block.
func (r transcriptRecord) blocks() []tBlock {
	var m tMessage
	if len(r.Message) == 0 || json.Unmarshal(r.Message, &m) != nil {
		return nil
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []tBlock{{Type: "text", Text: s}}
	}
	var bs []tBlock
	_ = json.Unmarshal(m.Content, &bs)
	return bs
}

func (r transcriptRecord) text() string {
	var parts []string
	for _, b := range r.blocks() {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (r transcriptRecord) queueContent() string {
	var s string
	_ = json.Unmarshal(r.Content, &s)
	return s
}

// isHumanUserRecord: a user prompt record (not a tool result, not meta).
func (r transcriptRecord) isPromptRecord() bool {
	if r.Type != "user" || r.IsSidechain || len(r.ToolUseResult) > 0 {
		return false
	}
	for _, b := range r.blocks() {
		if b.Type == "tool_result" {
			return false
		}
	}
	return true
}

// projectSlug mirrors Claude's project directory naming: every character
// other than [A-Za-z0-9] becomes '-' (e.g. /private/tmp/x → -private-tmp-x).
func projectSlug(cwd string) string {
	b := []byte(cwd)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// guessTranscriptPath computes the transcript path before any hook reported
// it. configDir is CLAUDE_CONFIG_DIR or ~/.claude.
func guessTranscriptPath(configDir, cwd, sessionID string) string {
	if configDir == "" || cwd == "" || sessionID == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	return filepath.Join(configDir, "projects", projectSlug(cwd), sessionID+".jsonl")
}

func defaultConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// entriesFromTranscript converts records to bounded transcript entries.
// Every record advances Seq, kept or not.
func entriesFromTranscript(runID string, recs []transcriptRecord, startSeq int64) []contracts.TranscriptEntry {
	var out []contracts.TranscriptEntry
	seq := startSeq
	for _, r := range recs {
		seq++
		if e, ok := transcriptEntry(runID, r, seq); ok {
			out = append(out, e)
		}
	}
	return out
}

// transcriptEntry converts one record numbered seq; false when sessions.read
// does not show it.
func transcriptEntry(runID string, r transcriptRecord, seq int64) (contracts.TranscriptEntry, bool) {
	e := contracts.TranscriptEntry{Seq: seq, RunID: runID, At: r.at()}
	switch {
	case r.Type == "user" && r.isPromptRecord():
		e.Kind = "user"
		e.Text = r.text()
		if r.IsMeta {
			e.Kind = "system"
		}
		if ms := findMarkers(e.Text); len(ms) > 0 {
			e.MessageID = ms[0]
			e.Kind = "pyre_message"
		}
	case r.Type == "user":
		e.Kind = "tool"
		e.Text = "tool result"
	case r.Type == "assistant" && !r.IsSidechain:
		var texts, tools []string
		for _, b := range r.blocks() {
			switch b.Type {
			case "text":
				texts = append(texts, b.Text)
			case "tool_use":
				tools = append(tools, toolUseSummary(b))
			}
		}
		if len(texts) > 0 {
			e.Kind = "assistant"
			e.Text = strings.Join(texts, "\n")
		} else if len(tools) > 0 {
			e.Kind = "tool"
			e.Text = strings.Join(tools, "; ")
		} else {
			return e, false
		}
	case r.Type == "system" && r.Subtype == "turn_duration":
		e.Kind = "system"
		e.Text = "turn ended (" + (time.Duration(r.DurationMs) * time.Millisecond).String() + ")"
	default:
		return e, false
	}
	if len(e.Text) > 2000 {
		e.Text = truncStr(e.Text, 2000)
		e.Truncated = true
	}
	return e, true
}

func toolUseSummary(b tBlock) string {
	var in map[string]any
	_ = json.Unmarshal(b.Input, &in)
	for _, k := range []string{"command", "file_path", "path", "pattern", "description", "url"} {
		if s := str(in, k); s != "" {
			return b.Name + ": " + truncStr(s, 200)
		}
	}
	return b.Name
}
