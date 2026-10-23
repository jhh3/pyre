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

// W-runtmp: every run's TMPDIR is its own 0700 folder under the state dir.
func TestRunEnvSetsRunTmpDir(t *testing.T) {
	state := t.TempDir()
	d := &Daemon{cfg: Config{StateDir: state, PyreBinDir: "/opt/synth/bin"}}
	cmd := contracts.StartRunCommand{RunID: "run_synth-tmp", Session: contracts.Session{SessionID: "ses_synth-tmp"}}
	env := d.runEnv(nil, cmd, t.TempDir())
	want := filepath.Join(state, "tmp", "runs", "run_synth-tmp")
	if env["TMPDIR"] != want {
		t.Fatalf("TMPDIR = %q, want %q", env["TMPDIR"], want)
	}
	st, err := os.Stat(want)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("run temp dir: %v %v", st, err)
	}
	if env["CLAUDE_CODE_TMPDIR"] != "" {
		t.Fatalf("CLAUDE_CODE_TMPDIR set by runEnv: %q", env["CLAUDE_CODE_TMPDIR"])
	}
}

// A TMPDIR the owner configured (environment profile, env file) or a node
// that turned per-run folders off keeps its TMPDIR.
func TestRunEnvKeepsOwnerTmpDir(t *testing.T) {
	state := t.TempDir()
	d := &Daemon{cfg: Config{StateDir: state, PyreBinDir: "/opt/synth/bin"}}
	cmd := contracts.StartRunCommand{RunID: "run_synth-owner", Session: contracts.Session{SessionID: "ses_synth-owner"}}
	if env := d.runEnv(map[string]string{"TMPDIR": "/synthetic/owner-tmp"}, cmd, t.TempDir()); env["TMPDIR"] != "/synthetic/owner-tmp" {
		t.Fatalf("owner TMPDIR overridden: %q", env["TMPDIR"])
	}
	if _, err := os.Stat(filepath.Join(state, "tmp", "runs", "run_synth-owner")); err == nil {
		t.Fatal("run temp dir created although the owner set TMPDIR")
	}
	d.cfg.RunTmpRoot = runTmpNone
	if env := d.runEnv(nil, cmd, t.TempDir()); env["TMPDIR"] != "" {
		t.Fatalf("run-tmp-root none: TMPDIR = %q", env["TMPDIR"])
	}
	d.cfg.RunTmpRoot = filepath.Join(state, "with space")
	if env := d.runEnv(nil, cmd, t.TempDir()); env["TMPDIR"] != "" {
		t.Fatalf("root with whitespace: TMPDIR = %q", env["TMPDIR"])
	}
}

// The real scan sees a file this process holds open under the root.
func TestOpenPathsUnderSeesOwnFile(t *testing.T) {
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "held.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	resolved, _ := filepath.EvalSymlinks(root)
	paths, ok := openPathsUnder([]string{root, resolved})
	if !ok {
		t.Skip("open files cannot be listed here")
	}
	if !inUse(root, paths) && !inUse(resolved, paths) {
		t.Fatalf("open file not seen: %v", paths)
	}
}

// Cleanup removes the folder of an archived session's ended run and of a run
// that ended over a day ago; it keeps a live run's, one a newer live run of
// the same session follows, one a process has open, and everything when
// open files cannot be listed.
func TestRunTmpCleanup(t *testing.T) {
	n := startNode(t, t.TempDir(), Config{})
	ctx := context.Background()
	now := time.Now()
	put := func(runID, sessionID, phase string, state contracts.RunState, ended time.Time, created time.Time) string {
		t.Helper()
		run := contracts.Run{RunID: runID, SessionID: sessionID, Adapter: "mock", Harness: contracts.HarnessMock, State: state}
		if !ended.IsZero() {
			run.EndedAt = &ended
		}
		rec := runRecord{Session: contracts.Session{SessionID: sessionID, Harness: contracts.HarnessMock}, Phase: phase, Run: run, CreatedAt: created}
		if err := sqlitex.Tx(ctx, n.db, func(tx *sql.Tx) error { return putRunTx(ctx, tx, rec, now) }); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{}
		n.d.setRunTmp(env, runID)
		if env["TMPDIR"] == "" {
			t.Fatalf("no temp dir for %s", runID)
		}
		if err := os.WriteFile(filepath.Join(env["TMPDIR"], "scratch.txt"), []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
		return env["TMPDIR"]
	}
	old := now.Add(-48 * time.Hour)
	stale := put("run_synth-stale", "ses_synth-stale", phaseEnded, contracts.RunExited, old, old)
	recent := put("run_synth-recent", "ses_synth-recent", phaseEnded, contracts.RunExited, now.Add(-time.Hour), now.Add(-2*time.Hour))
	live := put("run_synth-live", "ses_synth-live", phaseLive, contracts.RunRunning, time.Time{}, old)
	busy := put("run_synth-busy", "ses_synth-busy", phaseEnded, contracts.RunExited, old, old)
	resumed := put("run_synth-first", "ses_synth-resumed", phaseEnded, contracts.RunExited, old, old)
	put("run_synth-second", "ses_synth-resumed", phaseLive, contracts.RunRunning, time.Time{}, now.Add(-time.Hour))

	saved := runTmpOpenPaths
	t.Cleanup(func() { runTmpOpenPaths = saved })
	runTmpOpenPaths = func([]string) ([]string, bool) { return nil, false }
	if got := n.d.sweepRunTmp(ctx); got != 0 {
		t.Fatalf("removed %d with open files unknown", got)
	}
	runTmpOpenPaths = func(roots []string) ([]string, bool) {
		return []string{filepath.Join(busy, "scratch.txt")}, true
	}
	if got := n.d.sweepRunTmp(ctx); got != 1 {
		t.Fatalf("maintenance pass removed %d, want 1", got)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	for p, want := range map[string]bool{stale: false, recent: true, live: true, busy: true, resumed: true} {
		if exists(p) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(p), !want, want)
		}
	}
	// Archived: the session's ended runs go at once; a live run's never.
	if got := n.d.removeSessionRunTmp(ctx, "ses_synth-recent"); got != 1 || exists(recent) {
		t.Fatalf("archived session: removed %d, exists %v", got, exists(recent))
	}
	if got := n.d.removeSessionRunTmp(ctx, "ses_synth-live"); got != 0 || !exists(live) {
		t.Fatalf("live session: removed %d", got)
	}
	if got := n.d.removeSessionRunTmp(ctx, "ses_synth-busy"); got != 0 || !exists(busy) {
		t.Fatalf("in-use folder removed (%d)", got)
	}
}
