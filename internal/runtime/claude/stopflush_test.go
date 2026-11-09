package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// A turn-end read right after Stop waits for Claude's buffered write of the
// turn's final message: the hub's report otherwise relayed the previous
// turn's last message (iss_…: Stop at 10:37:59.5,
// the read at .600 ended at a 10:37:48 tool result).
func TestTailReadWaitsForStopMessage(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Options{TmuxSocket: filepath.Join(dir, "sock"), HookCommand: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	h := contracts.RunHandle{RunID: "run_flush", StateDir: filepath.Join(dir, "state")}
	os.MkdirAll(h.StateDir, 0o700)
	tpath := filepath.Join(dir, "t.jsonl")
	t0 := time.Date(2026, 11, 3, 10, 32, 17, 0, time.UTC)
	appendJSON := func(path string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		fh.Write(append(b, '\n'))
		fh.Close()
	}
	say := func(text string, at time.Time) map[string]any {
		return map[string]any{"type": "assistant", "timestamp": at.Format(time.RFC3339Nano),
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	stop := func(id, msg string, at time.Time) hookRecord {
		p, _ := json.Marshal(map[string]any{"last_assistant_message": msg})
		return hookRecord{ID: id, Event: "Stop", At: at, Payload: p}
	}
	events := filepath.Join(h.StateDir, fileEvents)
	const stale = "Waiting for the end-to-end run on v1 and then v2."
	const final = "Both findings are fixed and pushed as a3ee7515."
	appendJSON(events, hookRecord{ID: "e1", Event: "SessionStart", At: t0, TranscriptPath: tpath})
	appendJSON(tpath, say(stale, t0))
	appendJSON(events, stop("e2", stale, t0.Add(time.Second)))
	lastSaid := func(resp contracts.ReadSessionResponse) string {
		for i := len(resp.Entries) - 1; i >= 0; i-- {
			if resp.Entries[i].Kind == "assistant" {
				return resp.Entries[i].Text
			}
		}
		return ""
	}
	ctx := context.Background()
	if resp, _ := a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Tail: true, Limit: 40}); lastSaid(resp) != stale {
		t.Fatalf("first turn: %q", lastSaid(resp))
	}

	// A background task wakes it; the turn ends with Stop before Claude
	// has written the final message.
	t1 := t0.Add(5 * time.Minute)
	appendJSON(events, stop("e3", final, t1))
	go func() {
		time.Sleep(300 * time.Millisecond)
		appendJSON(tpath, say(final, t1))
	}()
	resp, _ := a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Tail: true, Limit: 40})
	if got := lastSaid(resp); got != final {
		t.Fatalf("turn-end read relayed %q, want the turn's final message", got)
	}

	// Once shown (or given up on), reads do not wait.
	start := time.Now()
	a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Tail: true, Limit: 1})
	if d := time.Since(start); d > time.Second {
		t.Fatalf("read after the flush waited %v", d)
	}
	appendJSON(events, stop("e4", "a message never written", t1.Add(time.Minute)))
	a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Tail: true, Limit: 1})
	start = time.Now()
	a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Tail: true, Limit: 1})
	if d := time.Since(start); d > time.Second {
		t.Fatalf("second read after a missing message waited %v", d)
	}
}
