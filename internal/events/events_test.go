package events

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func open(t *testing.T, path string, c *clock, r Retention) (*sql.DB, *Log) {
	t.Helper()
	db, err := sqlitex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(context.Background(), db, Options{Clock: c, Retention: r, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return db, l
}

func appendN(t *testing.T, db *sql.DB, l *Log, n int, kind contracts.EventKind, session string) []int64 {
	t.Helper()
	var out []int64
	err := sqlitex.Tx(context.Background(), db, func(tx *sql.Tx) error {
		for i := 0; i < n; i++ {
			ev, err := l.AppendTx(context.Background(), tx, contracts.Event{Kind: kind, Object: contracts.ObjectRef{Kind: contracts.KindSession, ID: session}, SessionID: session})
			if err != nil {
				return err
			}
			out = append(out, ev.Cursor)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	l.Notify()
	return out
}

func TestAppendMonotonicAcrossRestartAndRollback(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "e.db")
	db, l := open(t, path, c, Retention{})
	a := appendN(t, db, l, 3, contracts.EvSessionCreated, "ses_1")
	// A rolled-back append does not become visible.
	_ = sqlitex.Tx(context.Background(), db, func(tx *sql.Tx) error {
		l.AppendTx(context.Background(), tx, contracts.Event{Kind: contracts.EvRunStarted})
		return contracts.Errorf(contracts.ErrInternal, "boom")
	})
	db.Close()
	db, l = open(t, path, c, Retention{})
	defer db.Close()
	b := appendN(t, db, l, 2, contracts.EvRunStarted, "ses_1")
	all := append(a, b...)
	for i := 1; i < len(all); i++ {
		if all[i] <= all[i-1] {
			t.Fatalf("not monotonic: %v", all)
		}
	}
	resp, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{})
	if err != nil || len(resp.Events) != 5 || resp.HeadCursor != all[4] || resp.NextCursor != all[4] {
		t.Fatalf("list: %v %+v", err, resp)
	}
	if resp.Events[0].EventID == "" || resp.Events[0].Origin != "hub" || resp.Events[0].PayloadVersion != 1 {
		t.Fatalf("defaults: %+v", resp.Events[0])
	}
}

func TestFiltersAndNextCursorAdvance(t *testing.T) {
	c := &clock{t: time.Now()}
	db, l := open(t, filepath.Join(t.TempDir(), "e.db"), c, Retention{})
	defer db.Close()
	appendN(t, db, l, 3, contracts.EvSessionCreated, "ses_1")
	appendN(t, db, l, 2, contracts.EvRunStarted, "ses_2")
	resp, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{Kinds: []contracts.EventKind{contracts.EvRunStarted}})
	if err != nil || len(resp.Events) != 2 {
		t.Fatalf("kinds: %v %d", err, len(resp.Events))
	}
	resp, err = l.ListEvents(context.Background(), contracts.ListEventsRequest{SessionID: "ses_1", Limit: 2})
	if err != nil || len(resp.Events) != 2 || resp.NextCursor != resp.Events[1].Cursor {
		t.Fatalf("limit: %v %+v", err, resp)
	}
	// No matches: cursor still advances to the head so callers don't rescan.
	resp, err = l.ListEvents(context.Background(), contracts.ListEventsRequest{SessionID: "ses_none"})
	if err != nil || len(resp.Events) != 0 || resp.NextCursor != resp.HeadCursor || resp.HeadCursor != 5 {
		t.Fatalf("none: %v %+v", err, resp)
	}
	empty := ""
	resp, err = l.ListEvents(context.Background(), contracts.ListEventsRequest{ProjectID: &empty})
	if err != nil || len(resp.Events) != 5 {
		t.Fatalf("standalone filter: %v %d", err, len(resp.Events))
	}
	if _, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: -1}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatal("negative cursor accepted")
	}
}

func TestLongPollWakesOnNotifyAndPoll(t *testing.T) {
	c := &clock{t: time.Now()}
	db, l := open(t, filepath.Join(t.TempDir(), "e.db"), c, Retention{})
	defer db.Close()
	got := make(chan contracts.ListEventsResponse, 1)
	go func() {
		r, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{WaitSeconds: 5})
		if err != nil {
			t.Error(err)
		}
		got <- r
	}()
	time.Sleep(50 * time.Millisecond)
	// Commit without Notify: the poll interval still catches it.
	sqlitex.Tx(context.Background(), db, func(tx *sql.Tx) error {
		_, err := l.AppendTx(context.Background(), tx, contracts.Event{Kind: contracts.EvNoteCreated})
		return err
	})
	select {
	case r := <-got:
		if len(r.Events) != 1 {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long poll missed commit")
	}
	// Context cancellation ends the wait early.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	r, err := l.ListEvents(ctx, contracts.ListEventsRequest{AfterCursor: 1, WaitSeconds: 30})
	if err != nil || len(r.Events) != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("cancel: %v %v", err, time.Since(start))
	}
}

func TestRetentionResetRequired(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	db, l := open(t, filepath.Join(t.TempDir(), "e.db"), c, Retention{MaxEvents: 5, MaxAge: time.Hour})
	defer db.Close()
	appendN(t, db, l, 10, contracts.EvRunObserved, "ses_1")
	n, err := l.Prune(context.Background())
	if err != nil || n != 5 {
		t.Fatalf("prune count: %v %d", err, n)
	}
	// Cursor 5 is exactly the pruned boundary: nothing it hasn't seen was lost.
	resp, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: 5})
	if err != nil || len(resp.Events) != 5 {
		t.Fatalf("boundary: %v %+v", err, resp)
	}
	_, err = l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: 2})
	if !contracts.IsCode(err, contracts.ErrResetRequired) {
		t.Fatalf("want reset_required, got %v", err)
	}
	ce := contracts.AsError(err)
	if ce.Details["oldest_cursor"] != int64(6) || ce.Details["snapshot_cursor"] != int64(10) {
		t.Fatalf("details %+v", ce.Details)
	}
	// Age-based pruning.
	c.Add(2 * time.Hour)
	appendN(t, db, l, 1, contracts.EvRunObserved, "ses_1")
	if n, _ := l.Prune(context.Background()); n != 5 {
		t.Fatalf("age prune %d", n)
	}
	if _, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: 9}); !contracts.IsCode(err, contracts.ErrResetRequired) {
		t.Fatalf("after age prune: %v", err)
	}
	resp, err = l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: 10})
	if err != nil || len(resp.Events) != 1 || resp.Events[0].Cursor != 11 {
		t.Fatalf("snapshot resume: %v %+v", err, resp)
	}
	// A cursor ahead of the head (restored older hub) also requires reset.
	if _, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: 500}); !contracts.IsCode(err, contracts.ErrResetRequired) {
		t.Fatalf("future cursor: %v", err)
	}
}

func TestConcurrentAppendersNeverSkip(t *testing.T) {
	c := &clock{t: time.Now()}
	db, l := open(t, filepath.Join(t.TempDir(), "e.db"), c, Retention{})
	defer db.Close()
	const writers, per = 4, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				sqlitex.Tx(context.Background(), db, func(tx *sql.Tx) error {
					_, err := l.AppendTx(context.Background(), tx, contracts.Event{Kind: contracts.EvRunObserved})
					return err
				})
			}
		}()
	}
	// A reader pages concurrently and must see every event exactly once.
	seen := map[int64]bool{}
	var cursor int64
	deadline := time.Now().Add(10 * time.Second)
	for len(seen) < writers*per && time.Now().Before(deadline) {
		r, err := l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: cursor, Limit: 7})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range r.Events {
			if seen[ev.Cursor] {
				t.Fatalf("duplicate %d", ev.Cursor)
			}
			seen[ev.Cursor] = true
		}
		cursor = r.NextCursor
	}
	wg.Wait()
	r, _ := l.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: cursor, Limit: 1000})
	for _, ev := range r.Events {
		seen[ev.Cursor] = true
	}
	if len(seen) != writers*per {
		t.Fatalf("saw %d of %d", len(seen), writers*per)
	}
}

// A20 soak: the retention queries run every maintenance pass inside a
// write transaction and must not scan the log. Guard their query plans.
func TestPruneQueriesDoNotScanTheLog(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	db, l := open(t, filepath.Join(t.TempDir(), "e.db"), c, Retention{MaxEvents: 100, MaxAge: time.Hour})
	defer db.Close()
	appendN(t, db, l, 300, contracts.EvRunObserved, "ses_1")
	for _, q := range []string{pruneMaxCursorQuery, pruneAgeQuery} {
		rows, err := db.Query("EXPLAIN QUERY PLAN "+q, "2026-10-04T11:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "; ")
		if strings.Contains(joined, "SCAN events") || (q == pruneAgeQuery && !strings.Contains(joined, "events_ingested")) {
			t.Fatalf("%s: plan %q scans the log", q, joined)
		}
	}
	if n, err := l.Prune(context.Background()); err != nil || n != 200 {
		t.Fatalf("prune: %v %d", err, n)
	}
	var cnt int
	_ = db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&cnt)
	if cnt != 100 {
		t.Fatalf("kept %d events, want 100", cnt)
	}
}
