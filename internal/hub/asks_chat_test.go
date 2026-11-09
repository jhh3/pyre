package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/claude"
)

// ---- owner chat and open asks (asks_chat.go) ----

// chatFixture: a lead (top-level session) with two open asks.
type chatFixture struct {
	*leadFixture
	a, b    contracts.Ask
	entries []contracts.TranscriptEntry
	reads   int
}

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	f := &chatFixture{leadFixture: newLeadFixture(t)}
	f.x.h.chatTranscript = func(ctx context.Context, sessionID, runID string) ([]contracts.TranscriptEntry, error) {
		f.reads++
		return f.entries, nil
	}
	// The run's first turn (its launch prompt), then the turn that files
	// both asks.
	f.text = "Synthetic: done."
	f.turn()
	f.observeLead(contracts.RunRunning)
	lead := sesPrincipal(f.lead())
	f.a = *f.ask(lead, synthAsk("op_synth-chat-a", "synthetic-deploy", "Ship the synthetic build?", "b")).Ask
	f.x.clock.Add(time.Second)
	other := indexAsk("op_synth-chat-b")
	other.DefaultOption = "skip"
	f.b = *f.ask(lead, other).Ask
	f.observeLead(contracts.RunWaitingInput)
	return f
}

// nextTurn starts the lead's next turn after it sat idle for pause, and
// runs the answer-in-chat checks.
func (f *chatFixture) nextTurn(pause time.Duration) {
	f.x.t.Helper()
	f.x.clock.Add(pause)
	f.observeLead(contracts.RunRunning)
	f.x.clock.Add(askChatDelay)
	if _, err := f.x.h.ProcessAskChats(context.Background()); err != nil {
		f.x.t.Fatal(err)
	}
}

func (f *chatFixture) get(id string) contracts.Ask {
	f.x.t.Helper()
	a, err := f.x.h.GetAsk(as(owner), contracts.GetRequest{ID: id})
	if err != nil {
		f.x.t.Fatal(err)
	}
	return a
}

func (f *chatFixture) send(op string, req contracts.SendMessageRequest) contracts.MessageView {
	f.x.t.Helper()
	req.Mutation = contracts.Mutation{OperationID: op}
	if req.Recipient.Kind == "" {
		req.Recipient = contracts.Address{Kind: contracts.AddrSession, ID: f.lead()}
	}
	if req.Mode == "" {
		req.Mode = contracts.ModeQueue
	}
	mv, err := f.x.h.SendMessage(as(owner), req)
	if err != nil {
		f.x.t.Fatal(err)
	}
	return mv
}

// typed is a prompt the owner typed into the lead's current run now.
func (f *chatFixture) typed(text string) contracts.TranscriptEntry {
	return contracts.TranscriptEntry{RunID: f.x.session(f.lead()).Run.RunID, At: f.x.clock.Now(), Kind: "user", Text: text}
}

// decisions lists the decision messages the lead got, oldest first.
func (f *chatFixture) decisions() []contracts.MessageEnvelope {
	return f.x.envelopes(`recipient_kind='session' AND recipient_id=? AND envelope LIKE '%pyre ask%'`, f.lead())
}

// noted reports whether chat text was recorded on the ask, which stays open.
func (f *chatFixture) noted(id, text string) contracts.Ask {
	f.x.t.Helper()
	a := f.get(id)
	if a.State != contracts.AskOpen || a.Answer != nil || a.OwnerChat == nil || a.OwnerChat.Text != text {
		f.x.t.Fatalf("%s: want open with chat %q, got %s %+v %+v", id, text, a.State, a.Answer, a.OwnerChat)
	}
	return a
}

// iss_…: an unrelated owner message decided a
// yes/no ask. Chat is now recorded on the open asks; it decides nothing.
func TestOwnerMessageToTheSessionLeavesItsAsksOpen(t *testing.T) {
	f := newChatFixture(t)
	f.send("op_synth-chat-msg", contracts.SendMessageRequest{Body: contracts.MessageBody{Text: "  and all my synthetic desktops are reachable via a single url right?  "}})
	for _, id := range []string{f.a.AskID, f.b.AskID} {
		f.noted(id, "and all my synthetic desktops are reachable via a single url right?")
	}
	if d := f.decisions(); len(d) != 0 {
		t.Fatalf("decision sent for chat: %+v", d)
	}
	// Words that only mention an option still decide nothing.
	f.send("op_synth-chat-msg2", contracts.SendMessageRequest{Body: contracts.MessageBody{Text: "Ship it, and add the index."}})
	f.noted(f.a.AskID, "Ship it, and add the index.")
	// The deadline default still applies.
	f.x.clock.Add(3 * time.Hour)
	if err := f.x.h.resolveAsks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := f.get(f.a.AskID); a.State != contracts.AskDefaulted || a.Answer.OptionID != "b" {
		t.Fatalf("default after chat: %+v %+v", a, a.Answer)
	}
}

// The one narrow exception: the session's only open ask, and his whole
// text is one of its options.
func TestOwnerChatThatIsExactlyAnOptionAnswersTheOnlyAsk(t *testing.T) {
	f := newChatFixture(t)
	// Two open asks: an option label decides neither.
	f.send("op_synth-chat-two", contracts.SendMessageRequest{Body: contracts.MessageBody{Text: "Skip it"}})
	f.noted(f.b.AskID, "Skip it")
	if _, err := f.x.h.WithdrawAsk(as(sesPrincipal(f.lead())), contracts.WithdrawAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-chat-wd"}, AskID: f.a.AskID}); err != nil {
		t.Fatal(err)
	}
	f.send("op_synth-chat-near", contracts.SendMessageRequest{Body: contracts.MessageBody{Text: "skip it for now"}})
	f.noted(f.b.AskID, "skip it for now")
	f.send("op_synth-chat-one", contracts.SendMessageRequest{Body: contracts.MessageBody{Text: "  skip IT. "}})
	b := f.get(f.b.AskID)
	if b.State != contracts.AskAnswered || b.Answer.OptionID != "skip" || b.Answer.Source != contracts.AskSourceChat || b.Answer.By.Kind != contracts.PrincipalOwner {
		t.Fatalf("exact option: %+v %+v", b, b.Answer)
	}
	if d := f.decisions(); len(d) != 1 || d[0].Mode != contracts.ModeInfo || !strings.Contains(d[0].Body.Text, "owner chose skip — Skip it in chat: skip IT.") {
		t.Fatalf("decision: %+v", d)
	}
}

// The asking session records the option the owner chose in its chat.
func TestSessionRecordsTheOwnersChatAnswer(t *testing.T) {
	f := newChatFixture(t)
	if _, err := f.x.hold("op_synth-chat-hold", f.b.AskID, "add", ""); err != nil {
		t.Fatal(err)
	}
	lead := sesPrincipal(f.lead())
	answer := func(p contracts.Principal, op string, item contracts.AnswerAskItem) contracts.AnswerAskResult {
		t.Helper()
		resp, err := f.x.h.AnswerAsks(as(p), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: op}, Answers: []contracts.AnswerAskItem{item}})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Results[0]
	}
	other := f.x.start(owner, "op_synth-chat-other", nodeMac)
	for name, c := range map[string]struct {
		p    contracts.Principal
		item contracts.AnswerAskItem
		code contracts.ErrorCode
	}{
		"another session":    {sesPrincipal(other.Session.SessionID), contracts.AnswerAskItem{AskID: f.a.AskID, OptionID: "a", Text: "ship it"}, contracts.ErrForbidden},
		"no option":          {lead, contracts.AnswerAskItem{AskID: f.a.AskID, Text: "ship it"}, contracts.ErrInvalid},
		"no quote":           {lead, contracts.AnswerAskItem{AskID: f.a.AskID, OptionID: "a"}, contracts.ErrInvalid},
		"held by the owner":  {lead, contracts.AnswerAskItem{AskID: f.b.AskID, OptionID: "skip", Text: "skip it"}, contracts.ErrForbidden},
		"not one of options": {lead, contracts.AnswerAskItem{AskID: f.a.AskID, OptionID: "c", Text: "ship it"}, contracts.ErrInvalid},
	} {
		if r := answer(c.p, "op_synth-chat-bad-"+strings.ReplaceAll(name, " ", "-"), c.item); r.Error == nil || r.Error.Code != c.code {
			t.Errorf("%s: %+v", name, r.Error)
		}
	}
	r := answer(lead, "op_synth-chat-rec", contracts.AnswerAskItem{AskID: f.a.AskID, OptionID: "a", Text: "yes ship it today"})
	if r.Error != nil || r.Ask.State != contracts.AskAnswered || r.Ask.Answer.OptionID != "a" || r.Ask.Answer.Source != contracts.AskSourceChat ||
		r.Ask.Answer.By != lead || r.Ask.Answer.Text != "yes ship it today" {
		t.Fatalf("recorded: %+v", r)
	}
	if d := f.decisions(); len(d) != 0 {
		t.Fatalf("decision sent to the session that recorded it: %+v", d)
	}
	var pending int
	f.x.db.QueryRow(`SELECT decision_pending FROM asks WHERE ask_id=?`, f.a.AskID).Scan(&pending)
	if pending != 0 {
		t.Fatal("decision left pending for maintenance")
	}
}

func TestAskNotNotedByThreadReplyOrInfo(t *testing.T) {
	f := newChatFixture(t)
	// A reply in the ask's thread is a message to the session, not chat.
	f.send("op_synth-chat-thread", contracts.SendMessageRequest{ThreadID: f.a.ThreadID, InReplyTo: f.a.MessageID,
		Body: contracts.MessageBody{Text: "What do you think is better?"}})
	// So is a reply to one of its messages, and an info note (never delivered).
	f.send("op_synth-chat-reply", contracts.SendMessageRequest{InReplyTo: f.b.MessageID, Body: contracts.MessageBody{Text: "Why?"}})
	f.send("op_synth-chat-info", contracts.SendMessageRequest{Mode: contracts.ModeInfo, Body: contracts.MessageBody{Text: "fyi"}})
	// A session's message is not the owner's.
	other := f.x.start(owner, "op_synth-chat-other", nodeMac)
	if _, err := f.x.h.SendMessage(as(sesPrincipal(other.Session.SessionID)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-chat-peer"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: f.lead()}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "pick a"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.a.AskID, f.b.AskID} {
		if a := f.get(id); a.State != contracts.AskOpen || a.OwnerChat != nil {
			t.Fatalf("%s: %+v %+v", id, a.Answer, a.OwnerChat)
		}
	}
}

func TestAskChatSkipsHeldAnswers(t *testing.T) {
	f := newChatFixture(t)
	if _, err := f.x.hold("op_synth-chat-hold", f.b.AskID, "add", ""); err != nil {
		t.Fatal(err)
	}
	// The held ask is left out, so a is the only one: its option answers it.
	f.send("op_synth-chat-msg2", contracts.SendMessageRequest{Body: contracts.MessageBody{Text: "Wait for the synthetic soak"}})
	if a := f.get(f.a.AskID); a.State != contracts.AskAnswered || a.Answer.OptionID != "b" {
		t.Fatalf("a: %+v", a.Answer)
	}
	if b := f.get(f.b.AskID); b.State != contracts.AskOpen || b.OwnerChat != nil {
		t.Fatalf("held ask touched by chat: %+v %+v", b.Answer, b.OwnerChat)
	}
}

func TestAskNotedFromInputTypedIntoTheSession(t *testing.T) {
	f := newChatFixture(t)
	// The newest prompt (it started the turn) is what counts, by position:
	// older prompts and meta before it do not, and the node's clock is not
	// compared with the hub's (this one runs an hour behind).
	behind := func(e contracts.TranscriptEntry) contracts.TranscriptEntry { e.At = e.At.Add(-time.Hour); return e }
	f.entries = []contracts.TranscriptEntry{behind(f.typed("an earlier prompt")), behind(f.typed("<command-name>/compact</command-name>")),
		behind(f.typed("go with b for the deploy; skip the index"))}
	f.nextTurn(time.Minute)
	if f.reads != 1 {
		t.Fatalf("transcript reads: %d", f.reads)
	}
	for _, id := range []string{f.a.AskID, f.b.AskID} {
		f.noted(id, "go with b for the deploy; skip the index")
	}
	if d := f.decisions(); len(d) != 0 {
		t.Fatalf("decision: %+v", d)
	}
	// The next typed turn replaces the note.
	f.observeLead(contracts.RunWaitingInput)
	f.entries = []contracts.TranscriptEntry{f.typed("unrelated: is the synthetic url up?")}
	f.nextTurn(time.Minute)
	f.noted(f.a.AskID, "unrelated: is the synthetic url up?")
}

// Review of PR #23: a prompt typed while the asking turn ran is queued by
// Claude and starts the next turn at once; it was not written to answer.
func TestAskChatIgnoresAPromptQueuedDuringTheAskingTurn(t *testing.T) {
	f := newChatFixture(t)
	f.entries = []contracts.TranscriptEntry{f.typed("also fix the flaky test")}
	f.nextTurn(time.Second) // started 1 s after the asking turn ended
	// Or the hub never saw the turn end: the same turn, nothing to check.
	f.observeLead(contracts.RunRunning)
	if _, err := f.x.h.ProcessAskChats(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.reads != 0 {
		t.Fatalf("transcript read for a queued prompt: %d", f.reads)
	}
	if a := f.get(f.a.AskID); a.State != contracts.AskOpen || a.OwnerChat != nil {
		t.Fatalf("noted from a queued prompt: %+v", a.OwnerChat)
	}
	// An ask filed in the turn that just started is not answered by its
	// own prompt either.
	f.observeLead(contracts.RunWaitingInput)
	c := synthAsk("op_synth-chat-c", "synthetic-third", "Run the synthetic migration now?", "a")
	c.ConfirmNew = true
	f.x.clock.Add(time.Minute)
	f.observeLead(contracts.RunRunning)
	third := *f.ask(sesPrincipal(f.lead()), c).Ask
	f.observeLead(contracts.RunWaitingInput)
	f.entries = []contracts.TranscriptEntry{f.typed("yes, ship it")}
	f.nextTurn(time.Minute)
	f.noted(f.a.AskID, "yes, ship it")
	f.noted(third.AskID, "yes, ship it")
}

func TestAskChatNewestPromptMustBeTyped(t *testing.T) {
	f := newChatFixture(t)
	run := f.typed("").RunID
	// The turn was started by a Pyre message or a task notification: an
	// older typed prompt above it does not count.
	f.entries = []contracts.TranscriptEntry{f.typed("go with b"),
		{RunID: run, At: f.x.clock.Now(), Kind: "pyre_message", MessageID: "msg_synth-1", Text: "[pyre-msg:msg_synth-1] from session: hi"}}
	f.nextTurn(time.Minute)
	f.observeLead(contracts.RunWaitingInput)
	f.entries = []contracts.TranscriptEntry{f.typed("go with b"), {RunID: run, At: f.x.clock.Now(), Kind: "system", Text: "<task-notification>done"},
		{RunID: run, At: f.x.clock.Now(), Kind: "system", Text: "turn ended (3s)"}}
	f.nextTurn(time.Minute)
	f.observeLead(contracts.RunWaitingInput)
	// A harness that gave no time: skipped.
	f.entries = []contracts.TranscriptEntry{{RunID: run, Kind: "user", Text: "go with b"}}
	f.nextTurn(time.Minute)
	if a := f.get(f.a.AskID); a.State != contracts.AskOpen || a.OwnerChat != nil {
		t.Fatalf("noted: %+v", a.OwnerChat)
	}
	if f.reads != 3 {
		t.Fatalf("reads: %d", f.reads)
	}
}

// A run's first turn is its launch, resume or handoff prompt, however long
// the harness took to submit it.
func TestAskChatIgnoresTheRunsFirstPrompt(t *testing.T) {
	f := newLeadFixture(t)
	f.x.h.chatTranscript = func(ctx context.Context, sessionID, runID string) ([]contracts.TranscriptEntry, error) {
		return []contracts.TranscriptEntry{{RunID: runID, At: f.x.clock.Now(), Kind: "user", Text: "resume the synthetic work"}}, nil
	}
	a := *f.ask(sesPrincipal(f.lead()), synthAsk("op_synth-chat-first", "synthetic-first", "Ship the synthetic build?", "b")).Ask
	f.x.clock.Add(time.Hour)
	f.observeLead(contracts.RunRunning) // the run's first turn
	f.x.clock.Add(askChatDelay)
	if _, err := f.x.h.ProcessAskChats(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	f.x.db.QueryRow(`SELECT COUNT(*) FROM ask_chat_checks`).Scan(&n)
	got, _ := f.x.h.GetAsk(as(owner), contracts.GetRequest{ID: a.AskID})
	if n != 0 || got.State != contracts.AskOpen || got.OwnerChat != nil {
		t.Fatalf("first prompt of the run: %d checks, %+v", n, got.OwnerChat)
	}
}

func TestOwnerChatText(t *testing.T) {
	for in, want := range map[string]bool{
		"yes, ship it": true, "  b  ": true, "/compact": false, "/model opus": true, "": false,
		"[pyre-msg:msg_1] from owner: x": false, "<local-command-stdout>ok</local-command-stdout>": false,
		"Stop hook feedback:\nx": false, "[Request interrupted by user]": false,
		"[pyre] Continue where you left off (account switched from synth-a to synth-b because of the usage limit).": false,
		"<task-notification>\n<task-id>b1</task-id>":                                                                false, "The synth plugin sent a message:\n[check] reply": false,
		"This session is being continued from a previous conversation that ran out of context.": false,
		"Pyre handoff: this session (synthetic) continues previous run run_synth":               false,
		"The plan looks right, go ahead": true,
	} {
		if _, ok := ownerChatText(in); ok != want {
			t.Errorf("ownerChatText(%q) = %v, want %v", in, ok, want)
		}
	}
}

// Pyre's own Stop-hook nudges are never the owner's answer.
func TestOwnerChatTextSkipsPyreNudges(t *testing.T) {
	for _, s := range []string{claude.AskNudgeText, claude.PageNudgeText} {
		if _, ok := ownerChatText(s); ok {
			t.Errorf("nudge read as the owner: %.60q", s)
		}
	}
}

func TestOwnerWithdrawTellsTheSessionItWasDismissed(t *testing.T) {
	f := newChatFixture(t)
	if _, err := f.x.h.WithdrawAsk(as(owner), contracts.WithdrawAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-chat-dismiss"},
		AskID: f.a.AskID, Reason: contracts.AskDismissReason}); err != nil {
		t.Fatal(err)
	}
	d := f.decisions()
	if len(d) != 1 || d[0].Mode != contracts.ModeQueue || !strings.Contains(d[0].Body.Text, "withdrawn by the owner (dismissed by the owner") {
		t.Fatalf("dismiss decision: %+v", d)
	}
	f.x.clock.Add(3 * time.Hour)
	if err := f.x.h.resolveAsks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := f.get(f.a.AskID); a.State != contracts.AskWithdrawn || a.Answer.OptionID != "" {
		t.Fatalf("dismissed ask: %+v %+v", a, a.Answer)
	}
}

// A node that does not answer costs one bounded read, in parallel with the
// others, and never holds the child-report worker (its own loop and lock).
func TestAskChatSlowNodeIsBounded(t *testing.T) {
	f := newChatFixture(t)
	old := askChatReadTimeout
	askChatReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { askChatReadTimeout = old })
	f.x.h.chatTranscript = func(ctx context.Context, sessionID, runID string) ([]contracts.TranscriptEntry, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.x.clock.Add(time.Minute)
	f.observeLead(contracts.RunRunning)
	f.x.clock.Add(askChatDelay)
	done := make(chan struct{})
	go func() {
		f.x.h.reportMu.Lock() // the child reports' lock is not needed
		defer f.x.h.reportMu.Unlock()
		<-done
	}()
	start := time.Now()
	if _, err := f.x.h.ProcessAskChats(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(done)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("pass took %s", d)
	}
	var attempts int
	if err := f.x.db.QueryRow(`SELECT attempts FROM ask_chat_checks`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("deferred for a retry: %d %v", attempts, err)
	}
}

// Asking again on the topic clears the chat note: his earlier words
// predate the new question.
func TestAskingAgainClearsTheOwnerChatNote(t *testing.T) {
	f := newChatFixture(t)
	f.send("op_synth-chat-note", contracts.SendMessageRequest{Body: contracts.MessageBody{Text: "unrelated question"}})
	f.noted(f.a.AskID, "unrelated question")
	again := f.ask(sesPrincipal(f.lead()), synthAsk("op_synth-chat-again", "synthetic-deploy", "Ship the synthetic build?", "b"))
	if again.Status != contracts.AskMerged || again.Ask.OwnerChat != nil {
		t.Fatalf("merge: %s %+v", again.Status, again.Ask.OwnerChat)
	}
}
