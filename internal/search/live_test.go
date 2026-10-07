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

// TestLiveIndexRealTranscripts indexes real transcript stores read-only
// into a temporary database and runs one query, printing only counts and
// timings (never transcript text). Opt-in:
//
//	PYRE_SEARCH_LIVE_DIRS=~/.local/state/pyre/shared/projects:~/.local/state/pyre/shared/sessions \
//	PYRE_SEARCH_LIVE_QUERY="slack thread" go test ./internal/search -run TestLiveIndexRealTranscripts -v
func TestLiveIndexRealTranscripts(t *testing.T) {
	dirs := os.Getenv("PYRE_SEARCH_LIVE_DIRS")
	query := os.Getenv("PYRE_SEARCH_LIVE_QUERY")
	if dirs == "" || query == "" {
		t.Skip("set PYRE_SEARCH_LIVE_DIRS and PYRE_SEARCH_LIVE_QUERY")
	}
	var sources []Source
	var bytes int64
	for _, d := range filepath.SplitList(dirs) {
		_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			format := FormatClaude
			if strings.HasPrefix(filepath.Base(p), "rollout-") {
				format = FormatCodex
			} else if strings.Contains(p, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
				return nil
			}
			if fi, err := e.Info(); err == nil {
				bytes += fi.Size()
			}
			id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
			sources = append(sources, Source{RunID: "run_" + id, SessionID: "ses_" + id, Harness: contracts.Harness(format), Path: p, Format: format})
			return nil
		})
	}
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ix, err := Open(context.Background(), db, "node_live", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	st, err := ix.Update(ctx, sources)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	full := time.Since(t0)
	t1 := time.Now()
	st2, _ := ix.Update(context.Background(), sources) // incremental: nothing new
	incr := time.Since(t1)
	res, err := ix.Search(context.Background(), Query{Text: query, Limit: 20, Budget: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, h := range res.Hits {
		files[h.SessionID] = true
	}
	t.Logf("files=%d bytes=%dMB entries=%d partial=%v first-index=%s re-check=%s (+%d entries)",
		len(sources), bytes>>20, st.Indexed, st.Partial, full.Round(time.Millisecond), incr.Round(time.Millisecond), st2.Indexed)
	t.Logf("query %q: hits=%d in %d transcripts, truncated=%v, took=%dms", query, len(res.Hits), len(files), res.Truncated, res.TookMS)
}
