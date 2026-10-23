package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- digests, folded clean exits, waiting on own job (setup review F3/F5) ----

func (f *childFixture) observeSession(sessionID string, state contracts.RunState) {
	f.x.t.Helper()
	r := *f.x.session(sessionID).Run
	r.State, r.StateReason = state, "synthetic "+string(state)
	f.x.report(nodeMac, contracts.ReportRun, r)
}

// allToParent lists every message the parent received, oldest first.
func (f *childFixture) allToParent() []contracts.MessageView {
	f.x.t.Helper()
	rows, err := f.x.db.Query(`SELECT message_id FROM messages WHERE recipient_kind='session' AND recipient_id=? ORDER BY inbox_cursor`, f.parent.Session.SessionID)
	if err != nil {
		f.x.t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]contracts.MessageView, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.x.msg(id))
	}
	return out
}

func TestChildReportsDigestForBusyParent(t *testing.T) {
	f := newChildFixture(t, nil)
	second := f.startChild("op_synth-child-2", nil)
	parent := f.parent.Session.SessionID
	f.observeSession(parent, contracts.RunRunning) // the parent is mid-turn
	for _, id := range []string{f.child.Session.SessionID, second.Session.SessionID} {
		f.observeSession(id, contracts.RunRunning)
		f.observeSession(id, contracts.RunWaitingInput)
	}
	f.process()
	if n := len(f.allToParent()); n != 0 {
		t.Fatalf("a busy parent got %d messages: %v", n, f.statuses())
	}
	f.x.clock.Add(30 * time.Second)
	f.observeSession(parent, contracts.RunWaitingInput) // its turn ends
	f.process()
	msgs := f.allToParent()
	if len(msgs) != 1 {
		t.Fatalf("want one digest, got %d: %v", len(msgs), f.statuses())
	}
	d := msgs[0].Envelope
	if d.Sender.Kind != contracts.PrincipalSystem || !strings.HasPrefix(d.Body.Text, "2 reports from sessions you started: synthetic child finished") {
		t.Fatalf("digest: %+v\n%s", d.Sender, d.Body.Text)
	}
	for _, want := range []string{"[pyre digest]", "— 1/2 —", "— 2/2 —", "send_message to " + f.child.Session.SessionID, "send_message to " + second.Session.SessionID} {
		if !strings.Contains(d.Body.Text, want) {
			t.Fatalf("digest lacks %q:\n%s", want, d.Body.Text)
		}
	}
	if strings.Contains(d.Body.Text, "reply_to") {
		t.Fatalf("a digest must not suggest reply_to:\n%s", d.Body.Text)
	}
	for _, st := range f.statuses() {
		if !strings.HasPrefix(st, "idle:sent:in "+d.MessageID) {
			t.Fatalf("statuses: %v", f.statuses())
		}
	}
}

func TestChildReportBatchedAtMostChildBatchMax(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observeSession(f.parent.Session.SessionID, contracts.RunRunning)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	if len(f.toParent()) != 0 {
		t.Fatal("sent to a busy parent")
	}
	f.x.clock.Add(childBatchMax + time.Second)
	f.process()
	if msgs := f.toParent(); len(msgs) != 1 || msgs[0].Sender.ID != f.child.Session.SessionID {
		t.Fatalf("after childBatchMax a lone report goes out from its child: %v", f.statuses())
	}
}

func TestChildReportCleanExitFoldsIntoNextReport(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	if len(f.allToParent()) != 1 {
		t.Fatalf("idle: %v", f.statuses())
	}
	f.ackAll()
	f.x.clock.Add(time.Minute)
	f.observe(contracts.RunExited, "process exited")
	f.process()
	if n := len(f.allToParent()); n != 1 {
		t.Fatalf("a clean exit already reported woke the parent: %d %v", n, f.statuses())
	}
	f.x.clock.Add(time.Minute)
	// Started now: a prompted child idle at launch for longer than
	// childLaunchSettle is reported as having run its first turn.
	second := f.startChild("op_synth-child-2", nil)
	f.observeSession(second.Session.SessionID, contracts.RunRunning)
	f.observeSession(second.Session.SessionID, contracts.RunWaitingInput)
	f.process()
	msgs := f.allToParent()
	if len(msgs) != 2 {
		t.Fatalf("second child's report: %d %v", len(msgs), f.statuses())
	}
	last := msgs[1].Envelope
	if last.Sender.ID != second.Session.SessionID || !strings.Contains(last.Body.Text, "Also exited cleanly since") || !strings.Contains(last.Body.Text, f.child.Session.SessionID) {
		t.Fatalf("fold did not ride along:\n%s", last.Body.Text)
	}
	if st := f.statuses(); st[1] != "ended:sent:in "+last.MessageID {
		t.Fatalf("statuses: %v", st)
	}

	// A fold nobody picks up is dropped after childReportHold.
	g := newChildFixture(t, nil)
	g.observe(contracts.RunRunning, "working")
	g.observe(contracts.RunWaitingInput, "idle")
	g.process()
	g.ackAll()
	g.observe(contracts.RunExited, "process exited")
	g.process()
	g.x.clock.Add(childReportHold + time.Minute)
	g.process()
	if st := g.statuses(); len(st) != 2 || !strings.HasPrefix(st[1], "ended:skipped:clean exit already reported") || len(g.allToParent()) != 1 {
		t.Fatalf("expired fold: %v", st)
	}
}

func (f *childFixture) waitOn(sessionID, what string, minutes int) contracts.SetWaitingOnResponse {
	f.x.t.Helper()
	resp, err := f.x.h.SetWaitingOn(as(sesPrincipal(sessionID)), contracts.SetWaitingOnRequest{
		Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, What: what, ExpectMinutes: minutes})
	if err != nil {
		f.x.t.Fatal(err)
	}
	return resp
}

func TestChildReportWaitingOnOwnJob(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	resp := f.waitOn(f.child.Session.SessionID, "CI on synthetic PR #12", 30)
	if resp.Until == nil || !strings.Contains(resp.Detail, "CI on synthetic PR #12") {
		t.Fatalf("response: %+v", resp)
	}
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 {
		t.Fatalf("waiting report: %v", f.statuses())
	}
	body := msgs[0].Body.Text
	if lead, _, _ := strings.Cut(body, "\n"); lead != "synthetic child is waiting on its own job: CI on synthetic PR #12" {
		t.Fatalf("lead: %q", lead)
	}
	if !strings.Contains(body, "not idle") || !strings.Contains(body, "Don't give it new work") {
		t.Fatalf("body:\n%s", body)
	}
	// Nothing resumed it by the expected time: the parent hears it looks stuck.
	f.x.clock.Add(31 * time.Minute)
	f.process()
	msgs = f.toParent()
	if len(msgs) != 2 || !strings.HasPrefix(msgs[1].Body.Text, "synthetic child may be stuck") {
		t.Fatalf("overdue: %d %v", len(msgs), f.statuses())
	}

	// The watcher woke it in time: no overdue report; its next turn
	// without a marker reports idle.
	g := newChildFixture(t, nil)
	g.observe(contracts.RunRunning, "working")
	g.waitOn(g.child.Session.SessionID, "synthetic review", 10)
	g.observe(contracts.RunWaitingInput, "idle")
	g.process()
	g.x.clock.Add(5 * time.Minute)
	g.observe(contracts.RunRunning, "watcher woke it")
	g.observe(contracts.RunWaitingInput, "idle")
	g.x.clock.Add(10 * time.Minute)
	g.process()
	st := g.statuses()
	if len(st) != 3 || st[0] != "waiting:sent:" || st[1] != "waiting_overdue:skipped:the child started a new turn" || st[2] != "idle:sent:" {
		t.Fatalf("statuses: %v", st)
	}
}

func (f *childFixture) waitQuiet(what string, minutes int) contracts.SetWaitingOnResponse {
	f.x.t.Helper()
	resp, err := f.x.h.SetWaitingOn(as(sesPrincipal(f.child.Session.SessionID)), contracts.SetWaitingOnRequest{
		Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, What: what, ExpectMinutes: minutes, Quiet: true})
	if err != nil {
		f.x.t.Fatal(err)
	}
	return resp
}

// A quiet wait sends the parent nothing at the turn's end; the overdue
// report still fires, carrying the last message the parent never got.
func TestChildReportQuietWaitSendsNoReport(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	resp := f.waitQuiet("synthetic workers and CI", 30)
	if !resp.Quiet || !strings.Contains(resp.Detail, "quiet") || !strings.Contains(resp.Detail, "not told") {
		t.Fatalf("response: %+v", resp)
	}
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	if msgs := f.allToParent(); len(msgs) != 0 {
		t.Fatalf("a quiet wait reported: %v", f.statuses())
	}
	if st := f.statuses(); len(st) != 2 || st[0] != "waiting:skipped:"+reportQuietDetail || st[1] != "waiting_overdue:pending:" {
		t.Fatalf("statuses: %v", st)
	}
	f.x.clock.Add(31 * time.Minute)
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 {
		t.Fatalf("overdue: %d %v", len(msgs), f.statuses())
	}
	body := msgs[0].Body.Text
	for _, want := range []string{"synthetic child may be stuck", "a quiet wait", "Its last message: Synthetic final answer: the tests pass."} {
		if !strings.Contains(body, want) {
			t.Fatalf("overdue lacks %q:\n%s", want, body)
		}
	}

	// Woken in time (a child's report, a message): nothing at all reaches
	// the parent for the quiet turn.
	g := newChildFixture(t, nil)
	g.observe(contracts.RunRunning, "working")
	g.waitQuiet("synthetic review", 10)
	g.observe(contracts.RunWaitingInput, "idle")
	g.x.clock.Add(5 * time.Minute)
	g.observe(contracts.RunRunning, "woken by a message")
	g.x.clock.Add(10 * time.Minute)
	g.process()
	if n := len(g.allToParent()); n != 0 {
		t.Fatalf("woken in time, the parent got %d: %v", n, g.statuses())
	}
	if st := g.statuses(); len(st) != 2 || st[1] != "waiting_overdue:skipped:the child started a new turn" {
		t.Fatalf("statuses: %v", st)
	}
}

// A run that exits after a quiet turn is never quiet: the ended report goes
// out with the last message (the quiet turn counts as not reported).
func TestChildReportEndedAfterQuietWaitIsSent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state contracts.RunState
	}{{"exited", contracts.RunExited}, {"failed", contracts.RunFailed}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChildFixture(t, nil)
			f.observe(contracts.RunRunning, "working")
			f.waitQuiet("synthetic CI", 30)
			f.observe(contracts.RunWaitingInput, "idle")
			f.process()
			f.x.clock.Add(time.Minute)
			f.observe(tc.state, "process "+tc.name)
			f.process()
			msgs := f.allToParent()
			if len(msgs) != 1 || msgs[0].Envelope.Sender.ID != f.child.Session.SessionID {
				t.Fatalf("ended after a quiet wait: %d %v", len(msgs), f.statuses())
			}
			body := msgs[0].Envelope.Body.Text
			if !strings.Contains(body, "has "+tc.name) || !strings.Contains(body, "Synthetic final answer") {
				t.Fatalf("ended body:\n%s", body)
			}
			f.x.clock.Add(31 * time.Minute)
			f.process()
			if n := len(f.allToParent()); n != 1 {
				t.Fatalf("overdue after the run ended: %d %v", n, f.statuses())
			}
			st := f.statuses()
			if len(st) != 3 || st[0] != "waiting:skipped:"+reportQuietDetail || st[1] != "waiting_overdue:skipped:the child's run ended (its ended report covers it)" ||
				!strings.HasPrefix(st[2], "ended:sent:") {
				t.Fatalf("statuses: %v", st)
			}
		})
	}
}

func TestLeadWaitingOnOwnJobIsNotWaitingOnOwner(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	called := 0
	x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		called++
		return "Should I merge it?", nil
	}
	lead := x.start(owner, "op_synth-lead", nodeMac)
	r := *x.session(lead.Session.SessionID).Run
	r.State = contracts.RunRunning
	x.report(nodeMac, contracts.ReportRun, r)
	if _, err := x.h.SetWaitingOn(as(sesPrincipal(lead.Session.SessionID)), contracts.SetWaitingOnRequest{
		Mutation: contracts.Mutation{OperationID: "op_synth-wait"}, What: "CI"}); err != nil {
		t.Fatal(err)
	}
	r.State = contracts.RunWaitingInput
	x.report(nodeMac, contracts.ReportRun, r)
	var n int
	x.db.QueryRow(`SELECT COUNT(*) FROM lead_waits`).Scan(&n)
	if n != 0 {
		t.Fatalf("a lead waiting on its own job got a waiting-on-you check")
	}
	if _, err := x.h.SetWaitingOn(as(owner), contracts.SetWaitingOnRequest{Mutation: contracts.Mutation{OperationID: "op_synth-wait-owner"}, What: "x"}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("owner call: %v", err)
	}
}

// ackAll acks the parent's messages (it read them).
func (f *childFixture) ackAll() {
	f.x.t.Helper()
	for _, m := range f.allToParent() {
		if _, err := f.x.h.AckMessage(as(sesPrincipal(f.parent.Session.SessionID)), contracts.AckMessageRequest{
			Mutation: contracts.Mutation{OperationID: "op_synth-ack-" + m.Envelope.MessageID}, MessageID: m.Envelope.MessageID}); err != nil {
			f.x.t.Fatal(err)
		}
	}
}

// A crash between the message store and the settle loses nothing and
// sends nothing twice: the next pass resends the claimed text under the
// same operation ID.
func TestChildReportDigestSurvivesCrashBetweenSendAndSettle(t *testing.T) {
	f := newChildFixture(t, nil)
	second := f.startChild("op_synth-child-2", nil)
	parent := f.parent.Session.SessionID
	f.observeSession(parent, contracts.RunRunning)
	for _, id := range []string{f.child.Session.SessionID, second.Session.SessionID} {
		f.observeSession(id, contracts.RunRunning)
		f.observeSession(id, contracts.RunWaitingInput)
	}
	f.process()
	f.observeSession(parent, contracts.RunWaitingInput)
	afterChildSend = func() (bool, error) { return true, nil }
	f.process()
	afterChildSend = nil
	if n := len(f.allToParent()); n != 1 {
		t.Fatalf("crashed send stored %d messages", n)
	}
	for _, st := range f.statuses() {
		if !strings.HasPrefix(st, "idle:pending:") {
			t.Fatalf("rows settled despite the crash: %v", f.statuses())
		}
	}
	f.process() // restart: the claim is resumed
	msgs := f.allToParent()
	if len(msgs) != 1 {
		t.Fatalf("resume duplicated or lost the digest: %d", len(msgs))
	}
	for _, st := range f.statuses() {
		if st != "idle:sent:in "+msgs[0].Envelope.MessageID {
			t.Fatalf("after resume: %v", f.statuses())
		}
	}
}

// A digest that would pass childDigestMaxBytes is split: what fits goes
// now, the rest in the next message.
func TestChildReportDigestSplitsUnderSizeLimit(t *testing.T) {
	f := newChildFixture(t, nil)
	f.x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		return strings.Repeat("synthetic words ", 30), nil
	}
	parent := f.parent.Session.SessionID
	f.observeSession(parent, contracts.RunRunning)
	ids := []string{f.child.Session.SessionID}
	for i := range 2 {
		ids = append(ids, f.startChild("op_synth-split-"+string(rune('a'+i)), nil).Session.SessionID)
	}
	for _, id := range ids {
		f.observeSession(id, contracts.RunRunning)
		f.observeSession(id, contracts.RunWaitingInput)
	}
	f.process()
	f.observeSession(parent, contracts.RunWaitingInput)
	old := childDigestMaxBytes
	childDigestMaxBytes = 2500 // fits two reports
	defer func() { childDigestMaxBytes = old }()
	f.process()
	f.process()
	msgs := f.allToParent()
	total := 0
	for _, m := range msgs {
		if len(m.Envelope.Body.Text) > childDigestMaxBytes+600 {
			t.Fatalf("digest of %d bytes", len(m.Envelope.Body.Text))
		}
		total += max(strings.Count(m.Envelope.Body.Text, "[pyre auto-report]"), 0)
	}
	if len(msgs) < 2 || total != 3 {
		t.Fatalf("split: %d messages carrying %d reports; %v", len(msgs), total, f.statuses())
	}
}

// A failure after the message committed keeps the claim: the retry resends
// the stored text under the same operation (no loss, no duplicate), and a
// report arriving meanwhile goes in its own message without conflict.
func TestChildReportDigestFailureAfterSendKeepsClaim(t *testing.T) {
	f := newChildFixture(t, nil)
	second := f.startChild("op_synth-child-2", nil)
	parent := f.parent.Session.SessionID
	f.observeSession(parent, contracts.RunRunning)
	for _, id := range []string{f.child.Session.SessionID, second.Session.SessionID} {
		f.observeSession(id, contracts.RunRunning)
		f.observeSession(id, contracts.RunWaitingInput)
	}
	f.process()
	f.observeSession(parent, contracts.RunWaitingInput)
	afterChildSend = func() (bool, error) {
		return false, contracts.Errorf(contracts.ErrUnavailable, "synthetic failure after commit")
	}
	f.process()
	afterChildSend = nil
	if n := len(f.allToParent()); n != 1 {
		t.Fatalf("stored %d messages", n)
	}
	// Another report while the claim waits for its retry.
	f.observe(contracts.RunRunning, "working again")
	f.observe(contracts.RunWaitingInput, "idle again")
	f.process() // the claim is backing off; the new report goes alone
	f.x.clock.Add(10 * time.Second)
	f.process() // the claim retries
	msgs := f.allToParent()
	if len(msgs) != 2 {
		t.Fatalf("want the digest once plus the new report, got %d: %v", len(msgs), f.statuses())
	}
	for _, st := range f.statuses() {
		if !strings.HasPrefix(st, "idle:sent:") {
			t.Fatalf("statuses: %v", f.statuses())
		}
	}
	// No busy loop: nothing is due once everything settled.
	if next, err := f.x.h.ProcessChildReports(context.Background()); err != nil || !next.IsZero() && !next.After(f.x.clock.Now()) {
		t.Fatalf("next due %v (err %v)", next, err)
	}
}

// A claim whose retry keeps failing backs off instead of spinning.
func TestChildReportStuckClaimBacksOff(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	afterChildSend = func() (bool, error) { return false, contracts.Errorf(contracts.ErrUnavailable, "synthetic outage") }
	defer func() { afterChildSend = nil }()
	f.process()
	next, err := f.x.h.ProcessChildReports(context.Background())
	if err != nil || !next.After(f.x.clock.Now()) {
		t.Fatalf("a failed claim is due again at once (%v, now %v)", next, f.x.clock.Now())
	}
}

// A child whose launch failed (tmux refused the command line: issue
// iss_01M5KW3QAPJSDZZ4TAMKETXBF7, reported 17 minutes late) reaches its
// busy parent at once, on its own, like needs_input: never batched until
// the parent's turn ends (up to childBatchMax). The start command's failed
// result and the node's "launch failed" run report both take that path; an
// ordinary failure of a running child still waits for the parent's turn.
func TestChildReportLaunchFailureNotBatched(t *testing.T) {
	const tmuxErr = "start claude in tmux: tmux new-session: command too long: exit status 1"
	for _, via := range []string{"command result", "run report"} {
		t.Run(via, func(t *testing.T) {
			f := newChildFixture(t, nil)
			parent := f.parent.Session.SessionID
			f.observeSession(parent, contracts.RunRunning) // the parent is mid-turn
			resp, err := f.x.h.StartSession(as(sesPrincipal(parent)), contracts.StartSessionRequest{
				Mutation: contracts.Mutation{OperationID: "op_synth-child-lf"}, NodeID: nodeMac, Harness: contracts.HarnessClaude, Cwd: "/home/synth/child",
				Title: "synthetic failing child", InitialPrompt: "run the synthetic tests"})
			if err != nil {
				t.Fatal(err)
			}
			cmds := f.x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
			cmd := cmds[len(cmds)-1]
			f.x.ack(nodeMac, cmd)
			if via == "command result" {
				f.x.report(nodeMac, contracts.ReportCommandResult, contracts.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: contracts.OutcomeFailed,
					Error: contracts.Errorf(contracts.ErrUnavailable, tmuxErr), At: f.x.clock.Now()})
			} else {
				run := *resp.Session.Run
				run.State, run.StateReason = contracts.RunFailed, "launch failed: "+tmuxErr
				f.x.report(nodeMac, contracts.ReportRun, run)
			}
			f.process()
			msgs := f.allToParent()
			if len(msgs) != 1 {
				t.Fatalf("a launch failure for a busy parent: %d messages, want 1 at once: %v", len(msgs), f.statuses())
			}
			m := msgs[0].Envelope
			if m.Sender.ID != resp.Session.Session.SessionID || !strings.Contains(m.Body.Text, "could not start") || !strings.Contains(m.Body.Text, "command too long") {
				t.Fatalf("report from %+v:\n%s", m.Sender, m.Body.Text)
			}
		})
	}

	// Contrast: a running child that fails later is batched for a busy parent.
	f := newChildFixture(t, nil)
	f.observeSession(f.parent.Session.SessionID, contracts.RunRunning)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunFailed, "process exited with status 1")
	f.process()
	if n := len(f.allToParent()); n != 0 {
		t.Fatalf("an ordinary failure reached a busy parent mid-turn: %d %v", n, f.statuses())
	}
}
