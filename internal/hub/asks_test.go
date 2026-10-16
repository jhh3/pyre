package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- asks (W-asks) ----

// synthPad brings a synthetic ask's context past contracts.AskMinContext.
const synthPad = "\n\nSynthetic background for the owner: this decision is needed now because the synthetic work is blocked on it. " +
	"Already known: the synthetic checks pass and nothing was changed outside the synthetic sandbox. Each option is described below; " +
	"if nobody answers, the session takes the default and keeps working on the safe path."

func synthAsk(op, topic, title string, def string) contracts.CreateAskRequest {
	return contracts.CreateAskRequest{
		Mutation: contracts.Mutation{OperationID: op}, TopicKey: topic, Title: title,
		Context: "Synthetic context: the synthetic deploy is ready; choosing decides what the session does next." + synthPad,
		Options: []contracts.AskOption{
			{ID: "a", Label: "Ship the synthetic build today", Detail: "release now"},
			{ID: "b", Label: "Wait for the synthetic soak", Detail: "one more day"},
		},
		DefaultOption: def,
	}
}

func (f *childFixture) ask(p contracts.Principal, req contracts.CreateAskRequest) contracts.CreateAskResponse {
	f.x.t.Helper()
	resp, err := f.x.h.CreateAsk(as(p), req)
	if err != nil {
		f.x.t.Fatalf("create ask %s: %v", req.OperationID, err)
	}
	return resp
}

// envelopes lists stored messages matching where, oldest first.
func (x *harness) envelopes(where string, args ...any) []contracts.MessageEnvelope {
	x.t.Helper()
	rows, err := x.db.Query(`SELECT envelope FROM messages WHERE `+where+` ORDER BY stored_at, inbox_cursor`, args...)
	if err != nil {
		x.t.Fatal(err)
	}
	defer rows.Close()
	var out []contracts.MessageEnvelope
	for rows.Next() {
		var body string
		rows.Scan(&body)
		var e contracts.MessageEnvelope
		json.Unmarshal([]byte(body), &e)
		out = append(out, e)
	}
	return out
}

func (x *harness) askEvents(id string) []contracts.Ask {
	x.t.Helper()
	page, err := x.h.ListEvents(as(owner), contracts.ListEventsRequest{Kinds: []contracts.EventKind{contracts.EvAskChanged}, Limit: 500})
	if err != nil {
		x.t.Fatal(err)
	}
	var out []contracts.Ask
	for _, ev := range page.Events {
		var a contracts.Ask
		json.Unmarshal(ev.Payload, &a)
		if a.AskID == id {
			out = append(out, a)
		}
	}
	return out
}

func TestAskCreateMergeDecidedAndDuplicate(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	parentP := sesPrincipal(pid)

	r := f.ask(parentP, synthAsk("op_synth-ask1", "Synthetic Deploy Window!", "Ship the synthetic build?", "b"))
	if r.Status != contracts.AskCreated || r.Ask == nil || r.Ask.TopicKey != "synthetic-deploy-window" || r.Ask.SessionID != pid || r.Ask.ThreadID != r.Ask.AskID {
		t.Fatalf("create: %+v", r)
	}
	a := *r.Ask
	owner1 := x.envelopes(`recipient_kind='owner'`)
	if len(owner1) != 1 || owner1[0].MessageID != a.MessageID || owner1[0].ThreadID != a.AskID || owner1[0].Sender.ID != pid {
		t.Fatalf("owner message: %+v", owner1)
	}
	for _, want := range []string{"Ship the synthetic build?", "Synthetic context", "1. [a] Ship the synthetic build today", "2. [b] Wait for the synthetic soak — one more day", "Default: b — Wait", " PT"} {
		if !strings.Contains(owner1[0].Body.Text, want) {
			t.Fatalf("owner message lacks %q:\n%s", want, owner1[0].Body.Text)
		}
	}
	if ev := x.askEvents(a.AskID); len(ev) != 1 || ev[0].State != contracts.AskOpen {
		t.Fatalf("ask.changed events: %+v", ev)
	}

	// Same topic again: merged, no second owner message.
	again := synthAsk("op_synth-ask2", "synthetic-deploy-window", "Ship the synthetic build? (again)", "b")
	again.Context = "Synthetic follow-up: the soak finished green."
	m := f.ask(parentP, again)
	if m.Status != contracts.AskMerged || m.Ask.AskID != a.AskID || m.Ask.Count != 2 || len(m.Ask.Updates) != 1 || m.Ask.Updates[0].Text != again.Context {
		t.Fatalf("merge: %+v", m)
	}
	if n := len(x.envelopes(`recipient_kind='owner'`)); n != 1 {
		t.Fatalf("merge sent owner mail: %d messages", n)
	}
	// Idempotent retry of the create returns the stored result.
	if rr := f.ask(parentP, synthAsk("op_synth-ask1", "Synthetic Deploy Window!", "Ship the synthetic build?", "b")); rr.Status != contracts.AskCreated || rr.Ask.AskID != a.AskID {
		t.Fatalf("retry: %+v", rr)
	}

	// A near-identical ask under another topic is offered as a duplicate.
	dup := f.ask(parentP, synthAsk("op_synth-ask3", "ship-today", "Ship the synthetic build now?", "a"))
	if dup.Status != contracts.AskPossibleDuplicate || len(dup.Candidates) != 1 || dup.Candidates[0].AskID != a.AskID {
		t.Fatalf("duplicate: %+v", dup)
	}
	conf := synthAsk("op_synth-ask4", "ship-today", "Ship the synthetic build now?", "a")
	conf.ConfirmNew = true
	if c := f.ask(parentP, conf); c.Status != contracts.AskCreated {
		t.Fatalf("confirm_new: %+v", c)
	}
	// An unrelated ask is not a duplicate.
	other := contracts.CreateAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-ask5"}, TopicKey: "synthetic-db-index",
		Title: "Add the synthetic index?", Context: "Synthetic: queries on the synthetic table are slow." + synthPad,
		Options: []contracts.AskOption{{ID: "add", Label: "Add the index", Detail: "slower writes"}, {ID: "skip", Label: "Skip it", Detail: "queries stay slow"}}}
	if c := f.ask(parentP, other); c.Status != contracts.AskCreated {
		t.Fatalf("unrelated: %+v", c)
	}

	// Answer; then the same topic returns the decision.
	res, err := x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: "op_synth-ans1"},
		Answers: []contracts.AnswerAskItem{{AskID: a.AskID, OptionID: "a", Text: "synthetic go-ahead"}}})
	if err != nil || len(res.Results) != 1 || res.Results[0].Error != nil || res.Results[0].Ask.State != contracts.AskAnswered {
		t.Fatalf("answer: %+v %v", res, err)
	}
	dec := x.envelopes(`recipient_kind='session' AND recipient_id=? AND in_reply_to=?`, pid, a.MessageID)
	if len(dec) != 1 || !strings.Contains(dec[0].Body.Text, "[pyre ask decided] Ship the synthetic build?: owner chose a — Ship the synthetic build today (synthetic go-ahead)") {
		t.Fatalf("decision message: %+v", dec)
	}
	if res.Results[0].Ask.DecisionMessageID != dec[0].MessageID {
		t.Fatalf("decision message id %q != %q", res.Results[0].Ask.DecisionMessageID, dec[0].MessageID)
	}
	d := f.ask(parentP, synthAsk("op_synth-ask6", "synthetic-deploy-window", "Ship the synthetic build?", "b"))
	if d.Status != contracts.AskAlreadyDecided || d.Ask.AskID != a.AskID || d.Ask.Answer == nil || d.Ask.Answer.OptionID != "a" {
		t.Fatalf("decided: %+v", d)
	}
	reopen := synthAsk("op_synth-ask7", "synthetic-deploy-window", "Ship the synthetic build?", "b")
	reopen.Reopen = true
	reopen.ConfirmNew = true // "ship-today" is still open and similar
	if ro := f.ask(parentP, reopen); ro.Status != contracts.AskCreated || ro.Ask.AskID == a.AskID {
		t.Fatalf("reopen: %+v", ro)
	}

	// Validation.
	bad := synthAsk("op_synth-bad1", "x", "t", "zz")
	if _, err := x.h.CreateAsk(as(parentP), bad); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad default: %v", err)
	}
	bad = synthAsk("op_synth-bad2", "x", "t", "")
	bad.Context = " "
	if _, err := x.h.CreateAsk(as(parentP), bad); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("no context: %v", err)
	}
	bad = synthAsk("op_synth-bad3", "x", "t", "")
	bad.Options = bad.Options[:1]
	if _, err := x.h.CreateAsk(as(parentP), bad); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("one option: %v", err)
	}

	// List and get.
	page, err := x.h.ListAsks(as(parentP), contracts.ListAsksRequest{States: []contracts.AskState{contracts.AskOpen}})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("list open: %d %v", len(page.Items), err)
	}
	page, err = x.h.ListAsks(as(owner), contracts.ListAsksRequest{Query: "soak"})
	if err != nil || len(page.Items) != 3 { // a, ship-today, the reopened one
		t.Fatalf("list query: %d %v", len(page.Items), err)
	}
	page, err = x.h.ListAsks(as(owner), contracts.ListAsksRequest{PageRequest: contracts.PageRequest{Limit: 2}})
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page 1: %+v %v", page, err)
	}
	page2, err := x.h.ListAsks(as(owner), contracts.ListAsksRequest{PageRequest: contracts.PageRequest{Limit: 10, Cursor: page.NextCursor}})
	if err != nil || len(page2.Items) != 2 || page2.Items[0].AskID == page.Items[1].AskID {
		t.Fatalf("page 2: %+v %v", page2, err)
	}
	g, err := x.h.GetAsk(as(owner), contracts.GetRequest{ID: a.AskID})
	if err != nil || len(g.Updates) != 1 || g.State != contracts.AskAnswered {
		t.Fatalf("get: %+v %v", g, err)
	}
	if _, err := x.h.GetAsk(as(owner), contracts.GetRequest{ID: "ask_synthMissing"}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	// Only the owner answers.
	if _, err := x.h.AnswerAsks(as(parentP), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: "op_synth-ans2"},
		Answers: []contracts.AnswerAskItem{{AskID: a.AskID, OptionID: "a"}}}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("session answering: %v", err)
	}
}

func TestAskFromChildIsRedirectedAndChildCannotMailOwner(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	r := f.ask(sesPrincipal(cid), synthAsk("op_synth-cask", "synthetic-child-topic", "Child question?", "a"))
	if r.Status != contracts.AskRedirected || r.MessageID == "" || r.Ask != nil {
		t.Fatalf("redirect: %+v", r)
	}
	toParent := x.envelopes(`message_id=?`, r.MessageID)
	if len(toParent) != 1 || toParent[0].Recipient.ID != pid || toParent[0].Sender.ID != cid || !strings.Contains(toParent[0].Body.Text, "Child question?") ||
		!strings.Contains(toParent[0].Body.Text, "[b] Wait for the synthetic soak") {
		t.Fatalf("redirected message: %+v", toParent)
	}
	if f.ownerInbox() != 0 || x.count("asks") != 0 {
		t.Fatalf("child ask reached the owner: inbox %d asks %d", f.ownerInbox(), x.count("asks"))
	}

	// Sender rule: a child may not message the owner; its parent may.
	_, err := x.h.SendMessage(as(sesPrincipal(cid)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-cmail"},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Body: contracts.MessageBody{Text: "synthetic status"}})
	wantCode(t, err, contracts.ErrForbidden)
	if !strings.Contains(err.Error(), "only top-level sessions message the owner; send this to your parent") || !strings.Contains(err.Error(), pid) {
		t.Fatalf("sender rule message: %v", err)
	}
	if _, err := x.h.SendMessage(as(sesPrincipal(pid)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-pmail"},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Body: contracts.MessageBody{Text: "synthetic status"}}); err != nil {
		t.Fatalf("top-level session to owner: %v", err)
	}
	// The owner and system principals are exempt.
	if _, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-omail"},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Body: contracts.MessageBody{Text: "synthetic note"}}); err != nil {
		t.Fatalf("owner note: %v", err)
	}
}

func TestAskDeadlineRules(t *testing.T) {
	pt, _ := time.LoadLocation("America/Los_Angeles")
	if pt == nil {
		t.Skip("no tz database")
	}
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, pt) }
	cases := []struct {
		name   string
		now    time.Time
		secs   int
		urgent bool
		quiet  contracts.NotificationPrefs
		want   time.Time
		rule   string
	}{
		{"afternoon 1h", at(12, 14, 0), 0, false, askDefaultQuiet, at(12, 15, 0), "1h"},
		{"evening lands in quiet hours", at(12, 21, 30), 0, false, askDefaultQuiet, at(13, 8, 0), "morning"},
		{"after midnight", at(13, 1, 0), 0, false, askDefaultQuiet, at(13, 8, 0), "morning"},
		{"early morning ends after quiet", at(13, 7, 30), 0, false, askDefaultQuiet, at(13, 8, 30), "1h"},
		{"urgent keeps its deadline", at(12, 21, 30), 0, true, askDefaultQuiet, at(12, 22, 30), "urgent"},
		{"custom", at(12, 9, 0), 1800, false, askDefaultQuiet, at(12, 9, 30), "custom"},
		{"custom into quiet", at(12, 9, 0), 14 * 3600, false, askDefaultQuiet, at(13, 8, 0), "morning"},
		{"owner quiet hours", at(12, 14, 0), 0, false,
			contracts.NotificationPrefs{QuietHoursStart: "14:30", QuietHoursEnd: "16:00", TZ: "America/Los_Angeles"}, at(12, 16, 0), "morning"},
	}
	for _, c := range cases {
		got, rule := askDeadline(c.now.UTC(), c.secs, c.urgent, c.quiet)
		if !got.Equal(c.want) || rule != c.rule {
			t.Errorf("%s: got %s %s, want %s %s", c.name, got.In(pt), rule, c.want, c.rule)
		}
	}
}

func TestAskDeadlineDefaultsAndExpires(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	parentP := sesPrincipal(pid)
	// The harness clock is 05:00 PT: the 1h deadline moves to 08:00 PT.
	withDef := f.ask(parentP, synthAsk("op_synth-d1", "synthetic-deploy", "Ship the synthetic build?", "b")).Ask
	if withDef.DeadlineRule != "morning" || withDef.DeadlineAt.In(pacific).Hour() != 8 {
		t.Fatalf("morning rule: %s %s", withDef.DeadlineRule, withDef.DeadlineAt.In(pacific))
	}
	noDef := contracts.CreateAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-d2"}, TopicKey: "synthetic-index",
		Title: "Add the synthetic index?", Context: "Synthetic: queries on the synthetic table are slow." + synthPad, DeadlineSeconds: 600,
		Options: []contracts.AskOption{{ID: "add", Label: "Add the index", Detail: "slower writes"}, {ID: "skip", Label: "Skip it", Detail: "queries stay slow"}}}
	expiring := f.ask(parentP, noDef).Ask

	// Nothing is due yet.
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: withDef.AskID}); g.State != contracts.AskOpen {
		t.Fatalf("decided early: %s", g.State)
	}
	x.clock.Add(4 * time.Hour)
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: withDef.AskID})
	e, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: expiring.AskID})
	if d.State != contracts.AskDefaulted || d.Answer == nil || d.Answer.OptionID != "b" || !d.Answer.ByDefault || d.Answer.By.Kind != contracts.PrincipalSystem {
		t.Fatalf("defaulted: %+v", d)
	}
	if e.State != contracts.AskExpired || e.Answer == nil || !e.Answer.ByDefault {
		t.Fatalf("expired: %+v", e)
	}
	// One combined decision message to the asker, replying to the first ask.
	dec := x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='system'`, pid)
	if len(dec) != 1 || dec[0].InReplyTo == "" {
		t.Fatalf("decision messages: %+v", dec)
	}
	for _, want := range []string{"2 of your asks were decided", "default b — Wait for the synthetic soak applied", "use your judgement, prefer the reversible path", " PT"} {
		if !strings.Contains(dec[0].Body.Text, want) {
			t.Fatalf("decision lacks %q:\n%s", want, dec[0].Body.Text)
		}
	}
	if d.DecisionMessageID != dec[0].MessageID || e.DecisionMessageID != dec[0].MessageID {
		t.Fatalf("decision ids: %q %q", d.DecisionMessageID, e.DecisionMessageID)
	}
	// Another pass changes nothing.
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='system'`, pid)); n != 1 {
		t.Fatalf("decision resent: %d", n)
	}
}

func TestAskBatchAnswerOneMessagePerSession(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	parentP := sesPrincipal(pid)
	a1 := f.ask(parentP, synthAsk("op_synth-b1", "synthetic-deploy", "Ship the synthetic build?", "b")).Ask
	a2 := f.ask(parentP, contracts.CreateAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-b2"}, TopicKey: "synthetic-index",
		Title: "Add the synthetic index?", Context: "Synthetic: queries on the synthetic table are slow." + synthPad,
		Options: []contracts.AskOption{{ID: "add", Label: "Add the index", Detail: "slower writes"}, {ID: "skip", Label: "Skip it", Detail: "queries stay slow"}}}).Ask
	res, err := x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: "op_synth-batch"},
		Answers: []contracts.AnswerAskItem{{AskID: a1.AskID, OptionID: "a"}, {AskID: a2.AskID, Text: "synthetic: add it after the release"}, {AskID: a1.AskID, OptionID: "b"}}})
	if err != nil || len(res.Results) != 3 {
		t.Fatalf("batch: %+v %v", res, err)
	}
	if res.Results[0].Error != nil || res.Results[1].Error != nil || res.Results[1].Ask.Answer.Text == "" {
		t.Fatalf("batch results: %+v", res.Results)
	}
	if res.Results[2].Error == nil || res.Results[2].Error.Code != contracts.ErrConflict {
		t.Fatalf("second answer to the same ask: %+v", res.Results[2])
	}
	dec := x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='owner'`, pid)
	if len(dec) != 1 || dec[0].InReplyTo != a1.MessageID || !strings.Contains(dec[0].Body.Text, "2 of your asks were decided") ||
		!strings.Contains(dec[0].Body.Text, "owner answered: synthetic: add it after the release") {
		t.Fatalf("combined decision: %+v", dec)
	}
	// The whole call retried with the same operation ID: same results, no new mail.
	again, err := x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: "op_synth-batch"},
		Answers: []contracts.AnswerAskItem{{AskID: a1.AskID, OptionID: "a"}, {AskID: a2.AskID, Text: "synthetic: add it after the release"}, {AskID: a1.AskID, OptionID: "b"}}})
	if err != nil || again.Results[0].Error != nil || again.Results[0].Ask.State != contracts.AskAnswered {
		t.Fatalf("retry: %+v %v", again, err)
	}
	if n := len(x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind='owner'`, pid)); n != 1 {
		t.Fatalf("retry sent again: %d", n)
	}
}

func TestAskAnswerAndDeadlineRace(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	parentP := sesPrincipal(f.parent.Session.SessionID)
	a1 := f.ask(parentP, synthAsk("op_synth-r1", "synthetic-deploy", "Ship the synthetic build?", "b")).Ask
	a2 := f.ask(parentP, contracts.CreateAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-r2"}, TopicKey: "synthetic-index",
		Title: "Add the synthetic index?", Context: "Synthetic: queries on the synthetic table are slow." + synthPad, DefaultOption: "skip",
		Options: []contracts.AskOption{{ID: "add", Label: "Add the index", Detail: "slower writes"}, {ID: "skip", Label: "Skip it", Detail: "queries stay slow"}}}).Ask
	x.clock.Add(5 * time.Hour) // both past their deadline
	// The owner answers a1 before maintenance runs: the answer wins.
	if res, err := x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: "op_synth-r3"},
		Answers: []contracts.AnswerAskItem{{AskID: a1.AskID, OptionID: "a"}}}); err != nil || res.Results[0].Error != nil {
		t.Fatalf("answer: %+v %v", res, err)
	}
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	g1, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: a1.AskID})
	g2, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: a2.AskID})
	if g1.State != contracts.AskAnswered || g1.Answer.OptionID != "a" || g2.State != contracts.AskDefaulted {
		t.Fatalf("race: %s/%v %s", g1.State, g1.Answer, g2.State)
	}
	// Answering after the default applied is a conflict.
	res, err := x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: "op_synth-r4"},
		Answers: []contracts.AnswerAskItem{{AskID: a2.AskID, OptionID: "add"}}})
	if err != nil || res.Results[0].Error == nil || res.Results[0].Error.Code != contracts.ErrConflict {
		t.Fatalf("late answer: %+v %v", res, err)
	}
	// Exactly one ask.changed per decision.
	if ev := x.askEvents(a1.AskID); len(ev) != 2 || ev[1].State != contracts.AskAnswered {
		t.Fatalf("a1 events: %+v", ev)
	}
}

func TestAskWithdrawAndArchive(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	parentP := sesPrincipal(pid)
	a1 := f.ask(parentP, synthAsk("op_synth-w1", "synthetic-deploy", "Ship the synthetic build?", "b")).Ask
	a2 := f.ask(parentP, contracts.CreateAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-w2"}, TopicKey: "synthetic-index",
		Title: "Add the synthetic index?", Context: "Synthetic: queries on the synthetic table are slow." + synthPad,
		Options: []contracts.AskOption{{ID: "add", Label: "Add the index", Detail: "slower writes"}, {ID: "skip", Label: "Skip it", Detail: "queries stay slow"}}}).Ask

	// Another session may not withdraw it; the asker may.
	if _, err := x.h.WithdrawAsk(as(sesPrincipal(f.child.Session.SessionID)), contracts.WithdrawAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-w3"}, AskID: a1.AskID}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("foreign withdraw: %v", err)
	}
	w, err := x.h.WithdrawAsk(as(parentP), contracts.WithdrawAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-w4"}, AskID: a1.AskID, Reason: "synthetic: resolved itself"})
	if err != nil || w.State != contracts.AskWithdrawn || w.Answer.Text != "synthetic: resolved itself" {
		t.Fatalf("withdraw: %+v %v", w, err)
	}
	// The topic is free again (withdrawn is not a decision).
	if r := f.ask(parentP, synthAsk("op_synth-w5", "synthetic-deploy", "Ship the synthetic build?", "b")); r.Status != contracts.AskCreated {
		t.Fatalf("after withdraw: %+v", r)
	}

	// Archiving the asking session withdraws its open asks.
	yes := true
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-w6"}, SessionID: pid, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	page, err := x.h.ListAsks(as(owner), contracts.ListAsksRequest{SessionID: pid, States: []contracts.AskState{contracts.AskOpen}})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("open after archive: %+v %v", page.Items, err)
	}
	g2, _ := x.h.GetAsk(as(owner), contracts.GetRequest{ID: a2.AskID})
	if g2.State != contracts.AskWithdrawn || g2.Answer.By.Kind != contracts.PrincipalSystem {
		t.Fatalf("archived session's ask: %+v", g2)
	}
	// No decision message goes to an archived session.
	x.clock.Add(time.Hour)
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(x.envelopes(`recipient_kind='session' AND recipient_id=? AND sender_kind IN ('system','owner')`, pid)); n != 0 {
		t.Fatalf("decision mail to archived session: %d", n)
	}
}

func TestAskOwnerMessageRepairedByMaintenance(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	a := f.ask(sesPrincipal(f.parent.Session.SessionID), synthAsk("op_synth-m1", "synthetic-deploy", "Ship the synthetic build?", "b")).Ask
	// Simulate a crash between commit and send.
	if _, err := x.db.Exec(`DELETE FROM messages WHERE message_id=?`, a.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := x.db.Exec(`DELETE FROM operations WHERE operation_id=?`, "op_ask-"+a.AskID); err != nil {
		t.Fatal(err)
	}
	x.db.Exec(`UPDATE asks SET message_pending=1 WHERE ask_id=?`, a.AskID)
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(x.envelopes(`message_id=?`, a.MessageID)); n != 0 {
		t.Fatal("repaired before the grace period")
	}
	x.clock.Add(2 * time.Minute)
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(x.envelopes(`message_id=? AND recipient_kind='owner'`, a.MessageID)); n != 1 {
		t.Fatalf("owner message not repaired: %d", n)
	}
}

func TestAsksFeatureAdvertised(t *testing.T) {
	x := newHarness(t)
	hr, err := x.h.Hello(as(owner), contracts.HelloRequest{Protocol: contracts.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range hr.Features {
		if f == contracts.FeatureAsks {
			return
		}
	}
	t.Fatalf("features: %v", hr.Features)
}
