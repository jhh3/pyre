// Package search is Pyre's global search: an incremental, bounded full-text
// index of a node's Claude and Codex transcripts (SQLite FTS5 in the node
// database), FTS query building with match-highlighted snippets, and the
// merge/rank step the hub (and the mock backend) apply to per-node results.
package search

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// ParseFile parses a whole transcript file's complete lines (tests, tools).
func ParseFile(path, format string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p := newParser(format, 0)
	var out []Entry
	br := bufio.NewReaderSize(f, 256<<10)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return out, nil
		}
		if e, ok := p.line(line[:len(line)-1]); ok {
			out = append(out, e)
		}
	}
}

// Transcript formats.
const (
	FormatClaude = "claude" // ~/.claude/projects/<slug>/<uuid>.jsonl
	FormatCodex  = "codex"  // $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread>.jsonl
)

// maxEntryText bounds the text indexed per entry.
const maxEntryText = 8000

// Entry is one indexable transcript item.
type Entry struct {
	Seq  int64
	Kind string // user | assistant | tool | message
	Text string
	At   time.Time
}

// parser consumes complete JSONL lines of one transcript file and keeps the
// running sequence number (persisted with the byte offset).
type parser interface {
	line(b []byte) (Entry, bool)
	skip() // an unparsed (overlong) record
	seq() int64
}

func newParser(format string, seq int64) parser {
	if format == FormatCodex {
		return &codexParser{n: seq}
	}
	return &claudeParser{n: seq}
}

// ---- Claude ----

// claudeParser numbers records exactly as the Claude adapter's
// sessions.read does: every line that decodes as a JSON object record
// advances Seq, kept or not, so a hit's Seq opens the reader at it.
type claudeParser struct{ n int64 }

type claudeRecord struct {
	Type          string          `json:"type"`
	Timestamp     string          `json:"timestamp,omitempty"`
	IsMeta        bool            `json:"isMeta,omitempty"`
	IsSidechain   bool            `json:"isSidechain,omitempty"`
	Message       json.RawMessage `json:"message,omitempty"`
	ToolUseResult json.RawMessage `json:"toolUseResult,omitempty"`
}

type claudeBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (p *claudeParser) seq() int64 { return p.n }
func (p *claudeParser) skip()      { p.n++ }

func (p *claudeParser) line(b []byte) (Entry, bool) {
	var r claudeRecord
	if json.Unmarshal(b, &r) != nil {
		return Entry{}, false
	}
	p.n++
	e := Entry{Seq: p.n, At: parseTime(r.Timestamp)}
	if r.IsSidechain {
		return e, false
	}
	blocks := claudeBlocks(r.Message)
	switch r.Type {
	case "user":
		if r.IsMeta || len(r.ToolUseResult) > 0 {
			return e, false
		}
		var texts []string
		for _, bl := range blocks {
			if bl.Type == "tool_result" {
				return e, false
			}
			if bl.Type == "text" && bl.Text != "" {
				texts = append(texts, bl.Text)
			}
		}
		e.Kind, e.Text = "user", strings.Join(texts, "\n")
		if strings.Contains(e.Text, "[pyre-msg:") {
			e.Kind = "message"
		}
	case "assistant":
		var texts, tools []string
		for _, bl := range blocks {
			switch bl.Type {
			case "text":
				texts = append(texts, bl.Text)
			case "tool_use":
				tools = append(tools, toolSummary(bl.Name, bl.Input))
			}
		}
		switch {
		case len(texts) > 0:
			e.Kind, e.Text = "assistant", strings.Join(texts, "\n")
		case len(tools) > 0:
			e.Kind, e.Text = "tool", strings.Join(tools, "; ")
		}
	}
	e.Text = clipText(strings.TrimSpace(e.Text))
	return e, e.Kind != "" && e.Text != ""
}

func claudeBlocks(msg json.RawMessage) []claudeBlock {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if len(msg) == 0 || json.Unmarshal(msg, &m) != nil {
		return nil
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []claudeBlock{{Type: "text", Text: s}}
	}
	var bs []claudeBlock
	_ = json.Unmarshal(m.Content, &bs)
	return bs
}

// toolSummary is "Name: <command|path|pattern…>" like the adapters show.
func toolSummary(name string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	for _, k := range []string{"command", "cmd", "file_path", "path", "pattern", "query", "description", "url"} {
		if s, ok := in[k].(string); ok && s != "" {
			return name + ": " + truncRunes(s, 300)
		}
	}
	return name
}

// ---- Codex ----

// codexParser reads rollout files. Messages are taken from response_item
// records (present in every rollout version); the injected environment and
// instruction preambles are skipped. Seq counts kept entries.
type codexParser struct{ n int64 }

type codexLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
	} `json:"payload"`
}

func (p *codexParser) seq() int64 { return p.n }
func (p *codexParser) skip()      {}

func (p *codexParser) line(b []byte) (Entry, bool) {
	var l codexLine
	if json.Unmarshal(b, &l) != nil || l.Type != "response_item" {
		return Entry{}, false
	}
	e := Entry{At: parseTime(l.Timestamp)}
	pl := l.Payload
	switch pl.Type {
	case "message":
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(pl.Content, &blocks)
		var texts []string
		for _, bl := range blocks {
			if (bl.Type == "input_text" || bl.Type == "output_text" || bl.Type == "text") && bl.Text != "" {
				texts = append(texts, bl.Text)
			}
		}
		text := strings.TrimSpace(strings.Join(texts, "\n"))
		switch pl.Role {
		case "user":
			if injectedPreamble(text) {
				return e, false
			}
			e.Kind = "user"
			if strings.Contains(text, "[pyre-msg:") {
				e.Kind = "message"
			}
		case "assistant":
			e.Kind = "assistant"
		default:
			return e, false
		}
		e.Text = text
	case "function_call", "custom_tool_call", "local_shell_call":
		args := pl.Arguments
		if args == "" {
			args = pl.Input
		}
		sum := pl.Name
		if raw := json.RawMessage(args); json.Valid(raw) {
			sum = toolSummary(pl.Name, raw)
		} else if args != "" {
			sum = pl.Name + ": " + truncRunes(args, 300)
		}
		e.Kind, e.Text = "tool", sum
	default:
		return e, false
	}
	e.Text = clipText(e.Text)
	if e.Text == "" {
		return e, false
	}
	p.n++
	e.Seq = p.n
	return e, true
}

// injectedPreamble: Codex prepends environment context, AGENTS.md and
// permission instructions as user-role messages; they are not the user's.
func injectedPreamble(s string) bool {
	if strings.HasPrefix(s, "# AGENTS.md") {
		return true
	}
	if !strings.HasPrefix(s, "<") {
		return false
	}
	end := strings.IndexByte(s, '>')
	if end < 2 || end > 40 {
		return false
	}
	for _, r := range s[1:end] {
		if !(r >= 'a' && r <= 'z' || r == '_' || r == '-' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// ---- helpers ----

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func clipText(s string) string { return truncRunes(s, maxEntryText) }

func truncRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
