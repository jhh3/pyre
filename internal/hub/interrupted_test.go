package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// interruptMarks is what pyred records on a run it found gone at start
// (daemon interrupted.go).
func interruptMarks(r contracts.Run, cause string, boot, died time.Time, outcome, detail, resumedAs string) contracts.Run {
	ad := map[string]string{}
	for k, v := range r.AdapterData {
		ad[k] = v
	}
	ad[contracts.AdapterDataInterrupted] = cause
	if !boot.IsZero() {
		ad[contracts.AdapterDataInterruptedBootAt] = persistence.Time(boot)
	}
	ad[contracts.AdapterDataInterruptedDiedAt] = persistence.Time(died)
	ad[contracts.AdapterDataInterruptedOutcome] = outcome
	delete(ad, contracts.AdapterDataInterruptedDetail)
	delete(ad, contracts.AdapterDataInterruptedResumedAs)
	if detail != "" {
		ad[contracts.AdapterDataInterruptedDetail] = detail
	}
	if resumedAs != "" {
		ad[contracts.AdapterDataInterruptedResumedAs] = resumedAs
	}
	r.AdapterData = ad
	return r
}

// The child's machine rebooted mid-turn (issue iss_…):
// pyred ends the dead run with its cause, resumes the session as a new run,
// then records that on the dead run. The parent, though mid-turn, gets one
// report at once that names the machine, the cause and time, the resume and
// what was lost; no ended report is sent besides.
func TestInterruptedChildReportAfterReboot(t *testing.T) {
	f := newChildFixture(t, nil)
	parent, child := f.parent.Session.SessionID, f.child.Session.SessionID
	f.observeSession(parent, contracts.RunRunning) // the lead is mid-turn
	f.observe(contracts.RunRunning, "working")
	old := *f.x.session(child).Run
	f.x.clock.Add(time.Minute)
	died := f.x.clock.Now()
	f.x.clock.Add(70 * time.Second)
	boot := f.x.clock.Now().Add(-5 * time.Second)

	// pyred starts: the run's process is gone and the machine booted after
	// the run started. The resume decision is still pending.
	dead := interruptMarks(old, contracts.InterruptMachineRestarted, boot, died, contracts.InterruptResuming, "", "")
	dead.State, dead.StateReason = contracts.RunExited, "run process not found after pyred restart (exit not observed): the machine restarted"
	f.x.report(nodeMac, contracts.ReportRun, dead)
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("report sent before pyred decided: %d %v", n, f.statuses())
	}

	// Crash resume launches the continuation, then records the outcome.
	next := old
	next.RunID, next.PreviousRunID, next.ContinuationKind = "run_synth-resumed", old.RunID, contracts.ContinuationNativeResume
	next.State, next.StateReason, next.AdapterData = contracts.RunWaitingInput, "", nil
	f.x.report(nodeMac, contracts.ReportRun, next)
	decided := interruptMarks(dead, contracts.InterruptMachineRestarted, boot, died, contracts.InterruptResumed, "", next.RunID)
	f.x.report(nodeMac, contracts.ReportRun, decided)
	f.process()

	msgs := f.toParent()
	if len(msgs) != 1 {
		t.Fatalf("parent got %d messages, want 1: %v", len(msgs), f.statuses())
	}
	body := msgs[0].Body.Text
	t.Logf("report:\n%s", body)
	for _, want := range []string{"synthetic child was interrupted: synth-mac restarted; Pyre resumed it",
		"Cause: machine restarted", "it booted at", "after the run started at", "last seen alive at about",
		"resumed it as a new run run_synth-resumed", "was not replayed",
		"Lost: tool calls that were in flight and its background jobs", "Synthetic final answer"} {
		if !strings.Contains(body, want) {
			t.Fatalf("report lacks %q:\n%s", want, body)
		}
	}
	if got := f.statuses(); len(got) != 1 || !strings.HasPrefix(got[0], reportInterrupted+":"+reportSent) {
		t.Fatalf("report rows = %v, want one interrupted report and no ended report", got)
	}
	// Re-reports of the ended run (a later pyred start) send nothing more.
	f.x.report(nodeMac, contracts.ReportRun, decided)
	f.process()
	if n := len(f.toParent()); n != 1 {
		t.Fatalf("parent got %d messages after a re-report", n)
	}
}

// A run that died on its own (the machine did not reboot) and that Pyre
// left exited is reported at once, with why it was not resumed.
func TestInterruptedChildReportProcessGoneLeftExited(t *testing.T) {
	f := newChildFixture(t, nil)
	child := f.child.Session.SessionID
	r := *f.x.session(child).Run
	dead := interruptMarks(r, contracts.InterruptProcessGone, time.Time{}, f.x.clock.Now(), contracts.InterruptLeftExited, "stop was requested", "")
	dead.State, dead.StateReason = contracts.RunExited, "run process not found after pyred restart (exit not observed)"
	f.x.report(nodeMac, contracts.ReportRun, dead)
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 {
		t.Fatalf("parent got %d messages: %v", len(msgs), f.statuses())
	}
	body := msgs[0].Body.Text
	for _, want := range []string{"its process on synth-mac died; left exited", "Cause: process gone (the machine did not restart",
		"What Pyre did: left it exited (stop was requested)", "Lost: tool calls"} {
		if !strings.Contains(body, want) {
			t.Fatalf("report lacks %q:\n%s", want, body)
		}
	}
}

// pyred never says what it did (it died again): after interruptSettle the
// report goes with the hub's own view.
func TestInterruptedChildReportUndecided(t *testing.T) {
	f := newChildFixture(t, nil)
	r := *f.x.session(f.child.Session.SessionID).Run
	dead := interruptMarks(r, contracts.InterruptProcessGone, time.Time{}, f.x.clock.Now(), contracts.InterruptResuming, "", "")
	dead.State = contracts.RunExited
	f.x.report(nodeMac, contracts.ReportRun, dead)
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("sent before the settle time: %d", n)
	}
	f.x.clock.Add(interruptSettle + time.Second)
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body.Text, "Pyre has not resumed it") {
		t.Fatalf("undecided report: %d %v", len(msgs), msgs)
	}
}

// A plain pyred restart: the run survived and is reported as before, with
// no interrupted marks. Nothing reaches the parent.
func TestPlainPyredRestartSendsNoInterruptedReport(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	r := *f.x.session(f.child.Session.SessionID).Run
	f.x.disconnect(nodeMac)
	f.x.clock.Add(30 * time.Second)
	f.x.connect(nodeMac, 0, r)
	f.x.report(nodeMac, contracts.ReportRun, r)
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("parent got %d messages after a plain restart: %v", n, f.statuses())
	}
	for _, s := range f.statuses() {
		if strings.HasPrefix(s, reportInterrupted) {
			t.Fatalf("interrupted report recorded: %v", f.statuses())
		}
	}
}

// A run its adapter restarted in place after a reboot (Codex reloads its
// thread) is reported once per boot.
func TestInterruptedChildReportRestartedInPlace(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	r := *f.x.session(f.child.Session.SessionID).Run
	boot := f.x.clock.Now().Add(time.Hour)
	f.x.clock.Add(time.Hour + time.Minute)
	live := interruptMarks(r, contracts.InterruptMachineRestarted, boot, boot.Add(-time.Minute), contracts.InterruptResumed, "", r.RunID)
	f.x.report(nodeMac, contracts.ReportRun, live)
	f.x.report(nodeMac, contracts.ReportRun, live) // later observations carry the same marks
	f.process()
	msgs := f.toParent()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body.Text, "Pyre restarted it in place") ||
		!strings.Contains(msgs[0].Body.Text, "restarted it in place (same run "+r.RunID) {
		t.Fatalf("in-place report: %d %v", len(msgs), f.statuses())
	}
	// A later reboot is a new interruption.
	boot2 := boot.Add(2 * time.Hour)
	f.x.clock.Add(2 * time.Hour)
	f.x.report(nodeMac, contracts.ReportRun, interruptMarks(live, contracts.InterruptMachineRestarted, boot2, boot2, contracts.InterruptResumed, "", r.RunID))
	f.process()
	if n := len(f.toParent()); n != 2 {
		t.Fatalf("second reboot: %d messages, want 2: %v", n, f.statuses())
	}
	// Its later ordinary end is an ordinary ended report.
	f.x.clock.Add(time.Hour)
	end := interruptMarks(live, contracts.InterruptMachineRestarted, boot2, boot2, contracts.InterruptResumed, "", r.RunID)
	end.State, end.StateReason = contracts.RunFailed, "process exited with status 1"
	f.x.report(nodeMac, contracts.ReportRun, end)
	f.process()
	got := f.statuses()
	if last := got[len(got)-1]; !strings.HasPrefix(last, reportEnded+":") {
		t.Fatalf("end of a run restarted in place: %v", got)
	}
}

// While its node is disconnected a child's run shows as unknown (node
// disconnected, last observed state), in get_session and in session
// lists, never as its stored "running"; the stored state is unchanged.
func TestDisconnectedNodeChildShowsUnknown(t *testing.T) {
	f := newChildFixture(t, nil)
	child := f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	f.x.disconnect(nodeMac)
	check := func(how string, v contracts.SessionView) {
		t.Helper()
		if v.Run == nil || v.Run.State != contracts.RunUnknown || v.Run.StateReason != "node disconnected; last observed running" ||
			!v.Run.Freshness.Stale || v.Run.Freshness.Reason != "node disconnected" || !v.Freshness.Stale {
			t.Fatalf("%s: %+v / %+v", how, v.Run, v.Freshness)
		}
	}
	check("get_session", f.x.session(child))
	page, err := f.x.h.ListSessions(as(sesPrincipal(f.parent.Session.SessionID)), contracts.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range page.Items {
		if v.Session.SessionID == child {
			check("list_sessions", v)
			found = true
		}
	}
	if !found {
		t.Fatalf("child not listed: %d sessions", len(page.Items))
	}
	if f.x.storedRunState(f.x.session(child).Run.RunID) != contracts.RunRunning {
		t.Fatal("stored state changed")
	}
}
