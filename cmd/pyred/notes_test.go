package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

type fakeWatcher struct {
	calls atomic.Int32
	err   error
}

func (f *fakeWatcher) ListEvents(ctx context.Context, req contracts.ListEventsRequest) (contracts.ListEventsResponse, error) {
	return contracts.ListEventsResponse{HeadCursor: 41}, nil
}

func (f *fakeWatcher) WatchEventsFiltered(ctx context.Context, req contracts.ListEventsRequest, fn func(contracts.Event) error) error {
	if f.calls.Add(1) > 1 {
		return f.err
	}
	if req.AfterCursor != 41 || len(req.Kinds) != 1 || req.Kinds[0] != contracts.EvNoteCreated {
		return contracts.Errorf(contracts.ErrInvalid, "bad request %+v", req)
	}
	fn(contracts.Event{Kind: contracts.EvNoteCreated, Cursor: 42})
	fn(contracts.Event{Kind: contracts.EvNoteCreated, Cursor: 43})
	return contracts.Errorf(contracts.ErrResetRequired, "stale")
}

func TestWatchNotesNotifiesAndStopsWhenForbidden(t *testing.T) {
	w := &fakeWatcher{err: contracts.Errorf(contracts.ErrForbidden, "nodes cannot watch events")}
	var n atomic.Int32
	done := make(chan struct{})
	go func() {
		watchNotes(context.Background(), w, func() { n.Add(1) }, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop on forbidden")
	}
	if n.Load() != 2 || w.calls.Load() != 2 {
		t.Fatalf("notifies=%d watch calls=%d", n.Load(), w.calls.Load())
	}
}

// prunedHub behaves like a hub whose event log was pruned: cursor 0 (and
// anything below the oldest retained event, or above the head) is answered
// with reset_required carrying the snapshot cursor.
type prunedHub struct {
	lists   atomic.Int32
	watchAt atomic.Int64
}

func (p *prunedHub) ListEvents(ctx context.Context, req contracts.ListEventsRequest) (contracts.ListEventsResponse, error) {
	p.lists.Add(1)
	if req.AfterCursor < 999 || req.AfterCursor > 5000 {
		return contracts.ListEventsResponse{}, contracts.Errorf(contracts.ErrResetRequired, "cursor outside retained events").
			WithDetail("oldest_cursor", int64(1000)).WithDetail("snapshot_cursor", int64(5000))
	}
	return contracts.ListEventsResponse{HeadCursor: 5000}, nil
}

func (p *prunedHub) WatchEventsFiltered(ctx context.Context, req contracts.ListEventsRequest, fn func(contracts.Event) error) error {
	p.watchAt.Store(req.AfterCursor)
	<-ctx.Done()
	return ctx.Err()
}

// A20 soak round 2: once the hub had pruned events, a restarted pyred asked
// for the head with cursor 0, got reset_required, and retried at once,
// forever: about 200 requests/s from one node (27 MB of hub log in 25
// minutes). The head now comes from the reset's snapshot cursor.
func TestWatchNotesStartsAtSnapshotWhenHubPruned(t *testing.T) {
	p := &prunedHub{}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	watchNotes(ctx, p, func() {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n := p.lists.Load(); n > 3 {
		t.Fatalf("%d ListEvents calls in 0.5 s: retry storm on reset_required", n)
	}
	if got := p.watchAt.Load(); got != 5000 {
		t.Fatalf("watch started at cursor %d, want the snapshot cursor 5000", got)
	}
}

// resetOnceHub resets the first watch (a sparse filtered watch whose cursor
// fell behind retention, seen on reconnect after a hub restart), then lets
// the second one run.
type resetOnceHub struct {
	prunedHub
	watches atomic.Int32
}

func (r *resetOnceHub) WatchEventsFiltered(ctx context.Context, req contracts.ListEventsRequest, fn func(contracts.Event) error) error {
	if r.watches.Add(1) == 1 {
		return contracts.Errorf(contracts.ErrResetRequired, "cursor outside retained events").WithDetail("snapshot_cursor", int64(5000))
	}
	return r.prunedHub.WatchEventsFiltered(ctx, req, fn)
}

// A20 soak round 2: the same storm came from long-running nodes. A
// note.created watch only advances on notes, so after a hub restart its
// cursor was behind the pruned log, the watch was reset, and the old code
// re-listed from cursor 0 in a loop (hub at 190% CPU for 2.5 hours).
func TestWatchNotesRecoversFromWatchResetWithoutStorm(t *testing.T) {
	h := &resetOnceHub{}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	watchNotes(ctx, h, func() {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n := h.lists.Load(); n > 4 {
		t.Fatalf("%d ListEvents calls: retry storm after a watch reset", n)
	}
	if h.watches.Load() != 2 || h.watchAt.Load() != 5000 {
		t.Fatalf("watches=%d resumed at %d, want 2 watches resuming at 5000", h.watches.Load(), h.watchAt.Load())
	}
}
