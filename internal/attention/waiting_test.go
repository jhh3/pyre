package attention

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// "Waiting on you" (owner request 2026-10-14): one item per top-level
// session, raised by the hub at turn end, resolved by the session moving on
// or the owner engaging.

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
	// A repeat updates the one item (and reopens it).
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
	// A later turn asks again: the same item reopens (one per session).
	e.clock.Advance(time.Second)
	again := e.raiseWaiting("Ship it now?")
	if again.AttentionID != it.AttentionID || again.Count != 2 || again.State != contracts.AttnOpen {
		t.Fatalf("reopen: %+v", again)
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
		if g := e.byGroup("waiting:" + waitSID); len(g) != 1 || (g[0].State != contracts.AttnHandled && g[0].State != contracts.AttnWithdrawn) {
			t.Fatalf("not resolved: %+v", g)
		}
	}
	// Explicit stale-raise withdrawal is idempotent.
	if ok, err := e.store.WithdrawWaiting(context.Background(), waitSID, "x"); err != nil || ok {
		t.Fatalf("withdraw with nothing active: %v %v", ok, err)
	}
}
