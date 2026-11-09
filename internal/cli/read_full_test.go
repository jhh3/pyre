package cli

import (
	"strconv"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

func TestReadSeqAndFull(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "read", "ses_synth-b", "--tail", "3")
	expectCode(t, r, 0)
	def := mustJSON[c.ReadSessionResponse](t, r)
	if len(def.Entries) == 0 || def.Full || def.FullNote != "" {
		t.Fatalf("default read: %+v", def)
	}
	want := def.Entries[0]

	// One entry by seq, full: the mock's entries are never cut.
	r = run(t, e, nil, "--json", "read", "ses_synth-b", "--seq", strconv.FormatInt(want.Seq, 10), "--full")
	expectCode(t, r, 0)
	one := mustJSON[c.ReadSessionResponse](t, r)
	if len(one.Entries) != 1 || one.Entries[0].Seq != want.Seq || one.Entries[0].Text != want.Text || !one.Full {
		t.Fatalf("seq read: %+v", one)
	}
	r = run(t, e, nil, "read", "ses_synth-b", "--seq", strconv.FormatInt(want.Seq, 10))
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, want.Kind) || strings.Count(strings.TrimSpace(r.stdout), "\n") != strings.Count(want.Text, "\n") {
		t.Fatalf("seq read prints one entry:\n%s", r.stdout)
	}

	// --tail with --full keeps the same entries.
	r = run(t, e, nil, "--json", "read", "ses_synth-b", "--tail", "3", "--full")
	expectCode(t, r, 0)
	if full := mustJSON[c.ReadSessionResponse](t, r); len(full.Entries) != len(def.Entries) || !full.Full {
		t.Fatalf("tail full: %+v", full)
	}

	// A seq past the end is not found; --seq does not combine with paging.
	expectCode(t, run(t, e, nil, "--json", "read", "ses_synth-b", "--seq", "999999"), ExitNotFound)
	for _, extra := range [][]string{{"--tail", "3"}, {"--cursor", "1"}, {"--follow"}} {
		expectCode(t, run(t, e, nil, append([]string{"read", "ses_synth-b", "--seq", "1"}, extra...)...), ExitUsage)
	}
}

func TestPrintTranscriptFullNote(t *testing.T) {
	var b strings.Builder
	printTranscript(&b, c.ReadSessionResponse{Entries: []c.TranscriptEntry{{Seq: 1, Kind: "assistant", Text: "x"}},
		Full: true, FullNote: "seq 1: longer than the 256 KB a full read keeps per entry; cut there"}, false)
	if !strings.Contains(b.String(), "[full: seq 1: longer than the 256 KB") {
		t.Fatalf("note not printed:\n%s", b.String())
	}
}
