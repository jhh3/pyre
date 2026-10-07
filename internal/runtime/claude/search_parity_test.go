package claude

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/search"
)

// The search index must number Claude records exactly as sessions.read
// does, so a hit's Seq opens the reader at the matching entry.
func TestSearchIndexSeqMatchesReadTranscript(t *testing.T) {
	path := filepath.Join("..", "..", "search", "testdata", "claude-synthetic.jsonl")
	var recs []transcriptRecord
	if _, err := readJSONLFrom(path, 0, 0, func(line []byte, _ int64) {
		var r transcriptRecord
		if json.Unmarshal(line, &r) == nil {
			recs = append(recs, r)
		}
	}); err != nil {
		t.Fatal(err)
	}
	read := map[int64]string{}
	for _, e := range entriesFromTranscript("run_x", recs, 0) {
		read[e.Seq] = e.Text
	}
	idx, err := search.ParseFile(path, search.FormatClaude)
	if err != nil || len(idx) == 0 {
		t.Fatalf("parse %v %d", err, len(idx))
	}
	for _, e := range idx {
		got, ok := read[e.Seq]
		if !ok {
			t.Fatalf("indexed seq %d (%q) has no sessions.read entry", e.Seq, e.Text)
		}
		if got != e.Text && e.Kind != "tool" { // tool summaries may differ in wording
			t.Fatalf("seq %d: read %q, indexed %q", e.Seq, got, e.Text)
		}
	}
}
