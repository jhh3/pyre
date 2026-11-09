package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
	"github.com/jhh3/pyre/internal/runtime/claude"
	"github.com/jhh3/pyre/internal/runtime/codex"
)

// Every Claude and Codex adapter returns full text to the daemon.
var (
	_ transcriptFullReader = (*claude.Adapter)(nil)
	_ transcriptFullReader = (*claude.ExecAdapter)(nil)
	_ transcriptFullReader = (*codex.Adapter)(nil)
	_ transcriptFullReader = (*codex.ExecAdapter)(nil)
)

// cannedRead answers every read with the same entries and screen, as an
// adapter reads them from the harness's transcript (nothing redacted).
type cannedRead struct {
	contracts.RuntimeAdapter
	entries []contracts.TranscriptEntry
	screen  string
}

func (c *cannedRead) ReadTranscript(context.Context, contracts.RunHandle, contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	return contracts.ReadSessionResponse{Entries: append([]contracts.TranscriptEntry(nil), c.entries...), Screen: c.screen}, nil
}

// A default read (no full, no seq) is redacted on the node for Claude and
// Codex runs alike: secret-shaped substrings are masked in entries and the
// screen, and every other entry is returned byte for byte.
func TestDefaultReadRedacted(t *testing.T) {
	plain := []string{
		"Reply with the test results.\n\nKeep the token budget small; key ordering is fine.",
		"command: go test ./internal/daemon/ -run TestDefaultReadRedacted (exit 0)",
		`{"input_tokens": 1234, "topic_key": "inbox-asks"} — ünïcode ✓` + "\r\n",
		"see https://example.invalid/pull/77; ask-owner and disk-cleanup",
	}
	secret := []string{
		"export ANTHROPIC_API_KEY=sk-ant-api03-SYNTHETICsyntheticSYNTHETIC00",
		"curl -H 'Authorization: Bearer synthetic.bearer.value0123'",
		"session token pys_synthetic0123456789abcdef",
		"OPENAI_API_KEY: sk-proj-SYNTHETICsyntheticSYNTHETIC0123",
	}
	leaks := []string{"SYNTHETICsynthetic", "synthetic.bearer", "synthetic0123456789"}
	for _, h := range []contracts.Harness{contracts.HarnessClaude, contracts.HarnessCodex} {
		t.Run(string(h), func(t *testing.T) {
			var es []contracts.TranscriptEntry
			for i, s := range append(append([]string(nil), plain...), secret...) {
				es = append(es, contracts.TranscriptEntry{Seq: int64(i + 1), RunID: "run_r", Kind: "assistant", Text: s})
			}
			ad := &cannedRead{RuntimeAdapter: daemontest.New(daemontest.NewWorld(), h), entries: es,
				screen: "$ export GITHUB_TOKEN=ghp_SYNTHETICsyntheticSYNTHETIC1234\n> "}
			n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}})
			n.start(startCmd("ses_r", "run_r", h, t.TempDir()))
			res := n.do(command(contracts.CmdReadTranscript, contracts.ReadSessionRequest{SessionID: "ses_r", Tail: true, Limit: 40}))
			if res.Error != nil {
				t.Fatal(res.Error)
			}
			var resp contracts.ReadSessionResponse
			if err := json.Unmarshal(res.Result, &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Entries) != len(es) || resp.Full {
				t.Fatalf("entries %d full %v", len(resp.Entries), resp.Full)
			}
			for i, e := range resp.Entries {
				if i < len(plain) {
					if e.Text != plain[i] {
						t.Fatalf("ordinary entry %d changed: %q -> %q", e.Seq, plain[i], e.Text)
					}
					continue
				}
				if !strings.Contains(e.Text, "[REDACTED]") {
					t.Fatalf("entry %d not redacted: %q", e.Seq, e.Text)
				}
				for _, l := range leaks {
					if strings.Contains(e.Text, l) {
						t.Fatalf("entry %d leaks %q: %q", e.Seq, l, e.Text)
					}
				}
			}
			if strings.Contains(resp.Screen, "SYNTHETICsynthetic") || !strings.Contains(resp.Screen, "GITHUB_TOKEN=[REDACTED]") {
				t.Fatalf("screen not redacted: %q", resp.Screen)
			}
		})
	}
}

// codexExecRun writes the state of a pyre run codex exec run whose rollout
// holds messages (assistant texts after one user prompt).
func codexExecRun(t *testing.T, messages ...string) contracts.RunHandle {
	t.Helper()
	dir, home, thread := t.TempDir(), t.TempDir(), "0199a213-81c0-7800-8aa1-0000000f0110"
	launched := time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)
	meta, _ := json.Marshal(map[string]any{"run_id": "run_cxfull", "thread_id": thread, "codex_home": home, "launched_at": launched.UTC()})
	if err := os.WriteFile(filepath.Join(dir, "run.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(home, "sessions", launched.Format("2006"), launched.Format("01"), launched.Format("02"))
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(role, kind, text string) string {
		b, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": role,
			"content": []any{map[string]any{"type": kind, "text": text}}}})
		return string(b) + "\n"
	}
	rollout := `{"type":"session_meta","payload":{"id":"` + thread + `"}}` + "\n" + line("user", "input_text", "write the report")
	for _, m := range messages {
		rollout += line("assistant", "output_text", m)
	}
	if err := os.WriteFile(filepath.Join(day, "rollout-2026-10-23T12-00-00-"+thread+".jsonl"), []byte(rollout), 0o600); err != nil {
		t.Fatal(err)
	}
	return contracts.RunHandle{RunID: "run_cxfull", SessionID: "ses_cxfull", Adapter: codex.ExecAdapterName, Harness: contracts.HarnessCodex, StateDir: dir}
}

// A full read of a pyre run codex exec run through the real adapter: the
// long final message comes back whole with its newlines and redacted, and
// the per-entry and per-read bounds hold, newest first.
func TestCodexExecFullRead(t *testing.T) {
	base, err := codex.New(codex.Options{NodeID: "node_test", TmuxSocket: shortSock(t), StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	x := codex.NewExec(base)
	ctx := context.Background()
	report := "## Synthetic final report\n\n" + strings.Repeat("- proof line: ok\n", 300) + "token: synthetic-report-secret-0123\nEND"
	h := codexExecRun(t, "short", report)
	resp, err := x.ReadTranscript(ctx, h, contracts.ReadSessionRequest{RunID: h.RunID, Tail: true, Limit: 40})
	if err != nil || len(resp.Entries) != 3 || !resp.Entries[2].Truncated || strings.Contains(resp.Entries[2].Text, "\n") {
		t.Fatalf("default read: %v %+v", err, resp.Entries)
	}
	expandFull(ctx, x, h, &resp)
	want := strings.Replace(report, "synthetic-report-secret-0123", "[REDACTED]", 1)
	if e := resp.Entries[2]; e.Text != want || e.Truncated || resp.FullNote != "" || resp.Entries[1].Text != "short" {
		t.Fatalf("full read: truncated=%v note=%q text %d bytes (want %d)", e.Truncated, resp.FullNote, len(e.Text), len(want))
	}

	// Bounds: the newest answer (300 KB) is cut at 256 KB; of five 245 KB
	// answers before it the newest three fit the rest of the 1 MB read and
	// the oldest two keep their default cut.
	var msgs []string
	for i := 1; i <= 5; i++ {
		msgs = append(msgs, fmt.Sprintf("synthetic answer %d\n", i)+strings.Repeat(strings.Repeat("y", 1023)+"\n", 245))
	}
	msgs = append(msgs, "synthetic huge\n"+strings.Repeat("z", 300<<10))
	h = codexExecRun(t, msgs...)
	resp, err = x.ReadTranscript(ctx, h, contracts.ReadSessionRequest{RunID: h.RunID, Tail: true, Limit: 40})
	if err != nil || len(resp.Entries) != 7 {
		t.Fatalf("read: %v %d entries", err, len(resp.Entries))
	}
	expandFull(ctx, x, h, &resp)
	es := resp.Entries
	if huge := es[6]; !huge.Truncated || !strings.HasSuffix(huge.Text, "a full read keeps at most 256 KB per entry]") || !strings.HasPrefix(huge.Text, "synthetic huge\nzzz") {
		t.Fatalf("per-entry bound: truncated=%v len %d", huge.Truncated, len(huge.Text))
	}
	for i, m := range msgs[:5] {
		e := es[i+1]
		if whole := e.Text == strings.TrimSpace(m); whole != (i >= 2) || e.Truncated == whole { // a rollout message is trimmed
			t.Fatalf("per-read bound: answer %d whole=%v truncated=%v", i+1, whole, e.Truncated)
		}
	}
	total := 0
	for _, e := range es {
		b, _ := json.Marshal(e.Text)
		total += len(b)
	}
	if total > contracts.FullReadMax+8<<10 {
		t.Fatalf("response carries %d bytes of text", total)
	}
	if !strings.Contains(resp.FullNote, fmt.Sprintf("seq %d: longer than the 256 KB", es[6].Seq)) ||
		!strings.Contains(resp.FullNote, fmt.Sprintf("seq %d, %d: past the 1024 KB", es[1].Seq, es[2].Seq)) {
		t.Fatalf("note: %q", resp.FullNote)
	}
}

// A full text read from another numbering than the read's (a Codex run
// that stopped between the read and the full text) does not replace the
// entry: it keeps its default cut and the note says so.
func TestExpandFullRejectsOtherText(t *testing.T) {
	long := "synthetic live report " + strings.Repeat("line\n", 600)
	st := &fullStub{texts: map[int64]string{7: "a different rollout entry " + strings.Repeat("x", 3000)}}
	resp := contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{cutEntry(7, "assistant", long)}}
	want := resp.Entries[0].Text
	expandFull(context.Background(), st, contracts.RunHandle{}, &resp)
	if resp.Entries[0].Text != want || !resp.Entries[0].Truncated || !strings.Contains(resp.FullNote, "seq 7: not found again") {
		t.Fatalf("other text taken: %+v note %q", resp.Entries[0].Truncated, resp.FullNote)
	}
	// A Codex cut (whitespace folded onto one line) matches its full text.
	folded := strings.Join(strings.Fields(long), " ")[:2000] + "…"
	st.texts[7] = long
	resp = contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{{Seq: 7, Kind: "assistant", Text: folded, Truncated: true}}}
	expandFull(context.Background(), st, contracts.RunHandle{}, &resp)
	if resp.Entries[0].Text != long || resp.Entries[0].Truncated || resp.FullNote != "" {
		t.Fatalf("folded cut not expanded: truncated=%v note=%q", resp.Entries[0].Truncated, resp.FullNote)
	}
}

// Redacting a default read (40 entries of 2000 bytes) costs well under the
// few milliseconds a read may spend on it.
func TestRedactReadCost(t *testing.T) {
	base := "Ran go test ./internal/daemon/ -run TestExpandFull; it passed. The token budget and key ordering look fine, see https://example.invalid/pull/77.\n"
	text := strings.Repeat(base, 2000/len(base)+1)[:2000]
	ds := make([]time.Duration, 21)
	for i := range ds {
		resp := contracts.ReadSessionResponse{Screen: text}
		for j := 0; j < 40; j++ {
			resp.Entries = append(resp.Entries, contracts.TranscriptEntry{Seq: int64(j + 1), Text: text})
		}
		t0 := time.Now()
		redactRead(&resp, true)
		ds[i] = time.Since(t0)
		if resp.Entries[39].Text != text {
			t.Fatal("ordinary text changed")
		}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("redact 40 entries x 2000 bytes + screen: p50 %s max %s", ds[len(ds)/2], ds[len(ds)-1])
	if ds[len(ds)/2] > 10*time.Millisecond {
		t.Fatalf("redacting a default read takes %s", ds[len(ds)/2])
	}
}
