package web

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// countingHub counts session reads; batch exposes sessions.get_many.
type countingHub struct {
	c.API
	lists, many atomic.Int32
}

func (h *countingHub) ListSessions(ctx context.Context, req c.ListSessionsRequest) (c.Page[c.SessionView], error) {
	h.lists.Add(1)
	return h.API.ListSessions(ctx, req)
}

type batchHub struct {
	*countingHub
	b c.SessionBatchAPI
}

func (h batchHub) GetManySessions(ctx context.Context, req c.GetManySessionsRequest) (c.GetManySessionsResponse, error) {
	h.many.Add(1)
	return h.b.GetManySessions(ctx, req)
}

func patchEnv(t *testing.T, batch bool) (*tenv, *Server, *countingHub) {
	t.Helper()
	e := newTestEnv(t, true)
	ch := &countingHub{API: e.b}
	var api c.API = ch
	if batch {
		api = batchHub{countingHub: ch, b: e.b}
	}
	s, err := New(Options{API: api, StateDir: t.TempDir(), Now: e.clk.Now, Debounce: 5 * time.Millisecond, Heartbeat: time.Second, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Start(ctx)
	waitFor(t, "event stream", s.live.Load)
	return e, s, ch
}

func titleOf(t *testing.T, s *Server, sid string) string {
	t.Helper()
	ss, err := s.getSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range ss {
		if v.Session.SessionID == sid {
			return v.Session.Title
		}
	}
	return ""
}

func renameSession(t *testing.T, e *tenv, sid, title string) {
	t.Helper()
	if _, err := e.b.UpdateSession(ownerCtx(), c.UpdateSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, SessionID: sid, Title: &title}); err != nil {
		t.Fatal(err)
	}
}

// A session event refreshes that session with one get_many on the next
// read; the full list is not fetched again.
func TestSessionEventPatchesWithoutRelisting(t *testing.T) {
	e, s, ch := patchEnv(t, true)
	sid := e.start("synth patch target", "")
	waitFor(t, "session listed", func() bool { return titleOf(t, s, sid) == "synth patch target" })
	lists := ch.lists.Load()

	for i, title := range []string{"synth renamed once", "synth renamed twice"} {
		renameSession(t, e, sid, title)
		waitFor(t, "patched title", func() bool { return titleOf(t, s, sid) == title })
		if got := ch.lists.Load(); got != lists {
			t.Fatalf("rename %d re-listed every session (%d -> %d lists)", i, lists, got)
		}
	}
	if ch.many.Load() < 2 {
		t.Fatalf("get_many calls %d, want one per rename", ch.many.Load())
	}

	// A new session arrives by patch too, in the hub's order.
	sid2 := e.start("synth newer session", "")
	waitFor(t, "new session patched in", func() bool { return titleOf(t, s, sid2) == "synth newer session" })
	if got := ch.lists.Load(); got != lists {
		t.Fatalf("a new session re-listed every session (%d -> %d)", lists, got)
	}
	if ss, _ := s.getSessions(context.Background()); ss[0].Session.SessionID != sid2 {
		t.Fatalf("new session not first (newest first): %s", ss[0].Session.SessionID)
	}

	// Archived: the row leaves the active list, still without a re-list.
	arch := true
	if _, err := e.b.UpdateSession(ownerCtx(), c.UpdateSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, SessionID: sid, Archived: &arch}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "archived row gone", func() bool { return titleOf(t, s, sid) == "" })
	if got := ch.lists.Load(); got != lists {
		t.Fatalf("archive re-listed every session (%d -> %d)", lists, got)
	}

	// The safety net: past liveMaxAge the full list is read again.
	e.clk.Advance(6 * time.Minute)
	titleOf(t, s, sid2)
	if ch.lists.Load() == lists {
		t.Fatal("no full re-list after the safety-net age")
	}
}

// A hub without sessions.get_many (an older hub) re-lists as before.
func TestSessionEventWithoutBatchRelists(t *testing.T) {
	e, s, ch := patchEnv(t, false)
	sid := e.start("synth old hub target", "")
	waitFor(t, "session listed", func() bool { return titleOf(t, s, sid) == "synth old hub target" })
	lists := ch.lists.Load()
	renameSession(t, e, sid, "synth old hub renamed")
	waitFor(t, "renamed via full list", func() bool { return titleOf(t, s, sid) == "synth old hub renamed" })
	if ch.lists.Load() == lists {
		t.Fatal("expected a full sessions.list without get_many")
	}
}
