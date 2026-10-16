package delivery

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// W-delivery2 (incident 2026-10-07, rc37 on server): two messages the recipient
// read through MCP get_message at 07:05 were held behind the owner's
// writable attach for 8 h and pasted when the owner left. These tests run
// the hold/release cycle on the fake adapter.

type stepClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

// fakeFacts is the hub as the pre-paste gate sees it.
type fakeFacts struct {
	mu    sync.Mutex
	state map[string]contracts.RuntimeDeliveryState
	asked int
}

func (f *fakeFacts) Settled(ctx context.Context, sessionID, messageID string) (contracts.RuntimeDeliveryState, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	s, ok := f.state[messageID]
	if !ok {
		return "", "", false
	}
	return s, "hub: accepted (evidence pyre.read): read by the recipient session (ack); not pasted", true
}

func (f *fakeFacts) set(id string, s contracts.RuntimeDeliveryState) {
	f.mu.Lock()
	f.state[id] = s
	f.mu.Unlock()
}

// remindLike adds the doorbell's summary-line request to claudeLike.
type remindLike struct {
	*claudeLike
	mu       sync.Mutex
	reminded [][]string
}

func (r *remindLike) RemindUnread(h contracts.RunHandle, ids []string) {
	r.mu.Lock()
	r.reminded = append(r.reminded, append([]string(nil), ids...))
	r.mu.Unlock()
}

type heldEnv struct {
	*env
	c     *claudeLike
	r     *remindLike
	clk   *stepClock
	facts *fakeFacts
}

func newHeldEnv(t *testing.T) *heldEnv {
	e, c := newClaudeEnv(t)
	r := &remindLike{claudeLike: c}
	e.runs.wrap = func(a contracts.RuntimeAdapter) contracts.RuntimeAdapter { c.RuntimeAdapter = a; return r }
	h := &heldEnv{env: e, c: c, r: r, clk: &stepClock{}, facts: &fakeFacts{state: map[string]contracts.RuntimeDeliveryState{}}}
	e.engine = New(Config{NodeID: "node_test", Inbox: e.inbox, Runs: e.runs, Reporter: e.rep, Clock: h.clk, HubClock: contracts.SystemClock{},
		Facts: h.facts, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond})
	return h
}

func (h *heldEnv) hold() {
	h.runs.set(func() {
		h.runs.runs["run_1"].Lease = &contracts.InputLease{LeaseID: "lease_owner", RunID: "run_1",
			Holder: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "owner"}, ExpiresAt: time.Now().Add(24 * time.Hour)}
	})
}

func (h *heldEnv) release() {
	h.runs.set(func() { h.runs.runs["run_1"].Lease = nil })
}

// putAnnounced queues a message at the engine clock's now and records the
// doorbell telling the agent about it.
func (h *heldEnv) putAnnounced(id string) {
	h.t.Helper()
	d := contracts.DeliverCommand{SessionID: "ses_a", Message: contracts.MessageEnvelope{SchemaVersion: 1, MessageID: id, OperationID: "op_" + id,
		Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_sender"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_a"},
		Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "synthetic body of " + id}}}
	now := h.clk.Now()
	if _, _, err := h.inbox.Put(context.Background(), d, now); err != nil {
		h.t.Fatal(err)
	}
	h.c.mu.Lock()
	h.c.notified[id] = now
	h.c.mu.Unlock()
}

func (h *heldEnv) process() {
	h.engine.ProcessSession(context.Background(), "ses_a", true)
}

// The recipient reads the message (hub read withdrawal reaches the node)
// while the owner's lease holds it: nothing is pasted on release.
func TestReadDuringLeaseHoldNotPastedOnRelease(t *testing.T) {
	h := newHeldEnv(t)
	h.hold()
	h.putAnnounced("msg_read")
	h.process()
	if m := h.state("msg_read"); m.State != contracts.DeliveryQueued || m.Attempted {
		t.Fatalf("held = %+v", m)
	}
	if _, err := h.engine.ApplyRead(context.Background(), "msg_read", "run_1", contracts.EvidencePyreRead, "read by the recipient session (get_message); not pasted"); err != nil {
		t.Fatal(err)
	}
	h.release()
	h.process()
	if h.ad.Hits() != 0 {
		t.Fatalf("read message pasted on release (%d Deliver calls)", h.ad.Hits())
	}
	if m := h.state("msg_read"); m.State != contracts.DeliveryAccepted || m.ConsumedAt == nil {
		t.Fatalf("after release = %+v", m)
	}
}

// The recipient acks during the hold and the node never got the hub's
// withdrawal (older hub, lost command): the pre-paste hub check catches it.
func TestAckDuringLeaseHoldNotPastedOnRelease(t *testing.T) {
	h := newHeldEnv(t)
	h.hold()
	h.putAnnounced("msg_ack")
	h.process()
	h.facts.set("msg_ack", contracts.DeliveryAccepted)
	h.release()
	h.process()
	h.process()
	if h.ad.Hits() != 0 {
		t.Fatalf("acked message pasted on release (%d Deliver calls)", h.ad.Hits())
	}
	m := h.state("msg_ack")
	if m.State != contracts.DeliveryAccepted || m.BoundRunID != "run_1" {
		t.Fatalf("after release = %+v", m)
	}
	rs := h.rep.Receipts("msg_ack")
	last := rs[len(rs)-1]
	if last.State != contracts.DeliveryAccepted || last.Evidence != contracts.EvidencePyreRead || !strings.Contains(last.Detail, "not pasted") {
		t.Fatalf("receipts = %+v", rs)
	}
	// Hub-cancelled mail is closed the same way.
	h.hold()
	h.putAnnounced("msg_cancel")
	h.facts.set("msg_cancel", contracts.DeliveryCancelled)
	h.release()
	h.process()
	if m := h.state("msg_cancel"); m.State != contracts.DeliveryCancelled || h.ad.Hits() != 0 {
		t.Fatalf("hub-cancelled = %+v hits=%d", m, h.ad.Hits())
	}
}

// Announced mail held past HeldStale is left in the inbox on release with
// one summary-line request; fresh mail announced during the hold is pasted.
func TestStaleHeldBacklogSummaryNotPastes(t *testing.T) {
	h := newHeldEnv(t)
	h.hold()
	h.putAnnounced("msg_old1")
	h.putAnnounced("msg_old2")
	h.process()
	h.clk.advance(8 * time.Hour) // the owner stays attached all day
	h.putAnnounced("msg_fresh")
	h.process()
	if h.ad.Hits() != 0 {
		t.Fatalf("pasted during the hold: %d", h.ad.Hits())
	}
	h.release()
	h.process()
	for _, id := range []string{"msg_old1", "msg_old2"} {
		if m := h.state(id); m.State != contracts.DeliveryQueued || m.Attempted || !strings.Contains(m.Reason, "stale held mail") {
			t.Fatalf("%s = %+v", id, m)
		}
	}
	if m := h.state("msg_fresh"); m.State != contracts.DeliveryAccepted {
		t.Fatalf("fresh mail not pasted on release: %+v", m)
	}
	if h.ad.Hits() != 1 {
		t.Fatalf("Deliver calls = %d, want 1 (fresh only)", h.ad.Hits())
	}
	if len(h.r.reminded) != 1 || strings.Join(h.r.reminded[0], ",") != "msg_old1,msg_old2" {
		t.Fatalf("summary requests = %+v", h.r.reminded)
	}
	// Later passes: still parked, no second summary, no paste.
	h.process()
	h.process()
	if len(h.r.reminded) != 1 || h.ad.Hits() != 1 {
		t.Fatalf("repeat: reminders=%d hits=%d", len(h.r.reminded), h.ad.Hits())
	}
}

// Fresh unread mail held by the lease is still pasted on release (and the
// gate asked the hub first).
func TestFreshHeldMessagePastedOnRelease(t *testing.T) {
	h := newHeldEnv(t)
	h.hold()
	h.putAnnounced("msg_new")
	h.process()
	h.clk.advance(5 * time.Minute)
	h.release()
	h.process()
	if m := h.state("msg_new"); m.State != contracts.DeliveryAccepted || h.ad.Hits() != 1 || h.facts.asked == 0 {
		t.Fatalf("fresh held = %+v hits=%d asked=%d", m, h.ad.Hits(), h.facts.asked)
	}
}
