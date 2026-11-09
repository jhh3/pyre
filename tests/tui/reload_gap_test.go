package tui_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// listCounter counts the base list reads the reload gap spaces.
type listCounter struct {
	contracts.API
	accounts, nodes, sessions atomic.Int32
}

func (a *listCounter) ListAccounts(ctx context.Context, req contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error) {
	a.accounts.Add(1)
	return a.API.ListAccounts(ctx, req)
}

func (a *listCounter) ListNodes(ctx context.Context, req contracts.ListNodesRequest) (contracts.Page[contracts.Node], error) {
	a.nodes.Add(1)
	return a.API.ListNodes(ctx, req)
}

func (a *listCounter) ListSessions(ctx context.Context, req contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	a.sessions.Add(1)
	return a.API.ListSessions(ctx, req)
}

// A burst of account and node events (usage observations, heartbeats)
// re-lists accounts and nodes once per ReloadGap, the last time after the
// burst, instead of once per debounce window.
func TestReloadGapSpacesEventDrivenRelists(t *testing.T) {
	b, _ := tuitest.NewMock(t)
	api := &listCounter{API: b}
	events := make(chan contracts.Event, 64)
	opts := tuitest.Options()
	opts.Clock = b.Clock()
	opts.Debounce = 20 * time.Millisecond
	opts.ReloadGap = time.Second
	opts.Watch = func(ctx context.Context, _ int64, fn func(contracts.Event) error) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ev := <-events:
				if err := fn(ev); err != nil {
					return err
				}
			}
		}
	}
	h := tuitest.New(t, api, opts, 120, 40)
	h.WaitFor("base lists", 5*sec, func() bool { return h.M.Loaded("accounts") && h.M.Loaded("nodes") && h.M.Loaded("sessions") })
	h.Settle()
	pump(h, opts.ReloadGap) // the first event after a quiet spell reloads at once
	a0, n0, s0 := api.accounts.Load(), api.nodes.Load(), api.sessions.Load()

	cursor := int64(1000)
	emit := func(kind contracts.EventKind) {
		cursor++
		events <- contracts.Event{Cursor: cursor, Kind: kind, OccurredAt: time.Now()}
	}
	emit(contracts.EvAccountObserved)
	h.WaitFor("first reload", 2*sec, func() bool { return api.accounts.Load() == a0+1 })

	// A 400ms burst inside the gap: no re-list until the gap has passed.
	burst := time.Now()
	for i := 0; i < 10; i++ {
		emit(contracts.EvAccountObserved)
		emit(contracts.EventKind("node.status"))
		pump(h, 40*time.Millisecond)
	}
	if time.Since(burst) > opts.ReloadGap/2 {
		t.Skipf("burst took %s (a loaded machine); the spacing cannot be judged", time.Since(burst))
	}
	if got := api.accounts.Load() - a0; got != 1 {
		t.Fatalf("accounts re-listed %d times during the burst, want 1 (the first event)", got)
	}
	if got := api.nodes.Load() - n0; got > 1 {
		t.Fatalf("nodes re-listed %d times during the burst, want at most 1", got)
	}
	if got := api.sessions.Load() - s0; got > 1 {
		t.Fatalf("sessions re-listed %d times during the burst, want at most 1", got)
	}
	// The trailing re-list lands once the gap has passed: the burst's last
	// event is not lost.
	h.WaitFor("trailing reload", 3*sec, func() bool { return api.accounts.Load() >= a0+2 })
	pump(h, opts.ReloadGap+200*time.Millisecond)
	if got := api.accounts.Load() - a0; got > 3 {
		t.Fatalf("accounts re-listed %d times for one burst of 10 events, want at most 3", got)
	}
	if got := api.nodes.Load() - n0; got < 1 || got > 3 {
		t.Fatalf("nodes re-listed %d times for one burst of 10 events, want 1 to 3", got)
	}
}

// pump processes the TUI's messages for d (the harness only runs Update
// while a test steps it).
func pump(h *tuitest.Harness, d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end); {
		h.SettleFor(10*time.Millisecond, 10*time.Millisecond)
	}
}
