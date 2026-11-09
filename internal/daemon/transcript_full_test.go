package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
)

// fullStub serves full texts by seq (a transcriptFullReader).
type fullStub struct {
	contracts.RuntimeAdapter
	texts map[int64]string
	asked []int64
}

func (s *fullStub) Name() string { return "stub" }

func (s *fullStub) TranscriptFullText(_ context.Context, _ contracts.RunHandle, seqs []int64) (map[int64]string, error) {
	s.asked = append(s.asked, seqs...)
	out := map[int64]string{}
	for _, q := range seqs {
		if t, ok := s.texts[q]; ok {
			out[q] = t
		}
	}
	return out, nil
}

// noFullStub cannot return full text.
type noFullStub struct{ contracts.RuntimeAdapter }

func (noFullStub) Name() string { return "nofull" }

// cutEntry is what an adapter's default read returns for text.
func cutEntry(seq int64, kind, text string) contracts.TranscriptEntry {
	e := contracts.TranscriptEntry{Seq: seq, RunID: "run_synth", Kind: kind, Text: text}
	if len(text) > 2000 {
		e.Text, e.Truncated = text[:2000]+fmt.Sprintf("…[%d bytes truncated]", len(text)-2000), true
	}
	return e
}

func TestExpandFullLongFinalMessage(t *testing.T) {
	report := "synthetic final report\n" + strings.Repeat("proof line 0123456789\n", 400) + "END"
	st := &fullStub{texts: map[int64]string{842: report}}
	resp := contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{
		cutEntry(840, "tool", "Bash: go test ./..."), cutEntry(842, "assistant", report), cutEntry(843, "system", "turn ended (2s)"),
	}}
	expandFull(context.Background(), st, contracts.RunHandle{}, &resp)
	if !resp.Full || resp.FullNote != "" {
		t.Fatalf("full=%v note=%q", resp.Full, resp.FullNote)
	}
	if fmt.Sprint(st.asked) != "[842]" {
		t.Fatalf("asked for %v, want only the cut entry", st.asked)
	}
	if e := resp.Entries[1]; e.Text != report || e.Truncated {
		t.Fatalf("final message not whole: truncated=%v len=%d", e.Truncated, len(e.Text))
	}
	if resp.Entries[0].Text != "Bash: go test ./..." || resp.Entries[2].Text != "turn ended (2s)" {
		t.Fatalf("short entries changed: %+v", resp.Entries)
	}
}

func TestExpandFullRedacts(t *testing.T) {
	secret := "ANTHROPIC_API_KEY=sk-ant-synthetic0123456789abcdefghij and pyn_synthetic0123456789 and Bearer synthetic.token.0123456789"
	long := strings.Repeat("x", 3000) + " " + secret
	st := &fullStub{texts: map[int64]string{2: long}}
	resp := contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{
		cutEntry(1, "user", "short "+secret), cutEntry(2, "assistant", long),
	}}
	expandFull(context.Background(), st, contracts.RunHandle{}, &resp)
	for _, e := range resp.Entries {
		for _, leak := range []string{"sk-ant-synthetic", "pyn_synthetic", "synthetic.token"} {
			if strings.Contains(e.Text, leak) {
				t.Fatalf("seq %d leaks %q: %s", e.Seq, leak, e.Text[max(0, len(e.Text)-200):])
			}
		}
		if !strings.Contains(e.Text, "[REDACTED]") {
			t.Fatalf("seq %d not redacted: %s", e.Seq, e.Text[max(0, len(e.Text)-200):])
		}
	}
}

func TestExpandFullPerEntryCap(t *testing.T) {
	huge := strings.Repeat("é", contracts.FullEntryMax) // 2 bytes each: twice the cap
	st := &fullStub{texts: map[int64]string{5: huge}}
	resp := contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{cutEntry(5, "assistant", huge)}}
	expandFull(context.Background(), st, contracts.RunHandle{}, &resp)
	e := resp.Entries[0]
	if !e.Truncated || !strings.HasSuffix(e.Text, "bytes truncated: a full read keeps at most 256 KB per entry]") {
		t.Fatalf("cap not stated: truncated=%v tail=%q", e.Truncated, e.Text[len(e.Text)-80:])
	}
	body, _, _ := strings.Cut(e.Text, "…[")
	if len(body) > contracts.FullEntryMax || len(body) < contracts.FullEntryMax-1 || !utf8.ValidString(body) {
		t.Fatalf("kept %d bytes, want the cap (valid UTF-8)", len(body))
	}
	if !strings.Contains(resp.FullNote, "seq 5: longer than the 256 KB") {
		t.Fatalf("note: %q", resp.FullNote)
	}
}

func TestExpandFullPerReadCap(t *testing.T) {
	st := &fullStub{texts: map[int64]string{}}
	var es []contracts.TranscriptEntry
	for i := int64(1); i <= 5; i++ {
		txt := fmt.Sprintf("synthetic %d ", i) + strings.Repeat("y", 250<<10)
		st.texts[i] = txt
		es = append(es, cutEntry(i, "assistant", txt))
	}
	resp := contracts.ReadSessionResponse{Entries: es}
	expandFull(context.Background(), st, contracts.RunHandle{}, &resp)
	total := 0
	for _, e := range resp.Entries {
		b, _ := json.Marshal(e.Text)
		total += len(b)
	}
	if total > contracts.FullReadMax+4096 {
		t.Fatalf("response carries %d bytes of text", total)
	}
	// Newest first: 2-5 whole, 1 keeps the default cut and is named.
	for _, e := range resp.Entries {
		whole := e.Text == st.texts[e.Seq]
		if whole != (e.Seq > 1) || e.Truncated == whole {
			t.Fatalf("seq %d: whole=%v truncated=%v", e.Seq, whole, e.Truncated)
		}
	}
	if !strings.Contains(resp.FullNote, "seq 1: past the 1024 KB of full text one read returns") {
		t.Fatalf("note: %q", resp.FullNote)
	}
}

// JSON escaping counts against the per-read bound ('<' encodes as <).
func TestExpandFullCountsEncodedBytes(t *testing.T) {
	txt := strings.Repeat("<", 200<<10) // 1.2 MB encoded
	st := &fullStub{texts: map[int64]string{1: txt}}
	resp := contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{cutEntry(1, "assistant", txt)}}
	expandFull(context.Background(), st, contracts.RunHandle{}, &resp)
	if resp.Entries[0].Text == txt || !resp.Entries[0].Truncated || !strings.Contains(resp.FullNote, "seq 1: past") {
		t.Fatalf("encoded size not bounded: truncated=%v note=%q", resp.Entries[0].Truncated, resp.FullNote)
	}
}

func TestExpandFullUnsupportedAdapter(t *testing.T) {
	long := strings.Repeat("z", 5000)
	resp := contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{cutEntry(1, "assistant", long)}}
	want := resp.Entries[0].Text
	expandFull(context.Background(), noFullStub{}, contracts.RunHandle{}, &resp)
	if !resp.Full || resp.Entries[0].Text != want || !resp.Entries[0].Truncated ||
		resp.FullNote != "the nofull adapter does not return full entry text yet; entries keep the default cut" {
		t.Fatalf("unsupported: %+v", resp)
	}
}
