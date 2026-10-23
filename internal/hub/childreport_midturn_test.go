package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- a mid-turn message is not the turn's result (iss_01M5J3XGZGGWF1APPPE7PZ0B25) ----

// childSays sends the parent a message from child (the child's own
// send_message, not an automatic report).
func (f *childFixture) childSays(child, op, text string) {
	f.x.t.Helper()
	if _, err := f.x.h.SendMessage(as(sesPrincipal(child)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: op},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: f.parent.Session.SessionID}, Body: contracts.MessageBody{Text: text}}); err != nil {
		f.x.t.Fatal(err)
	}
}

// exitClean reports that child's run exited with code 0.
func (f *childFixture) exitClean(child string) {
	f.x.t.Helper()
	r := *f.x.session(child).Run
	code := 0
	r.State, r.StateReason, r.ExitCode = contracts.RunExited, "process exited", &code
	f.x.report(nodeMac, contracts.ReportRun, r)
}

// foldLine is the digest's closing "also exited" line, or "".
func foldLine(text string) string {
	_, line, ok := strings.Cut(text, "Also exited cleanly since")
	if !ok {
		return ""
	}
	return line
}

// A long turn that asked the parent something early and ended with its
// result long after: the idle report is sent.
func TestChildReportMidTurnMessageDoesNotSkipIdle(t *testing.T) {
	f := newChildFixture(t, nil)
	child := f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	f.childSays(child, "op_synth-q1", "Question: should the synthetic suite also cover the edge case?")
	f.x.clock.Add(12 * time.Minute)
	f.childSays(child, "op_synth-q2", "Coordination: I will take the synthetic module.")
	f.x.clock.Add(childRecentMessage + 7*time.Minute) // the turn goes on well past the window
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	msgs := f.toParent()
	if len(msgs) != 3 || !strings.Contains(msgs[2].Body.Text, "Synthetic final answer") {
		t.Fatalf("the turn's result never reached the parent: %d %v", len(msgs), f.statuses())
	}
	if st := f.statuses(); len(st) != 1 || st[0] != "idle:sent:" {
		t.Fatalf("statuses: %v", st)
	}
}

// The overnight case: the idle report is replaced by the ended report
// (Pyre stopped the child after its last turn); a mid-turn message does
// not fold it, and it does not claim the result already reached the
// parent.
func TestChildReportMidTurnMessageDoesNotFoldEnded(t *testing.T) {
	f := newChildFixture(t, nil)
	child := f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	f.childSays(child, "op_synth-q1", "Question: which synthetic fixture should I use?")
	f.x.clock.Add(40 * time.Minute)
	f.observe(contracts.RunWaitingInput, "idle")
	f.x.clock.Add(3 * time.Second)
	f.exitClean(child) // before the idle report was processed: replaced
	f.process()
	msgs := f.toParent()
	if len(msgs) != 2 || !strings.Contains(msgs[1].Body.Text, "Synthetic final answer") || foldLine(msgs[1].Body.Text) != "" {
		t.Fatalf("ended report folded or missing: %d %v", len(msgs), f.statuses())
	}
	if st := f.statuses(); len(st) != 2 || st[0] != "idle:skipped:"+reportReplacedDetail || st[1] != "ended:sent:" {
		t.Fatalf("statuses: %v", st)
	}
}

// No regression: a child that sends its result right before its turn
// ends gets no idle report, and its clean exit is folded, not repeated.
func TestChildReportResultMessageBeforeEndStillFolds(t *testing.T) {
	f := newChildFixture(t, nil)
	child := f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	f.x.clock.Add(40 * time.Minute)
	f.childSays(child, "op_synth-result", "Result: all synthetic tests pass.")
	f.x.clock.Add(time.Minute)
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	if msgs := f.toParent(); len(msgs) != 1 {
		t.Fatalf("idle report duplicated the child's result: %d %v", len(msgs), f.statuses())
	}
	f.x.clock.Add(time.Minute)
	f.exitClean(child)
	f.process()
	if msgs := f.toParent(); len(msgs) != 1 {
		t.Fatalf("clean exit after the child's result woke the parent: %d %v", len(msgs), f.statuses())
	}
	if st := f.statuses(); len(st) != 2 || st[0] != "idle:skipped:"+childMessagedSkip || !strings.HasPrefix(st[1], "ended:pending:") {
		t.Fatalf("statuses: %v", st)
	}

	// Same when the ended report replaced the idle one.
	g := newChildFixture(t, nil)
	gc := g.child.Session.SessionID
	g.observe(contracts.RunRunning, "working")
	g.x.clock.Add(40 * time.Minute)
	g.childSays(gc, "op_synth-result", "Result: all synthetic tests pass.")
	g.x.clock.Add(time.Minute)
	g.observe(contracts.RunWaitingInput, "idle")
	g.exitClean(gc)
	g.process()
	if msgs := g.toParent(); len(msgs) != 1 {
		t.Fatalf("replaced idle: clean exit after the child's result woke the parent: %d %v", len(msgs), g.statuses())
	}
}

// The digest's "their last messages already reached you" names only
// children whose result did reach the parent; a child whose only message
// was mid-turn gets a full section instead.
func TestChildReportDigestFoldLineOnlyWhenTrue(t *testing.T) {
	f := newChildFixture(t, nil)
	asker := f.child.Session.SessionID
	finisher := f.startChild("op_synth-child-2", nil).Session.SessionID
	third := f.startChild("op_synth-child-3", nil).Session.SessionID
	parent := f.parent.Session.SessionID
	f.observeSession(parent, contracts.RunRunning) // the parent is mid-turn

	f.observeSession(asker, contracts.RunRunning)
	f.observeSession(finisher, contracts.RunRunning)
	f.childSays(asker, "op_synth-ask", "Question: may I touch the synthetic schema?")
	f.x.clock.Add(30 * time.Minute)
	f.childSays(finisher, "op_synth-done", "Result: synthetic module done.")
	f.x.clock.Add(time.Minute)
	for _, id := range []string{asker, finisher} {
		f.observeSession(id, contracts.RunWaitingInput)
		f.exitClean(id)
	}
	f.observeSession(third, contracts.RunRunning)
	f.observeSession(third, contracts.RunWaitingInput)
	f.process()
	f.x.clock.Add(30 * time.Second)
	f.observeSession(parent, contracts.RunWaitingInput) // its turn ends
	f.process()

	var digest string
	for _, m := range f.allToParent() {
		if m.Envelope.Sender.Kind == contracts.PrincipalSystem {
			digest = m.Envelope.Body.Text
		}
	}
	if !strings.HasPrefix(digest, "2 reports from sessions you started:") {
		t.Fatalf("want a digest of the asker's ended report and the third child's idle report: %v\n%s", f.statuses(), digest)
	}
	fold := foldLine(digest)
	if !strings.Contains(fold, "already reached you") || !strings.Contains(fold, finisher) || strings.Contains(fold, asker) {
		t.Fatalf("fold line %q (finisher %s, asker %s)", fold, finisher, asker)
	}
	if body, _, _ := strings.Cut(digest, "Also exited cleanly since"); !strings.Contains(body, "("+asker+"), which you started, has exited") {
		t.Fatalf("the asker's result has no section:\n%s", digest)
	}
}
