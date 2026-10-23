package attention

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// "Waiting on you" (owner request 2026-10-14): an item per question of a
// top-level session (one active at a time), raised by the hub at turn end,
// resolved by the session moving on or the owner engaging.

const waitSID = "ses_synth-lead"

func (e *env) raiseWaiting(q string) contracts.AttentionItem {
	e.t.Helper()
	it, err := e.store.RaiseWaiting(context.Background(), waitSID, nil, q, "Synthetic report done. "+q+" Default: ship.")
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) runAt(st contracts.RunState, at time.Time) {
	r := contracts.Run{RunID: "run_synth-lead", SessionID: waitSID, State: st}
	b, _ := json.Marshal(r)
	e.log.Append(contracts.Event{Origin: "hub", Kind: contracts.EvRunObserved, Object: contracts.ObjectRef{Kind: contracts.KindRun, ID: r.RunID},
		SessionID: waitSID, Payload: b, OccurredAt: at})
}

func TestWaitingRaiseFlagsAndRepeatUpdates(t *testing.T) {
	e := newEnv(t)
	it := e.raiseWaiting("Should I build the synthetic voice-match score?\nMore detail.")
	if it.Kind != contracts.AttnWaiting || it.GroupKey != "waiting:"+waitSID || it.Object != (contracts.ObjectRef{Kind: contracts.KindSession, ID: waitSID}) ||
		it.SessionID != waitSID || it.Severity != "normal" || it.State != contracts.AttnOpen ||
		it.Title != "Waiting on you: Should I build the synthetic voice-match score?" || !strings.Contains(it.Summary, "Default: ship.") {
		t.Fatalf("item: %+v", it)
	}
	n, f := e.flag(waitSID)
	if n != 1 || f == nil || f.Level != contracts.FlagNeedsYou || f.Reason != "waiting" || f.Text != it.Title || f.Kind != string(contracts.AttnWaiting) {
		t.Fatalf("flag = %d %+v", n, f)
	}
	// Read: still flags (the question is unanswered).
	if _, err := e.store.UpdateAttention(ownerCtx(), contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-read"},
		AttentionID: it.AttentionID, State: contracts.AttnRead}); err != nil {
		t.Fatal(err)
	}
	if _, f := e.flag(waitSID); f == nil || f.Reason != "waiting" {
		t.Fatalf("read flag = %+v", f)
	}
	// A raise while it is active updates the item (and reopens it).
	e.clock.Advance(time.Minute)
	again := e.raiseWaiting(strings.Repeat("Pick A or B? ", 20))
	if again.AttentionID != it.AttentionID || again.Count != 2 || again.State != contracts.AttnOpen || !again.UpdatedAt.After(it.UpdatedAt) ||
		len([]rune(again.Title)) > len("Waiting on you: ")+101 {
		t.Fatalf("repeat: %+v", again)
	}
	if g := e.byGroup("waiting:" + waitSID); len(g) != 1 {
		t.Fatalf("duplicated: %+v", g)
	}
}

func TestWaitingResolvedByNextTurnAskEngageArchiveAndEnd(t *testing.T) {
	e := newEnv(t)
	it := e.raiseWaiting("Ship it?")
	// An event from before the raise (late-processed) leaves it alone.
	e.runAt(contracts.RunRunning, t0.Add(-time.Second))
	e.drain()
	if g := e.byGroup("waiting:" + waitSID); g[0].State != contracts.AttnOpen {
		t.Fatalf("stale running event withdrew it: %+v", g)
	}
	// The next turn starts: withdrawn.
	e.clock.Advance(time.Second)
	e.runAt(contracts.RunRunning, e.clock.Now())
	e.drain()
	if g := e.byGroup("waiting:" + waitSID); g[0].State != contracts.AttnWithdrawn || g[0].Summary != "The session started its next turn." {
		t.Fatalf("next turn: %+v", g)
	}
	if _, f := e.flag(waitSID); f != nil {
		t.Fatalf("flag after next turn: %+v", f)
	}
	// A later turn asks something new: a new item, its own conversation
	// (owner report 2026-10-19: one item reopened for weeks showed the
	// session's whole history as one thread); the old one stays withdrawn.
	e.clock.Advance(time.Second)
	again := e.raiseWaiting("Ship it now?")
	if again.AttentionID == it.AttentionID || again.Count != 1 || again.State != contracts.AttnOpen || !again.CreatedAt.After(it.CreatedAt) {
		t.Fatalf("new question: %+v", again)
	}
	if old, err := e.store.GetAttention(ownerCtx(), contracts.GetRequest{ID: it.AttentionID}); err != nil || old.State != contracts.AttnWithdrawn {
		t.Fatalf("old item: %+v %v", old, err)
	}
	// The session files an ask: withdrawn.
	e.log.AppendPayload(contracts.EvAskChanged, contracts.ObjectRef{Kind: contracts.KindAsk, ID: "ask_synth-w"}, waitSID, nil,
		contracts.Ask{AskID: "ask_synth-w", SessionID: waitSID, Title: "Ship?", State: contracts.AskOpen, Count: 1})
	e.drain()
	if g := e.byGroup("waiting:" + waitSID); g[0].State != contracts.AttnWithdrawn || g[0].Summary != "The session filed an ask." {
		t.Fatalf("ask: %+v", g)
	}
	// The owner typing into the session handles it.
	e.raiseWaiting("Ship it today?")
	if n, err := e.store.OwnerEngaged(context.Background(), waitSID, "You typed into the session.", EngageKinds...); err != nil || n != 1 {
		t.Fatalf("engage: %d %v", n, err)
	}
	if g := e.byGroup("waiting:" + waitSID); g[0].State != contracts.AttnHandled {
		t.Fatalf("engaged: %+v", g)
	}
	// Reading its message, the owner messaging it, archive and run end.
	for _, resolve := range []func(){
		func() { _, _ = e.store.OwnerEngaged(context.Background(), waitSID, "Read.", ReadKinds...) },
		func() { e.ownerToSession("msg_synth-o", waitSID, time.Time{}); e.drain() },
		func() { e.archived(waitSID); e.drain() },
		func() { e.runEnded(waitSID, contracts.RunExited, time.Time{}); e.drain() },
	} {
		e.clock.Advance(time.Second)
		e.raiseWaiting("Ship it later?")
		resolve()
		for _, g := range e.byGroup("waiting:" + waitSID) {
			if g.State != contracts.AttnHandled && g.State != contracts.AttnWithdrawn {
				t.Fatalf("not resolved: %+v", g)
			}
		}
	}
	// Explicit stale-raise withdrawal is idempotent.
	if ok, err := e.store.WithdrawWaiting(context.Background(), waitSID, "x"); err != nil || ok {
		t.Fatalf("withdraw with nothing active: %v %v", ok, err)
	}
}

// A turn that repeats the same question neither counts nor reopens: an
// active item stays, a handled one stays handled, and one withdrawn when
// the session moved on comes back open (no push, no count).
func TestWaitingRepeatLeavesItAsIs(t *testing.T) {
	e := newEnv(t)
	if _, ok, err := e.store.RepeatWaiting(context.Background(), waitSID, ""); err != nil || ok {
		t.Fatalf("no item: %v %v", ok, err)
	}
	it := e.raiseWaiting("Still waiting on your yes to publish the synthetic post.")
	since := it.AttentionID // the hub remembers the item it raised
	same, ok, err := e.store.RepeatWaiting(context.Background(), waitSID, since)
	if err != nil || !ok || same.Count != 1 || same.State != contracts.AttnOpen || !same.UpdatedAt.Equal(it.UpdatedAt) {
		t.Fatalf("active repeat: %v %v %+v", ok, err, same)
	}
	// The next turn starts (withdrawn); it ends with the same request.
	e.clock.Advance(time.Second)
	e.runAt(contracts.RunRunning, e.clock.Now())
	e.drain()
	e.clock.Advance(time.Second)
	back, ok, err := e.store.RepeatWaiting(context.Background(), waitSID, since)
	if err != nil || !ok || back.AttentionID != it.AttentionID || back.State != contracts.AttnOpen || back.Count != 1 {
		t.Fatalf("restored: %v %v %+v", ok, err, back)
	}
	if _, f := e.flag(waitSID); f == nil || f.Reason != "waiting" {
		t.Fatalf("flag = %+v", f)
	}
	// The owner dismisses it: a repeat leaves it dismissed.
	if _, err := e.store.UpdateAttention(ownerCtx(), contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-dismiss"},
		AttentionID: it.AttentionID, State: contracts.AttnHandled}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Second)
	if got, ok, _ := e.store.RepeatWaiting(context.Background(), waitSID, since); !ok || got.State != contracts.AttnHandled {
		t.Fatalf("dismissed repeat: %+v", got)
	}
	if _, f := e.flag(waitSID); f != nil && f.Reason == "waiting" {
		t.Fatalf("dismissed item flags: %+v", f)
	}
}

// Acknowledging an older unthreaded message leaves the sender's item open
// when it stands for a newer one (PR #25 review: a phone view of an old
// message cleared a newer question).
func TestOlderMessageAckLeavesNewerItem(t *testing.T) {
	e := newEnv(t)
	msg := func(id string, ack contracts.AckState) contracts.MessageView {
		return contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: id, Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: waitSID},
			Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "Should I ship " + id + "?"}},
			Status: contracts.DeliveryStatus{Ack: ack}}
	}
	emit := func(kind contracts.EventKind, v contracts.MessageView) {
		b, _ := json.Marshal(v)
		e.log.Append(contracts.Event{Origin: "hub", Kind: kind, Object: contracts.ObjectRef{Kind: contracts.KindMessage, ID: v.Envelope.MessageID}, Payload: b, OccurredAt: e.clock.Now()})
		e.drain()
	}
	emit(contracts.EvMessageStored, msg("msg_synth-old", contracts.AckNone))
	e.clock.Advance(time.Second)
	emit(contracts.EvMessageStored, msg("msg_synth-new", contracts.AckNone))
	group := "message:from:session:" + waitSID
	emit(contracts.EvMessageAck, msg("msg_synth-old", contracts.AckAcknowledged))
	if g := e.byGroup(group); len(g) != 1 || g[0].State != contracts.AttnOpen {
		t.Fatalf("older ack handled the item: %+v", g)
	}
	emit(contracts.EvMessageAck, msg("msg_synth-new", contracts.AckAcknowledged))
	if g := e.byGroup(group); g[0].State != contracts.AttnHandled {
		t.Fatalf("newest ack: %+v", g)
	}
}

// A repeat acts on the item raised for the question. When the hub has no
// item for it (a question recorded before items were linked) and the
// session's latest item is withdrawn, that item may have stood for earlier
// questions too (reopened in place before 2026-10-19: the live item had 56
// reopenings since Oct 15), so it is not brought back with that history;
// the hub raises a new item. An item other than the latest is never
// restored; a handled one stays handled.
func TestWaitingRepeatOfAnOlderItem(t *testing.T) {
	e := newEnv(t)
	old := e.raiseWaiting("Should I create the synthetic sub-issues?")
	e.clock.Advance(time.Second)
	e.runAt(contracts.RunRunning, e.clock.Now())
	e.drain()
	e.clock.Advance(time.Second)
	if got, ok, err := e.store.RepeatWaiting(context.Background(), waitSID, ""); err != nil || ok {
		t.Fatalf("unlinked withdrawn item came back: %v %v %+v", ok, err, got)
	}
	fresh := e.raiseWaiting("Should I create the synthetic sub-issues?")
	if fresh.AttentionID == old.AttentionID || fresh.Count != 1 {
		t.Fatalf("raise after the repeat: %+v", fresh)
	}
	// The owner dismisses the old item later (its updated_at moves past
	// the fresh one's): a repeat still judges the fresh item.
	e.clock.Advance(time.Second)
	e.runAt(contracts.RunRunning, e.clock.Now())
	e.drain()
	e.clock.Advance(time.Second)
	if _, err := e.store.UpdateAttention(ownerCtx(), contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-dismiss-old"},
		AttentionID: old.AttentionID, State: contracts.AttnHandled}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Second)
	back, ok, err := e.store.RepeatWaiting(context.Background(), waitSID, fresh.AttentionID)
	if err != nil || !ok || back.AttentionID != fresh.AttentionID || back.State != contracts.AttnOpen {
		t.Fatalf("linked withdrawn item: %v %v %+v", ok, err, back)
	}
	// A repeat naming an item that is no longer the latest finds nothing.
	if _, ok, _ := e.store.RepeatWaiting(context.Background(), waitSID, old.AttentionID); ok {
		t.Fatal("a repeat restored an older item")
	}
	// Unlinked, an active or handled latest item is left as it is.
	if got, ok, _ := e.store.RepeatWaiting(context.Background(), waitSID, ""); !ok || got.AttentionID != fresh.AttentionID || got.Count != 1 {
		t.Fatalf("unlinked active item: %v %+v", ok, got)
	}
}
