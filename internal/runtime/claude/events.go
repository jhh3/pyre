package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/jhh3/pyre/internal/osutil"
)

// Files inside a run's private state dir (0700).
const (
	fileHooks      = "pyre-hooks.json"  // settings passed with `claude --settings`
	fileEvents     = "events.jsonl"     // hook + Pyre broker events (append, flock, fsync)
	fileDeliveries = "deliveries.jsonl" // delivery journal (attempting before paste)
	fileBroker     = "broker.json"      // approval broker config (presence enables it)
	fileRun        = "run.json"         // launch record
	fileExit       = "exit.json"        // exit status captured before the pane is removed
	dirPending     = "pending"          // approval requests written by the hook
	dirAnswers     = "answers"          // answers written by Answer()
	dirInject      = "inject"           // Stop-hook injection queue
)

// Synthetic events written by the Pyre side of the hook.
const (
	evApprovalAnswered  = "PyreApprovalAnswered"
	evApprovalTimeout   = "PyreApprovalTimeout"
	evApprovalWithdrawn = "PyreApprovalWithdrawn"
	evStopInjected      = "PyreStopInjected"
)

// hookRecord is one line of events.jsonl.
type hookRecord struct {
	ID             string          `json:"id"`
	At             time.Time       `json:"at"`
	Event          string          `json:"event"`
	SessionID      string          `json:"session_id,omitempty"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	Markers        []string        `json:"markers,omitempty"` // pyre message ids found in the prompt
	RequestID      string          `json:"request_id,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	Behavior       string          `json:"behavior,omitempty"`
	MessageID      string          `json:"message_id,omitempty"`
	HookPID        int             `json:"hook_pid,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`

	offset int64 // byte offset of the line (not serialized)
}

// payload decodes the bounded hook payload.
func (r *hookRecord) payload() map[string]any {
	var m map[string]any
	if len(r.Payload) > 0 {
		_ = json.Unmarshal(r.Payload, &m)
	}
	return m
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

// markerRe matches "[pyre-msg:<id>]" and the dated header "[pyre-msg:<id> · sent …]".
var markerRe = regexp.MustCompile(`\[pyre-msg:([A-Za-z0-9_.:\-]{1,200})(?:\]| · [^\]\n]{0,80}\])`)

// findMarkers returns the Pyre message ids whose markers appear in s.
func findMarkers(s string) []string {
	var out []string
	for _, m := range markerRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

var safeIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,200}$`)

func safeID(id string) bool { return safeIDRe.MatchString(id) && id != "." && id != ".." }

func newEventID() string {
	return "h-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
}

// appendJSONL appends one JSON line under an exclusive flock and fsyncs.
func appendJSONL(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// O_RDWR, not O_WRONLY: Go opens an O_APPEND file on Windows with
	// append-only access, and LockFileEx then fails (access denied), so the
	// lock below never succeeded and nothing was written (GCP Windows node).
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

// writeFileAtomic writes via a temp file and rename (0600).
func writeFileAtomic(path string, b []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readJSONLFrom reads complete lines starting at byte offset off. It returns
// the offset after the last complete line (a trailing partial line is left
// for the next read).
func readJSONLFrom(path string, off int64, maxBytes int64, fn func(line []byte, offset int64)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return off, nil
		}
		return off, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return off, err
	}
	if st.Size() < off { // truncated/replaced
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	pos := off
	var read int64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			fn(line[:len(line)-1], pos)
			pos += int64(len(line))
			read += int64(len(line))
			if maxBytes > 0 && read >= maxBytes {
				return pos, nil
			}
			continue
		}
		if err == io.EOF {
			return pos, nil
		}
		if err != nil {
			return pos, err
		}
	}
}

// readEvents returns hook records after offset.
func readEvents(stateDir string, off int64) ([]hookRecord, int64, error) {
	var recs []hookRecord
	next, err := readJSONLFrom(filepath.Join(stateDir, fileEvents), off, 0, func(line []byte, o int64) {
		var r hookRecord
		if json.Unmarshal(line, &r) == nil && r.Event != "" {
			r.offset = o
			recs = append(recs, r)
		}
	})
	return recs, next, err
}

// boundPayload limits stored hook payloads: long strings are truncated and an
// oversized payload is reduced to its scalar top-level fields. Payloads stay
// on the node (state dir is 0700) and are never reported verbatim.
func boundPayload(raw []byte) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	b, _ := json.Marshal(truncateValue(m, 0))
	if len(b) <= 64<<10 {
		return b
	}
	small := map[string]any{"pyre_reduced": true}
	for k, v := range m {
		switch x := v.(type) {
		case string:
			small[k] = truncStr(x, 2048)
		case bool, float64, nil:
			small[k] = x
		}
	}
	b, _ = json.Marshal(small)
	return b
}

func truncateValue(v any, depth int) any {
	switch x := v.(type) {
	case string:
		return truncStr(x, 8192)
	case map[string]any:
		if depth > 8 {
			return "…"
		}
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = truncateValue(vv, depth+1)
		}
		return out
	case []any:
		if depth > 8 {
			return "…"
		}
		n := len(x)
		if n > 200 {
			n = 200
		}
		out := make([]any, n)
		for i := 0; i < n; i++ {
			out[i] = truncateValue(x[i], depth+1)
		}
		return out
	}
	return v
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// keep valid UTF-8
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…[%d bytes truncated]", len(s)-cut)
}
