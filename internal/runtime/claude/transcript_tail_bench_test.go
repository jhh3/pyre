package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// writeSyntheticTranscript writes an obviously synthetic Claude transcript
// of about size bytes: turns of a prompt, assistant text, a tool call, a
// large tool result and a turn_duration record, plus records sessions.read
// skips (sidechain, snapshots). It returns the number of lines written.
func writeSyntheticTranscript(t testing.TB, path string, size int64) int {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	filler := strings.Repeat("synthetic tool output line 0123456789 abcdefghijklmnopqrstuvwxyz\n", 150) // ~10 KB
	var written int64
	n := 0
	emit := func(v any) {
		b, _ := json.Marshal(v)
		w.Write(b)
		w.WriteByte('\n')
		written += int64(len(b) + 1)
		n++
	}
	for turn := 0; written < size; turn++ {
		ts := func(s int) string { return base.Add(time.Duration(turn*60+s) * time.Second).Format(time.RFC3339Nano) }
		emit(map[string]any{"type": "user", "uuid": fmt.Sprintf("u%d", turn), "timestamp": ts(0),
			"message": map[string]any{"role": "user", "content": fmt.Sprintf("synthetic prompt %d: please do the thing", turn)}})
		emit(map[string]any{"type": "assistant", "timestamp": ts(1), "message": map[string]any{"role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("synthetic answer %d ", turn) + strings.Repeat("words ", 100)}}}})
		emit(map[string]any{"type": "assistant", "timestamp": ts(2), "message": map[string]any{"role": "assistant",
			"content": []any{map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": fmt.Sprintf("echo synthetic %d", turn)}}}}})
		emit(map[string]any{"type": "user", "timestamp": ts(3), "toolUseResult": map[string]any{"stdout": filler},
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": filler}}}})
		emit(map[string]any{"type": "assistant", "isSidechain": true, "timestamp": ts(4), "message": map[string]any{"role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "synthetic sidechain"}}}})
		emit(map[string]any{"type": "file-history-snapshot", "snapshot": map[string]any{"files": strings.Repeat("x", 500)}})
		emit(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 1500 + turn, "timestamp": ts(5)})
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return n
}

// fullParseEntries is the pre-cache reader: every record of the file
// parsed from the start (the reference the tail reader must equal).
func fullParseEntries(path, runID string) []contracts.TranscriptEntry {
	var recs []transcriptRecord
	_, _ = readJSONLFrom(path, 0, 0, func(line []byte, _ int64) {
		var t transcriptRecord
		if json.Unmarshal(line, &t) == nil {
			recs = append(recs, t)
		}
	})
	return entriesFromTranscript(runID, recs, 0)
}

func benchTranscript(b *testing.B) string {
	path := filepath.Join(b.TempDir(), "synthetic.jsonl")
	writeSyntheticTranscript(b, path, 50<<20)
	return path
}

func BenchmarkTranscriptFullParse50MB(b *testing.B) {
	path := benchTranscript(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fullParseEntries(path, "run_bench")
	}
}

// The tail reader on the same 50 MB file: the first read of a file
// (backwards for the window, records before it only counted), a repeat
// read (stat only), and a read after a record was appended.
func BenchmarkTranscriptTailCold50MB(b *testing.B) {
	path := benchTranscript(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var c tailCache
		if _, err := c.read(path, "run_bench", 0, 30, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTranscriptTailWarm50MB(b *testing.B) {
	path := benchTranscript(b)
	var c tailCache
	c.read(path, "run_bench", 0, 30, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.read(path, "run_bench", 0, 30, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTranscriptTailAppend50MB(b *testing.B) {
	path := benchTranscript(b)
	var c tailCache
	c.read(path, "run_bench", 0, 30, true)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	rec := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"synthetic appended answer"}]}}` + "\n"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.WriteString(rec)
		if _, err := c.read(path, "run_bench", 0, 30, true); err != nil {
			b.Fatal(err)
		}
	}
}

// A forward page far behind the window (the reader paging old history)
// starts at the nearest mark, not at the start of the file.
func BenchmarkTranscriptForwardPage50MB(b *testing.B) {
	path := benchTranscript(b)
	var c tailCache
	c.read(path, "run_bench", 0, 30, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.read(path, "run_bench", 1000, 200, false); err != nil {
			b.Fatal(err)
		}
	}
}
