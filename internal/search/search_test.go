package search

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func openTestIndex(t *testing.T) *Index {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ix, err := Open(context.Background(), db, "node_synthetic", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	ix.SetClock(func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) })
	return ix
}

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func search(t *testing.T, ix *Index, q string) contracts.TranscriptSearchResult {
	t.Helper()
	res, err := ix.Search(context.Background(), Query{Text: q})
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	return res
}

func TestClaudeFixtureIndexesTurnsWithAdapterSeq(t *testing.T) {
	ix := openTestIndex(t)
	p := copyFixture(t, "claude-synthetic.jsonl")
	src := Source{RunID: "run_c1", SessionID: "ses_c1", Harness: contracts.HarnessClaude, Title: "synthetic shop", Path: p, Format: FormatClaude}
	st, err := ix.Update(context.Background(), []Source{src})
	if err != nil || st.Indexed != 5 || st.Partial {
		t.Fatalf("update %+v %v", st, err)
	}
	res := search(t, ix, "stripe webhook")
	if len(res.Hits) != 2 {
		t.Fatalf("hits %+v", res.Hits)
	}
	got := map[int64]string{}
	for _, h := range res.Hits {
		got[h.Seq] = h.Kind
		if h.SessionID != "ses_c1" || h.RunID != "run_c1" || h.NodeID != "node_synthetic" || h.Title != "synthetic shop" || h.Harness != contracts.HarnessClaude {
			t.Fatalf("hit fields %+v", h)
		}
		if !strings.Contains(h.Snippet, contracts.SearchMarkOpen) || !strings.Contains(strings.ToLower(h.Snippet), "«stripe»") {
			t.Fatalf("snippet not highlighted: %q", h.Snippet)
		}
	}
	// Seq follows sessions.read numbering: every decodable record counts,
	// the non-JSON line does not.
	if got[2] != "user" || got[3] != "assistant" {
		t.Fatalf("seqs %v", got)
	}
	if r := search(t, ix, "grep"); len(r.Hits) != 1 || r.Hits[0].Kind != "tool" || r.Hits[0].Seq != 4 {
		t.Fatalf("tool hit %+v", r.Hits)
	}
	if r := search(t, ix, "invoice"); len(r.Hits) != 2 || r.Hits[0].Seq < 9 {
		t.Fatalf("message hits %+v", r.Hits)
	}
	for _, q := range []string{"zebra", "giraffes", "pelicans"} { // tool output, meta, sidechain
		if r := search(t, ix, q); len(r.Hits) != 0 {
			t.Fatalf("%s should not be indexed: %+v", q, r.Hits)
		}
	}
	if r := search(t, ix, "webh"); len(r.Hits) < 2 {
		t.Fatalf("prefix: %+v", r.Hits)
	}
	if r := search(t, ix, "webhooks"); len(r.Hits) < 2 { // porter stemming
		t.Fatalf("stem: %+v", r.Hits)
	}
}

func TestIncrementalUpdatePartialLineAndReset(t *testing.T) {
	ix := openTestIndex(t)
	p := copyFixture(t, "claude-synthetic.jsonl")
	src := Source{RunID: "run_c1", SessionID: "ses_c1", Harness: contracts.HarnessClaude, Path: p, Format: FormatClaude}
	if _, err := ix.Update(context.Background(), []Source{src}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	line := `{"type":"assistant","timestamp":"2026-10-04T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"the octopus ledger is reconciled"}]}}`
	f.WriteString(line[:40]) // partial: no newline yet
	f.Close()
	if st, _ := ix.Update(context.Background(), []Source{src}); st.Indexed != 0 {
		t.Fatalf("partial line indexed: %+v", st)
	}
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(line[40:] + "\n")
	f.Close()
	if st, _ := ix.Update(context.Background(), []Source{src}); st.Indexed != 1 {
		t.Fatalf("appended line: %+v", st)
	}
	r := search(t, ix, "octopus")
	if len(r.Hits) != 1 || r.Hits[0].Seq != 11 {
		t.Fatalf("appended hit %+v", r.Hits)
	}
	// Unchanged file: nothing re-read.
	if st, _ := ix.Update(context.Background(), []Source{src}); st.Indexed != 0 {
		t.Fatalf("re-indexed: %+v", st)
	}
	// Replaced by a shorter file: re-indexed from the start, old rows gone.
	os.WriteFile(p, []byte(`{"type":"user","timestamp":"2026-10-05T10:00:00.000Z","message":{"role":"user","content":"fresh start about lighthouses"}}`+"\n"), 0o600)
	if st, _ := ix.Update(context.Background(), []Source{src}); st.Indexed != 1 {
		t.Fatalf("reset: %+v", st)
	}
	if r := search(t, ix, "octopus"); len(r.Hits) != 0 {
		t.Fatalf("stale rows after reset: %+v", r.Hits)
	}
	if r := search(t, ix, "lighthouses"); len(r.Hits) != 1 || r.Hits[0].Seq != 1 {
		t.Fatalf("after reset %+v", r.Hits)
	}
}

func TestCodexRolloutFixture(t *testing.T) {
	ix := openTestIndex(t)
	p := copyFixture(t, "codex-rollout-synthetic.jsonl")
	src := Source{RunID: "run_x1", SessionID: "ses_x1", Harness: contracts.HarnessCodex, Title: "billing", Path: p, Format: FormatCodex}
	st, err := ix.Update(context.Background(), []Source{src})
	if err != nil || st.Indexed != 3 {
		t.Fatalf("update %+v %v", st, err)
	}
	r := search(t, ix, "billing cron")
	if len(r.Hits) != 3 {
		t.Fatalf("hits %+v", r.Hits)
	}
	kinds := map[string]bool{}
	for _, h := range r.Hits {
		kinds[h.Kind] = true
		if h.Harness != contracts.HarnessCodex {
			t.Fatalf("harness %+v", h)
		}
	}
	if !kinds["user"] || !kinds["assistant"] || !kinds["tool"] {
		t.Fatalf("kinds %v", kinds)
	}
	for _, q := range []string{"kangaroo", "walruses", "narwhals"} { // preambles, reasoning, tool output
		if r := search(t, ix, q); len(r.Hits) != 0 {
			t.Fatalf("%s indexed: %+v", q, r.Hits)
		}
	}
}

func TestSearchFiltersLimitsAndHostileQueries(t *testing.T) {
	ix := openTestIndex(t)
	a := copyFixture(t, "claude-synthetic.jsonl")
	b := copyFixture(t, "codex-rollout-synthetic.jsonl")
	ix.Update(context.Background(), []Source{
		{RunID: "run_c1", SessionID: "ses_c1", Harness: contracts.HarnessClaude, Path: a, Format: FormatClaude},
		{RunID: "run_x1", SessionID: "ses_x1", Harness: contracts.HarnessCodex, Path: b, Format: FormatCodex},
	})
	since := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	res, err := ix.Search(context.Background(), Query{Text: "synthetic", Since: &since})
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("since %+v %v", res, err)
	}
	for _, h := range res.Hits {
		if h.At.Before(since) {
			t.Fatalf("since filter: %+v", h)
		}
	}
	res, _ = ix.Search(context.Background(), Query{Text: "the", SessionIDs: []string{"ses_c1"}, Limit: 1})
	if len(res.Hits) != 1 || !res.Truncated || res.Hits[0].SessionID != "ses_c1" {
		t.Fatalf("limit/session %+v", res)
	}
	if res.Runs != 2 {
		t.Fatalf("runs %d", res.Runs)
	}
	for _, q := range []string{`"`, `NEAR(a b)`, `a OR`, `*`, `col:x`, `"unterminated phrase`, `^start`, `a AND NOT`, `)(`} {
		if _, err := ix.Search(context.Background(), Query{Text: q}); err != nil && !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Fatalf("query %q: %v", q, err)
		}
	}
	if _, err := ix.Search(context.Background(), Query{Text: "  ?? "}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("empty query: %v", err)
	}
}

func TestChunkedUpdateIsBounded(t *testing.T) {
	db, _ := sqlitex.Open(filepath.Join(t.TempDir(), "n.db"))
	defer db.Close()
	ix, err := Open(context.Background(), db, "n", Limits{ChunkBytes: 300})
	if err != nil {
		t.Fatal(err)
	}
	p := copyFixture(t, "claude-synthetic.jsonl")
	src := Source{RunID: "r", SessionID: "s", Harness: contracts.HarnessClaude, Path: p, Format: FormatClaude}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // budget already spent: nothing read, reported partial
	st, _ := ix.Update(ctx, []Source{src})
	if !st.Partial || st.Indexed != 0 {
		t.Fatalf("expired budget %+v", st)
	}
	st, _ = ix.Update(context.Background(), []Source{src})
	if st.Indexed != 5 || st.Partial {
		t.Fatalf("chunked passes %+v", st)
	}
}

func TestTermsAndFTSQuery(t *testing.T) {
	if got := Terms(`stripe "web hook"  x`); len(got) != 3 || got[1] != "web hook" {
		t.Fatalf("terms %q", got)
	}
	if got := FTSQuery(`stripe webh`); got != `"stripe" "webh"*` {
		t.Fatalf("fts %q", got)
	}
	if got := FTSQuery(`say "hi"`); got != `"say" "hi"` {
		t.Fatalf("fts phrase %q", got)
	}
	if got := FTSQuery(`a"b`); got != `"a""b"*` {
		t.Fatalf("fts quote %q", got)
	}
	s := Snippet("The quick Stripe webhook verifies signatures; stripe again", []string{"stripe", "webhook"}, 0)
	if s != "The quick «Stripe» «webhook» verifies signatures; «stripe» again" {
		t.Fatalf("snippet %q", s)
	}
	if mt := MarkedTerms(s); len(mt) != 2 || mt[0] != "webhook" {
		t.Fatalf("marked %q", mt)
	}
	long := strings.Repeat("filler ", 40) + "needle " + strings.Repeat("tail ", 40)
	if s := Snippet(long, []string{"needle"}, 60); !strings.HasPrefix(s, "…") || !strings.Contains(s, "«needle»") || !strings.HasSuffix(s, "…") {
		t.Fatalf("windowed %q", s)
	}
}

func TestRankRelevanceRecencyAndSessionCap(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var hits []contracts.SearchHit
	for i := 0; i < 5; i++ { // one chatty session
		hits = append(hits, contracts.SearchHit{Source: contracts.SearchSourceTranscript, SessionID: "ses_a", Seq: int64(i), Score: 10, At: now})
	}
	hits = append(hits,
		contracts.SearchHit{Source: contracts.SearchSourceTranscript, SessionID: "ses_old", Score: 10, At: now.AddDate(0, -6, 0)},
		contracts.SearchHit{Source: contracts.SearchSourceTranscript, SessionID: "ses_b", Score: 9, At: now.Add(-time.Hour)},
		contracts.SearchHit{Source: contracts.SearchSourceMemory, Ref: "m1", At: now.AddDate(0, 0, -1)},
	)
	ranked, cut := Rank(hits, now, 10)
	if !cut {
		t.Fatal("per-session cap should cut")
	}
	n := 0
	for _, h := range ranked {
		if h.SessionID == "ses_a" {
			n++
		}
	}
	if n != PerSessionCap {
		t.Fatalf("cap %d", n)
	}
	if ranked[len(ranked)-1].SessionID != "ses_old" {
		t.Fatalf("old hit should rank last: %+v", ranked)
	}
	if r, cut := Rank(hits, now, 2); len(r) != 2 || !cut {
		t.Fatalf("limit %d %v", len(r), cut)
	}
}

// agy transcripts (transcript_full.jsonl): user requests without agy's
// metadata, Pyre messages, replies and tool calls; every step advances Seq
// as the agy adapter's sessions.read numbers them.
func TestParseAgyTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript_full.jsonl")
	lines := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-18T16:45:26Z","content":"<USER_REQUEST>\nfind the synthetic flake\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: now.\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-18T16:45:27Z","tool_calls":[{"name":"run_command","args":{"CommandLine":"go test ./synthetic/..."}}]}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-10-18T16:45:28Z","content":"tool output"}`,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-18T16:45:29Z","content":"the flake is a race"}`,
		`{"step_index":4,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-18T16:46:00Z","content":"<USER_REQUEST>\n[pyre-msg:msg_synth] status?\n</USER_REQUEST>"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	es, err := ParseFile(path, FormatAgy)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 4 || es[0].Kind != "user" || es[0].Text != "find the synthetic flake" || es[1].Kind != "tool" || es[1].Text != "run_command: go test ./synthetic/..." ||
		es[2].Kind != "assistant" || es[2].Seq != 4 || es[3].Kind != "message" || es[3].Seq != 5 {
		t.Fatalf("entries %+v", es)
	}
}
