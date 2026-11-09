package search

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// sharedTranscript lays out one transcript the way account profiles share
// it: shared/projects/<uuid>.jsonl, and profiles a and b whose projects/
// are symlinks to it. It returns the file's path under each profile.
func sharedTranscript(t *testing.T) (a, b string) {
	t.Helper()
	root := t.TempDir()
	shared := filepath.Join(root, "shared", "projects")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "claude-synthetic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "5fe8f819.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, prof := range []string{"a", "b"} {
		dir := filepath.Join(root, "profiles", prof)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(shared, filepath.Join(dir, "projects")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		out = append(out, filepath.Join(dir, "projects", "5fe8f819.jsonl"))
	}
	return out[0], out[1]
}

func claudeSrc(run, path string) Source {
	return Source{RunID: run, SessionID: "ses_shared", Harness: contracts.HarnessClaude, Title: "shared", Path: path, Format: FormatClaude}
}

func (ix *Index) countEntries(t *testing.T) int {
	t.Helper()
	var n int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM search_fts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A session resumed on another account names its transcript under a new
// profile path; the file is indexed once, under the latest run when
// neither path was indexed before.
func TestSharedTranscriptIndexedOnce(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	a, b := sharedTranscript(t)
	st, err := ix.Update(ctx, []Source{claudeSrc("run_1", a), claudeSrc("run_2", b)})
	if err != nil || st.Indexed != 5 {
		t.Fatalf("update %+v %v", st, err)
	}
	res := search(t, ix, "stripe webhook")
	if len(res.Hits) != 2 || res.Hits[0].RunID != "run_2" {
		t.Fatalf("hits %+v", res.Hits)
	}
	// A later pass does not index the file again under either path.
	if st, err = ix.Update(ctx, []Source{claudeSrc("run_1", a), claudeSrc("run_2", b)}); err != nil || st.Indexed != 0 {
		t.Fatalf("second update %+v %v", st, err)
	}
	if n := ix.countEntries(t); n != 5 {
		t.Fatalf("entries %d, want 5", n)
	}
	if n, err := ix.PruneAliases(ctx); err != nil || n != 0 {
		t.Fatalf("prune with nothing to prune: %d %v", n, err)
	}
}

// An index built before aliases were recognized holds the file once per
// path: the path of the latest run keeps it, the other's entries are
// deleted and it is not indexed again; another session is untouched.
func TestPruneAliasesDropsDuplicatesFromOlderIndex(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	a, b := sharedTranscript(t)
	other := copyFixture(t, "claude-synthetic.jsonl")
	otherSrc := Source{RunID: "run_9", SessionID: "ses_other", Harness: contracts.HarnessClaude, Path: other, Format: FormatClaude}
	// The old behavior: each path indexed on its own.
	for _, s := range []Source{claudeSrc("run_1", a), claudeSrc("run_2", b), otherSrc} {
		if _, err := ix.Update(ctx, []Source{s}); err != nil {
			t.Fatal(err)
		}
	}
	if n := ix.countEntries(t); n != 15 {
		t.Fatalf("setup entries %d", n)
	}
	all := []Source{claudeSrc("run_1", a), claudeSrc("run_2", b), otherSrc}
	if st, err := ix.Update(ctx, all); err != nil || st.Indexed != 0 {
		t.Fatalf("update %+v %v", st, err)
	}
	n, err := ix.PruneAliases(ctx)
	if err != nil || n != 5 {
		t.Fatalf("pruned %d %v, want 5", n, err)
	}
	if n := ix.countEntries(t); n != 10 {
		t.Fatalf("entries %d, want 10 (one copy of the shared file, the other session's)", n)
	}
	hits := search(t, ix, "stripe webhook").Hits
	runs := map[string]int{}
	for _, h := range hits {
		runs[h.RunID]++
	}
	if len(hits) != 4 || runs["run_2"] != 2 || runs["run_9"] != 2 {
		t.Fatalf("hits by run %v", runs)
	}
	// Later passes neither re-index the alias nor find anything to prune.
	if st, err := ix.Update(ctx, all); err != nil || st.Indexed != 0 {
		t.Fatalf("update after prune %+v %v", st, err)
	}
	if n, err := ix.PruneAliases(ctx); err != nil || n != 0 {
		t.Fatalf("second prune %d %v", n, err)
	}
	// Appending to the shared file indexes the new line once.
	f, err := os.OpenFile(b, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"user","uuid":"u-extra","timestamp":"2026-10-05T12:00:00Z","message":{"role":"user","content":"one more pelicanfree question about kumquats"}}` + "\n")
	f.Close()
	if st, err := ix.Update(ctx, all); err != nil || st.Indexed != 1 {
		t.Fatalf("append %+v %v", st, err)
	}
	if hits := search(t, ix, "kumquats").Hits; len(hits) != 1 || hits[0].RunID != "run_2" {
		t.Fatalf("appended hits %+v", hits)
	}
}

// Runs of a session resumed on the same account name the same path. The
// path is indexed once and never becomes an alias of itself: later passes
// neither prune its entries nor index it again (server, 2026-10-28: such paths
// were re-indexed from the start every pass).
func TestSamePathUnderTwoRunsIsNotItsOwnAlias(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	a, b := sharedTranscript(t)
	all := []Source{claudeSrc("run_1", a), claudeSrc("run_3", a), claudeSrc("run_2", b)}
	if st, err := ix.Update(ctx, all); err != nil || st.Indexed != 5 {
		t.Fatalf("update %+v %v", st, err)
	}
	for pass := 0; pass < 3; pass++ {
		if n, err := ix.PruneAliases(ctx); err != nil || n != 0 {
			t.Fatalf("pass %d: pruned %d %v", pass, n, err)
		}
		if st, err := ix.Update(ctx, all); err != nil || st.Indexed != 0 {
			t.Fatalf("pass %d: update %+v %v", pass, st, err)
		}
	}
	if n := ix.countEntries(t); n != 5 {
		t.Fatalf("entries %d, want 5", n)
	}
	var self int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM search_files WHERE alias_of = path`).Scan(&self); err != nil || self != 0 {
		t.Fatalf("self aliases %d %v", self, err)
	}
	// The latest run of the path holds it.
	if hits := search(t, ix, "stripe webhook").Hits; len(hits) != 2 || hits[0].RunID != "run_3" {
		t.Fatalf("hits %+v", hits)
	}
}

// A self-alias left by an older version is never pruned.
func TestPruneAliasesSkipsSelfAlias(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	p := copyFixture(t, "claude-synthetic.jsonl")
	if _, err := ix.Update(ctx, []Source{claudeSrc("run_1", p)}); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`UPDATE search_files SET alias_of=path`); err != nil {
		t.Fatal(err)
	}
	if n, err := ix.PruneAliases(ctx); err != nil || n != 0 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if n := ix.countEntries(t); n != 5 {
		t.Fatalf("entries %d, want 5", n)
	}
}

type countPacer struct{ calls, read int64 }

func (p *countPacer) Wait(_ context.Context, read int64) error { p.calls++; p.read += read; return nil }

// UpdatePaced reports every chunk's bytes to the pacer, and nothing once
// the index is up to date.
func TestUpdatePacedReportsBytes(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	p := copyFixture(t, "claude-synthetic.jsonl")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	var pc countPacer
	if _, err := ix.UpdatePaced(ctx, []Source{claudeSrc("run_1", p)}, &pc); err != nil {
		t.Fatal(err)
	}
	if pc.calls != 1 || pc.read != fi.Size() {
		t.Fatalf("pacer %+v, want 1 call of %d bytes", pc, fi.Size())
	}
	pc = countPacer{}
	if _, err := ix.UpdatePaced(ctx, []Source{claudeSrc("run_1", p)}, &pc); err != nil || pc.calls != 0 {
		t.Fatalf("second pass pacer %+v %v", pc, err)
	}
}
