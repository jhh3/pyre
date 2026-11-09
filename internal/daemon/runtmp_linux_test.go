package daemon

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// startSessionJob starts a detached "sleep 60" (its own terminal session)
// whose environment names sessionID; the test kills it at the end.
func startSessionJob(t *testing.T, sessionID string) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "60")
	c.Env = append(os.Environ(), "PYRE_SESSION_ID="+sessionID, "PYRE_SESSION_TOKEN=synthetic-token-value")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c
}

// session.cleanup marks an archived session's run temp folder instead of
// removing it, and lists (never stops) a process the session started that
// runs on detached: its environment names the session. A process started
// after Pyre asked the run to stop (a SessionEnd hook's detached helper,
// iss_…) is not listed.
func TestSessionCleanupKeepsTmpAndListsLeftRunning(t *testing.T) {
	saved := runTmpOpenPaths
	t.Cleanup(func() { runTmpOpenPaths = saved })
	runTmpOpenPaths = func([]string) ([]string, bool) { return nil, true }
	n := startNode(t, t.TempDir(), Config{})
	ctx := context.Background()
	sid, runID := "ses_synth-leftjob", "run_synth-leftjob"
	// A detached job of the session, one of another session, and one the
	// run's exit path started after the stop was asked for.
	job := startSessionJob(t, sid)
	startSessionJob(t, "ses_synth-someone-else")
	time.Sleep(100 * time.Millisecond) // /proc start times are 10 ms ticks
	stopAsked := time.Now()
	time.Sleep(100 * time.Millisecond)
	hook := startSessionJob(t, sid)
	ended := time.Now()
	run := contracts.Run{RunID: runID, SessionID: sid, Adapter: "mock", Harness: contracts.HarnessMock, State: contracts.RunExited, EndedAt: &ended,
		AdapterData: map[string]string{adapterDataStopRequested: ftime(stopAsked) + " session:ses_synth-parent"}}
	rec := runRecord{Session: contracts.Session{SessionID: sid, Harness: contracts.HarnessMock}, Phase: phaseEnded, Run: run, CreatedAt: ended.Add(-time.Hour)}
	if err := sqlitex.Tx(ctx, n.db, func(tx *sql.Tx) error { return putRunTx(ctx, tx, rec, ended) }); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	n.d.setRunTmp(env, runID)
	tmp := env["TMPDIR"]
	if tmp == "" {
		t.Fatal("no temp dir")
	}
	res := n.d.cleanSessions(ctx, []string{sid})
	if len(res) != 1 || res[0].Error != "" {
		t.Fatalf("cleanup = %+v", res)
	}
	c := res[0]
	if c.ProcessesStopped != 0 || c.LeftRunningCount != 1 || len(c.LeftRunning) != 1 || c.LeftRunning[0].PID != job.Process.Pid {
		t.Fatalf("left running = %+v (job pid %d, hook helper pid %d)", c, job.Process.Pid, hook.Process.Pid)
	}
	if c.LeftRunning[0].Command != "sleep 60" {
		t.Errorf("command = %q", c.LeftRunning[0].Command)
	}
	for _, p := range []*exec.Cmd{job, hook} {
		if err := p.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("cleanup stopped pid %d: %v", p.Process.Pid, err)
		}
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("archived session's temp dir removed at once: %v", err)
	}
	if _, ok := runTmpArchivedAt(tmp); !ok {
		t.Fatal("temp dir not marked archived")
	}
}

// leftRunning counts a process started before the session's stop and not
// one started after it; with no stop time known, both count.
func TestLeftRunningStartedBeforeStop(t *testing.T) {
	sid := "ses_synth-cutoff"
	before := startSessionJob(t, sid)
	time.Sleep(100 * time.Millisecond)
	cut := time.Now()
	time.Sleep(100 * time.Millisecond)
	after := startSessionJob(t, sid)
	got := leftRunning(map[string]leftScan{sid: {before: cut}}, nil)[sid]
	if got.count != 1 || len(got.procs) != 1 || got.procs[0].PID != before.Process.Pid {
		t.Fatalf("with stop time: %+v (before %d, after %d)", got, before.Process.Pid, after.Process.Pid)
	}
	if got := leftRunning(map[string]leftScan{sid: {}}, nil)[sid]; got.count != 2 {
		t.Fatalf("without stop time: %+v", got)
	}
}

func TestRunStoppedAt(t *testing.T) {
	asked := time.Date(2026, 10, 25, 2, 13, 31, 221588612, time.UTC)
	ended := asked.Add(2 * time.Second)
	for name, tc := range map[string]struct {
		run  contracts.Run
		want time.Time
	}{
		"stop requested": {contracts.Run{EndedAt: &ended, AdapterData: map[string]string{adapterDataStopRequested: ftime(asked) + " session:ses_synth"}}, asked},
		"ended only":     {contracts.Run{EndedAt: &ended}, ended},
		"bad marker":     {contracts.Run{EndedAt: &ended, AdapterData: map[string]string{adapterDataStopRequested: "garbage"}}, ended},
		"neither":        {contracts.Run{}, time.Time{}},
	} {
		if got := runStoppedAt(tc.run); !got.Equal(tc.want) {
			t.Errorf("%s: runStoppedAt = %v, want %v", name, got, tc.want)
		}
	}
}

func TestRunScopeRun(t *testing.T) {
	for cg, want := range map[string]string{
		"0::/user.slice/user-1000.slice/user@1000.service/pyre-runs.slice/pyre-run-run_…-1a2b3c.scope\n": "run_…",
		"0::/user.slice/user-1000.slice/user@1000.service/pyre-runs.slice/pyre-run-codex-as-k1-1a2b3c.scope\n":                    "codex-as-k1",
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/pyre-tmux-0123456789ab.scope\n":                               "",
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/vmo-score.service\n":                                          "",
	} {
		if got := runScopeRun(cg); got != want {
			t.Errorf("runScopeRun(%q) = %q, want %q", cg, got, want)
		}
	}
}
