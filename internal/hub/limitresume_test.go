package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Decision I70: a session that stopped on an account limit with no account
// free resumes by itself once capacity returns (iss_…
// left 18 sessions "resumable after" until someone resumed each).

type limitFixture struct {
	*childFixture
	mobot, me string
}

func newLimitFixture(t *testing.T) *limitFixture {
	t.Helper()
	f := &limitFixture{childFixture: newChildFixture(t, nil)}
	f.mobot, f.me = f.account("mobot"), f.account("me")
	f.serveLimitResume()
	return f
}

// serveLimitResume makes the node's stored hello advertise
// session.limit_resume (a pyred with this change).
func (f *limitFixture) serveLimitResume() {
	f.x.t.Helper()
	if _, err := f.x.db.Exec(`UPDATE node_state SET hello=json_set(hello,'$.capabilities',json(?)) WHERE node_id=?`,
		`["local_api","`+contracts.NodeCapLimitResume+`"]`, nodeMac); err != nil {
		f.x.t.Fatal(err)
	}
}

func (f *limitFixture) processResumes() {
	f.x.t.Helper()
	if err := f.x.h.ProcessLimitResumes(context.Background()); err != nil {
		f.x.t.Fatal(err)
	}
}

// resumeRequests lists the session.limit_resume requests sent to the node.
func (f *limitFixture) resumeRequests() []contracts.NodeCommand {
	var out []contracts.NodeCommand
	for _, c := range f.x.ch.sentKinds(nodeMac, contracts.CmdAction) {
		var req contracts.NodeActionRequest
		if json.Unmarshal(c.Payload, &req) == nil && req.Action == contracts.ActionSessionLimitResume {
			out = append(out, c)
		}
	}
	return out
}

func resumeParams(t *testing.T, c contracts.NodeCommand) contracts.LimitResumeParams {
	t.Helper()
	var req contracts.NodeActionRequest
	var p contracts.LimitResumeParams
	if err := json.Unmarshal(c.Payload, &req); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *limitFixture) row(sessionID string) limitResume {
	f.x.t.Helper()
	r, ok, err := f.x.h.getLimitResume(context.Background(), f.x.db, sessionID)
	if err != nil || !ok {
		f.x.t.Fatalf("no limit resume for %s: %v", sessionID, err)
	}
	return r
}

func (f *limitFixture) noRow(sessionID string) bool {
	_, ok, err := f.x.h.getLimitResume(context.Background(), f.x.db, sessionID)
	return err == nil && !ok
}

// runOf is the session's current run.
func (f *limitFixture) runOf(sessionID string) contracts.Run {
	f.x.t.Helper()
	return *f.x.session(sessionID).Run
}

// stopNoRoom reports what pyred does when sessionID's turn hits mobot's
// spend limit and no account has room: the limit-switch stop (the run
// ends, marked) and the waiting plan naming the earliest reset.
func (f *limitFixture) stopNoRoom(sessionID string, reset time.Time) string {
	f.x.t.Helper()
	run := f.exitForLimit(sessionID)
	f.planNoRoom(sessionID, run, reset)
	return run
}

// exitForLimit reports the limit-switch stop of sessionID's run (the run
// ends, marked).
func (f *limitFixture) exitForLimit(sessionID string) string {
	f.x.t.Helper()
	r := f.runOf(sessionID)
	r.State, r.StateReason = contracts.RunRunning, "working"
	f.x.report(nodeMac, contracts.ReportRun, r)
	at, zero := f.x.clock.Now(), 0
	r.State, r.StateReason, r.ExitCode, r.EndedAt = contracts.RunExited, "stopped by pyre (/exit)", &zero, &at
	r.AdapterData = map[string]string{contracts.AdapterDataAccountSwitch: contracts.AccountSwitchLimit}
	f.x.report(nodeMac, contracts.ReportRun, r)
	return r.RunID
}

// planNoRoom reports the waiting plan naming the earliest reset.
func (f *limitFixture) planNoRoom(sessionID, run string, reset time.Time) {
	f.x.t.Helper()
	lim := spendLimit(reset)
	lim.At = f.x.clock.Now()
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sessionID, FromRunID: run, FromAccountID: f.mobot,
		Outcome: contracts.FallbackWaiting, PlannedAt: f.x.clock.Now(), Limit: lim,
		Reason: "no eligible anthropic subscription; work preserved and resumable; earliest reset " + reset.UTC().Format(time.RFC3339) + " (account \"me\": cooldown)"})
}

// successorOf reports the run pyred's continuation started.
func (f *limitFixture) successorOf(sessionID, prev, runID string) {
	f.x.t.Helper()
	old := f.runOf(sessionID)
	f.x.report(nodeMac, contracts.ReportRun, contracts.Run{RunID: runID, SessionID: sessionID, NodeID: nodeMac, Adapter: "claude-synth", Harness: contracts.HarnessClaude,
		Cwd: old.Cwd, State: contracts.RunStarting, StateReason: "launching", StartedAt: f.x.clock.Now(), Capabilities: old.Capabilities,
		PreviousRunID: prev, ContinuationKind: contracts.ContinuationNativeResume, AccountID: f.me})
}

// answer reports the node's final result of a limit resume request.
func (f *limitFixture) answer(c contracts.NodeCommand, res contracts.LimitResumeResult) {
	f.x.t.Helper()
	f.x.result(nodeMac, c, contracts.OutcomeSucceeded, res)
}

// The waiting session resumes at its reset: the parent's report says it
// resumes automatically, nothing is asked before the reset, the node is
// asked once just after it, and once the continuation holds the parent
// gets one "resumed on account" line and the resume is audited.
func TestLimitResumeAtReset(t *testing.T) {
	f := newLimitFixture(t)
	sid := f.child.Session.SessionID
	reset := f.x.clock.Now().Add(80 * time.Minute).UTC().Truncate(time.Second)
	run := f.stopNoRoom(sid, reset)
	f.process()
	text := f.lastReport()
	t.Logf("report:\n%s", text)
	want := `synthetic child stopped on "mobot"'s spend limit; no other account has room; resumes automatically after ` + askClock(reset)
	if lead, _, _ := strings.Cut(text, "\n"); lead != want {
		t.Fatalf("lead %q, want %q", lead, want)
	}
	if strings.Contains(text, "does not restart it by itself") {
		t.Fatalf("report still says Pyre does not restart it:\n%s", text)
	}

	f.processResumes()
	f.x.clock.Add(80*time.Minute + limitResumeSlack - time.Second)
	f.processResumes()
	if n := len(f.resumeRequests()); n != 0 {
		t.Fatalf("%d requests before the reset", n)
	}
	f.x.clock.Add(2 * time.Second)
	f.processResumes()
	reqs := f.resumeRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests at the reset, want 1", len(reqs))
	}
	if p := resumeParams(t, reqs[0]); p.SessionID != sid || p.RunID != run || p.Mode != contracts.LimitResumeContinue {
		t.Fatalf("params %+v", p)
	}
	f.processResumes()
	if n := len(f.resumeRequests()); n != 1 {
		t.Fatalf("asked again while the first request is open: %d", n)
	}

	f.successorOf(sid, run, "run_synth-resumed")
	f.answer(reqs[0], contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: f.me, NewRunID: "run_synth-resumed", Kind: string(contracts.ContinuationNativeResume)})
	f.processResumes()
	if r := f.row(sid); r.State != lrWatching || r.ResumedRun != "run_synth-resumed" {
		t.Fatalf("row %+v", r)
	}
	e := findAudit(t, f.x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionLimitResumed)
	wantSummary(t, e, "automatically after an account limit", `on account "me"`, `after "mobot"'s spend limit`)

	before := len(f.live())
	f.process()
	if len(f.live()) != before {
		t.Fatal("the resumed line went out before the resume held")
	}
	f.x.clock.Add(limitResumeConfirm + time.Second)
	f.process()
	msgs := f.live()
	if len(msgs) != before+1 || !strings.HasPrefix(msgs[len(msgs)-1].Body.Text, `synthetic child resumed on account "me"`) {
		t.Fatalf("reports %d (before %d): %v", len(msgs), before, f.statuses())
	}
	t.Logf("resumed line:\n%s", msgs[len(msgs)-1].Body.Text)

	f.x.clock.Add(limitResumeWatch)
	f.processResumes()
	if r := f.row(sid); r.State != lrDone {
		t.Fatalf("after the watch: %+v", r)
	}
	if n := len(f.resumeRequests()); n != 1 {
		t.Fatalf("%d requests in all", n)
	}
}

// Another account regaining room on the node brings the resume forward:
// the node is asked at once, hours before the reset.
func TestLimitResumeEarlyWhenAccountFrees(t *testing.T) {
	f := newLimitFixture(t)
	sid := f.child.Session.SessionID
	reset := f.x.clock.Now().Add(3 * time.Hour)
	f.stopNoRoom(sid, reset)
	obs := func(el contracts.Eligibility) {
		f.x.report(nodeMac, contracts.ReportAccountObs, contracts.AccountObservation{AccountID: f.me, Provider: contracts.ProviderAnthropic, ReportingNode: nodeMac,
			ObservedAt: f.x.clock.Now(), Source: "usage-poll", Confidence: "high", Eligibility: el})
	}
	f.x.clock.Add(5 * time.Minute)
	obs(contracts.EligibleExhausted)
	f.processResumes()
	if n := len(f.resumeRequests()); n != 0 {
		t.Fatalf("asked while every account is out: %d", n)
	}
	f.x.clock.Add(10 * time.Minute)
	obs(contracts.EligibleYes)
	f.processResumes()
	if n := len(f.resumeRequests()); n != 1 {
		t.Fatalf("%d requests after %q regained room, want 1", n, "me")
	}

	// The node found no room after all (a cooldown it knows of): asked
	// again at the reset it names, and a flapping observation does not
	// ask again meanwhile.
	f.answer(f.resumeRequests()[0], contracts.LimitResumeResult{Outcome: contracts.LimitResumeNoRoom, ResetAt: &reset, Detail: "account \"me\": cooldown"})
	f.processResumes()
	if r := f.row(sid); r.State != lrScheduled || !r.DueAt.Equal(reset.UTC().Add(limitResumeSlack)) {
		t.Fatalf("after no room: %+v", r)
	}
	f.x.clock.Add(time.Minute)
	obs(contracts.EligibleUnknown)
	obs(contracts.EligibleYes)
	f.processResumes()
	if n := len(f.resumeRequests()); n != 1 {
		t.Fatalf("asked again within the early gap: %d", n)
	}
}

// No automatic resume for a session that was archived, one the owner
// typed into or messaged, one another session messaged, or a stop that
// was not a limit.
func TestLimitResumeLeavesSessionsAlone(t *testing.T) {
	cases := []struct {
		name string
		act  func(f *limitFixture, sid string)
	}{
		{"archived", func(f *limitFixture, sid string) {
			yes := true
			if _, err := f.x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-archive"}, SessionID: sid, Archived: &yes}); err != nil {
				t.Fatal(err)
			}
		}},
		{"owner typed", func(f *limitFixture, sid string) {
			if _, err := f.x.h.markOwnerActive(context.Background(), sid, f.x.clock.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"parent messaged", func(f *limitFixture, sid string) {
			if _, err := f.x.h.SendMessage(as(sesPrincipal(f.parent.Session.SessionID)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-nudge"},
				Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "synthetic: hold off, I will take this over"}}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newLimitFixture(t)
			sid := f.child.Session.SessionID
			reset := f.x.clock.Now().Add(time.Hour)
			f.stopNoRoom(sid, reset)
			f.x.clock.Add(time.Minute)
			c.act(f, sid)
			f.x.clock.Add(time.Hour)
			f.processResumes()
			if n := len(f.resumeRequests()); n != 0 {
				t.Fatalf("%d requests", n)
			}
			if r := f.row(sid); r.State != lrCancelled {
				t.Fatalf("row %+v", r)
			}
			t.Logf("dropped: %s", f.row(sid).Detail)
		})
	}

	t.Run("not a limit", func(t *testing.T) {
		f := newLimitFixture(t)
		sid := f.child.Session.SessionID
		run := f.switchExit(contracts.AccountSwitchLimit)
		f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, FromAccountID: f.mobot,
			Outcome: contracts.FallbackIntervention, PlannedAt: f.x.clock.Now(), Limit: &contracts.LimitEvent{Kind: "auth_invalid", Detail: "synthetic: login rejected"},
			Reason: "run used the ambient default login; automatic account fallback is disabled for ambient runs"})
		// A waiting plan from an older pyred (no limit recorded) is not one either.
		f2 := newLimitFixture(t)
		sid2 := f2.child.Session.SessionID
		run2 := f2.switchExit(contracts.AccountSwitchLimit)
		f2.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid2, FromRunID: run2, FromAccountID: f2.mobot,
			Outcome: contracts.FallbackWaiting, PlannedAt: f2.x.clock.Now(), Reason: "no eligible anthropic subscription; earliest reset " + f2.x.clock.Now().Add(time.Hour).Format(time.RFC3339)})
		f.x.clock.Add(2 * time.Hour)
		f2.x.clock.Add(2 * time.Hour)
		f.processResumes()
		f2.processResumes()
		if !f.noRow(sid) || !f2.noRow(sid2) || len(f.resumeRequests()) != 0 || len(f2.resumeRequests()) != 0 {
			t.Fatal("a stop that was not a usage limit was scheduled")
		}
	})

	t.Run("off", func(t *testing.T) {
		f := newLimitFixture(t)
		f.x.opts.LimitAutoResumeOff = true
		f.x.restart()
		f.x.connect(nodeMac, 0, f.runOf(f.parent.Session.SessionID), f.runOf(f.child.Session.SessionID))
		f.serveLimitResume()
		sid := f.child.Session.SessionID
		reset := f.x.clock.Now().Add(time.Hour).UTC().Truncate(time.Second)
		f.stopNoRoom(sid, reset)
		f.process()
		if text := f.lastReport(); !strings.Contains(text, "resumable after "+askClock(reset)) {
			t.Fatalf("off: report\n%s", text)
		}
		f.x.clock.Add(2 * time.Hour)
		f.processResumes()
		if !f.noRow(sid) || len(f.resumeRequests()) != 0 {
			t.Fatal("scheduled with the switch off")
		}
	})
}

// An account reset frees many sessions at once: they are resumed
// LimitResumeGap apart, leads before their children.
func TestLimitResumeStaggered(t *testing.T) {
	f := newLimitFixture(t)
	reset := f.x.clock.Now().Add(time.Hour)
	ids := []string{f.child.Session.SessionID}
	for _, op := range []string{"op_synth-child2", "op_synth-child3"} { // the node runs 4
		ids = append(ids, f.startChild(op, nil).Session.SessionID)
	}
	f.x.clock.Add(time.Second)
	for _, id := range ids {
		f.stopNoRoom(id, reset)
	}
	f.stopNoRoom(f.parent.Session.SessionID, reset)
	f.x.clock.Add(time.Hour + limitResumeSlack)
	var at []time.Time
	for i := 0; i < 60 && len(f.resumeRequests()) < 4; i++ {
		n := len(f.resumeRequests())
		f.processResumes()
		if len(f.resumeRequests()) > n {
			at = append(at, f.x.clock.Now())
		}
		f.x.clock.Add(time.Second)
	}
	if len(at) != 4 {
		t.Fatalf("%d of 4 sessions resumed", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < defaultLimitResumeGap {
			t.Fatalf("resume %d came %s after the previous one", i, gap)
		}
	}
	if p := resumeParams(t, f.resumeRequests()[0]); p.SessionID != f.parent.Session.SessionID {
		t.Fatalf("first resumed %s, want the lead %s", p.SessionID, f.parent.Session.SessionID)
	}
}

// A continuation that hits the limit again at once is retried after a
// growing backoff, not in a loop; its "resumed" line is not sent; after
// limitResumeMaxRepeats repeats Pyre gives up and says so once.
func TestLimitResumeBacksOffOnRepeat(t *testing.T) {
	f := newLimitFixture(t)
	sid := f.child.Session.SessionID
	reset := f.x.clock.Now().Add(time.Hour)
	run := f.stopNoRoom(sid, reset)
	f.process()
	f.x.clock.Add(time.Hour + limitResumeSlack)
	for i := 1; i <= limitResumeMaxRepeats+1; i++ {
		f.processResumes()
		reqs := f.resumeRequests()
		if len(reqs) != i {
			t.Fatalf("attempt %d: %d requests", i, len(reqs))
		}
		next := "run_synth-again-" + string(rune('a'+i))
		f.successorOf(sid, run, next)
		f.answer(reqs[i-1], contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: f.me, NewRunID: next, Kind: string(contracts.ContinuationNativeResume)})
		f.processResumes()
		// The continuation's first turn hits the limit right away.
		f.x.clock.Add(20 * time.Second)
		again := f.x.clock.Now().Add(5 * time.Minute)
		run = f.exitForLimit(sid)
		f.processResumes() // the continuation ended before its plan arrived
		f.x.clock.Add(time.Second)
		f.planNoRoom(sid, run, again)
		f.process()
		r := f.row(sid)
		if i > limitResumeMaxRepeats {
			if r.State != lrGaveUp {
				t.Fatalf("after %d repeats: %+v", i, r)
			}
			break
		}
		if r.State != lrScheduled || r.Repeats != i || r.RunID != run {
			t.Fatalf("repeat %d: %+v", i, r)
		}
		wait := limitResumeBackoffFor(i)
		if got := r.DueAt.Sub(f.x.clock.Now()); got < wait-time.Second {
			t.Fatalf("repeat %d retried after %s, want at least %s", i, got, wait)
		}
		f.processResumes()
		if n := len(f.resumeRequests()); n != i {
			t.Fatalf("repeat %d asked again at once", i)
		}
		f.x.clock.Add(wait)
	}
	f.x.clock.Add(limitResumeConfirm + time.Second)
	f.process()
	var resumed, gaveUp, promised int
	for _, m := range f.allToParent() { // a digest when several are ready
		resumed += strings.Count(m.Envelope.Body.Text, "resumed on account")
		promised += strings.Count(m.Envelope.Body.Text, "; resumes automatically after")
		if n := strings.Count(m.Envelope.Body.Text, "Pyre stopped resuming it automatically"); n > 0 {
			gaveUp += n
			t.Logf("gave up:\n%s", m.Envelope.Body.Text)
		}
	}
	if resumed != 0 || gaveUp != 1 || promised != 1 {
		t.Fatalf("resumed lines %d, give-up lines %d, stop reports %d: %v", resumed, gaveUp, promised, f.statuses())
	}
}

// The timer survives a hub restart: armed before it, the session is still
// resumed at the reset after it, and an answer that arrives after another
// restart is applied.
func TestLimitResumeDurableAcrossHubRestart(t *testing.T) {
	f := newLimitFixture(t)
	sid := f.child.Session.SessionID
	reset := f.x.clock.Now().Add(time.Hour)
	run := f.stopNoRoom(sid, reset)
	reconnect := func() {
		f.x.restart()
		f.x.connect(nodeMac, 0, f.runOf(f.parent.Session.SessionID))
		f.serveLimitResume()
	}
	reconnect()
	f.x.clock.Add(time.Hour + limitResumeSlack)
	f.processResumes()
	reqs := f.resumeRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests after the restart", len(reqs))
	}
	reconnect()
	f.successorOf(sid, run, "run_synth-after-restart")
	f.answer(reqs[0], contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: f.me, NewRunID: "run_synth-after-restart", Kind: string(contracts.ContinuationNativeResume)})
	f.processResumes()
	if r := f.row(sid); r.State != lrWatching || r.ResumedRun != "run_synth-after-restart" {
		t.Fatalf("row %+v", r)
	}
}

// A paused switch leaves the run up on its account: at that account's
// reset the node is asked to continue it in place; a turn that hits the
// limit again (a newer pyre.limit_at) is backed off.
func TestLimitResumeInPlace(t *testing.T) {
	f := newLimitFixture(t)
	sid := f.child.Session.SessionID
	reset := f.x.clock.Now().Add(90 * time.Minute)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "turn failed (StopFailure)")
	r := f.runOf(sid)
	lim := spendLimit(reset)
	lim.At = f.x.clock.Now()
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: r.RunID, FromAccountID: f.mobot,
		Outcome: contracts.FallbackIntervention, PlannedAt: f.x.clock.Now(), Limit: lim,
		Reason: "account switching paused: repeated limits within 30m (last automatic switch 23:14Z)"})
	f.x.clock.Add(limitPlanRecheck + time.Second)
	f.process()
	text := f.lastReport()
	t.Logf("report:\n%s", text)
	if lead, _, _ := strings.Cut(text, "\n"); !strings.HasSuffix(lead, "automatic account switching is paused; resumes automatically after "+askClock(reset)) {
		t.Fatalf("lead %q", lead)
	}
	f.x.clock.Add(90*time.Minute + limitResumeSlack)
	f.processResumes()
	reqs := f.resumeRequests()
	if len(reqs) != 1 || resumeParams(t, reqs[0]).Mode != contracts.LimitResumeInPlace {
		t.Fatalf("requests %d", len(reqs))
	}
	f.answer(reqs[0], contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: f.mobot, NewRunID: r.RunID, Kind: contracts.LimitResumeInPlace})
	f.processResumes()

	// The continue prompt's turn fails on the limit again.
	f.x.clock.Add(30 * time.Second)
	r = f.runOf(sid)
	r.State, r.StateReason = contracts.RunRunning, "working"
	f.x.report(nodeMac, contracts.ReportRun, r)
	r.State, r.StateReason = contracts.RunWaitingInput, "turn failed (StopFailure)"
	r.AdapterData = map[string]string{contracts.AdapterDataLimitAt: persistence.Time(f.x.clock.Now())}
	f.x.report(nodeMac, contracts.ReportRun, r)
	f.processResumes()
	if row := f.row(sid); row.State != lrScheduled || row.Repeats != 1 || row.DueAt.Before(f.x.clock.Now().Add(limitResumeBackoff-time.Second)) {
		t.Fatalf("after a repeat: %+v", row)
	}
	f.x.clock.Add(limitResumeConfirm + time.Second)
	f.process()
	for _, m := range f.allToParent() {
		if strings.Contains(m.Envelope.Body.Text, "resumed on account") {
			t.Fatalf("resumed line for a resume that hit the limit again:\n%s", m.Envelope.Body.Text)
		}
	}
}

// The continuation's own limit plan arrives before the node's answer to
// the request that started it: still a repeat (backoff, audit), and the
// late answer changes nothing.
func TestLimitResumeRepeatBeforeAnswer(t *testing.T) {
	f := newLimitFixture(t)
	sid := f.child.Session.SessionID
	run := f.stopNoRoom(sid, f.x.clock.Now().Add(time.Hour))
	f.x.clock.Add(time.Hour + limitResumeSlack)
	f.processResumes()
	reqs := f.resumeRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	f.successorOf(sid, run, "run_synth-early")
	f.x.clock.Add(10 * time.Second)
	next := f.stopNoRoom(sid, f.x.clock.Now().Add(5*time.Minute))
	if r := f.row(sid); r.State != lrScheduled || r.Repeats != 1 || r.RunID != next {
		t.Fatalf("row %+v", r)
	}
	findAudit(t, f.x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionLimitResumed)
	f.answer(reqs[0], contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: f.me, NewRunID: "run_synth-early"})
	f.processResumes()
	if r := f.row(sid); r.State != lrScheduled || r.Repeats != 1 {
		t.Fatalf("after the late answer: %+v", r)
	}
}

// In place: the owner typed into the session just before the stop (his
// typing is marked once a minute, so nothing newer shows): left alone.
func TestLimitResumeInPlaceOwnerJustBefore(t *testing.T) {
	f := newLimitFixture(t)
	sid := f.child.Session.SessionID
	if _, err := f.x.h.markOwnerActive(context.Background(), sid, f.x.clock.Now()); err != nil {
		t.Fatal(err)
	}
	f.x.clock.Add(30 * time.Second)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "turn failed (StopFailure)")
	reset := f.x.clock.Now().Add(time.Hour)
	lim := spendLimit(reset)
	lim.At = f.x.clock.Now()
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: f.runOf(sid).RunID, FromAccountID: f.mobot,
		Outcome: contracts.FallbackIntervention, PlannedAt: f.x.clock.Now(), Limit: lim, Reason: "account switching paused: repeated limits within 30m"})
	f.x.clock.Add(time.Hour + limitResumeSlack)
	f.processResumes()
	if n := len(f.resumeRequests()); n != 0 || f.row(sid).State != lrCancelled {
		t.Fatalf("%d requests, row %+v", n, f.row(sid))
	}
}
