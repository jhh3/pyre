package hub

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- automatic reports to the parent session (W-childnotify) ----

type childFixture struct {
	x      *harness
	parent contracts.SessionView
	child  contracts.SessionView
}

// newChildFixture starts an owner session (the parent) and a session the
// parent starts (the child), both idle after launch.
func newChildFixture(t *testing.T, report *bool, tags ...string) *childFixture {
	t.Helper()
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		return "Synthetic final answer: the tests pass.", nil
	}
	f := &childFixture{x: x, parent: x.start(owner, "op_synth-parent", nodeMac)}
	f.child = f.startChild("op_synth-child", report, tags...)
	x.clock.Add(time.Second)
	return f
}

func (f *childFixture) startChild(op string, report *bool, tags ...string) contracts.SessionView {
	x := f.x
	x.t.Helper()
	resp, err := x.h.StartSession(as(sesPrincipal(f.parent.Session.SessionID)), contracts.StartSessionRequest{
		Mutation: contracts.Mutation{OperationID: op}, NodeID: nodeMac, Harness: contracts.HarnessClaude, Cwd: "/home/synth/child",
		Title: "synthetic child", InitialPrompt: "run the synthetic tests", ReportToParent: report, Tags: tags})
	if err != nil {
		x.t.Fatalf("start child: %v", err)
	}
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	cmd := cmds[len(cmds)-1]
	x.ack(nodeMac, cmd)
	run := *resp.Session.Run
	run.State = contracts.RunWaitingInput
	run.Adapter = "claude-synth"
	x.result(nodeMac, cmd, contracts.OutcomeSucceeded, run)
	return x.session(resp.Session.Session.SessionID)
}

// observe reports a state of the child's current run.
func (f *childFixture) observe(state contracts.RunState, reason string) {
	f.x.t.Helper()
	r := *f.x.session(f.child.Session.SessionID).Run
	r.State, r.StateReason = state, reason
	f.x.report(nodeMac, contracts.ReportRun, r)
}

func (f *childFixture) process() {
	f.x.t.Helper()
	if _, err := f.x.h.ProcessChildReports(context.Background()); err != nil {
		f.x.t.Fatal(err)
	}
}

// toParent lists messages the parent received from the child, oldest first.
func (f *childFixture) toParent() []contracts.MessageEnvelope {
	f.x.t.Helper()
	rows, err := f.x.db.Query(`SELECT envelope FROM messages WHERE recipient_kind='session' AND recipient_id=? AND sender_id=? ORDER BY inbox_cursor`,
		f.parent.Session.SessionID, f.child.Session.SessionID)
	if err != nil {
		f.x.t.Fatal(err)
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

func (f *childFixture) ownerInbox() int {
	var n int
	f.x.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE recipient_kind='owner'`).Scan(&n)
	return n
}

func (f *childFixture) statuses() []string {
	rows, _ := f.x.db.Query(`SELECT kind||':'||status||':'||COALESCE(detail,'') FROM child_reports ORDER BY report_id`)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func boolp(b bool) *bool { return &b }

func TestChildReportTagsAtStart(t *testing.T) {
	f := newChildFixture(t, nil)
	if !slices.Contains(f.child.Session.Tags, contracts.TagReportParent) || f.child.Session.Lineage.ParentSessionID != f.parent.Session.SessionID {
		t.Fatalf("child started by a session reports by default: %+v", f.child.Session)
	}
	for _, tg := range f.parent.Session.Tags {
		if strings.HasPrefix(tg, "report:") {
			t.Fatalf("owner-started session got a report tag: %v", f.parent.Session.Tags)
		}
	}
	off := f.startChild("op_synth-child-off", boolp(false))
	if !slices.Contains(off.Session.Tags, contracts.TagReportNone) || slices.Contains(off.Session.Tags, contracts.TagReportParent) {
		t.Fatalf("report_to_parent=false: %v", off.Session.Tags)
	}
	viaTag := f.startChild("op_synth-child-tag", nil, contracts.TagReportNone)
	if slices.Contains(viaTag.Session.Tags, contracts.TagReportParent) {
		t.Fatalf("report:none tag opts out: %v", viaTag.Session.Tags)
	}
	// An owner naming a parent explicitly does not opt in by default.
	// (The node's advertised capacity is 4 runs, so this is checked on
	// the tag rule directly.)
	if got := reportTags([]string{"x"}, f.parent.Session.SessionID, nil, false); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("owner start with parent: %v", got)
	}
	if got := reportTags([]string{contracts.TagReportParent}, f.parent.Session.SessionID, boolp(true), false); !slices.Equal(got, []string{contracts.TagReportParent}) {
		t.Fatalf("owner opt-in: %v", got)
	}
	if got := reportTags([]string{contracts.TagReportParent}, "", nil, true); len(got) != 0 {
		t.Fatalf("no parent, no report tag: %v", got)
	}
}

func TestChildReportIdleOncePerTurnWithSummary(t *testing.T) {
	f := newChildFixture(t, nil)
	// Launch without a turn (starting -> waiting_input) reports nothing.
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("idle launch reported %d", n)
	}
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle at prompt")
	f.observe(contracts.RunWaitingInput, "idle at prompt (re-observed)") // same turn: coalesced
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 {
		t.Fatalf("want one report, got %d: %v", len(msgs), f.statuses())
	}
	m := msgs[0]
	if m.Sender.Kind != contracts.PrincipalSession || m.Sender.ID != f.child.Session.SessionID || m.Mode != contracts.ModeQueue {
		t.Fatalf("report envelope: %+v", m)
	}
	for _, want := range []string{"finished its turn", "synthetic child", f.child.Session.SessionID, "Synthetic final answer: the tests pass.", "state: waiting_input", "Machine: "} {
		if !strings.Contains(m.Body.Text, want) {
			t.Fatalf("report text lacks %q:\n%s", want, m.Body.Text)
		}
	}
	// The parent run gets it through the normal delivery path.
	if v := f.x.msg(m.MessageID); v.Status.Runtime == contracts.DeliveryNotApplicable {
		t.Fatalf("report not queued for runtime delivery: %+v", v.Status)
	}
	// A second turn reports again, once.
	f.observe(contracts.RunRunning, "working on a reply")
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	f.process()
	if n := len(f.toParent()); n != 2 {
		t.Fatalf("second turn: %d reports", n)
	}
	if f.ownerInbox() != 0 {
		t.Fatal("owner inbox used")
	}
}

func TestChildReportApprovalThenIdleCoalesced(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	run := *f.x.session(f.child.Session.SessionID).Run
	ap := approvalFor(run, "apr_synth-child", "toolu_synth_c1", contracts.ApprovalPermission, f.x.clock.Now())
	f.x.report(nodeMac, contracts.ReportApproval, ap)
	f.observe(contracts.RunWaitingApproval, "permission prompt")
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatal("needs-input report sent before the settle delay")
	}
	f.x.clock.Add(3 * time.Second)
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body.Text, "needs input") || !strings.Contains(msgs[0].Body.Text, "apr_synth-child") ||
		!strings.Contains(msgs[0].Body.Text, "answer_approval") {
		t.Fatalf("needs-input report: %d %v", len(msgs), msgs)
	}
	// Answered: the same turn continues and ends with one idle report.
	f.observe(contracts.RunRunning, "approval answered")
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	msgs = f.toParent()
	if len(msgs) != 2 || !strings.Contains(msgs[1].Body.Text, "finished its turn") {
		t.Fatalf("after approval: %d %v", len(msgs), f.statuses())
	}
}

func TestChildReportNeedsInputSkippedWhenAnsweredMeanwhile(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingApproval, "permission prompt")
	f.observe(contracts.RunRunning, "answered in the terminal")
	f.x.clock.Add(3 * time.Second)
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("stale needs-input report sent: %v", f.statuses())
	}
}

func TestChildReportSkippedWhenChildAlreadyMessagedParent(t *testing.T) {
	f := newChildFixture(t, nil)
	// The parent asks; the child's turn answers with send_message reply_to.
	ask, err := f.x.h.SendMessage(as(sesPrincipal(f.parent.Session.SessionID)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-ask"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: f.child.Session.SessionID}, Body: contracts.MessageBody{Text: "status?"}})
	if err != nil {
		t.Fatal(err)
	}
	f.observe(contracts.RunRunning, "working on a message")
	if _, err := f.x.h.SendMessage(as(sesPrincipal(f.child.Session.SessionID)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-reply"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: f.parent.Session.SessionID}, InReplyTo: ask.Envelope.MessageID,
		Body: contracts.MessageBody{Text: "all green"}}); err != nil {
		t.Fatal(err)
	}
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 || msgs[0].Body.Text != "all green" {
		t.Fatalf("duplicate auto report: %d %v", len(msgs), f.statuses())
	}
	// The next turn, without a message of its own, is reported.
	f.x.clock.Add(time.Second)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	if n := len(f.toParent()); n != 2 {
		t.Fatalf("next turn: %d", n)
	}
}

func TestChildReportOptOut(t *testing.T) {
	f := newChildFixture(t, boolp(false))
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.observe(contracts.RunExited, "exited")
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("opted-out child reported %d", n)
	}
	var rows int
	f.x.db.QueryRow(`SELECT COUNT(*) FROM child_reports`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("opted-out child recorded %d reports", rows)
	}
}

func TestChildReportParentGone(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	archived := true
	if _, err := f.x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-archive-parent"},
		SessionID: f.parent.Session.SessionID, Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	f.process()
	if n := len(f.toParent()); n != 0 || f.ownerInbox() != 0 {
		t.Fatalf("report to an archived parent: %d, owner %d", n, f.ownerInbox())
	}
	if st := f.statuses(); len(st) != 1 || !strings.Contains(st[0], "skipped:parent session is archived") {
		t.Fatalf("statuses: %v", st)
	}

	// An ended (not archived) parent: nothing either.
	g := newChildFixture(t, nil)
	pr := *g.x.session(g.parent.Session.SessionID).Run
	pr.State, pr.StateReason = contracts.RunExited, "exited"
	g.x.report(nodeMac, contracts.ReportRun, pr)
	g.observe(contracts.RunRunning, "working")
	g.observe(contracts.RunExited, "process exited")
	g.process()
	if n := len(g.toParent()); n != 0 || g.ownerInbox() != 0 {
		t.Fatalf("report to an ended parent: %d", n)
	}
}

func TestChildReportExitAndFailure(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	r := *f.x.session(f.child.Session.SessionID).Run
	r.State, r.StateReason = contracts.RunFailed, "claude exited with an API error"
	code := 1
	r.ExitCode = &code
	f.x.report(nodeMac, contracts.ReportRun, r)
	f.x.report(nodeMac, contracts.ReportRun, r) // repeated observation
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 {
		t.Fatalf("want one exit report, got %d: %v", len(msgs), f.statuses())
	}
	for _, want := range []string{"has failed", "claude exited with an API error", "exit code 1", "Synthetic final answer"} {
		if !strings.Contains(msgs[0].Body.Text, want) {
			t.Fatalf("exit report lacks %q:\n%s", want, msgs[0].Body.Text)
		}
	}
}

func TestChildReportSummaryClippedAndUnavailable(t *testing.T) {
	f := newChildFixture(t, nil)
	f.x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		return strings.Repeat("é", 4000), nil
	}
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 || strings.Count(msgs[0].Body.Text, "é") != childSummaryMax || !strings.Contains(msgs[0].Body.Text, "…") {
		t.Fatalf("summary not clipped to %d runes", childSummaryMax)
	}
	f.x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		return "", contracts.Errorf(contracts.ErrUnavailable, "node offline")
	}
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	msgs = f.toParent()
	if len(msgs) != 2 || !strings.Contains(msgs[1].Body.Text, "unavailable") {
		t.Fatalf("unavailable summary: %v", msgs)
	}
}

func TestChildReportSurvivesRestartAndTagToggle(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.x.restart()
	f.x.connect(nodeMac, 0)
	f.x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) { return "after restart", nil }
	f.process()
	if msgs := f.toParent(); len(msgs) != 1 || !strings.Contains(msgs[0].Body.Text, "after restart") {
		t.Fatalf("pending report lost across restart: %v", f.statuses())
	}
	// Removing the tag turns reports off.
	tags := []string{}
	if _, err := f.x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-tags"},
		SessionID: f.child.Session.SessionID, Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	if n := len(f.toParent()); n != 1 {
		t.Fatalf("report after the tag was removed: %d", n)
	}
}

func TestChildReportRateLimited(t *testing.T) {
	f := newChildFixture(t, nil)
	for range childReportBurst + 3 {
		f.observe(contracts.RunRunning, "working")
		f.observe(contracts.RunWaitingInput, "idle")
		f.process()
	}
	if n := len(f.toParent()); n != childReportBurst {
		t.Fatalf("rate limit: %d reports", n)
	}
}

// A session may archive itself and the sessions it started, not unrelated
// ones; it may rename any session (not over the owner's title).
func TestSessionPrincipalArchiveAndRenameScope(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	parentP := sesPrincipal(f.parent.Session.SessionID)
	childP := sesPrincipal(f.child.Session.SessionID)
	other := x.start(owner, "op_synth-unrelated", nodeMac)
	yes, no := true, false
	upd := func(p contracts.Principal, op, sid string, archived *bool, title *string) error {
		_, err := x.h.UpdateSession(as(p), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: op}, SessionID: sid, Archived: archived, Title: title})
		return err
	}
	// Refused: the child archiving its parent, a session archiving an unrelated one.
	wantCode(t, upd(childP, "op_synth-a1", f.parent.Session.SessionID, &yes, nil), contracts.ErrForbidden)
	wantCode(t, upd(parentP, "op_synth-a2", other.Session.SessionID, &yes, nil), contracts.ErrForbidden)
	if x.session(other.Session.SessionID).Session.Archived || x.session(f.parent.Session.SessionID).Session.Archived {
		t.Fatal("refused archive took effect")
	}
	// Allowed: the parent archives (and unarchives) the session it started; a session archives itself.
	if err := upd(parentP, "op_synth-a3", f.child.Session.SessionID, &yes, nil); err != nil || !x.session(f.child.Session.SessionID).Session.Archived {
		t.Fatalf("parent archiving its child: %v", err)
	}
	if err := upd(parentP, "op_synth-a4", f.child.Session.SessionID, &no, nil); err != nil || x.session(f.child.Session.SessionID).Session.Archived {
		t.Fatalf("parent unarchiving its child: %v", err)
	}
	if err := upd(childP, "op_synth-a5", f.child.Session.SessionID, &yes, nil); err != nil {
		t.Fatalf("self archive: %v", err)
	}
	// Rename: any session, never over the owner's title. The title the
	// parent gave the child at launch is agent-chosen, so the parent can
	// rename its reviewer later; the unrelated session's title is automatic.
	if !slices.Contains(f.child.Session.Tags, "title:agent") {
		t.Fatalf("launch title chosen by a session not marked agent-chosen: %v", f.child.Session.Tags)
	}
	name := "Synthetic reviewer"
	if err := upd(parentP, "op_synth-r1", f.child.Session.SessionID, nil, &name); err != nil || x.session(f.child.Session.SessionID).Session.Title != name {
		t.Fatalf("parent renaming its child: %v", err)
	}
	if err := upd(childP, "op_synth-r2", other.Session.SessionID, nil, &name); err != nil || x.session(other.Session.SessionID).Session.Title != name {
		t.Fatalf("renaming an unrelated automatic session: %v", err)
	}
	mine := "Owner title"
	if err := upd(owner, "op_synth-r3", other.Session.SessionID, nil, &mine); err != nil {
		t.Fatal(err)
	}
	if err := upd(parentP, "op_synth-r4", other.Session.SessionID, nil, &name); err != nil {
		t.Fatalf("renaming over an owner title: %v", err)
	}
}
