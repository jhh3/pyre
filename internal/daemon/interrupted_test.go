package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/resumenote"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// bootClock is a settable machine boot time (Config.BootTime).
type bootClock struct{ at atomic.Value }

func (b *bootClock) set(t time.Time) { b.at.Store(t) }
func (b *bootClock) get() (time.Time, error) {
	t, _ := b.at.Load().(time.Time)
	return t, nil
}

// runReportsWith lists the run reports of runID that carry the interrupted
// cause, oldest first.
func (n *node) runReportsWith(runID string) []contracts.Run {
	n.t.Helper()
	var out []contracts.Run
	for _, rp := range n.rep.Reports(contracts.ReportRun) {
		var r contracts.Run
		if err := json.Unmarshal(rp.Payload, &r); err != nil {
			n.t.Fatal(err)
		}
		if r.RunID == runID && r.AdapterData[contracts.AdapterDataInterrupted] != "" {
			out = append(out, r)
		}
	}
	return out
}

// A pyred start after the machine rebooted (boot time after the runs'
// start; their processes are gone): each dead run is ended with the cause
// "machine restarted", the boot and death times and, once crash resume has
// decided, what Pyre did (resumed as a new run, or left exited and why),
// reported to the hub. The resumed session's note names the restart.
func TestInterruptedRunsMarkedAfterReboot(t *testing.T) {
	shortCrashResume(t)
	ma := mock.NewAdapter()
	boot := &bootClock{}
	boot.set(time.Now().Add(-time.Hour)) // booted long before these runs
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ma}, NodeName: "synth-box", BootTime: boot.get})
	cwd := t.TempDir()
	start := func(session, run string) {
		sc := startCmd(session, run, contracts.HarnessMock, cwd)
		sc.SessionToken = "tok-synthetic-" + session
		sc.Session.Lineage.ParentSessionID = "ses_synth-lead"
		n.start(sc)
	}
	start("ses_synth-worker", "run_w1")
	start("ses_synth-stopped", "run_s1")
	ma.SetState("run_w1", contracts.RunRunning)
	n.d.PollNow(context.Background(), "run_w1")
	lr, _ := n.d.getLive("run_s1")
	n.d.markStopRequested(context.Background(), lr, "owner:test")

	// The machine reboots: every process dies, and it boots after the
	// runs started.
	n.stop()
	ma.Vanish("run_w1")
	ma.Vanish("run_s1")
	booted := time.Now().Add(time.Minute) // clears bootSkew past the runs' start
	boot.set(booted)
	n = n.restart()

	var resumed runRecord
	waitFor(t, "worker resumed", func() bool {
		resumed = n.latestRun("ses_synth-worker")
		return resumed.Run.PreviousRunID == "run_w1" && resumed.Run.State != contracts.RunStarting
	})
	waitFor(t, "outcome recorded", func() bool {
		return n.run("run_w1").Run.AdapterData[contracts.AdapterDataInterruptedOutcome] == contracts.InterruptResumed
	})
	old := n.run("run_w1")
	ad := old.Run.AdapterData
	if old.Phase != phaseEnded || old.Run.State != contracts.RunExited || old.Run.StateReason != "run process not found after pyred restart (exit not observed): the machine restarted" {
		t.Fatalf("dead run = %s %s %q", old.Phase, old.Run.State, old.Run.StateReason)
	}
	if ad[contracts.AdapterDataInterrupted] != contracts.InterruptMachineRestarted || ad[contracts.AdapterDataInterruptedResumedAs] != resumed.Run.RunID {
		t.Fatalf("dead run marks = %v", ad)
	}
	if at, err := sqlitex.ParseTime(ad[contracts.AdapterDataInterruptedBootAt]); err != nil || !at.Equal(booted) {
		t.Fatalf("boot_at = %q (%v), want %v", ad[contracts.AdapterDataInterruptedBootAt], err, booted)
	}
	if died, err := sqlitex.ParseTime(ad[contracts.AdapterDataInterruptedDiedAt]); err != nil || died.IsZero() || died.After(booted) {
		t.Fatalf("died_at = %q", ad[contracts.AdapterDataInterruptedDiedAt])
	}
	// The hub hears the end (decision pending), then the decision.
	reps := n.runReportsWith("run_w1")
	if len(reps) < 2 || reps[0].State != contracts.RunExited || reps[0].AdapterData[contracts.AdapterDataInterruptedOutcome] != contracts.InterruptResuming ||
		reps[len(reps)-1].AdapterData[contracts.AdapterDataInterruptedOutcome] != contracts.InterruptResumed {
		t.Fatalf("run reports of the dead run = %+v", reps)
	}
	if resumed.Run.AdapterData[contracts.AdapterDataInterrupted] != "" {
		t.Fatalf("the resumed run inherited the marks: %v", resumed.Run.AdapterData)
	}

	// The stopped run is left exited, with why.
	s := n.run("run_s1").Run.AdapterData
	if s[contracts.AdapterDataInterrupted] != contracts.InterruptMachineRestarted || s[contracts.AdapterDataInterruptedOutcome] != contracts.InterruptLeftExited ||
		s[contracts.AdapterDataInterruptedDetail] != "stop was requested" {
		t.Fatalf("stopped run marks = %v", s)
	}

	// The resumed session is told: the machine restarted, nothing replayed.
	var body string
	waitFor(t, "note", func() bool {
		var ok bool
		body, ok = n.outboxBody(resumenote.MessageID("ses_synth-worker", resumed.Run.RunID))
		return ok
	})
	for _, want := range []string{"[from Pyre] synth-box restarted (it booted at", "died at about", "with it and Pyre resumed you at",
		"Tool calls that were in flight did not finish and nothing was replayed", "check in with it before continuing"} {
		if !strings.Contains(body, want) {
			t.Fatalf("note lacks %q:\n%s", want, body)
		}
	}
}

// A run that died while the machine stayed up (boot before the run
// started) is "process gone"; a plain pyred restart with the process alive
// marks nothing and reports nothing new.
func TestInterruptedCauseProcessGoneAndPlainRestart(t *testing.T) {
	shortCrashResume(t)
	ma := mock.NewAdapter()
	boot := &bootClock{}
	boot.set(time.Now().Add(-time.Hour))
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ma}, BootTime: boot.get})
	cwd := t.TempDir()
	for _, s := range []string{"ses_synth-alive", "ses_synth-gone"} {
		sc := startCmd(s, "run_"+strings.TrimPrefix(s, "ses_"), contracts.HarnessMock, cwd)
		sc.Session.Lineage.ParentSessionID = "ses_synth-lead"
		n.start(sc)
	}
	n.stop()
	ma.Vanish("run_synth-gone")
	n = n.restart()

	gone := n.run("run_synth-gone")
	if gone.Run.AdapterData[contracts.AdapterDataInterrupted] != contracts.InterruptProcessGone ||
		gone.Run.StateReason != "run process not found after pyred restart (exit not observed)" {
		t.Fatalf("gone run = %q %v", gone.Run.StateReason, gone.Run.AdapterData)
	}
	alive := n.run("run_synth-alive")
	if alive.Phase != phaseLive || alive.Run.AdapterData[contracts.AdapterDataInterrupted] != "" {
		t.Fatalf("surviving run = %s %v", alive.Phase, alive.Run.AdapterData)
	}
	if reps := n.runReportsWith("run_synth-alive"); len(reps) != 0 {
		t.Fatalf("surviving run reported as interrupted: %+v", reps)
	}
}

// A run its adapter brought back although the machine booted after it
// started (Codex reloads its thread) was restarted in place: marked once
// for that boot, and the session gets the note; another plain restart
// does not mark it again.
func TestInterruptedRunRestartedInPlace(t *testing.T) {
	shortCrashResume(t)
	ma := mock.NewAdapter()
	boot := &bootClock{}
	boot.set(time.Now().Add(-time.Hour))
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ma}, NodeName: "synth-box", BootTime: boot.get})
	sc := startCmd("ses_synth-codexish", "run_c1", contracts.HarnessMock, t.TempDir())
	sc.SessionToken = "tok-synthetic-codexish"
	n.start(sc)
	if err := n.d.st.setMeta(context.Background(), metaAliveAt, ftime(time.Now())); err != nil { // as aliveLoop does
		t.Fatal(err)
	}
	n.stop()
	booted := time.Now().Add(time.Minute)
	boot.set(booted)
	n = n.restart() // the mock recovers the run: restarted in place

	r := n.run("run_c1")
	if r.Phase != phaseLive || r.Run.AdapterData[contracts.AdapterDataInterrupted] != contracts.InterruptMachineRestarted ||
		r.Run.AdapterData[contracts.AdapterDataInterruptedOutcome] != contracts.InterruptResumed || r.Run.AdapterData[contracts.AdapterDataInterruptedResumedAs] != "run_c1" {
		t.Fatalf("in-place run = %s %v", r.Phase, r.Run.AdapterData)
	}
	var body string
	waitFor(t, "note", func() bool {
		var ok bool
		body, ok = n.outboxBody(resumenote.MessageID("ses_synth-codexish", "run_c1"))
		return ok
	})
	if !strings.Contains(body, "synth-box restarted") || !strings.Contains(body, "Pyre restarted you in the same conversation") {
		t.Fatalf("in-place note:\n%s", body)
	}
	before := len(n.runReportsWith("run_c1"))
	n = n.restart() // same boot: nothing new
	time.Sleep(100 * time.Millisecond)
	if got := len(n.runReportsWith("run_c1")); got != before {
		t.Fatalf("plain restart re-marked the run: %d reports, want %d", got, before)
	}
}

// The cause and in-place rules: a machine restart needs a boot after the
// run's start (by bootSkew) and after the previous pyred was last alive; a
// run restarted in place is marked once per boot.
func TestInterruptCauseRules(t *testing.T) {
	start := time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC)
	boot := start.Add(time.Hour)
	r := contracts.Run{RunID: "run_synth", StartedAt: start}
	for _, tc := range []struct {
		name          string
		boot, aliveAt time.Time
		want          string
		inPlace       bool
	}{
		{"rebooted since pyred last ran", boot, boot.Add(-time.Minute), contracts.InterruptMachineRestarted, true},
		{"no alive stamp", boot, time.Time{}, contracts.InterruptMachineRestarted, false},
		{"pyred ran after an older reboot", boot, boot.Add(time.Minute), contracts.InterruptProcessGone, false},
		{"booted before the run started", start.Add(-time.Hour), start.Add(time.Minute), contracts.InterruptProcessGone, false},
		{"within the skew", start.Add(bootSkew / 2), start.Add(-time.Minute), contracts.InterruptProcessGone, false},
		{"boot time unknown", time.Time{}, start, contracts.InterruptProcessGone, false},
	} {
		if got := interruptCause(r, tc.boot, tc.aliveAt); got != tc.want {
			t.Errorf("%s: cause %s, want %s", tc.name, got, tc.want)
		}
		if got := restartedInPlace(r, tc.boot, tc.aliveAt); got != tc.inPlace {
			t.Errorf("%s: in place %v, want %v", tc.name, got, tc.inPlace)
		}
	}
	marked := r
	marked.AdapterData = withInterrupted(nil, contracts.InterruptMachineRestarted, boot.Add(800*time.Millisecond), boot, contracts.InterruptResumed, "", r.RunID)
	if restartedInPlace(marked, boot, boot.Add(-time.Minute)) {
		t.Error("a run already marked for this boot is marked again")
	}
	if !restartedInPlace(marked, boot.Add(24*time.Hour), boot.Add(23*time.Hour)) {
		t.Error("a later reboot is not marked")
	}
}
