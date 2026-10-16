package hub

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- "waiting on you" at a top-level session's turn end (waiting.go) ----

type waitCall struct {
	op, session, question, summary string
}

// fakeWaiting records RaiseWaiting/WithdrawWaiting calls.
type fakeWaiting struct {
	mu      sync.Mutex
	calls   []waitCall
	onRaise func()
}

func (f *fakeWaiting) RaiseWaiting(ctx context.Context, sessionID string, projectID *string, question, summary string) (contracts.AttentionItem, error) {
	f.mu.Lock()
	f.calls = append(f.calls, waitCall{"raise", sessionID, question, summary})
	hook := f.onRaise
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return contracts.AttentionItem{Kind: contracts.AttnWaiting, SessionID: sessionID}, nil
}

func (f *fakeWaiting) WithdrawWaiting(ctx context.Context, sessionID, summary string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, waitCall{op: "withdraw", session: sessionID})
	return true, nil
}

func (f *fakeWaiting) got() []waitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]waitCall(nil), f.calls...)
}

const leadQuestion = "Synthetic report is ready.\n\nStill waiting on you: should I build the synthetic voice-match score?\nDefault if I hear nothing: skip it."

type leadFixture struct {
	*childFixture
	w    *fakeWaiting
	text string
}

// newLeadFixture: an owner-started (top-level) session and its child, the
// lead's last message leadQuestion.
func newLeadFixture(t *testing.T) *leadFixture {
	t.Helper()
	f := &leadFixture{childFixture: newChildFixture(t, nil), w: &fakeWaiting{}, text: leadQuestion}
	f.x.h.waiting = f.w
	f.x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		return f.text, nil
	}
	return f
}

func (f *leadFixture) lead() string { return f.parent.Session.SessionID }

// observeLead reports a state of the lead's current run.
func (f *leadFixture) observeLead(state contracts.RunState) {
	f.x.t.Helper()
	r := *f.x.session(f.lead()).Run
	r.State = state
	f.x.report(nodeMac, contracts.ReportRun, r)
	f.x.clock.Add(time.Second)
}

func (f *leadFixture) turn() {
	f.x.t.Helper()
	f.observeLead(contracts.RunRunning)
	f.observeLead(contracts.RunWaitingInput)
}

func TestWaitingLeadTurnEndsWithQuestion(t *testing.T) {
	f := newLeadFixture(t)
	f.turn()
	f.process()
	got := f.w.got()
	if len(got) != 1 || got[0].op != "raise" || got[0].session != f.lead() ||
		got[0].question != "Still waiting on you: should I build the synthetic voice-match score?" ||
		!strings.Contains(got[0].summary, "Still waiting on you") || !strings.Contains(got[0].summary, "Default if I hear nothing") {
		t.Fatalf("calls: %+v", got)
	}
	// Processed once: a second pass does nothing.
	f.process()
	if n := len(f.w.got()); n != 1 {
		t.Fatalf("re-processed: %d calls", n)
	}
	// The next turn ends with a question again: raised again (the store
	// refreshes the one item).
	f.turn()
	f.process()
	if got := f.w.got(); len(got) != 2 || got[1].op != "raise" || got[1].session != f.lead() {
		t.Fatalf("repeat: %+v", got)
	}
	// A turn that ends without a question raises nothing.
	f.text = "Done: the synthetic tests pass."
	f.turn()
	f.process()
	if n := len(f.w.got()); n != 2 {
		t.Fatalf("no question raised: %+v", f.w.got())
	}
	// Mid-turn approvals and the session's first (launch) idle are not turn ends.
	f.text = leadQuestion
	f.observeLead(contracts.RunRunning)
	f.observeLead(contracts.RunWaitingApproval)
	f.process()
	if n := len(f.w.got()); n != 2 {
		t.Fatalf("approval wait raised: %+v", f.w.got())
	}
}

func TestWaitingNeverForChildSession(t *testing.T) {
	f := newLeadFixture(t)
	f.observe(contracts.RunRunning, "")
	f.observe(contracts.RunWaitingInput, "")
	f.process()
	for _, c := range f.w.got() {
		if c.session == f.child.Session.SessionID {
			t.Fatalf("child raised: %+v", c)
		}
	}
	var n int
	f.x.db.QueryRow(`SELECT COUNT(*) FROM lead_waits`).Scan(&n)
	if n != 0 {
		t.Fatalf("lead_waits rows: %d", n)
	}
	if len(f.toParent()) != 1 {
		t.Fatalf("child report still goes to the parent: %+v", f.toParent())
	}
}

func TestWaitingSkippedWhenAskFiledInTurn(t *testing.T) {
	f := newLeadFixture(t)
	f.observeLead(contracts.RunRunning)
	f.ask(sesPrincipal(f.lead()), synthAsk("op_synth-wait-ask", "synthetic-voice-match", "Build the voice-match score?", "b"))
	f.x.clock.Add(time.Second)
	f.observeLead(contracts.RunWaitingInput)
	f.process()
	if got := f.w.got(); len(got) != 0 {
		t.Fatalf("ask filed this turn, still raised: %+v", got)
	}
	// A later turn files nothing new but asks in chat again: raised (the
	// old ask was filed in an earlier turn).
	f.turn()
	f.process()
	if got := f.w.got(); len(got) != 1 {
		t.Fatalf("later turn: %+v", got)
	}
}

func TestWaitingStaleWhenTheSessionMovedOn(t *testing.T) {
	f := newLeadFixture(t)
	// The next turn started before the check ran: nothing.
	f.turn()
	f.observeLead(contracts.RunRunning)
	f.process()
	if got := f.w.got(); len(got) != 0 {
		t.Fatalf("stale turn raised: %+v", got)
	}
	// The next turn starts while the transcript is read: nothing.
	f.observeLead(contracts.RunWaitingInput)
	f.x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		f.observeLead(contracts.RunRunning)
		return leadQuestion, nil
	}
	f.process()
	if got := f.w.got(); len(got) != 0 {
		t.Fatalf("moved on during the read, still raised: %+v", got)
	}
	// The next turn starts while the item is raised: withdrawn at once.
	f.x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) { return leadQuestion, nil }
	f.w.onRaise = func() { f.observeLead(contracts.RunRunning) }
	f.observeLead(contracts.RunWaitingInput)
	f.process()
	if got := f.w.got(); len(got) != 2 || got[0].op != "raise" || got[1].op != "withdraw" || got[1].session != f.lead() {
		t.Fatalf("raced raise: %+v", got)
	}
	// An archived session raises nothing.
	f.w.onRaise = nil
	f.observeLead(contracts.RunWaitingInput)
	archived := true
	if _, err := f.x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-wait-arch"},
		SessionID: f.lead(), Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	f.process()
	if got := f.w.got(); len(got) != 2 {
		t.Fatalf("archived raised: %+v", got)
	}
}

func TestWaitingExcerptAroundQuestion(t *testing.T) {
	long := strings.Repeat("Synthetic background. ", 40) + "Should I ship the synthetic build? " + strings.Repeat("More detail. ", 40)
	ex := waitingExcerpt(long, "Should I ship the synthetic build?")
	if !strings.Contains(ex, "Should I ship the synthetic build?") || len([]rune(ex)) > waitingExcerptMax+2 || !strings.HasPrefix(ex, "…") || !strings.HasSuffix(ex, "…") {
		t.Fatalf("excerpt: %q", ex)
	}
	if ex := waitingExcerpt("Short. Ship it?", "nothing like it"); ex != "Short. Ship it?" {
		t.Fatalf("short excerpt: %q", ex)
	}
}
