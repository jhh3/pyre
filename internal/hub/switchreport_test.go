package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Decision I67: the ended report of a run Pyre stopped for an account
// switch says where the session went and what the switch ended, never that
// Pyre archives it (iss_…).

func (f *childFixture) account(label string) string {
	f.x.t.Helper()
	a, err := f.x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: "op_acct-" + label},
		Provider: contracts.ProviderAnthropic, Label: label})
	if err != nil {
		f.x.t.Fatal(err)
	}
	return a.AccountID
}

// switchExit reports the child's run ended by pyred's stop for an account
// switch (marked before the stop, as QuiesceRun does).
func (f *childFixture) switchExit(kind string) string {
	f.x.t.Helper()
	r := *f.x.session(f.child.Session.SessionID).Run
	at, zero := f.x.clock.Now(), 0
	r.State, r.StateReason, r.ExitCode, r.EndedAt = contracts.RunExited, "stopped by pyre (/exit)", &zero, &at
	r.AdapterData = map[string]string{contracts.AdapterDataAccountSwitch: kind}
	f.x.report(nodeMac, contracts.ReportRun, r)
	return r.RunID
}

func (f *childFixture) lastReport() string {
	f.x.t.Helper()
	msgs := f.live()
	if len(msgs) == 0 {
		f.x.t.Fatalf("no report: %v", f.statuses())
	}
	return msgs[len(msgs)-1].Body.Text
}

// A proactive move between turns: the report waits for the node's plan,
// then says "restarted on account X (account switch)" and that nothing was
// lost; Pyre does not archive the session.
func TestChildReportProactiveSwitchRestart(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	mobot, jh := f.account("mobot"), f.account("personal")
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "turn ended")
	f.x.clock.Add(2 * time.Minute)
	run := f.switchExit(contracts.AccountSwitchProactive)

	f.process()
	if msgs := f.live(); len(msgs) != 0 {
		t.Fatalf("reported before the plan arrived: %s", msgs[0].Body.Text)
	}
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, FromAccountID: mobot, ToAccountID: jh,
		Outcome: contracts.FallbackNativeResume, NewRunID: "run_synth-next", PlannedAt: f.x.clock.Now(),
		Reason: `proactive move between turns: account "mobot" at 5h 91% used, weekly 75%; account "personal" (5h 75%)`})
	f.x.clock.Add(switchPlanRecheck + time.Second)
	f.process()

	text := f.lastReport()
	t.Logf("report:\n%s", text)
	if lead, _, _ := strings.Cut(text, "\n"); lead != `synthetic child restarted on account "personal" (account switch)` {
		t.Fatalf("lead %q", lead)
	}
	for _, want := range []string{
		`was restarted on account "personal" (account switch from "mobot"; same session, conversation kept)`,
		"It is not finished and Pyre does not archive it.",
		"Nothing was lost",
		`Why: proactive move between turns: account "mobot" at 5h 91% used`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"archives it in about five minutes", "has exited"} {
		if strings.Contains(text, bad) {
			t.Fatalf("report says %q:\n%s", bad, text)
		}
	}
	f.x.clock.Add(time.Hour)
	if got, _ := f.x.h.ArchiveFinishedChildren(context.Background()); len(got) != 0 {
		t.Fatalf("archived a switched session: %v", got)
	}
}

// A limit switch that ended background tasks says what was lost.
func TestChildReportLimitSwitchLostTasks(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	mobot, jh := f.account("mobot"), f.account("personal")
	f.observe(contracts.RunRunning, "working")
	run := f.switchExit(contracts.AccountSwitchLimit)
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, FromAccountID: mobot, ToAccountID: jh,
		Outcome: contracts.FallbackNativeResume, NewRunID: "run_synth-next", PlannedAt: f.x.clock.Now(), UncertainWork: true,
		StoppedTasks: []string{"shell: until grep -q synced /tmp/synthetic.log"},
		Reason:       "usage_limit on mobot; personal eligible; native resume; background tasks were stopped by the account switch: shell: until grep -q synced /tmp/synthetic.log"})
	f.process()

	text := f.lastReport()
	t.Logf("report:\n%s", text)
	for _, want := range []string{
		`synthetic child restarted on account "personal" (account switch)`,
		"Lost: background tasks were stopped by the account switch (shell: until grep -q synced /tmp/synthetic.log)",
		"re-check its jobs",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "archives it") {
		t.Fatalf("report:\n%s", text)
	}
}

// No plan within switchPlanWait: the report goes out saying so.
func TestChildReportSwitchWithoutPlan(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	f.switchExit(contracts.AccountSwitchLimit)
	f.process()
	if len(f.live()) != 0 {
		t.Fatal("reported before the wait ended")
	}
	f.x.clock.Add(switchPlanWait + time.Second)
	f.process()
	text := f.lastReport()
	if lead, _, _ := strings.Cut(text, "\n"); lead != "synthetic child was stopped for an account switch" || !strings.Contains(text, "has not said where it went") || strings.Contains(text, "archives it in about") {
		t.Fatalf("report:\n%s", text)
	}
}

// A switch that did not complete is not reported as a restart.
func TestChildReportSwitchThatFailed(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	mobot := f.account("mobot")
	f.observe(contracts.RunRunning, "working")
	run := f.switchExit(contracts.AccountSwitchLimit)
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, FromAccountID: mobot,
		Outcome: contracts.FallbackWaiting, PlannedAt: f.x.clock.Now(), Reason: "no eligible anthropic subscription; work preserved and resumable"})
	f.process()
	text := f.lastReport()
	if lead, _, _ := strings.Cut(text, "\n"); lead != "synthetic child was stopped for an account switch that did not complete" ||
		!strings.Contains(text, "Why: no eligible anthropic subscription") || strings.Contains(text, "restarted") {
		t.Fatalf("report:\n%s", text)
	}
}

// A plain exit still reads as before.
func TestChildReportPlainExitUnchanged(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	zero := 0
	f.observeExit(&zero)
	f.process()
	if text := f.lastReport(); !strings.Contains(text, "Pyre archives it in about five minutes") || strings.Contains(text, "account switch") {
		t.Fatalf("report:\n%s", text)
	}
}

// iss_…: Pyre stopped two Codex children for an
// account switch that could not move them; the parent, mid-turn, saw them
// exit, archived them, and the held report was dropped as "the parent
// archived the child session". A switch that needs someone to act is sent
// at once, and no stranded switch report is dropped for the archive.
func TestChildReportStrandedSwitchReachesBusyParent(t *testing.T) {
	f := newChildFixture(t, nil)
	sid, parent := f.child.Session.SessionID, f.parent.Session.SessionID
	jh := f.account("personal")
	f.observeSession(parent, contracts.RunRunning) // the parent is mid-turn
	f.observe(contracts.RunRunning, "working")
	run := f.switchExit(contracts.AccountSwitchLimit)
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, FromAccountID: jh,
		Outcome: contracts.FallbackIntervention, PlannedAt: f.x.clock.Now(), Reason: "run used the ambient default login; automatic account fallback is disabled for ambient runs",
		Limit: &contracts.LimitEvent{Kind: "auth_invalid", Detail: "workspace routing discovery unauthorized (401)"}})
	f.process()
	text := f.lastReport()
	if !strings.Contains(text, "did not complete") || !strings.Contains(text, "Why: run used the ambient default login") {
		t.Fatalf("report:\n%s", text)
	}

	// A wait for a reset is batched for the busy parent, and still sent
	// after the parent archives the child.
	f = newChildFixture(t, nil)
	sid, parent = f.child.Session.SessionID, f.parent.Session.SessionID
	jh = f.account("personal")
	f.observeSession(parent, contracts.RunRunning)
	f.observe(contracts.RunRunning, "working")
	run = f.switchExit(contracts.AccountSwitchLimit)
	reset := f.x.clock.Now().Add(3 * time.Hour)
	f.x.report(nodeMac, contracts.ReportFallback, contracts.FallbackPlan{SessionID: sid, FromRunID: run, FromAccountID: jh,
		Outcome: contracts.FallbackWaiting, PlannedAt: f.x.clock.Now(), Reason: "no eligible openai subscription; work preserved and resumable",
		Limit: &contracts.LimitEvent{Kind: "usage_limit", ResetAt: &reset}})
	f.process()
	if n := len(f.live()); n != 0 {
		t.Fatalf("a wait was not batched for the busy parent: %v", f.statuses())
	}
	f.x.clock.Add(time.Minute)
	yes := true
	if _, err := f.x.h.UpdateSession(as(sesPrincipal(parent)), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-parent-archive-stranded"},
		SessionID: sid, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	f.observeSession(parent, contracts.RunWaitingInput) // its turn ends
	f.process()
	if text := f.lastReport(); !strings.Contains(text, "no other account has room") {
		t.Fatalf("report:\n%s\n%v", text, f.statuses())
	}
}
