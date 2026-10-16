package claudeplugin

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// listAPI records messages.list calls (the hub side).
type listAPI struct {
	contracts.API // nil: other calls panic
	mu            sync.Mutex
	lists         []contracts.ListMessagesRequest
	items         int  // items in the next answer
	failNext      bool // fail the next list
}

func (a *listAPI) ListMessages(ctx context.Context, r contracts.ListMessagesRequest) (contracts.Page[contracts.MessageView], error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lists = append(a.lists, r)
	if a.failNext {
		a.failNext = false
		return contracts.Page[contracts.MessageView]{}, contracts.Errorf(contracts.ErrUnavailable, "socket gone (test)")
	}
	pg := contracts.Page[contracts.MessageView]{Items: []contracts.MessageView{}}
	for i := 0; i < a.items; i++ {
		c := r.AfterCursor + int64(i) + 1
		pg.Items = append(pg.Items, contracts.MessageView{Cursor: c, Envelope: contracts.MessageEnvelope{MessageID: fmt.Sprintf("msg_%d", c), Mode: contracts.ModeQueue},
			Status: contracts.DeliveryStatus{Ack: contracts.AckNone, Runtime: contracts.DeliveryQueued}})
	}
	a.items = 0
	return pg, nil
}

func (a *listAPI) last() contracts.ListMessagesRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lists[len(a.lists)-1]
}

// localAPI is a pyred socket offering inbox.wait (or, with unsupported, an
// older pyred whose 404 the client reports as unsupported).
type localAPI struct {
	listAPI
	unsupported bool
	waits       []contracts.InboxWaitRequest
	gen         int64
}

func (a *localAPI) InboxWait(ctx context.Context, r contracts.InboxWaitRequest) (contracts.InboxWaitResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.waits = append(a.waits, r)
	if a.unsupported {
		return contracts.InboxWaitResponse{}, contracts.Errorf(contracts.ErrUnsupported, "no inbox.wait (test)")
	}
	a.gen++
	return contracts.InboxWaitResponse{Epoch: "e1", Generation: a.gen, Woken: true}, nil
}

func TestAPISourceWaitsLocallyThenListsWithoutWaiting(t *testing.T) {
	api := &localAPI{}
	s := &APISource{API: api, Session: "ses_x"}
	ctx := context.Background()
	if _, _, err := s.Poll(ctx, 0, 50*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(api.waits) != 1 || api.waits[0].Epoch != "" || api.waits[0].WaitSeconds != 50 || api.waits[0].SessionID != "ses_x" {
		t.Fatalf("first wait = %+v", api.waits)
	}
	if r := api.last(); r.WaitSeconds != 0 || !r.Unacked || r.Address == nil || r.Address.ID != "ses_x" {
		t.Fatalf("list after a local wait must not long-poll the hub: %+v", r)
	}
	if _, _, err := s.Poll(ctx, 0, 50*time.Second); err != nil {
		t.Fatal(err)
	}
	if w := api.waits[1]; w.Epoch != "e1" || w.AfterGeneration != 1 {
		t.Fatalf("re-armed wait = %+v", w)
	}
	if !s.LocalWait() || s.hubPolls != 0 {
		t.Fatalf("local=%v hubPolls=%d", s.LocalWait(), s.hubPolls)
	}

	// A full page: the next wait answers at once so the rest is fetched.
	api.items = pollLimit
	msgs, next, err := s.Poll(ctx, 0, 50*time.Second)
	if err != nil || len(msgs) != pollLimit || next != pollLimit {
		t.Fatalf("full page = %d %d %v", len(msgs), next, err)
	}
	if _, _, err := s.Poll(ctx, next, 50*time.Second); err != nil {
		t.Fatal(err)
	}
	if w := api.waits[len(api.waits)-1]; w.WaitSeconds != 0 {
		t.Fatalf("wait after a full page = %+v", w)
	}
	if r := api.last(); r.AfterCursor != pollLimit {
		t.Fatalf("next page list = %+v", r)
	}
}

func TestAPISourceFallsBackToHubLongPoll(t *testing.T) {
	ctx := context.Background()

	// An older pyred: inbox.wait unsupported -> hub long-poll, no re-probe
	// until a poll fails (pyred restarted, maybe upgraded).
	old := &localAPI{unsupported: true}
	s := &APISource{API: old, Session: "ses_y"}
	for i := 0; i < 2; i++ {
		if _, _, err := s.Poll(ctx, 0, 50*time.Second); err != nil {
			t.Fatal(err)
		}
		if r := old.last(); r.WaitSeconds != 50 {
			t.Fatalf("fallback list = %+v", r)
		}
	}
	if len(old.waits) != 1 || s.LocalWait() || s.hubPolls != 2 {
		t.Fatalf("probes=%d local=%v hubPolls=%d", len(old.waits), s.LocalWait(), s.hubPolls)
	}
	old.failNext = true
	if _, _, err := s.Poll(ctx, 0, 50*time.Second); err == nil {
		t.Fatal("expected the list failure")
	}
	old.unsupported = false // pyred came back upgraded
	if _, _, err := s.Poll(ctx, 0, 50*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(old.waits) != 2 || !s.LocalWait() || old.last().WaitSeconds != 0 {
		t.Fatalf("after re-probe: probes=%d local=%v list=%+v", len(old.waits), s.LocalWait(), old.last())
	}

	// A hub client without the interface at all.
	hub := &listAPI{}
	h := &APISource{API: hub, Session: "ses_z"}
	if _, _, err := h.Poll(ctx, 0, 50*time.Second); err != nil {
		t.Fatal(err)
	}
	if r := hub.last(); r.WaitSeconds != 50 {
		t.Fatalf("hub long-poll = %+v", r)
	}
}
