package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Interrupted launches and graceful shutdown (W-resilience).
//
// A launch is in flight from the commit of its "launching" record until its
// handle is recorded or it fails. If pyred stops in that window the next
// start reconciles the record instead of leaving it "unknown" forever:
//
//   - a tmux session / process for the run exists: it is adopted (recovered
//     through the adapter), never relaunched;
//   - nothing exists and the launch provably never reached adapter.Launch
//     (no launch marker, no adapter state in the run dir): the run failed
//     and nothing ran, so the owner may relaunch it (Pyre never does);
//   - otherwise the outcome stays unknown (the adapter may have started
//     something that already exited).

// LaunchMarkerFile is written in the run dir immediately before
// adapter.Launch. Its absence proves the adapter never started anything.
const LaunchMarkerFile = "launch-started"

// NeverStartedReason is the state reason of a launch proven never started.
const NeverStartedReason = "launch interrupted (pyred restarted before the harness started); nothing ran — relaunch to retry"

// Run.AdapterData keys clients use to offer direct archive / relaunch:
// AdapterDataNeverStarted marks a run proven never started (failed, nothing
// ran); AdapterDataNoHandle marks an orphaned run with no handle (it cannot
// be stopped through Pyre; it may or may not exist).
const (
	AdapterDataNeverStarted = "pyre.never_started"
	AdapterDataNoHandle     = "pyre.no_handle"
)

// preLaunchFiles are written by the daemon (not the adapter) before the
// launch marker; their presence does not mean the harness started.
var preLaunchFiles = map[string]bool{"pyre-mcp.json": true}

func writeLaunchMarker(runDir string, at time.Time) error {
	return os.WriteFile(filepath.Join(runDir, LaunchMarkerFile), []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
}

// launchMayHaveStarted reports whether the run dir shows that the adapter
// was (or may have been) invoked: the marker, or any file the daemon does
// not write before the marker (adapter state from older builds).
func launchMayHaveStarted(runDir string) bool {
	ents, err := os.ReadDir(runDir)
	if err != nil {
		return !os.IsNotExist(err) // unreadable: cannot prove anything
	}
	for _, e := range ents {
		if !preLaunchFiles[e.Name()] {
			return true
		}
	}
	return false
}

// runProcessFound reports whether a live process carries PYRE_RUN_ID=runID
// in its environment. supported=false where environments of other
// processes cannot be read (macOS); the tmux check then stands alone.
var runProcessFound = func(runID string) (found, supported bool) {
	if runtime.GOOS != "linux" {
		return false, false
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	want := []byte("PYRE_RUN_ID=" + runID)
	self := os.Getpid()
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		for _, kv := range bytes.Split(b, []byte{0}) {
			if bytes.Equal(kv, want) {
				return true, true
			}
		}
	}
	return false, true
}

// reconcileInterruptedLaunch resolves a run record left without a handle
// by an earlier pyred (phase launching/orphaned).
func (d *Daemon) reconcileInterruptedLaunch(ctx context.Context, rec runRecord) error {
	runID := rec.Run.RunID
	dir := d.runDir(runID)
	a := d.adapters[rec.Run.Adapter]
	probed := a != nil
	if a != nil {
		// Every tmux-based adapter names the run's session after the run ID;
		// the codex adapter reads its run.json from the run dir.
		h := contracts.RunHandle{RunID: runID, SessionID: rec.Run.SessionID, Adapter: rec.Run.Adapter, Harness: rec.Run.Harness,
			TmuxTarget: runID, StateDir: dir}
		rctx, cancel := context.WithTimeout(ctx, d.cfg.Poll.Timeout)
		got, err := a.Recover(rctx, h)
		cancel()
		switch {
		case err == nil:
			rec.Phase = phaseLive
			rec.Handle = &got
			if got.NativeRef != nil {
				rec.Run.NativeRef = got.NativeRef
			}
			rec.Run.State = contracts.RunStarting
			rec.Run.StateReason = "adopted after pyred restarted during launch"
			rec.Run.Freshness = d.freshness(false, "")
			if err := d.persistAndReport(ctx, rec); err != nil {
				return err
			}
			d.cfg.Logger.Info("adopted interrupted launch", "run", runID, "session", rec.Run.SessionID)
			d.addLive(&liveRun{rec: rec, adapter: a, recovered: true, interval: d.cfg.Poll.Interval})
			return nil
		case contracts.IsCode(err, contracts.ErrNotFound):
		default:
			probed = false // tmux unavailable etc.: cannot prove absence
		}
	}
	if probed && !launchMayHaveStarted(dir) {
		if found, _ := runProcessFound(runID); !found {
			return d.failNeverStarted(ctx, rec)
		}
	}
	if rec.Phase == phaseOrphaned && rec.Run.State == contracts.RunUnknown && rec.Run.AdapterData[AdapterDataNoHandle] != "" {
		return nil // unchanged; already reported
	}
	if rec.Run.AdapterData == nil {
		rec.Run.AdapterData = map[string]string{}
	}
	rec.Run.AdapterData[AdapterDataNoHandle] = "true"
	rec.Phase = phaseOrphaned
	rec.Run.State = contracts.RunUnknown
	rec.Run.StateReason = "pyred stopped during launch before a run handle was recorded; the process may exist under " + dir + " and is not relaunched"
	rec.Run.Freshness = d.freshness(true, "launch outcome unknown")
	return d.persistAndReport(ctx, rec)
}

// failNeverStarted ends a run whose launch provably never started. The run
// report (failed) drives the hub's child->parent "ended" report.
func (d *Daemon) failNeverStarted(ctx context.Context, rec runRecord) error {
	now := d.now()
	rec.Phase = phaseEnded
	rec.Run.State = contracts.RunFailed
	rec.Run.StateReason = NeverStartedReason
	if rec.Run.AdapterData == nil {
		rec.Run.AdapterData = map[string]string{}
	}
	rec.Run.AdapterData[AdapterDataNeverStarted] = "true"
	delete(rec.Run.AdapterData, AdapterDataNoHandle)
	rec.Run.EndedAt = &now
	rec.Run.PendingApprovalIDs = nil
	rec.Run.Activity = nil
	rec.Run.Freshness = d.freshness(false, "")
	if err := d.persistAndReport(ctx, rec); err != nil {
		return err
	}
	d.cfg.Logger.Warn("interrupted launch never started; marked failed", "run", rec.Run.RunID, "session", rec.Run.SessionID)
	d.releaseAccount(rec.Run.RunID)
	return nil
}

// ---- in-flight launch tracking and shutdown drain ----

func errShuttingDown() error {
	return contracts.Errorf(contracts.ErrUnavailable, "pyred is shutting down; retry the launch once it is back")
}

func (d *Daemon) isDraining() bool {
	d.launchMu.Lock()
	defer d.launchMu.Unlock()
	return d.draining
}

// beginLaunch counts a launch in flight unless a drain began.
func (d *Daemon) beginLaunch() bool {
	d.launchMu.Lock()
	defer d.launchMu.Unlock()
	if d.draining {
		return false
	}
	d.inflight++
	return true
}

func (d *Daemon) endLaunch() {
	d.launchMu.Lock()
	d.inflight--
	d.launchMu.Unlock()
}

func (d *Daemon) launchesInFlight() int {
	d.launchMu.Lock()
	defer d.launchMu.Unlock()
	return d.inflight
}

// Drain refuses new launches from now on and waits up to timeout for
// in-flight launches to record their handle or fail. It returns the number
// still in flight. Idempotent.
func (d *Daemon) Drain(timeout time.Duration) int {
	d.launchMu.Lock()
	first := !d.draining
	d.draining = true
	d.launchMu.Unlock()
	n := d.launchesInFlight()
	if n > 0 && first {
		d.cfg.Logger.Info("shutdown: waiting for in-flight launches", "launches", n, "grace", timeout)
	}
	deadline := time.Now().Add(timeout)
	for n > 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		n = d.launchesInFlight()
	}
	return n
}

// waitTimeout waits for wg up to timeout; false on timeout.
func waitTimeout(wg interface{ Wait() }, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// watchLaunchStall logs a warning, and writes the goroutine stacks to
// <state>/logs/launch-stall-<run>.txt once, if the launch has not finished
// after limit. The returned func stops the watch.
func (d *Daemon) watchLaunchStall(runID string, limit time.Duration) func() {
	t := time.AfterFunc(limit, func() {
		path := filepath.Join(d.cfg.StateDir, "logs", "launch-stall-"+runID+".txt")
		var buf bytes.Buffer
		if p := pprof.Lookup("goroutine"); p != nil {
			_ = p.WriteTo(&buf, 2)
		}
		if buf.Len() > 4<<20 {
			buf.Truncate(4 << 20)
		}
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		werr := os.WriteFile(path, buf.Bytes(), 0o600)
		d.noteErr("launch %s still not finished after %s; goroutine stacks: %s", runID, limit, fmt.Sprint(path, errSuffix(werr)))
	})
	return func() { t.Stop() }
}

func errSuffix(err error) string {
	if err != nil {
		return " (not written: " + err.Error() + ")"
	}
	return ""
}

// ---- relaunch of a never-started launch ----

// launchSpec is what a relaunch needs that the run record does not hold.
// Kept (daemon_meta, node-local, never reported) from the launching commit
// until the handle is recorded or the launch fails, so it survives exactly
// for launches a restart interrupts.
type launchSpec struct {
	InitialPrompt string                     `json:"initial_prompt,omitempty"`
	Args          []string                   `json:"args,omitempty"`
	EnvProfile    string                     `json:"env_profile,omitempty"`
	AccountPolicy contracts.AccountPolicy    `json:"account_policy"`
	Checkout      *contracts.CheckoutRequest `json:"checkout,omitempty"`
	Cwd           string                     `json:"cwd,omitempty"`
	LaunchContext *contracts.LaunchContext   `json:"launch_context,omitempty"`
}

func launchSpecKey(runID string) string { return "launchspec:" + runID }

func putLaunchSpecTx(ctx context.Context, tx querier, runID string, sc contracts.StartRunCommand, rawArgs []string) error {
	b, err := json.Marshal(launchSpec{InitialPrompt: sc.InitialPrompt, Args: rawArgs, EnvProfile: sc.EnvProfile, AccountPolicy: sc.AccountPolicy,
		Checkout: sc.Checkout, Cwd: sc.Cwd, LaunchContext: sc.LaunchContext})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO daemon_meta(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, launchSpecKey(runID), string(b))
	return err
}

func deleteLaunchSpecTx(ctx context.Context, tx querier, runID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM daemon_meta WHERE key=?`, launchSpecKey(runID))
	return err
}

func (d *Daemon) launchSpecOf(ctx context.Context, runID string) (launchSpec, bool) {
	v, err := d.st.meta(ctx, launchSpecKey(runID))
	if err != nil || v == "" {
		return launchSpec{}, false
	}
	var s launchSpec
	if json.Unmarshal([]byte(v), &s) != nil {
		return launchSpec{}, false
	}
	return s, true
}

// neverStartedPrev returns the local record of a resume's previous run when
// pyred proved that run never started: the "resume" is then a fresh launch
// with the original spec (nothing to continue).
func (d *Daemon) neverStartedPrev(ctx context.Context, prev *contracts.Run) (runRecord, bool) {
	if prev == nil {
		return runRecord{}, false
	}
	rec, ok, err := d.st.run(ctx, prev.RunID)
	if err != nil || !ok || rec.Phase != phaseEnded || rec.Run.AdapterData[AdapterDataNeverStarted] != "true" {
		return runRecord{}, false
	}
	return rec, true
}

// applyRelaunchSpec fills a relaunch command from the never-started run's
// stored spec (fields the resume command left empty), so it relaunches
// with the same prompt, args, profile, checkout and account policy.
func (d *Daemon) applyRelaunchSpec(ctx context.Context, sc *contracts.StartRunCommand, prevRunID string) bool {
	spec, ok := d.launchSpecOf(ctx, prevRunID)
	if !ok {
		return false
	}
	if sc.InitialPrompt == "" {
		sc.InitialPrompt = spec.InitialPrompt
	}
	if len(sc.Args) == 0 {
		// A headless (pyre run) start is never relaunched as one: its
		// command already ran without Pyre. The session continues as an
		// ordinary run, like any resume of it.
		sc.Args, _, _, _ = splitExecArgs(spec.Args)
	}
	if sc.EnvProfile == "" {
		sc.EnvProfile = spec.EnvProfile
	}
	if sc.Checkout == nil {
		sc.Checkout = spec.Checkout
	}
	if spec.Cwd != "" {
		sc.Cwd = spec.Cwd
	}
	if sc.LaunchContext == nil {
		sc.LaunchContext = spec.LaunchContext
	}
	if spec.AccountPolicy.Mode != "" {
		sc.AccountPolicy = spec.AccountPolicy
	}
	return true
}

// InterruptedLaunchReason is the error of a start_run/resume command that
// was executing when pyred stopped and provably created nothing: no run
// record, no run dir, no tmux session or process for the run.
const InterruptedLaunchReason = "launch interrupted by a pyred restart; the run never started"

// RecoverCommand resolves a start_run/resume command that was executing
// when pyred stopped (transport.NodeLinkConfig.RecoverCommand; it runs
// after Start, so recoverRuns has reconciled every run record):
//
//   - the run has a record: the command's result is the run as it stands
//     now (adopted, failed as never started, or orphaned/unknown), exactly
//     like a duplicate start_run; observation reports it from here;
//   - nothing exists for the run (the restart came before the launching
//     record committed, e.g. during account placement): the command failed
//     and nothing ran, so the hub fails the run and the parent is told;
//   - anything else (something exists without a record, or absence cannot
//     be proved) stays indeterminate.
//
// Other command kinds are left indeterminate.
func (d *Daemon) RecoverCommand(ctx context.Context, cmd contracts.NodeCommand) (contracts.CommandResult, bool) {
	var sc contracts.StartRunCommand
	switch cmd.Kind {
	case contracts.CmdStartRun:
		v, err := decode[contracts.StartRunCommand](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, false
		}
		sc = v
	case contracts.CmdResume:
		v, err := decode[contracts.ResumeCommand](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, false
		}
		sc = v.StartRunCommand
	default:
		return contracts.CommandResult{}, false
	}
	runID := sc.RunID
	if !contracts.ValidID(runID, contracts.PrefixRun) {
		return contracts.CommandResult{}, false
	}
	rec, ok, err := d.st.run(ctx, runID)
	if err != nil {
		return contracts.CommandResult{}, false
	}
	if ok {
		if lr, live := d.getLive(runID); live {
			rec, _ = lr.snapshot()
		}
		res, err := okResult(contracts.OutcomeSucceeded, rec.Run)
		return res, err == nil
	}
	if launchMayHaveStarted(d.runDir(runID)) {
		return contracts.CommandResult{}, false // adapter state without a record: cannot prove nothing ran
	}
	if found, _ := runProcessFound(runID); found {
		return contracts.CommandResult{}, false
	}
	if a := d.byHarness[sc.Session.Harness]; a != nil {
		h := contracts.RunHandle{RunID: runID, SessionID: sc.Session.SessionID, Adapter: a.Name(), Harness: sc.Session.Harness,
			TmuxTarget: runID, StateDir: d.runDir(runID)}
		rctx, cancel := context.WithTimeout(ctx, d.cfg.Poll.Timeout)
		_, rerr := a.Recover(rctx, h)
		cancel()
		if !contracts.IsCode(rerr, contracts.ErrNotFound) {
			return contracts.CommandResult{}, false // it exists, or tmux could not answer
		}
	}
	d.releaseAccount(runID) // an account bound before the restart is not held for nothing
	d.cfg.Logger.Warn("interrupted launch left nothing behind; reporting it failed", "run", runID, "session", sc.Session.SessionID, "command", cmd.CommandID)
	return contracts.CommandResult{Outcome: contracts.OutcomeFailed, Error: contracts.Errorf(contracts.ErrUnavailable, "%s", InterruptedLaunchReason)}, true
}
