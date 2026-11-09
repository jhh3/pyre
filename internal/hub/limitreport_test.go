package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Reports of turns that failed on an account limit
// (iss_…, iss_…).

const spendDetail = "rate_limit: You've hit your individual spend limit · ask your admin to raise it at https://claude.ai/admin-settings/usage · your session limit resets 1am (UTC)"

func spendLimit(reset time.Time) *contracts.LimitEvent {
	return &contracts.LimitEvent{Kind: "usage_limit", Detail: spendDetail, ResetAt: &reset}
}

// A paused switch: the idle report of the failed turn waits for the node's
// plan, then says which limit stopped the session, once; later failed
// turns on the same run (its own monitors wake it) are not reported.
func TestChildReportLimitPausedReportedOnce(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	mobot := f.account("mobot")
	run := f.x.session(sid).Run.RunID
	reset := f.x.clock.Now().Add(90 * time.Minute)

	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "turn failed (StopFailure)")
	f.process()
	if msgs := f.live(); len(msgs) != 0 {
		t.Fatalf("reported before the plan arrived: %s", msgs[0].Body.Text)
	}
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, FromAccountID: mobot,
		Outcome: contracts.FallbackIntervention, PlannedAt: f.x.clock.Now(), Limit: spendLimit(reset),
		Reason: "account switching paused: repeated limits within 30m (last automatic switch 23:14Z)"})
	f.x.clock.Add(limitPlanRecheck + time.Second)
	f.process()
	text := f.lastReport()
	t.Logf("report:\n%s", text)
	if lead, _, _ := strings.Cut(text, "\n"); lead != `synthetic child stopped on "mobot"'s spend limit; automatic account switching is paused` {
		t.Fatalf("lead %q", lead)
	}
	for _, want := range []string{
		`is stopped on account "mobot": its turn failed on the account's spend limit (resets ` + askClock(reset) + `)`,
		"repeated limits within 30m",
		"It is not finished.",
		"Pyre holds its messages until the reset.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, " finished:") || strings.Contains(text, "finished its turn") {
		t.Fatalf("a limit read as a finish:\n%s", text)
	}

	// Its background monitor wakes it every few seconds; every turn fails.
	for i := 0; i < 5; i++ {
		f.x.clock.Add(20 * time.Second)
		f.observe(contracts.RunRunning, "working")
		f.observe(contracts.RunWaitingInput, "turn failed (StopFailure)")
		f.x.clock.Add(limitPlanRecheck + time.Second)
		f.process()
	}
	if n := len(f.live()); n != 1 {
		t.Fatalf("%d reports to the parent, want 1: %v", n, f.statuses())
	}
}

// No account has room: the run was stopped for the switch, and the ended
// report says which limit stopped it and when an account has room again.
func TestChildReportLimitNoRoom(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	mobot := f.account("mobot")
	f.observe(contracts.RunRunning, "working")
	run := f.switchExit(contracts.AccountSwitchLimit)
	reset := f.x.clock.Now().Add(80 * time.Minute).UTC().Truncate(time.Second)
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, FromAccountID: mobot,
		Outcome: contracts.FallbackWaiting, PlannedAt: f.x.clock.Now(), Limit: spendLimit(reset.Add(10 * time.Minute)),
		Reason: "no eligible anthropic subscription; work preserved and resumable; earliest reset " + reset.Format(time.RFC3339) + " (account \"code\": cooldown)"})
	f.process()
	text := f.lastReport()
	t.Logf("report:\n%s", text)
	want := `synthetic child stopped on "mobot"'s spend limit; no other account has room; resumable after ` + askClock(reset)
	if lead, _, _ := strings.Cut(text, "\n"); lead != want {
		t.Fatalf("lead %q, want %q", lead, want)
	}
	for _, want := range []string{"It is not finished and Pyre does not archive it.", "resume it after " + askClock(reset)} {
		if !strings.Contains(text, want) {
			t.Fatalf("report lacks %q:\n%s", want, text)
		}
	}
}

// An agy child on the machine's own login (iss_…):
// its main conversation stops with subagents still running
// (waiting_on_background: no report), then every turn fails on the Google
// quota. The parent reads that it stopped on the quota and was left
// running, never "finished its turn".
func TestChildReportAgyOwnLoginQuota(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	run := f.x.session(sid).Run.RunID
	reset := f.x.clock.Now().Add(13*time.Hour + 20*time.Minute)

	f.observe(contracts.RunRunning, "turn in progress")
	f.observe(contracts.RunWaitingBackground, "turn failed (ERROR: usage_limit); 2 subagent(s) may start a new turn")
	f.x.clock.Add(limitPlanWait + time.Second)
	f.process()
	if msgs := f.live(); len(msgs) != 0 {
		t.Fatalf("reported while subagents ran: %s", msgs[0].Body.Text)
	}
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run,
		Outcome: contracts.FallbackIntervention, PlannedAt: f.x.clock.Now(),
		Limit:  &contracts.LimitEvent{Kind: "usage_limit", Detail: "Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 13h20m47s.", ResetAt: &reset},
		Reason: ownLoginLimitPrefix + ": agy runs on this machine's own login, which Pyre does not manage; the run stays up and its turns fail until the limit resets"})
	f.observe(contracts.RunRunning, "turn in progress")
	f.observe(contracts.RunWaitingInput, "turn failed (ERROR: usage_limit)")
	f.process()
	text := f.lastReport()
	t.Logf("report:\n%s", text)
	if lead, _, _ := strings.Cut(text, "\n"); lead != `synthetic child stopped on "this machine's own login"'s quota` {
		t.Fatalf("lead %q", lead)
	}
	for _, want := range []string{
		`is stopped on account "this machine's own login": its turn failed on the account's quota (resets ` + askClock(reset) + `)`,
		"Pyre neither stopped nor moved it",
		"It is not finished",
		"Pyre holds its messages until the reset.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "finished its turn") || strings.Contains(text, "could not move it") {
		t.Fatalf("wrong wording:\n%s", text)
	}
}

// A failed turn that was no limit (no plan within limitPlanWait) is
// reported as before, after the wait.
func TestChildReportFailedTurnWithoutPlan(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "turn failed (StopFailure)")
	f.process()
	if len(f.live()) != 0 {
		t.Fatal("reported before the wait ended")
	}
	f.x.clock.Add(limitPlanWait + time.Second)
	f.process()
	if text := f.lastReport(); !strings.HasPrefix(text, "synthetic child finished") {
		t.Fatalf("report:\n%s", text)
	}
}

// A failed turn whose limit switched the session: only the restart report
// goes out.
func TestChildReportLimitSwitchNoIdleReport(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	mobot, me := f.account("mobot"), f.account("me")
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "turn failed (StopFailure)")
	f.process()
	run := f.switchExit(contracts.AccountSwitchLimit)
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, FromAccountID: mobot, ToAccountID: me,
		Outcome: contracts.FallbackNativeResume, NewRunID: "run_synth-next", PlannedAt: f.x.clock.Now(), Limit: spendLimit(f.x.clock.Now().Add(time.Hour)),
		Reason: "usage_limit on mobot; me most headroom; native resume over the shared transcript store"})
	f.x.clock.Add(limitPlanRecheck + time.Second)
	f.process()
	msgs := f.live()
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Body.Text, `synthetic child restarted on account "me" (account switch)`) {
		t.Fatalf("reports %d: %v", len(msgs), f.statuses())
	}
}
