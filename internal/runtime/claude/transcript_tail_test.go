package claude

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
)

// refSelect is sessions.read's selection over a full parse from the start
// (the behaviour before the tail cache).
func refSelect(all []contracts.TranscriptEntry, start int64, limit int, tail bool) []contracts.TranscriptEntry {
	var sel []contracts.TranscriptEntry
	for _, e := range all {
		if e.Seq > start {
			sel = append(sel, e)
		}
	}
	if tail && len(sel) > limit {
		sel = sel[len(sel)-limit:]
	} else if len(sel) > limit {
		sel = sel[:limit]
	}
	return sel
}

type readCase struct {
	start int64
	limit int
	tail  bool
}

func readCases(maxSeq int64) []readCase {
	var out []readCase
	for _, start := range []int64{0, 1, 7, maxSeq / 3, maxSeq / 2, maxSeq - 600, maxSeq - 40, maxSeq - 1, maxSeq, maxSeq + 5} {
		if start < 0 {
			continue
		}
		for _, limit := range []int{1, 30, 200, 500} {
			out = append(out, readCase{start, limit, true}, readCase{start, limit, false})
		}
	}
	return out
}

func assertSameReads(t *testing.T, c *tailCache, path string) {
	t.Helper()
	all := fullParseEntries(path, "run_t")
	var maxSeq int64
	if len(all) > 0 {
		maxSeq = all[len(all)-1].Seq
	}
	for _, rc := range readCases(maxSeq) {
		got, err := c.read(path, "run_t", rc.start, rc.limit, rc.tail)
		if err != nil {
			t.Fatalf("%+v: %v", rc, err)
		}
		want := refSelect(all, rc.start, rc.limit, rc.tail)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s %+v: got %d entries (first %v), want %d (first %v)", filepath.Base(path), rc, len(got), firstSeq(got), len(want), firstSeq(want))
		}
	}
}

func firstSeq(es []contracts.TranscriptEntry) any {
	if len(es) == 0 {
		return nil
	}
	return [2]int64{es[0].Seq, es[len(es)-1].Seq}
}

// oddLines are records sessions.read numbers or skips in unusual ways.
var oddLines = []string{
	``,                                   // blank: not a record
	`not json at all`,                    // not a record
	`{"type":"system","durationMs":"x"}`, // type mismatch: not a record
	`[1,2,3]`,                            // not an object: not a record
	`null`,                               // decodes (no fields): a record, no entry
	`{"type":"user","message":{"role":"user","content":"synthetic crlf prompt"}}` + "\r",
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("long ", 80000) + `"}]}}`, // > tailChunk
	`{"type":"system","subtype":"turn_duration","durationMs":42}`,
	`{"TYPE":"user","message":{"role":"user","content":"synthetic upper-case key"}}`,
}

func writeOddTranscript(t *testing.T, path string, turns int) {
	t.Helper()
	writeSyntheticTranscript(t, path, 0)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < turns; i++ {
		for j, l := range oddLines {
			if j == 6 && i%40 != 0 { // a few huge lines only
				continue
			}
			fmt.Fprintln(f, l)
		}
		fmt.Fprintf(f, `{"type":"user","timestamp":"2026-10-01T00:00:%02dZ","message":{"role":"user","content":"synthetic prompt %d [pyre-msg:msg_synth%d]"}}`+"\n", i%60, i, i)
		fmt.Fprintf(f, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"/synthetic/%d"}}]}}`+"\n", i)
	}
}

func TestTailReadEqualsFullParse(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]func(string){
		"empty": func(p string) { os.WriteFile(p, nil, 0o600) },
		"partial": func(p string) {
			os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":"half`), 0o600)
		},
		"small": func(p string) { writeSyntheticTranscript(t, p, 20<<10) },
		"large": func(p string) { writeSyntheticTranscript(t, p, 12<<20) }, // parallel prefix count
		"odd":   func(p string) { writeOddTranscript(t, p, 300) },
		"odd-partial-tail": func(p string) {
			writeOddTranscript(t, p, 120)
			f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
			f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"still being writ`)
			f.Close()
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name+".jsonl")
			mk(p)
			var c tailCache
			assertSameReads(t, &c, p)
			if _, _, colds := c.stats(); colds != 1 {
				t.Fatalf("file read from the end %d times, want once", colds)
			}
		})
	}
}

func TestTailReadMissingFile(t *testing.T) {
	var c tailCache
	got, err := c.read(filepath.Join(t.TempDir(), "absent.jsonl"), "run_t", 0, 30, true)
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file: %v %v", got, err)
	}
	if n, _, _ := c.stats(); n != 0 {
		t.Fatalf("missing file cached (%d files)", n)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestTailReadPicksUpAppends(t *testing.T) {
	p := filepath.Join(t.TempDir(), "grow.jsonl")
	writeSyntheticTranscript(t, p, 3<<20)
	var c tailCache
	assertSameReads(t, &c, p)
	// A record written in two pieces: the half line is not read until its
	// newline arrives.
	appendFile(t, p, `{"type":"user","message":{"role":"user","content":"synthetic appended`)
	assertSameReads(t, &c, p)
	appendFile(t, p, ` prompt"}}`+"\n")
	assertSameReads(t, &c, p)
	got, _ := c.read(p, "run_t", 0, 1, true)
	if len(got) != 1 || got[0].Text != "synthetic appended prompt" {
		t.Fatalf("appended record not read: %+v", got)
	}
	// Enough appended entries to push the window and add marks.
	var b strings.Builder
	for i := 0; i < 2*tailWindow; i++ {
		fmt.Fprintf(&b, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"synthetic more %d %s"}]}}`+"\n", i, strings.Repeat("y", 2000))
	}
	appendFile(t, p, b.String())
	assertSameReads(t, &c, p)
	if _, _, colds := c.stats(); colds != 1 {
		t.Fatalf("appends re-read the file from the end (%d cold reads)", colds)
	}
}

func TestTailReadTruncatedOrReplaced(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	writeSyntheticTranscript(t, p, 2<<20)
	var c tailCache
	assertSameReads(t, &c, p)
	// Truncated and rewritten shorter.
	writeOddTranscript(t, p, 30)
	assertSameReads(t, &c, p)
	// Rewritten in place to a longer file whose old end bytes differ.
	writeSyntheticTranscript(t, p, 3<<20)
	appendFile(t, p, `{"type":"user","message":{"role":"user","content":"synthetic after rewrite"}}`+"\n")
	assertSameReads(t, &c, p)
	// Replaced by rename (another inode).
	q := filepath.Join(dir, "new.jsonl")
	writeOddTranscript(t, q, 50)
	if err := os.Rename(q, p); err != nil {
		t.Fatal(err)
	}
	assertSameReads(t, &c, p)
	if _, _, colds := c.stats(); colds != 4 {
		t.Fatalf("cold reads %d, want 4 (initial, truncate, rewrite, rename)", colds)
	}
	// Removed: no entries, dropped from the cache.
	os.Remove(p)
	if got, err := c.read(p, "run_t", 0, 30, true); err != nil || len(got) != 0 {
		t.Fatalf("removed file: %v %v", got, err)
	}
	if n, _, _ := c.stats(); n != 0 {
		t.Fatalf("removed file still cached")
	}
}

func TestTailCacheBounded(t *testing.T) {
	dir := t.TempDir()
	var c tailCache
	for i := 0; i < tailCacheFiles+10; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.jsonl", i))
		writeSyntheticTranscript(t, p, 4<<10)
		if _, err := c.read(p, "run_t", 0, 30, true); err != nil {
			t.Fatal(err)
		}
	}
	if n, _, _ := c.stats(); n != tailCacheFiles {
		t.Fatalf("cache holds %d files, want %d", n, tailCacheFiles)
	}
	// Byte bound: windows of large entries evict older files.
	big := strings.Repeat("z", 1900)
	for i := 0; i < 40; i++ {
		p := filepath.Join(dir, fmt.Sprintf("big%d.jsonl", i))
		var b strings.Builder
		for j := 0; j < tailWindow; j++ {
			fmt.Fprintf(&b, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"%s %d"}]}}`+"\n", big, j)
		}
		os.WriteFile(p, []byte(b.String()), 0o600)
		if _, err := c.read(p, "run_t", 0, 30, true); err != nil {
			t.Fatal(err)
		}
	}
	n, bytes, _ := c.stats()
	if bytes > tailCacheBytes || n >= 40 {
		t.Fatalf("cache over its byte bound: %d files, %d bytes (bound %d)", n, bytes, tailCacheBytes)
	}
}

// TestCountRecordMatchesTranscriptRecord: countRecord must have every
// typed field of transcriptRecord (same name, tag and type) and nothing
// else, so a line counts exactly when sessions.read decodes it.
func TestCountRecordMatchesTranscriptRecord(t *testing.T) {
	raw := reflect.TypeOf(json.RawMessage{})
	want := map[string]reflect.StructField{}
	tr := reflect.TypeOf(transcriptRecord{})
	for i := 0; i < tr.NumField(); i++ {
		if f := tr.Field(i); f.Type != raw {
			want[f.Name] = f
		}
	}
	cr := reflect.TypeOf(countRecord{})
	if cr.NumField() != len(want) {
		t.Fatalf("countRecord has %d fields, transcriptRecord %d typed fields", cr.NumField(), len(want))
	}
	for i := 0; i < cr.NumField(); i++ {
		f := cr.Field(i)
		w, ok := want[f.Name]
		if !ok || w.Type != f.Type || w.Tag != f.Tag {
			t.Fatalf("countRecord.%s differs from transcriptRecord", f.Name)
		}
	}
	for _, l := range oddLines {
		var r transcriptRecord
		if (json.Unmarshal([]byte(l), &r) == nil) != countsAsRecord([]byte(l)) {
			t.Fatalf("decode outcome differs for %.60q", l)
		}
	}
}

// TestReadTranscriptLatency50MB measures sessions.read on the node (the
// adapter's ReadTranscript: transcript tail + screen capture, real tmux,
// fake Claude) with a 50 MB transcript, against the old full parse. Set
// PYRE_MEASURE_READ=1 to run it.
func TestReadTranscriptLatency50MB(t *testing.T) {
	if os.Getenv("PYRE_MEASURE_READ") == "" {
		t.Skip("set PYRE_MEASURE_READ=1 to measure")
	}
	f := newFakeEnv(t, nil)
	h := f.launch("run_lat", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	path, _ := f.a.entry(h).tpath.Load().(string)
	if path == "" {
		t.Fatal("transcript path not known")
	}
	big := filepath.Join(t.TempDir(), "big.jsonl")
	writeSyntheticTranscript(t, big, 50<<20)
	b, err := os.ReadFile(big)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := contracts.ReadSessionRequest{Tail: true, Limit: 30}
	// The observer catches up with the appended bytes (8 MB per refresh),
	// as pyred's poller does between reads.
	for i := 0; i < 10; i++ {
		f.a.Observe(ctx, h, "")
	}
	measure := func(name string, n int, fn func()) {
		ds := make([]time.Duration, n)
		for i := range ds {
			t0 := time.Now()
			fn()
			ds[i] = time.Since(t0)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("%s: n=%d p50=%s p90=%s max=%s", name, n, ds[n/2], ds[n*9/10], ds[n-1])
	}
	t0 := time.Now()
	resp, err := f.a.ReadTranscript(ctx, h, req)
	if err != nil || len(resp.Entries) != 30 || resp.Screen == "" {
		t.Fatalf("read: %v %d entries", err, len(resp.Entries))
	}
	t.Logf("first read (file read from the end): %s, response %d bytes", time.Since(t0), len(mustJSON(t, resp)))
	measure("ReadTranscript tail 30 (cached)", 50, func() { f.a.ReadTranscript(ctx, h, req) })
	measure("ReadTranscript tail 30 after an append", 50, func() {
		appendFile(t, path, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"synthetic"}]}}`+"\n")
		f.a.ReadTranscript(ctx, h, req)
	})
	measure("before: full parse of the file (old reader, no screen)", 5, func() { fullParseEntries(path, h.RunID) })
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTailReadConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.jsonl")
	writeSyntheticTranscript(t, p, 1<<20)
	var c tailCache
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			appendFile(t, p, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"synthetic %d"}}`+"\n", i))
		}
	}()
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		go func() {
			for i := 0; i < 50; i++ {
				es, err := c.read(p, "run_t", 0, 30, true)
				if err == nil && len(es) != 30 {
					err = fmt.Errorf("%d entries", len(es))
				}
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for g := 0; g < 8; g++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	<-done
	assertSameReads(t, &c, p)
}
