package tui_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// countingAPI counts full session lists. Embedding the interface hides the
// mock's optional SessionBatchAPI (an old hub) unless batch is set.
type countingAPI struct {
	contracts.API
	lists atomic.Int32
}

func (a *countingAPI) ListSessions(ctx context.Context, req contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	a.lists.Add(1)
	return a.API.ListSessions(ctx, req)
}

type batchingAPI struct {
	*countingAPI
	b    *mock.Backend
	many atomic.Int32
	ids  atomic.Value // []string of the last call
}

func (a *batchingAPI) GetManySessions(ctx context.Context, req contracts.GetManySessionsRequest) (contracts.GetManySessionsResponse, error) {
	a.many.Add(1)
	a.ids.Store(append([]string(nil), req.IDs...))
	return a.b.GetManySessions(ctx, req)
}

func patchHarness(t *testing.T, api contracts.API, b *mock.Backend) *tuitest.Harness {
	t.Helper()
	opts := tuitest.Options()
	opts.Clock = b.Clock()
	opts.Watch = tui.PollWatcher(b)
	opts.Debounce = 40 * time.Millisecond
	h := tuitest.New(t, api, opts, 120, 40)
	h.WaitFor("sessions", 5*sec, func() bool { return h.M.Loaded("sessions") })
	h.Settle()
	return h
}

func rename(t *testing.T, b *mock.Backend, sid, title string) {
	t.Helper()
	if _, err := b.UpdateSession(ctx(), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_" + title},
		SessionID: sid, Title: &title}); err != nil {
		t.Fatal(err)
	}
}

// A session event refreshes that one row with sessions.get_many; the full
// list is not fetched again.
func TestSessionEventPatchesOneRow(t *testing.T) {
	b, _ := tuitest.NewMock(t)
	api := &batchingAPI{countingAPI: &countingAPI{API: b}, b: b}
	h := patchHarness(t, api, b)
	before := api.lists.Load()

	rename(t, b, "ses_synth-b", "renamed-synthetic-row")
	h.WaitFor("patched row", 5*sec, func() bool { return h.Contains("renamed-synthetic-row") })
	h.Settle()

	if got := api.lists.Load(); got != before {
		t.Fatalf("a session event re-listed all sessions (%d -> %d)", before, got)
	}
	if api.many.Load() < 1 || h.M.Stats().Patches < 1 {
		t.Fatalf("no sessions.get_many (calls %d, patches %d)", api.many.Load(), h.M.Stats().Patches)
	}
	if ids, _ := api.ids.Load().([]string); len(ids) != 1 || ids[0] != "ses_synth-b" {
		t.Fatalf("get_many ids: %v", ids)
	}

	// Archiving drops the row from the (non-archived) list, still without
	// a full reload.
	arch := true
	if _, err := b.UpdateSession(ctx(), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_patch-arch"},
		SessionID: "ses_synth-b", Archived: &arch}); err != nil {
		t.Fatal(err)
	}
	h.WaitFor("archived row gone", 5*sec, func() bool { return !h.Contains("renamed-synthetic-row") })
	if got := api.lists.Load(); got != before {
		t.Fatalf("archive re-listed all sessions (%d -> %d)", before, got)
	}
}

// Without sessions.get_many (an older hub) the TUI re-lists as before.
func TestSessionEventFallsBackToFullList(t *testing.T) {
	b, _ := tuitest.NewMock(t)
	api := &countingAPI{API: b}
	h := patchHarness(t, api, b)
	before := api.lists.Load()

	rename(t, b, "ses_synth-b", "fallback-synthetic-row")
	h.WaitFor("reloaded row", 5*sec, func() bool { return h.Contains("fallback-synthetic-row") })
	if api.lists.Load() == before {
		t.Fatal("expected a full sessions.list without get_many")
	}
	if h.M.Stats().Patches != 0 {
		t.Fatalf("patches without get_many: %d", h.M.Stats().Patches)
	}
}
