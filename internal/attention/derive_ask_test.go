package attention

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// W-asks: one "ask" item per ask; a merged repeat is not new attention;
// the message carrying the ask raises no item of its own.
func TestDeriveAskLifecycle(t *testing.T) {
	e := newEnv(t)
	dl := t0.Add(time.Hour)
	ask := contracts.Ask{AskID: "ask_synth-1", TopicKey: "synthetic-deploy", SessionID: "ses_synth-a", Title: "Ship the synthetic build?",
		Options: []contracts.AskOption{{ID: "a", Label: "Ship"}, {ID: "b", Label: "Wait"}}, DefaultOption: "b", Urgent: true,
		DeadlineAt: &dl, State: contracts.AskOpen, Count: 1}
	emit := func(a contracts.Ask) {
		e.log.AppendPayload(contracts.EvAskChanged, contracts.ObjectRef{Kind: contracts.KindAsk, ID: a.AskID}, a.SessionID, nil, a)
	}
	emit(ask)
	e.ownerMessage("msg_synth-ask", ask.AskID, "ses_synth-a")
	e.drain()
	items := e.list()
	if len(items) != 1 || items[0].Kind != contracts.AttnAsk || items[0].GroupKey != "ask:ask_synth-1" || items[0].Severity != "urgent" ||
		items[0].Object.Kind != contracts.KindAsk || items[0].SessionID != "ses_synth-a" || items[0].Title != "Ask: Ship the synthetic build?" {
		t.Fatalf("items: %+v", items)
	}
	// The owner reads it; a merged repeat neither reopens nor re-notifies it.
	if _, err := e.store.UpdateAttention(ownerCtx(), contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-r"},
		AttentionID: items[0].AttentionID, State: contracts.AttnRead}); err != nil {
		t.Fatal(err)
	}
	jobs := e.jobs()
	merged := ask
	merged.Count = 2
	emit(merged)
	e.drain()
	if g := e.byGroup("ask:ask_synth-1"); len(g) != 1 || g[0].State != contracts.AttnRead || e.jobs() != jobs {
		t.Fatalf("after merge: %+v jobs %d->%d", g, jobs, e.jobs())
	}
	decided := ask
	decided.State = contracts.AskDefaulted
	emit(decided)
	e.drain()
	if g := e.byGroup("ask:ask_synth-1"); len(g) != 1 || g[0].State != contracts.AttnHandled {
		t.Fatalf("after decision: %+v", g)
	}

	// Withdrawn before the deriver saw it raised: a tombstone, never an open item.
	w := ask
	w.AskID, w.State = "ask_synth-2", contracts.AskWithdrawn
	emit(w)
	w.State = contracts.AskOpen
	emit(w)
	e.drain()
	if g := e.byGroup("ask:ask_synth-2"); len(g) != 1 || g[0].State != contracts.AttnWithdrawn {
		t.Fatalf("withdrawn: %+v", g)
	}
}
