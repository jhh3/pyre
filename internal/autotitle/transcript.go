package autotitle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Formats match internal/search.
const (
	FormatClaude = "claude"
	FormatCodex  = "codex"
)

// maxLine bounds one transcript record the scanner parses; longer records
// (pasted files, big tool results) are skipped.
const maxLine = 1 << 20

// Scanner incrementally reads one run's transcript files and keeps the best
// title evidence seen so far. Only complete lines are consumed; offsets are
// kept per file so each poll reads only what was appended.
type Scanner struct {
	Format  string
	offsets map[string]int64

	custom      string // Claude custom-title (the person ran /rename in Claude)
	native      string // Claude ai-title/summary, Codex thread name (latest wins)
	firstPrompt string
	codexThread string
	codexHome   string
}

// NewScanner returns a scanner for a transcript format.
func NewScanner(format string) *Scanner {
	return &Scanner{Format: format, offsets: map[string]int64{}}
}

// Best returns the best title known and its source ("" when none yet).
func (s *Scanner) Best() (title, source string) {
	switch {
	case s.custom != "":
		return s.custom, SourceCustom
	case s.native != "":
		return s.native, SourceNative
	case s.firstPrompt != "":
		return s.firstPrompt, SourcePrompt
	}
	return "", ""
}

// HasPrompt reports that a first user prompt was seen (the first turn began).
func (s *Scanner) HasPrompt() bool { return s.firstPrompt != "" }

// Feed reads what was appended to the given files since the last call.
func (s *Scanner) Feed(paths []string) {
	for _, p := range paths {
		s.feedFile(p)
	}
	if s.Format == FormatCodex && s.codexThread != "" && s.codexHome != "" {
		if n := codexIndexName(filepath.Join(s.codexHome, "session_index.jsonl"), s.codexThread); n != "" {
			s.native = n
		}
	}
}

func (s *Scanner) feedFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	off := s.offsets[path]
	if st, err := f.Stat(); err != nil || st.Size() < off {
		off = 0 // truncated or replaced: start over
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	if s.Format == FormatCodex && s.codexHome == "" {
		// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl
		s.codexHome = filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))))
	}
	br := bufio.NewReaderSize(f, 64<<10)
	var long bool
	var buf []byte
	committed := off
	for {
		chunk, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			off += int64(len(chunk))
			if !long && len(buf)+len(chunk) <= maxLine {
				buf = append(buf, chunk...)
			} else {
				long, buf = true, nil
			}
			continue
		}
		if err != nil {
			break // partial last line: re-read next time
		}
		off += int64(len(chunk))
		committed = off
		if !long {
			line := chunk
			if len(buf) > 0 {
				line = append(buf, chunk...)
			}
			s.line(bytes.TrimSpace(line))
		}
		long, buf = false, nil
	}
	s.offsets[path] = committed
}

func (s *Scanner) line(b []byte) {
	if len(b) == 0 {
		return
	}
	if s.Format == FormatCodex {
		s.codexLine(b)
		return
	}
	s.claudeLine(b)
}

type claudeRec struct {
	Type          string          `json:"type"`
	AITitle       string          `json:"aiTitle"`
	CustomTitle   string          `json:"customTitle"`
	Summary       string          `json:"summary"`
	IsMeta        bool            `json:"isMeta"`
	IsSidechain   bool            `json:"isSidechain"`
	Message       json.RawMessage `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

func (s *Scanner) claudeLine(b []byte) {
	// Cheap prefilter: only records that can matter are decoded.
	if !bytes.Contains(b, []byte(`"ai-title"`)) && !bytes.Contains(b, []byte(`"custom-title"`)) &&
		!bytes.Contains(b, []byte(`"summary"`)) && (s.firstPrompt != "" || !bytes.Contains(b, []byte(`"user"`))) {
		return
	}
	var r claudeRec
	if json.Unmarshal(b, &r) != nil {
		return
	}
	switch r.Type {
	case "custom-title":
		if t := Clean(r.CustomTitle); t != "" {
			s.custom = t
		}
	case "ai-title":
		if t := Clean(r.AITitle); t != "" {
			s.native = t
		}
	case "summary":
		if t := Clean(r.Summary); t != "" {
			s.native = t
		}
	case "user":
		if s.firstPrompt != "" || r.IsMeta || r.IsSidechain || len(r.ToolUseResult) > 0 {
			return
		}
		text, ok := claudeUserText(r.Message)
		if !ok {
			return
		}
		s.firstPrompt = FromPrompt(text)
	}
}

func claudeUserText(msg json.RawMessage) (string, bool) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if len(msg) == 0 || json.Unmarshal(msg, &m) != nil {
		return "", false
	}
	var str string
	if json.Unmarshal(m.Content, &str) == nil {
		return str, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return "", false
	}
	var texts []string
	for _, bl := range blocks {
		if bl.Type == "tool_result" {
			return "", false
		}
		if bl.Type == "text" {
			texts = append(texts, bl.Text)
		}
	}
	return strings.Join(texts, "\n"), len(texts) > 0
}

type codexRec struct {
	Type    string `json:"type"`
	Payload struct {
		Type       string `json:"type"`
		ID         string `json:"id"`
		Message    string `json:"message"`
		ThreadName string `json:"thread_name"`
		Name       string `json:"name"`
	} `json:"payload"`
}

func (s *Scanner) codexLine(b []byte) {
	if !bytes.Contains(b, []byte(`"session_meta"`)) && !bytes.Contains(b, []byte(`thread_name`)) &&
		(s.firstPrompt != "" || !bytes.Contains(b, []byte(`"user_message"`))) {
		return
	}
	var r codexRec
	if json.Unmarshal(b, &r) != nil {
		return
	}
	switch {
	case r.Type == "session_meta":
		if s.codexThread == "" {
			s.codexThread = r.Payload.ID
		}
	case r.Payload.Type == "thread_name_updated":
		n := r.Payload.ThreadName
		if n == "" {
			n = r.Payload.Name
		}
		if t := Clean(n); t != "" {
			s.native = t
		}
	case r.Type == "event_msg" && r.Payload.Type == "user_message" && s.firstPrompt == "":
		s.firstPrompt = FromPrompt(r.Payload.Message)
	}
}

// codexIndexName returns the latest thread_name for a thread in Codex's
// session_index.jsonl ("" if absent). The file is small (one row per named
// thread); only the last 256 KiB are read.
func codexIndexName(path, thread string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 256<<10 {
		_, _ = f.Seek(st.Size()-256<<10, io.SeekStart)
	}
	name := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		ln := sc.Bytes()
		if !bytes.Contains(ln, []byte(thread)) {
			continue
		}
		var r struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(ln, &r) == nil && r.ID == thread {
			if t := Clean(r.ThreadName); t != "" {
				name = t
			}
		}
	}
	return name
}
