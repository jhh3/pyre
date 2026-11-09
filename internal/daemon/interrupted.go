package daemon

import (
	"context"
	"maps"
	"os"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/resumenote"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Interrupted runs (Oct 23 2026, devbox rebooted for an OS upgrade:
// pyred resumed its Claude sessions by itself, but the parent lead was
// told nothing it could use: the "has exited" report waited behind the
// lead's long turn and said nothing of the resume or of what was lost).
//
// When pyred starts and finds a run's process gone, the run did not
// survive (a plain pyred restart leaves runs alive): the machine restarted
// (it booted after the run started) or the process died on its own (crash,
// OOM kill, a killed tmux server). recoverRuns records that on the run
// (contracts.AdapterDataInterrupted*: cause, boot time, when the process was
// last seen) before it ends it, and crash resume records what Pyre did:
// resumed it as a new run, or left it exited and why. The hub turns the
// marked run into one "interrupted" report to the parent (hub
// childreport.go), sent once the outcome is known.
//
// A run whose adapter brought it back after a reboot (Codex restarts its
// own app-server and reloads the thread) did not survive either: it is
// marked restarted in place, and gets the same report and note, once per
// boot.

// bootSkew is the margin a boot time must clear a run's start by to count
// as a machine restart. Both are this node's wall clock (the run's start is
// stamped at launch), but the boot time is derived (Linux floors it to the
// second; Windows subtracts the uptime from now).
const bootSkew = 2 * time.Second

// interruptCause classifies a run whose process did not survive: the
// machine restarted when it booted (boot; zero when unknown) after the run
// started and after the previous pyred was last alive (aliveAt; a pyred
// that ran after the boot already handled that reboot, so this start is a
// plain restart).
func interruptCause(r contracts.Run, boot, aliveAt time.Time) string {
	if !boot.IsZero() && !r.StartedAt.IsZero() && boot.After(r.StartedAt.Add(bootSkew)) && (aliveAt.IsZero() || aliveAt.Before(boot)) {
		return contracts.InterruptMachineRestarted
	}
	return contracts.InterruptProcessGone
}

// bootTime reads the machine's boot time once per start (zero when the
// platform cannot tell).
func (d *Daemon) bootTime() time.Time {
	d.bootOnce.Do(func() {
		if t, err := d.cfg.BootTime(); err == nil {
			d.bootAt = t
		} else {
			d.cfg.Logger.Info("boot time unknown; interrupted runs are reported as process gone", "err", err)
		}
	})
	return d.bootAt
}

// withInterrupted returns a copy of ad with the interruption recorded.
func withInterrupted(ad map[string]string, cause string, boot, died time.Time, outcome, detail, resumedAs string) map[string]string {
	out := maps.Clone(ad)
	if out == nil {
		out = map[string]string{}
	}
	out[contracts.AdapterDataInterrupted] = cause
	delete(out, contracts.AdapterDataInterruptedBootAt)
	if !boot.IsZero() {
		out[contracts.AdapterDataInterruptedBootAt] = ftime(boot)
	}
	delete(out, contracts.AdapterDataInterruptedDiedAt)
	if !died.IsZero() {
		out[contracts.AdapterDataInterruptedDiedAt] = ftime(died)
	}
	setInterruptOutcome(out, outcome, detail, resumedAs)
	return out
}

func setInterruptOutcome(ad map[string]string, outcome, detail, resumedAs string) {
	ad[contracts.AdapterDataInterruptedOutcome] = outcome
	delete(ad, contracts.AdapterDataInterruptedDetail)
	delete(ad, contracts.AdapterDataInterruptedResumedAs)
	if detail != "" {
		ad[contracts.AdapterDataInterruptedDetail] = redact(detail)
	}
	if resumedAs != "" {
		ad[contracts.AdapterDataInterruptedResumedAs] = resumedAs
	}
}

// interruptedDiedAt is when a run found gone was last seen alive (diedAt);
// after a machine restart, no later than the boot.
func interruptedDiedAt(rec runRecord, aliveAt, boot time.Time, cause string) time.Time {
	t := diedAt(rec, aliveAt)
	if cause == contracts.InterruptMachineRestarted && (t.IsZero() || t.After(boot)) {
		t = boot
	}
	return t
}

// restartedInPlace reports whether a run its adapter recovered was started
// before the machine booted, and the machine booted since the previous
// pyred was last alive (so its process was restarted, not kept), and it is
// not yet marked for this boot. With no alive stamp (aliveAt zero) nothing
// tells this start from a plain restart after an older reboot, so it is
// not marked.
func restartedInPlace(r contracts.Run, boot, aliveAt time.Time) bool {
	if aliveAt.IsZero() || interruptCause(r, boot, aliveAt) != contracts.InterruptMachineRestarted {
		return false
	}
	if at, err := sqlitex.ParseTime(r.AdapterData[contracts.AdapterDataInterruptedBootAt]); err == nil && !at.IsZero() {
		if d := at.Sub(boot); d < time.Minute && d > -time.Minute { // the same boot (Windows derives it from uptime)
			return false
		}
	}
	return true
}

// interruptNote describes the interruption of oldRun for the note to the
// session resumed as runID (the same run when restarted in place).
func (d *Daemon) interruptNote(ctx context.Context, oldRun, runID string, died time.Time) resumenote.Interrupt {
	n := resumenote.Interrupt{Machine: d.cfg.NodeName, DiedAt: died, ResumedAt: d.now(), InPlace: oldRun == runID}
	if n.Machine == "" {
		n.Machine, _ = os.Hostname()
	}
	rec, ok, err := d.st.run(ctx, oldRun)
	if err != nil || !ok {
		return n
	}
	ad := rec.Run.AdapterData
	n.MachineRestarted = ad[contracts.AdapterDataInterrupted] == contracts.InterruptMachineRestarted
	if t, err := sqlitex.ParseTime(ad[contracts.AdapterDataInterruptedBootAt]); err == nil {
		n.BootAt = t
	}
	if t, err := sqlitex.ParseTime(ad[contracts.AdapterDataInterruptedDiedAt]); err == nil && !t.IsZero() {
		n.DiedAt = t
	}
	return n
}

// recordInterruptOutcome records on a dead run what crash resume did and
// reports it, so the hub's interrupted report can go out.
func (d *Daemon) recordInterruptOutcome(ctx context.Context, runID, outcome, detail, resumedAs string) {
	rec, ok, err := d.st.run(ctx, runID)
	if err != nil || !ok {
		if err != nil {
			d.noteErr("record interrupted outcome of run %s: %v", runID, err)
		}
		return
	}
	if rec.Run.AdapterData[contracts.AdapterDataInterrupted] == "" {
		return
	}
	rec.Run.AdapterData = maps.Clone(rec.Run.AdapterData)
	setInterruptOutcome(rec.Run.AdapterData, outcome, detail, resumedAs)
	if err := d.persistAndReport(ctx, rec); err != nil {
		d.noteErr("record interrupted outcome of run %s: %v", runID, err)
	}
}
