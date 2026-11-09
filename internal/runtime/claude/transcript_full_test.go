package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// fullParseTexts is every shown entry's uncut text by seq, parsed from the
// start of the file.
func fullParseTexts(path string) map[int64]string {
	out := map[int64]string{}
	var seq int64
	_, _ = readJSONLFrom(path, 0, 0, func(line []byte, _ int64) {
		var r transcriptRecord
		if json.Unmarshal(line, &r) != nil {
			return
		}
		seq++
		if e, ok := fullTranscriptEntry("", r, seq); ok {
			out[seq] = e.Text
		}
	})
	return out
}

func TestFullTextMatchesFullParse(t *testing.T) {
	p := filepath.Join(t.TempDir(), "odd.jsonl")
	writeOddTranscript(t, p, 300) // 400 KB assistant lines among the rest, several marks
	want := fullParseTexts(p)
	all := fullParseEntries(p, "run_t")
	var c tailCache
	var cut []int64
	for _, e := range all {
		if e.Truncated {
			cut = append(cut, e.Seq)
		}
	}
	if len(cut) < 3 {
		t.Fatalf("synthetic transcript has %d cut entries, want several", len(cut))
	}
	// Default reads are unchanged by a full read before or after them.
	assertSameReads(t, &c, p)
	for _, seqs := range [][]int64{cut, cut[:1], cut[len(cut)-1:], {cut[0], cut[len(cut)-1]}, {all[0].Seq, cut[1]}} {
		got, err := c.fullText(p, seqs)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(seqs) {
			t.Fatalf("seqs %v: got %d texts", seqs, len(got))
		}
		for _, s := range seqs {
			if got[s] != want[s] {
				t.Fatalf("seq %d: full text differs (got %d bytes, want %d)", s, len(got[s]), len(want[s]))
			}
		}
	}
	assertSameReads(t, &c, p)
	// A seq with no shown entry, or past the end, is left out.
	got, err := c.fullText(p, []int64{1 << 40})
	if err != nil || len(got) != 0 {
		t.Fatalf("absent seq: %v %v", got, err)
	}
}

// A child's long final report: a default read cuts it, the full text read
// through the adapter returns it whole.
func TestFullTextLongFinalMessage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "final.jsonl")
	writeSyntheticTranscript(t, p, 2<<20)
	report := "synthetic final report\n" + strings.Repeat("proof line 0123456789 ünïcode\n", 300) + "END OF REPORT"
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant",
		"content": []any{map[string]any{"type": "text", "text": report}}}})
	appendFile(t, p, string(b)+"\n"+`{"type":"system","subtype":"turn_duration","durationMs":42}`+"\n")

	var c tailCache
	tail, err := c.read(p, "run_t", 0, 3, true)
	if err != nil || len(tail) != 3 {
		t.Fatalf("tail: %v %v", tail, err)
	}
	last := tail[1]
	if !last.Truncated || last.Kind != "assistant" || strings.Contains(last.Text, "END OF REPORT") || !strings.Contains(last.Text, "bytes truncated]") {
		t.Fatalf("default read changed: %+v", last)
	}
	got, err := c.fullText(p, []int64{last.Seq})
	if err != nil || got[last.Seq] != report {
		t.Fatalf("full text: %d bytes, err %v", len(got[last.Seq]), err)
	}
}

// The adapter's TranscriptFullText reads the run's own transcript.
func TestAdapterTranscriptFullText(t *testing.T) {
	f := newFakeEnv(t, nil)
	h := f.launch("run_full", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	path, _ := f.a.entry(h).tpath.Load().(string)
	if path == "" {
		t.Fatal("transcript path not known")
	}
	report := "synthetic child report " + strings.Repeat("evidence ", 1000) + "DONE"
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant",
		"content": []any{map[string]any{"type": "text", "text": report}}}})
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resp, err := f.a.ReadTranscript(ctx, h, contracts.ReadSessionRequest{Tail: true, Limit: 1})
	if err != nil || len(resp.Entries) != 1 || !resp.Entries[0].Truncated {
		t.Fatalf("default read: %v %+v", err, resp.Entries)
	}
	seq := resp.Entries[0].Seq
	got, err := f.a.TranscriptFullText(ctx, h, []int64{seq})
	if err != nil || got[seq] != report {
		t.Fatalf("full text of seq %d: %d bytes, err %v", seq, len(got[seq]), err)
	}
	x := NewExec(f.a)
	if got, _ := x.TranscriptFullText(ctx, h, []int64{seq}); got[seq] != report {
		t.Fatal("exec adapter does not delegate full text")
	}
}
