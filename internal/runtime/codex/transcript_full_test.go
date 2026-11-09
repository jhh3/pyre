package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/search"
)

// rolloutMsg is a rollout response_item line of role's message text.
func rolloutMsg(role, text string) string {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	b, _ := json.Marshal(map[string]any{"timestamp": "2026-10-21T04:48:53.164Z", "type": "response_item", "payload": map[string]any{
		"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}}}})
	return string(b) + "\n"
}

// writeRunRollout replaces the synthetic rollout of a stopped run (see
// stoppedRun) with content and returns its path.
func writeRunRollout(t *testing.T, home, thread, content string) string {
	t.Helper()
	p := filepath.Join(home, "sessions", rolloutDay.Format("2006"), rolloutDay.Format("01"), rolloutDay.Format("02"), "rollout-2026-10-20T21-48-50-"+thread+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendRollout(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// longReport is a child's final message: several KB over many lines (and
// over the search index's 8000-character clip).
func longReport(tag string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Synthetic report %s\n\n", tag)
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "- proof line %03d: ünïcode ok, `go test` passed\n", i)
	}
	b.WriteString("\nEND OF REPORT " + tag)
	return b.String()
}

// A Codex run read from its rollout (pyre run codex exec, or a stopped
// app-server run): a default read cuts the long final message and folds it
// onto one line as before; the full text is the whole message with its
// newlines, for the exec adapter and the app-server adapter alike.
func TestCodexRolloutFullText(t *testing.T) {
	e := newEnv(t, nil)
	thread := "01a15cca-0000-7000-8000-0000fullroll"
	h := stoppedRun(t, e.dir, "run_full", e.home, e.home, thread)
	report := longReport("rollout")
	writeRunRollout(t, e.home, thread, rolloutMsg("user", "Write the report")+rolloutMsg("assistant", "short note")+
		rolloutMsg("assistant", report)+`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n")
	ctx := context.Background()
	x := NewExec(e.a)
	for name, ad := range map[string]interface {
		ReadTranscript(context.Context, contracts.RunHandle, contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error)
		TranscriptFullText(context.Context, contracts.RunHandle, []int64) (map[int64]string, error)
	}{"exec": x, "app-server": e.a} {
		resp, err := ad.ReadTranscript(ctx, h, contracts.ReadSessionRequest{RunID: h.RunID, Tail: true, Limit: 40})
		if err != nil || len(resp.Entries) != 3 {
			t.Fatalf("%s: read %v %+v", name, err, resp.Entries)
		}
		last := resp.Entries[2]
		if !last.Truncated || last.Text != oneLine(report, 2000) || strings.Contains(last.Text, "\n") || resp.Entries[1].Text != "short note" {
			t.Fatalf("%s: default read changed: %+v", name, last)
		}
		got, err := ad.TranscriptFullText(ctx, h, []int64{last.Seq, 1 << 40})
		if err != nil || got[last.Seq] != report || len(got) != 1 {
			t.Fatalf("%s: full text %d bytes (want %d), %d texts, err %v", name, len(got[last.Seq]), len(report), len(got), err)
		}
	}
	// No rollout: nothing, no error.
	gone := stoppedRun(t, e.dir, "run_full_gone", filepath.Join(e.dir, "home-b"), filepath.Join(e.dir, "elsewhere"), "thr-missing")
	if got, err := x.TranscriptFullText(ctx, gone, []int64{1}); err != nil || len(got) != 0 {
		t.Fatalf("no rollout: %v %v", got, err)
	}
}

// A rollout that grows is parsed only from where the last parse ended, and
// default reads answer exactly what a parse of the whole file answers; a
// rewritten file is parsed again from the start.
func TestCodexRolloutIncrementalParse(t *testing.T) {
	e := newEnv(t, nil)
	thread := "01a15cca-0000-7000-8000-0000incparse"
	h := stoppedRun(t, e.dir, "run_inc", e.home, e.home, thread)
	p := writeRunRollout(t, e.home, thread, syntheticRollout(thread))
	ref := func() []contracts.TranscriptEntry {
		parsed, err := search.ParseFile(p, search.FormatCodex)
		if err != nil {
			t.Fatal(err)
		}
		var out []contracts.TranscriptEntry
		for _, pe := range parsed {
			en := rolloutEntry(pe)
			en.RunID = h.RunID
			out = append(out, en)
		}
		return out
	}
	read := func() []contracts.TranscriptEntry {
		sel, ok := rolloutTranscript(h.StateDir, runMeta{ThreadID: thread, Home: e.home, LaunchedAt: rolloutDay}, h.RunID, contracts.ReadSessionRequest{}, 500)
		if !ok {
			t.Fatal("no rollout")
		}
		return sel
	}
	check := func(step string) {
		t.Helper()
		if got, want := read(), ref(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: read %+v, want %+v", step, got, want)
		}
	}
	check("first")
	before := rolloutParsed.m[p].end
	appendRollout(t, p, rolloutMsg("assistant", longReport("a"))+rolloutMsg("user", "next")+`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"par`)
	time.Sleep(10 * time.Millisecond) // a new mtime on coarse clocks
	check("after an append ending in a partial line")
	if c := rolloutParsed.m[p]; c.end <= before || c.end >= c.size {
		t.Fatalf("parse end %d (was %d, size %d): the partial line is not left for later", c.end, before, c.size)
	}
	appendRollout(t, p, `tial"}]}}`+"\n")
	check("after the partial line completes")
	if got := read(); got[len(got)-1].Text != "partial" {
		t.Fatalf("completed line: %+v", got[len(got)-1])
	}
	// Replaced by another rollout (another session_meta), no shorter.
	old, _ := os.ReadFile(p)
	rewritten := strings.Replace(strings.Replace(string(old), thread, "01a15cca-0000-7000-8000-0000rewrites", 1), "Reply with just: ok", "Reply with just: no", 1)
	if err := os.WriteFile(p, []byte(rewritten+rolloutMsg("assistant", "more")), 0o600); err != nil {
		t.Fatal(err)
	}
	check("after a rewrite")
	if got := read(); got[0].Text != "Reply with just: no" {
		t.Fatalf("rewrite not parsed again: %+v", got[0])
	}
	if err := os.WriteFile(p, []byte(syntheticRollout(thread)), 0o600); err != nil {
		t.Fatal(err)
	}
	check("after the file shrank")
}

// A live run (app-server): a default read cuts and folds the long answer
// as before; the full text is the whole answer and the whole prompt, read
// from the kept settled page before the oldest seq, not from the first
// turn.
func TestCodexLiveFullText(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.a.turnsPage = 2
	h := e.launch(e.a, "run_livefull", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	for i := 0; i < 5; i++ {
		dlv(t, e.a, h, fmt.Sprintf("msg_lf%d", i), fmt.Sprintf("synthetic turn %d", i), contracts.ModeQueue)
		e.wait(e.a, h, 5*time.Second, idle)
	}
	report := longReport("live")
	dlv(t, e.a, h, "msg_lfreport", "FAKE_ECHO\n"+report, contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	ctx := context.Background()
	resp, err := e.a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Tail: true, Limit: 2})
	if err != nil || len(resp.Entries) != 2 {
		t.Fatalf("read: %v %+v", err, resp.Entries)
	}
	prompt, answer := resp.Entries[0], resp.Entries[1]
	if !answer.Truncated || answer.Kind != "assistant" || answer.Text != oneLine(report, 2000) || !prompt.Truncated {
		t.Fatalf("default read changed: %+v", resp.Entries)
	}
	logPath := filepath.Join(e.home, "fake-turns-list.log")
	_ = os.Remove(logPath)
	got, err := e.a.TranscriptFullText(ctx, h, []int64{prompt.Seq, answer.Seq})
	if err != nil || got[answer.Seq] != report || !strings.HasSuffix(got[prompt.Seq], "FAKE_ECHO\n"+report) {
		t.Fatalf("full text: answer %d bytes (want %d), prompt %d bytes, err %v", len(got[answer.Seq]), len(report), len(got[prompt.Seq]), err)
	}
	b, _ := os.ReadFile(logPath)
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) == 0 || strings.HasPrefix(calls[0], "<nil> ") || strings.HasPrefix(calls[0], " ") {
		t.Fatalf("listed from the first turn: %v", calls)
	}
	// Without kept pages (a fresh connection) it lists from the first turn.
	rr, _ := e.a.run(h)
	rr.readMu.Lock()
	rr.turns = nil
	rr.readMu.Unlock()
	if got, err := e.a.TranscriptFullText(ctx, h, []int64{answer.Seq}); err != nil || got[answer.Seq] != report {
		t.Fatalf("from the first turn: %d bytes, err %v", len(got[answer.Seq]), err)
	}
	// A kept cursor the app-server no longer knows: listed from the start.
	e.a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Tail: true, Limit: 2})
	rr.readMu.Lock()
	rr.turns.cursor, rr.turns.marks = "c99", nil
	rr.readMu.Unlock()
	if got, err := e.a.TranscriptFullText(ctx, h, []int64{answer.Seq}); err != nil || got[answer.Seq] != report {
		t.Fatalf("after an unknown cursor: %d bytes, err %v", len(got[answer.Seq]), err)
	}
}

// Timing on a large rollout (about 12 MB, 4000 messages): the first read
// parses the file once; later reads, a read after an append and a full
// read of the newest entries do not parse it again.
func TestCodexLargeRolloutTiming(t *testing.T) {
	e := newEnv(t, nil)
	thread := "01a15cca-0000-7000-8000-00000large"
	h := stoppedRun(t, e.dir, "run_large", e.home, e.home, thread)
	var b strings.Builder
	b.WriteString(`{"timestamp":"2026-10-21T04:48:51.191Z","type":"session_meta","payload":{"id":"` + thread + `"}}` + "\n")
	for i := 0; b.Len() < 12<<20; i++ {
		b.WriteString(rolloutMsg("user", fmt.Sprintf("synthetic question %d", i)))
		b.WriteString(rolloutMsg("assistant", strings.Repeat(fmt.Sprintf("synthetic answer %d line\n", i), 120)))
	}
	p := writeRunRollout(t, e.home, thread, b.String())
	fi, _ := os.Stat(p)
	ctx, x := context.Background(), NewExec(e.a)
	req := contracts.ReadSessionRequest{RunID: h.RunID, Tail: true, Limit: 40}
	timed := func(fn func()) time.Duration {
		t0 := time.Now()
		fn()
		return time.Since(t0)
	}
	var resp contracts.ReadSessionResponse
	cold := timed(func() { resp, _ = x.ReadTranscript(ctx, h, req) })
	if len(resp.Entries) != 40 {
		t.Fatalf("read %d entries", len(resp.Entries))
	}
	median := func(fn func()) time.Duration {
		ds := make([]time.Duration, 15)
		for i := range ds {
			ds[i] = timed(fn)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2]
	}
	cached := median(func() { x.ReadTranscript(ctx, h, req) })
	n := 0
	appended := median(func() {
		n++
		appendRollout(t, p, rolloutMsg("assistant", fmt.Sprintf("appended %d", n)))
		x.ReadTranscript(ctx, h, req)
	})
	resp, _ = x.ReadTranscript(ctx, h, req)
	var cut []int64
	for _, en := range resp.Entries {
		if en.Truncated {
			cut = append(cut, en.Seq)
		}
	}
	var full map[int64]string
	fullRead := median(func() { full, _ = x.TranscriptFullText(ctx, h, cut) })
	if len(cut) < 10 || len(full) != len(cut) || !strings.Contains(full[cut[0]], " line\nsynthetic answer") {
		t.Fatalf("full text of %d cut entries: %d texts", len(cut), len(full))
	}
	reparse := median(func() { search.ParseFile(p, search.FormatCodex) })
	t.Logf("rollout %.1f MB, %d entries: first read %s; cached read p50 %s; read after an append p50 %s; full text of %d cut entries p50 %s; whole-file parse (each read after a change, before) p50 %s",
		float64(fi.Size())/(1<<20), resp.Entries[len(resp.Entries)-1].Seq, cold, cached, appended, len(cut), fullRead, reparse)
	if appended > reparse/4 || fullRead > reparse/4 {
		t.Fatalf("a read after an append (%s) or a full read (%s) costs about a whole-file parse (%s)", appended, fullRead, reparse)
	}
}
