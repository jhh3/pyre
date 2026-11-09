//go:build linux || darwin

package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// W-runtmp: a run's Claude session folder under CLAUDE_CODE_TMPDIR follows
// the run's $TMPDIR rules.
func TestHarnessTmpCleanup(t *testing.T) {
	saved := runTmpOpenPaths
	t.Cleanup(func() { runTmpOpenPaths = saved })
	n := startNode(t, t.TempDir(), Config{})
	// The daemon's own startup sweep would otherwise race the passes below
	// (on a slow runner it removed the idle folder first).
	<-n.d.startupSwept
	ctx := context.Background()
	now := time.Now()
	root := filepath.Join(n.dir, "tmp", "claude")
	record := func(runID, sessionID, uuid, phase string, state contracts.RunState, ended, created time.Time) {
		t.Helper()
		run := contracts.Run{RunID: runID, SessionID: sessionID, Adapter: "mock", Harness: contracts.HarnessMock, State: state,
			NativeRef: &contracts.NativeRef{Kind: "claude_session", ID: uuid}}
		if !ended.IsZero() {
			run.EndedAt = &ended
		}
		rec := runRecord{Session: contracts.Session{SessionID: sessionID, Harness: contracts.HarnessMock}, Phase: phase, Run: run, CreatedAt: created}
		if err := sqlitex.Tx(ctx, n.db, func(tx *sql.Tx) error { return putRunTx(ctx, tx, rec, now) }); err != nil {
			t.Fatal(err)
		}
	}
	folder := func(key, uuid string, at time.Time) string {
		t.Helper()
		dir := filepath.Join(root, "claude-1000", key, uuid)
		if err := os.MkdirAll(filepath.Join(dir, "scratchpad"), 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, "scratchpad", "render.bin")
		if err := os.WriteFile(f, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(f, at, at)
		return dir
	}
	const (
		uSuper   = "11111111-1111-4111-8111-111111111111"
		uLive    = "22222222-2222-4222-8222-222222222222"
		uCurrent = "33333333-3333-4333-8333-333333333333"
		uIdle    = "44444444-4444-4444-8444-444444444444"
		uShared  = "55555555-5555-4555-8555-555555555555"
		uNoRun   = "66666666-6666-4666-8666-666666666666"
		uBusy    = "77777777-7777-4777-8777-777777777777"
	)
	old, h2 := now.Add(-48*time.Hour), now.Add(-2*time.Hour)
	// Superseded 2 h ago by a live run of the session.
	record("run_synth-h1", "ses_synth-h", uSuper, phaseEnded, contracts.RunExited, h2, now.Add(-5*time.Hour))
	record("run_synth-h2", "ses_synth-h", uLive, phaseLive, contracts.RunRunning, time.Time{}, now.Add(-time.Hour))
	super := folder("-home-synth-only", uSuper, h2)
	live := folder("-home-synth-a", uLive, now)
	// A session's latest run that ended 2 h ago, and one 2 days ago.
	record("run_synth-cur", "ses_synth-cur", uCurrent, phaseEnded, contracts.RunExited, h2, now.Add(-3*time.Hour))
	record("run_synth-idle", "ses_synth-idle", uIdle, phaseEnded, contracts.RunExited, old, old)
	current := folder("-home-synth-a", uCurrent, h2)
	idle := folder("-home-synth-a", uIdle, old)
	// A resume that kept the Claude session: the superseded run is done
	// with it, its latest run (ended 2 h ago) is not.
	record("run_synth-s1", "ses_synth-s", uShared, phaseEnded, contracts.RunExited, now.Add(-4*time.Hour), now.Add(-6*time.Hour))
	record("run_synth-s2", "ses_synth-s", uShared, phaseEnded, contracts.RunExited, h2, now.Add(-3*time.Hour))
	shared := folder("-home-synth-a", uShared, h2)
	noRun := folder("-home-synth-a", uNoRun, old)
	record("run_synth-busy", "ses_synth-busy", uBusy, phaseEnded, contracts.RunExited, old, old)
	busy := folder("-home-synth-a", uBusy, old)

	runTmpOpenPaths = func([]string) ([]string, bool) { return nil, false }
	if got := n.d.sweepHarnessTmp(ctx); got != 0 {
		t.Fatalf("removed %d with open files unknown", got)
	}
	runTmpOpenPaths = func([]string) ([]string, bool) {
		return []string{filepath.Join(busy, "scratchpad", "render.bin")}, true
	}
	if got := n.d.sweepHarnessTmp(ctx); got != 2 {
		t.Fatalf("maintenance pass removed %d, want 2", got)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	for p, want := range map[string]bool{super: false, live: true, current: true, idle: false, shared: true, noRun: true, busy: true} {
		if exists(p) != want {
			t.Errorf("%s exists=%v, want %v", p, !want, want)
		}
	}
	if exists(filepath.Dir(super)) {
		t.Error("emptied cwd-key folder kept")
	}

	// Archived: marked, removed an hour later. A mark from before the run
	// ended (an earlier archive of a resumed session) does not count.
	if got := n.d.markSessionRunTmp(ctx, "ses_synth-cur"); got != 1 {
		t.Fatalf("marked %d, want 1", got)
	}
	if got := n.d.markSessionRunTmp(ctx, "ses_synth-h"); got != 0 {
		t.Fatalf("live session marked %d", got) // its ended run's folder is gone; the live run's is not marked
	}
	if got := n.d.sweepHarnessTmp(ctx); got != 0 {
		t.Fatalf("removed %d within the archive grace", got)
	}
	early := []byte(now.Add(-3*time.Hour).UTC().Format(time.RFC3339) + "\n")
	if err := os.WriteFile(filepath.Join(shared, runTmpArchivedFile), early, 0o600); err != nil {
		t.Fatal(err)
	}
	past := []byte(now.Add(-runTmpGrace-time.Minute).UTC().Format(time.RFC3339) + "\n")
	if err := os.WriteFile(filepath.Join(current, runTmpArchivedFile), past, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := n.d.sweepHarnessTmp(ctx); got != 1 || exists(current) || !exists(shared) {
		t.Fatalf("after the grace: removed %d (current exists %v, shared %v)", got, exists(current), exists(shared))
	}

	// pyred start: folders no live run uses, untouched for a day, go.
	runTmpOpenPaths = func([]string) ([]string, bool) { return nil, true }
	n.d.startupHarnessTmpSweep()
	for p, want := range map[string]bool{noRun: false, busy: false, live: true, shared: true} {
		if exists(p) != want {
			t.Errorf("startup sweep: %s exists=%v, want %v", p, !want, want)
		}
	}
}
