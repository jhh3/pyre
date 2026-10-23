package attention

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/notifications"
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

// W-grants: an ask a standing grant answered is created decided; it never
// raises an ask item. The owner gets one informational "grant_fyi" item
// per grant: raised when it is created, collecting each use, handled when
// it is revoked, and never pushed by default.
func TestDeriveGrantFYI(t *testing.T) {
	e := newEnv(t)
	pid := "prj_synth-1"
	g := contracts.StandingGrant{GrantID: "sgr_synth-1", Scope: contracts.GrantScopeProject, ProjectID: &pid, Action: "merge", Option: "a",
		ExpiresAt: t0.Add(7 * 24 * time.Hour), CreatedAt: t0, RecordedBy: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "owner"}}
	grantEv := func(g contracts.StandingGrant) {
		e.log.AppendPayload(contracts.EvStandingGrantChanged, contracts.ObjectRef{Kind: contracts.KindStandingGrant, ID: g.GrantID}, "", g.ProjectID, g)
	}
	grantEv(g)
	e.drain()
	fyi := e.byGroup("grant:sgr_synth-1")
	if len(fyi) != 1 || fyi[0].Kind != contracts.AttnGrantFYI || fyi[0].Severity != "info" || fyi[0].State != contracts.AttnOpen ||
		fyi[0].Title != "Standing go-ahead: merge → a" || fyi[0].SessionID != "" || fyi[0].Count != 1 {
		t.Fatalf("created: %+v", fyi)
	}
	if st, _ := notifications.Evaluate(contracts.NotificationPrefs{}, fyi[0], t0); st != notifications.StateDisabled {
		t.Fatalf("an FYI pushes by default: %q", st)
	}
	for i, id := range []string{"ask_synth-g1", "ask_synth-g2", "ask_synth-g3"} {
		a := contracts.Ask{AskID: id, TopicKey: "merge-pr-" + id, SessionID: "ses_synth-a", ProjectID: &pid, Title: "Merge synthetic PR " + id + "?", Action: "merge",
			Options: []contracts.AskOption{{ID: "a", Label: "Merge"}, {ID: "b", Label: "Wait"}}, State: contracts.AskAnswered, Count: 1, GrantID: g.GrantID,
			Answer: &contracts.AskAnswer{OptionID: "a", By: contracts.Principal{Kind: contracts.PrincipalSystem, ID: "grants"}, At: t0}}
		e.log.AppendPayload(contracts.EvAskChanged, contracts.ObjectRef{Kind: contracts.KindAsk, ID: a.AskID}, a.SessionID, nil, a)
		e.drain()
		if g := e.byGroup("ask:" + id); len(g) != 0 {
			t.Fatalf("granted ask %d raised an ask item: %+v", i, g)
		}
	}
	fyi = e.byGroup("grant:sgr_synth-1")
	if len(fyi) != 1 || fyi[0].Count != 4 || fyi[0].Title != "Standing go-ahead used: merge" || fyi[0].Summary != `Answered "Merge synthetic PR ask_synth-g3?"` {
		t.Fatalf("uses collapse into one item: %+v", fyi)
	}
	now := t0.Add(time.Hour)
	g.RevokedAt = &now
	grantEv(g)
	e.drain()
	if fyi = e.byGroup("grant:sgr_synth-1"); len(fyi) != 1 || fyi[0].State != contracts.AttnHandled {
		t.Fatalf("revoked: %+v", fyi)
	}
}
