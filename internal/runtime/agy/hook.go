package agy

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jhh3/pyre/internal/osutil"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// HookEvents are the agy lifecycle hooks Pyre registers in the run's
// hooks.json (agy 1.4.2 "Lifecycle Hooks"). PreToolUse is deliberately
// absent: its handler must answer with a permission decision, and an empty
// answer denies the tool call (observed on 1.4.2), so a recording hook there
// would break every run. Interrupting a turn (Escape) fires no hook; the
// screen tells that (screen.go).
var HookEvents = []string{"SessionStart", "PreInvocation", "PostToolUse", "PostInvocation", "Stop"}

// toolMatcherEvents take a tool-name matcher group.
var toolMatcherEvents = map[string]bool{"PostToolUse": true}

const (
	fileEvents     = "agy-events.jsonl" // hook records (this package's HookMain)
	fileRun        = "run.json"         // runMeta
	fileExit       = "exit.json"
	fileDeliveries = "deliveries.jsonl"
)

// hookRecord is one recorded hook call. Only the fields Pyre uses are kept:
// tool arguments (which may hold anything the agent typed) never reach the
// state dir.
type hookRecord struct {
	Event          string    `json:"event"`
	At             time.Time `json:"at"`
	ConversationID string    `json:"conversation_id,omitempty"`
	TranscriptPath string    `json:"transcript_path,omitempty"`
	Model          string    `json:"model,omitempty"`
	Tool           string    `json:"tool,omitempty"`
	ToolError      bool      `json:"tool_error,omitempty"`
	Termination    string    `json:"termination,omitempty"` // Stop: NO_TOOL_CALL, ERROR, USER_CANCELED, ...
	FullyIdle      *bool     `json:"fully_idle,omitempty"`
	Error          string    `json:"error,omitempty"`

	offset int64
}

// hookPayload is the subset of agy's hook stdin Pyre reads (camelCase
// protojson).
type hookPayload struct {
	ConversationID    string `json:"conversationId"`
	TranscriptPath    string `json:"transcriptPath"`
	ModelName         string `json:"modelName"`
	TerminationReason string `json:"terminationReason"`
	FullyIdle         *bool  `json:"fullyIdle"`
	Error             string `json:"error"`
	ToolCall          *struct {
		Name string `json:"name"`
	} `json:"toolCall"`
}

// HookMain is `pyred hook agy <Event> <state-dir>`: it appends the hook call
// to <state-dir>/agy-events.jsonl and answers "{}" (no decision: agy carries
// on as without the hook). It always exits 0 and never blocks agy beyond a
// bounded stdin read.
func HookMain(args []string, stdin io.Reader, stdout io.Writer) int {
	defer func() { _, _ = io.WriteString(stdout, "{}\n") }()
	if len(args) < 2 {
		return 0
	}
	event, dir := args[0], args[1]
	if !filepath.IsAbs(dir) {
		return 0
	}
	raw, _ := io.ReadAll(io.LimitReader(bufio.NewReader(stdin), 4<<20))
	var p hookPayload
	_ = json.Unmarshal(raw, &p)
	r := hookRecord{Event: event, At: time.Now().UTC(), ConversationID: p.ConversationID, TranscriptPath: p.TranscriptPath,
		Model: p.ModelName, Termination: p.TerminationReason, FullyIdle: p.FullyIdle, Error: clip(p.Error, 300)}
	if p.ToolCall != nil {
		r.Tool = clip(p.ToolCall.Name, 80)
		r.ToolError = p.Error != ""
	}
	if !safeConversationID(r.ConversationID) {
		r.ConversationID = ""
	}
	_ = appendJSONL(filepath.Join(dir, fileEvents), r)
	return 0
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// hooksFile is the run's hooks.json: one named hook ("pyre") whose handlers
// run hookCmd + [event, stateDir]. Hooks from the owner's own customization
// roots (global, workspace) are merged by agy and keep running.
func hooksFile(hookCmd []string, stateDir string) ([]byte, error) {
	if len(hookCmd) == 0 {
		return nil, errors.New("agy: HookCommand is required")
	}
	type handler struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type group struct {
		Matcher string    `json:"matcher"`
		Hooks   []handler `json:"hooks"`
	}
	spec := map[string]any{}
	for _, ev := range HookEvents {
		h := handler{Type: "command", Command: tmux.ShellJoin(append(append([]string{}, hookCmd...), ev, stateDir)), Timeout: 10}
		if toolMatcherEvents[ev] {
			spec[ev] = []group{{Matcher: "*", Hooks: []handler{h}}}
		} else {
			spec[ev] = []handler{h}
		}
	}
	return json.MarshalIndent(map[string]any{"pyre": spec}, "", "  ")
}

// ---- files ----

func appendJSONL(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := osutil.Lock(f); err != nil {
		return err
	}
	defer osutil.Unlock(f)
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

func writeFileAtomic(path string, b []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readJSONLFrom calls fn for each complete line from byte offset off and
// returns the offset after the last complete line (a partial last line is
// left for the next read). A file shorter than off is read from the start.
func readJSONLFrom(path string, off int64, fn func(line []byte, offset int64)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return off, nil
		}
		return off, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() < off {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	pos := off
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			fn(line[:len(line)-1], pos)
			pos += int64(len(line))
		}
		if err != nil {
			return pos, nil
		}
	}
}

func readEvents(stateDir string, off int64) ([]hookRecord, int64) {
	var out []hookRecord
	next, _ := readJSONLFrom(filepath.Join(stateDir, fileEvents), off, func(line []byte, o int64) {
		var r hookRecord
		if json.Unmarshal(line, &r) == nil && r.Event != "" {
			r.offset = o
			out = append(out, r)
		}
	})
	return out, next
}
