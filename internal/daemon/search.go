package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/search"
)

// Transcript search (W-search). The node keeps an incremental FTS5 index of
// its runs' native transcripts in its own database. It is brought up to
// date by one background goroutine: shortly after start (backfill), after a
// turn ends (a run settles into waiting_input or ends; debounced), and every
// few minutes. Hooks never index. A query first catches up on changed files
// within part of its time budget (skipped while a background pass runs),
// then searches.

// transcriptFiler is implemented by adapters whose runs have native
// transcript files (Claude, Codex).
type transcriptFiler interface {
	TranscriptFiles(h contracts.RunHandle) (format string, paths []string)
}

const (
	searchPassBudget   = 30 * time.Second // one background pass
	searchPassInterval = 3 * time.Minute
	searchBackfillWait = 15 * time.Second
	searchRetryEmpty   = 30 * time.Minute // re-resolve an ended run without files
	searchKickDebounce = 2 * time.Second
)

type searchState struct {
	ix   *search.Index
	kick chan struct{}

	mu    sync.Mutex
	empty map[string]time.Time // ended runs with no transcript file yet
	last  search.Stats
}

// initSearch opens the index (namespace "search" in the node database) and
// registers transcript.search. A failure leaves the action unregistered
// (callers see unsupported) and is noted, never fatal.
func (d *Daemon) initSearch() {
	ix, err := search.Open(context.Background(), d.db, d.nodeID, search.Limits{})
	if err != nil {
		d.noteErr("transcript search disabled: %v", err)
		return
	}
	d.search = &searchState{ix: ix, kick: make(chan struct{}, 1), empty: map[string]time.Time{}}
	d.RegisterSyncAction(contracts.ActionTranscriptSearch, d.actionTranscriptSearch)
}

// searchTurnSettled kicks the indexer when a run's turn ended.
func (d *Daemon) searchTurnSettled(prev, next contracts.RunState) {
	if d.search == nil || prev == next {
		return
	}
	switch next {
	case contracts.RunWaitingInput, contracts.RunExited, contracts.RunFailed:
		select {
		case d.search.kick <- struct{}{}:
		default:
		}
	}
}

func (d *Daemon) searchLoop(ctx context.Context) {
	if d.search == nil {
		return
	}
	tick := time.NewTicker(searchPassInterval)
	defer tick.Stop()
	next := time.NewTimer(searchBackfillWait)
	defer next.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-next.C:
		case <-d.search.kick:
			select { // coalesce a burst of turn ends
			case <-ctx.Done():
				return
			case <-time.After(searchKickDebounce):
			}
		}
		pctx, cancel := context.WithTimeout(ctx, searchPassBudget)
		st, err := d.search.ix.Update(pctx, d.searchSources(pctx))
		cancel()
		if err != nil && ctx.Err() == nil {
			d.noteErr("transcript index: %v", err)
		}
		d.search.mu.Lock()
		d.search.last = st
		d.search.mu.Unlock()
		if st.Partial { // backfill continues soon, in bounded passes
			next.Reset(5 * time.Second)
		}
	}
}

// searchSources lists every run's transcript files: live runs are asked
// each time (a /clear starts a new file); ended runs reuse what the index
// already knows.
func (d *Daemon) searchSources(ctx context.Context) []search.Source {
	recs, err := d.st.runs(ctx, `handle IS NOT NULL`)
	if err != nil {
		return nil
	}
	known, _ := d.search.ix.KnownPaths(ctx)
	now := d.now()
	var out []search.Source
	for _, rec := range recs {
		if rec.Handle == nil {
			continue
		}
		f, ok := d.adapters[rec.Run.Adapter].(transcriptFiler)
		if !ok {
			continue
		}
		runID := rec.Run.RunID
		paths := known[runID]
		_, live := d.getLive(runID)
		format := ""
		d.search.mu.Lock()
		retryAt, wasEmpty := d.search.empty[runID]
		d.search.mu.Unlock()
		if live || (len(paths) == 0 && (!wasEmpty || now.After(retryAt))) {
			var found []string
			format, found = f.TranscriptFiles(*rec.Handle)
			for _, p := range found {
				if !containsStr(paths, p) {
					paths = append(paths, p)
				}
			}
			d.search.mu.Lock()
			if len(paths) == 0 && !live {
				d.search.empty[runID] = now.Add(searchRetryEmpty)
			} else {
				delete(d.search.empty, runID)
			}
			d.search.mu.Unlock()
		}
		if format == "" {
			format = string(rec.Run.Harness)
			if rec.Run.Harness != contracts.HarnessCodex && rec.Run.Harness != contracts.HarnessAgy {
				format = search.FormatClaude
			}
		}
		for _, p := range paths {
			out = append(out, search.Source{RunID: runID, SessionID: rec.Run.SessionID, Harness: rec.Run.Harness,
				Title: rec.Session.Title, Path: p, Format: format})
		}
	}
	return out
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// actionTranscriptSearch handles transcript.search.
func (d *Daemon) actionTranscriptSearch(ctx context.Context, raw json.RawMessage) (any, error) {
	var p contracts.TranscriptSearchParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, contracts.Errorf(contracts.ErrInvalid, "invalid params: %v", err)
		}
	}
	return d.searchLocal(ctx, p)
}

func (d *Daemon) searchLocal(ctx context.Context, p contracts.TranscriptSearchParams) (contracts.TranscriptSearchResult, error) {
	if d.search == nil {
		return contracts.TranscriptSearchResult{}, contracts.Errorf(contracts.ErrUnsupported, "transcript search is not available on this node")
	}
	start := time.Now()
	budget := time.Duration(p.BudgetMS) * time.Millisecond
	if budget <= 0 {
		budget = 1500 * time.Millisecond
	}
	budget = min(budget, 10*time.Second)
	// Catch up on changed files with up to 40% of the budget.
	cctx, cancel := context.WithTimeout(ctx, budget*4/10)
	st, busy, err := d.search.ix.TryUpdate(cctx, d.searchSources(cctx))
	cancel()
	if err != nil {
		d.noteErr("transcript index catch-up: %v", err)
	}
	if busy {
		d.search.mu.Lock()
		st = d.search.last
		d.search.mu.Unlock()
		st.Partial = true // a background pass is still indexing
	}
	res, err := d.search.ix.Search(ctx, search.Query{Text: p.Query, Since: p.Since, SessionIDs: p.SessionIDs, Limit: p.Limit,
		Budget: max(budget-time.Since(start), 100*time.Millisecond)})
	if err != nil {
		return res, err
	}
	res.Partial = res.Partial || st.Partial
	res.Pending = st.Pending
	res.TookMS = time.Since(start).Milliseconds()
	return res, nil
}
