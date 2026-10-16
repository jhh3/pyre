package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- held (batched) answers and ask context quality (W-web-inbox) ----

func indexAsk(op string) contracts.CreateAskRequest {
	return contracts.CreateAskRequest{Mutation: contracts.Mutation{OperationID: op}, TopicKey: "synthetic-index",
		Title: "Add the synthetic index?", Context: "Synthetic: queries on the synthetic table are slow." + synthPad,
		Options: []contracts.AskOption{{ID: "add", Label: "Add the index", Detail: "slower writes"}, {ID: "skip", Label: "Skip it", Detail: "queries stay slow"}}}
}

func (x *harness) hold(op, askID, option, text string) (contracts.HeldAnswers, error) {
	return x.h.HoldAnswer(as(owner), contracts.HoldAnswerRequest{Mutation: contracts.Mutation{OperationID: op},
		AnswerAskItem: contracts.AnswerAskItem{AskID: askID, OptionID: option, Text: text}})
}

func (x *harness) heldList() contracts.HeldAnswers {
	x.t.Helper()
	h, err := x.h.ListHeldAnswers(as(owner), struct{}{})
	if err != nil {
		x.t.Fatal(err)
	}
	return h
}

func (x *harness) maintain() {
	x.t.Helper()
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		x.t.Fatal(err)
	}
}

func (x *harness) askState(id string) contracts.Ask {
	x.t.Helper()
	a, err := x.h.GetAsk(as(owner), contracts.GetRequest{ID: id})
	if err != nil {
		x.t.Fatal(err)
	}
	return a
}

func (x *harness) heldEvents() int {
	x.t.Helper()
	page, err := x.h.ListEvents(as(owner), contracts.ListEventsRequest{Kinds: []contracts.EventKind{contracts.EvAskHeldChanged}, Limit: 500})
	if err != nil {
		x.t.Fatal(err)
	}
	return len(page.Events)
}

func TestAskHoldQuietWindowBatchesOneMessage(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	a1 := f.ask(sesPrincipal(pid), synthAsk("op_synth-h1", "synthetic-deploy", "Ship the synthetic build?", "b")).Ask
	a2 := f.ask(sesPrincipal(pid), indexAsk("op_synth-h2")).Ask

	// Validation matches AnswerAsks; only the owner holds.
	if _, err := x.hold("op_synth-hx1", a1.AskID, "zzz", ""); !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), `"zzz" is not an option`) {
		t.Fatalf("bad option: %v", err)
	}
	if _, err := x.hold("op_synth-hx2", a1.AskID, "", " "); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("empty answer: %v", err)
	}
	if _, err := x.h.HoldAnswer(as(sesPrincipal(pid)), contracts.HoldAnswerRequest{Mutation: contracts.Mutation{OperationID: "op_synth-hx3"},
		AnswerAskItem: contracts.AnswerAskItem{AskID: a1.AskID, OptionID: "a"}}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("session hold: %v", err)
	}
	if _, err := x.h.ListHeldAnswers(as(sesPrincipal(pid)), struct{}{}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("session list: %v", err)
	}

	t0 := x.clock.Now()
	h, err := x.hold("op_synth-h3", a1.AskID, "a", "")
	if err != nil || len(h.Items) != 1 || h.SendsAt == nil || !h.SendsAt.Equal(t0.Add(contracts.AskBatchQuiet)) {
		t.Fatalf("hold: %+v %v", h, err)
	}
	x.clock.Add(3 * time.Minute)
	if h, err = x.hold("op_synth-h4", a2.AskID, "", "synthetic: add it after the release"); err != nil || len(h.Items) != 2 {
		t.Fatalf("second hold: %+v %v", h, err)
	}
	// Re-holding replaces the answer and restarts the window.
	x.clock.Add(3 * time.Minute)
	if h, err = x.hold("op_synth-h5", a1.AskID, "b", "synthetic: after the soak"); err != nil || len(h.Items) != 2 {
		t.Fatalf("replace: %+v %v", h, err)
	}
	if want := x.clock.Now().Add(contracts.AskBatchQuiet); !h.SendsAt.Equal(want) {
		t.Fatalf("sends_at %s, want %s", h.SendsAt, want)
	}
	// 6 minutes after the first hold, but only 0 after the newest: nothing sent.
	x.maintain()
	if x.askState(a1.AskID).State != contracts.AskOpen || x.askState(a2.AskID).State != contracts.AskOpen {
		t.Fatal("sent before the quiet window passed")
	}
	x.clock.Add(contracts.AskBatchQuiet - time.Second)
	x.maintain()
	if x.askState(a1.AskID).State != contracts.AskOpen {
		t.Fatal("sent a second early")
	}
	x.clock.Add(time.Second)
	x.maintain()
	g1, g2 := x.askState(a1.AskID), x.askState(a2.AskID)
	if g1.State != contracts.AskAnswered || g1.Answer.OptionID != "b" || g1.Answer.Text != "synthetic: after the soak" || g1.Answer.By != owner || g1.Answer.ByDefault {
		t.Fatalf("a1: %+v", g1)
	}
	if g2.State != contracts.AskAnswered || g2.Answer.Text != "synthetic: add it after the release" {
		t.Fatalf("a2: %+v", g2)
	}
	// One combined decision message to the asking session.
	dec := x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='owner'`, pid)
	// (It replies to the first held ask's message: a2's, as a1 was re-held later.)
	if len(dec) != 1 || dec[0].InReplyTo != a2.MessageID || g1.DecisionMessageID != dec[0].MessageID || g2.DecisionMessageID != dec[0].MessageID {
		t.Fatalf("decision messages: %+v (a1 %s a2 %s)", dec, g1.DecisionMessageID, g2.DecisionMessageID)
	}
	if h := x.heldList(); len(h.Items) != 0 || h.SendsAt != nil {
		t.Fatalf("queue after send: %+v", h)
	}
	if n := x.heldEvents(); n < 4 {
		t.Fatalf("ask.held_changed events: %d", n)
	}
	// Nothing left to send; a later pass is a no-op.
	x.clock.Add(time.Hour)
	x.maintain()
	if n := len(x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='owner'`, pid)); n != 1 {
		t.Fatalf("resent: %d", n)
	}
	// Holding a decided ask is a conflict; unholding a sent one is not found.
	if _, err := x.hold("op_synth-h6", a1.AskID, "a", ""); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("hold decided: %v", err)
	}
	if _, err := x.h.UnholdAnswer(as(owner), contracts.UnholdAnswerRequest{Mutation: contracts.Mutation{OperationID: "op_synth-h7"}, AskID: a1.AskID}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("unhold sent: %v", err)
	}
}

func TestAskHoldFlushUnholdAndDecidedElsewhere(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	a1 := f.ask(sesPrincipal(pid), synthAsk("op_synth-f1", "synthetic-deploy", "Ship the synthetic build?", "b")).Ask
	a2 := f.ask(sesPrincipal(pid), indexAsk("op_synth-f2")).Ask

	// Decided another way while held: the held answer is dropped.
	if _, err := x.hold("op_synth-f3", a1.AskID, "a", ""); err != nil {
		t.Fatal(err)
	}
	res, err := x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: "op_synth-f4"},
		Answers: []contracts.AnswerAskItem{{AskID: a1.AskID, OptionID: "b"}}})
	if err != nil || res.Results[0].Error != nil {
		t.Fatalf("answer: %+v %v", res, err)
	}
	if h := x.heldList(); len(h.Items) != 0 {
		t.Fatalf("held after answer: %+v", h)
	}
	// Withdrawn while held: dropped too.
	if _, err := x.hold("op_synth-f5", a2.AskID, "add", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.WithdrawAsk(as(sesPrincipal(pid)), contracts.WithdrawAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-f6"}, AskID: a2.AskID}); err != nil {
		t.Fatal(err)
	}
	if h := x.heldList(); len(h.Items) != 0 {
		t.Fatalf("held after withdraw: %+v", h)
	}

	// Unhold takes one back; flush sends the rest now in one message.
	b1 := f.ask(sesPrincipal(pid), synthAsk("op_synth-f7", "synthetic-deploy-2", "Ship the second synthetic build?", "b")).Ask
	b2 := f.ask(sesPrincipal(pid), func() contracts.CreateAskRequest {
		r := indexAsk("op_synth-f8")
		r.TopicKey, r.Title = "synthetic-index-2", "Add the second synthetic index?"
		return r
	}()).Ask
	b3 := f.ask(sesPrincipal(pid), func() contracts.CreateAskRequest {
		r := indexAsk("op_synth-f9")
		r.TopicKey, r.Title, r.ConfirmNew = "synthetic-index-3", "Add the third synthetic index?", true
		return r
	}()).Ask
	for i, id := range []string{b1.AskID, b2.AskID, b3.AskID} {
		opt := "a"
		if i > 0 {
			opt = "skip"
		}
		if _, err := x.hold("op_synth-fh"+string(rune('a'+i)), id, opt, ""); err != nil {
			t.Fatal(err)
		}
	}
	h, err := x.h.UnholdAnswer(as(owner), contracts.UnholdAnswerRequest{Mutation: contracts.Mutation{OperationID: "op_synth-f10"}, AskID: b3.AskID})
	if err != nil || len(h.Items) != 2 {
		t.Fatalf("unhold: %+v %v", h, err)
	}
	before := len(x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='owner'`, pid))
	fr, err := x.h.FlushHeldAnswers(as(owner), contracts.FlushHeldAnswersRequest{Mutation: contracts.Mutation{OperationID: "op_synth-f11"}})
	if err != nil || len(fr.Results) != 2 || fr.Results[0].Error != nil || fr.Results[1].Error != nil {
		t.Fatalf("flush: %+v %v", fr, err)
	}
	if after := len(x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='owner'`, pid)); after != before+1 {
		t.Fatalf("flush messages: %d -> %d", before, after)
	}
	if x.askState(b1.AskID).State != contracts.AskAnswered || x.askState(b2.AskID).State != contracts.AskAnswered || x.askState(b3.AskID).State != contracts.AskOpen {
		t.Fatal("flush outcome")
	}
	if h := x.heldList(); len(h.Items) != 0 {
		t.Fatalf("held after flush: %+v", h)
	}
}

func TestAskHoldWinsOverDeadlineDefault(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	req := synthAsk("op_synth-d1", "synthetic-deploy", "Ship the synthetic build?", "b")
	req.Urgent, req.DeadlineSeconds = true, 120 // keeps the deadline at night
	a := f.ask(sesPrincipal(pid), req).Ask
	if _, err := x.hold("op_synth-d2", a.AskID, "a", ""); err != nil {
		t.Fatal(err)
	}
	// Past the deadline, before the quiet window: the held answer is sent
	// instead of the default.
	x.clock.Add(3 * time.Minute)
	x.maintain()
	g := x.askState(a.AskID)
	if g.State != contracts.AskAnswered || g.Answer.OptionID != "a" || g.Answer.ByDefault || g.Answer.By != owner {
		t.Fatalf("deadline with a held answer: %+v", g)
	}
	if h := x.heldList(); len(h.Items) != 0 {
		t.Fatalf("held after deadline flush: %+v", h)
	}
}

func TestAskContextQuality(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	short := synthAsk("op_synth-q1", "synthetic-deploy", "Ship the synthetic build?", "b")
	short.Context = "Synthetic: ship it?"
	_, err := x.h.CreateAsk(as(sesPrincipal(pid)), short)
	if !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("short context: %v", err)
	}
	for _, want := range []string{"at least 300", "what is being decided", "what each option leads to", "by default"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("guidance lacks %q: %v", want, err)
		}
	}
	noDetail := synthAsk("op_synth-q2", "synthetic-deploy", "Ship the synthetic build?", "b")
	noDetail.Options[0].Detail = " "
	if _, err := x.h.CreateAsk(as(sesPrincipal(pid)), noDetail); !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), `option "a" has no detail`) {
		t.Fatalf("missing detail: %v", err)
	}
	// Markdown up to AskMaxContext is accepted.
	long := synthAsk("op_synth-q3", "synthetic-deploy", "Ship the synthetic build?", "b")
	long.Context = "## Synthetic\n\n" + strings.Repeat("- synthetic finding\n", 700)
	r, err := x.h.CreateAsk(as(sesPrincipal(pid)), long)
	if err != nil || r.Status != contracts.AskCreated {
		t.Fatalf("long markdown context: %+v %v", r, err)
	}
	// A follow-up on the open topic merges and may be short.
	follow := synthAsk("op_synth-q4", "synthetic-deploy", "Ship the synthetic build?", "b")
	follow.Context = "Synthetic: soak is green."
	if m := f.ask(sesPrincipal(pid), follow); m.Status != contracts.AskMerged {
		t.Fatalf("short follow-up: %+v", m)
	}
}
