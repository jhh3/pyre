//go:build linux || darwin

package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// W-runtmp: every run's TMPDIR is its own short 0700 folder under
// $HOME/.ptmp whose .run file names the run.
func TestRunEnvSetsRunTmpDir(t *testing.T) {
	// The state dir lies deeper than HOME, as ~/.local/state/pyre does.
	state, home := filepath.Join(t.TempDir(), ".local", "state", "pyre"), t.TempDir()
	d := &Daemon{cfg: Config{StateDir: state, UserHome: home, PyreBinDir: "/opt/synth/bin"}}
	cmd := contracts.StartRunCommand{RunID: "run_synth-tmp", Session: contracts.Session{SessionID: "ses_synth-tmp"}}
	env := d.runEnv(nil, cmd, t.TempDir())
	want := filepath.Join(home, ".ptmp", runTmpName("run_synth-tmp", 0))
	if env["TMPDIR"] != want || len(filepath.Base(want)) != runTmpIDLen {
		t.Fatalf("TMPDIR = %q, want %q", env["TMPDIR"], want)
	}
	st, err := os.Stat(want)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("run temp dir: %v %v", st, err)
	}
	if got := runTmpOwner(want); got != "run_synth-tmp" {
		t.Fatalf(".run = %q", got)
	}
	if env["CLAUDE_CODE_TMPDIR"] != "" {
		t.Fatalf("CLAUDE_CODE_TMPDIR set by runEnv: %q", env["CLAUDE_CODE_TMPDIR"])
	}
	// A resume or account switch is a new run of the session: its own,
	// new folder, never the earlier run's.
	next := contracts.StartRunCommand{RunID: "run_synth-tmp2", Session: cmd.Session}
	env2 := d.runEnv(nil, next, t.TempDir())
	if env2["TMPDIR"] == "" || env2["TMPDIR"] == want || runTmpOwner(env2["TMPDIR"]) != "run_synth-tmp2" {
		t.Fatalf("resumed run TMPDIR = %q (first run's %q)", env2["TMPDIR"], want)
	}
}

// A TMPDIR the owner configured (environment profile, env file) or a node
// that turned per-run folders off keeps its TMPDIR.
func TestRunEnvKeepsOwnerTmpDir(t *testing.T) {
	state, home := filepath.Join(t.TempDir(), ".local", "state", "pyre"), t.TempDir()
	d := &Daemon{cfg: Config{StateDir: state, UserHome: home, PyreBinDir: "/opt/synth/bin"}}
	cmd := contracts.StartRunCommand{RunID: "run_synth-owner", Session: contracts.Session{SessionID: "ses_synth-owner"}}
	if env := d.runEnv(map[string]string{"TMPDIR": "/synthetic/owner-tmp"}, cmd, t.TempDir()); env["TMPDIR"] != "/synthetic/owner-tmp" {
		t.Fatalf("owner TMPDIR overridden: %q", env["TMPDIR"])
	}
	if _, err := os.Stat(filepath.Join(home, ".ptmp")); err == nil {
		t.Fatal("run temp root created although the owner set TMPDIR")
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

// iss_…: Chromium's
// $TMPDIR/.org.chromium.Chromium.XXXXXX/SingletonSocket (and tmux, ssh
// ControlPath, gpg-agent sockets) must fit the 104-byte macOS / 108-byte
// Linux sun_path limit.
func TestRunTmpPathShort(t *testing.T) {
	cfg := Config{UserHome: "/home/user", StateDir: "/home/user/.local/state/pyre"}
	root := runTmpRootFor(cfg, "linux")
	if root != "/home/user/.ptmp" {
		t.Fatalf("root = %q", root)
	}
	dir := filepath.Join(root, runTmpName("run_…", 0))
	if len(dir) > 40 {
		t.Fatalf("TMPDIR %q is %d characters, want <= 40", dir, len(dir))
	}
	socket := "/.org.chromium.Chromium.XXXXXX/SingletonSocket"
	socket += strings.Repeat("x", 60-len(socket))
	if p := dir + socket; len(socket) != 60 || len(p) >= 104 {
		t.Fatalf("socket path %q is %d bytes", p, len(p))
	}
	// A long HOME: the shorter state-dir root; no HOME: the state dir.
	longHome := "/" + strings.Repeat("h", 80)
	if got := runTmpRootFor(Config{UserHome: longHome, StateDir: "/srv/synth/state"}, "linux"); got != "/srv/synth/state/rt" {
		t.Fatalf("long HOME root = %q", got)
	}
	if got := runTmpRootFor(Config{StateDir: "/srv/synth/state"}, "linux"); got != "/srv/synth/state/rt" {
		t.Fatalf("no HOME root = %q", got)
	}
	if got := runTmpRootFor(Config{UserHome: "/home/user", RunTmpRoot: "/srv/synth/rt"}, "linux"); got != "/srv/synth/rt" {
		t.Fatalf("configured root = %q", got)
	}
}

// macOS gets a per-run TMPDIR under $HOME/.ptmp although its default state
// dir has a space; not when HOME has whitespace, never on Windows.
func TestRunTmpRootByOS(t *testing.T) {
	macState := "/Users/synth/Library/Application Support/pyre"
	if got := runTmpRootFor(Config{UserHome: "/Users/synth", StateDir: macState}, "darwin"); got != "/Users/synth/.ptmp" {
		t.Fatalf("darwin root = %q", got)
	}
	if got := runTmpRootFor(Config{UserHome: "/Users/sy nth", StateDir: macState}, "darwin"); got != "" {
		t.Fatalf("darwin HOME with a space: root = %q", got)
	}
	if got := runTmpRootFor(Config{UserHome: "/Users/synth", StateDir: macState, RunTmpRoot: runTmpNone}, "darwin"); got != "" {
		t.Fatalf("darwin none: root = %q", got)
	}
	if got := runTmpRootFor(Config{UserHome: `C:\Users\synth`, StateDir: `C:\synth\state`}, "windows"); got != "" {
		t.Fatalf("windows root = %q", got)
	}
}

// Folder names are unique per run: a name another run holds is skipped,
// the same run gets its folder back, and cleanup finds it by its .run file.
func TestRunTmpNameCollision(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rt")
	a, err := makeRunTmp(root, "run_synth-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := makeRunTmp(root, "run_synth-b")
	if err != nil || a == b {
		t.Fatalf("distinct runs share %q (%v)", a, err)
	}
	if again, err := makeRunTmp(root, "run_synth-a"); err != nil || again != a {
		t.Fatalf("same run: %q, want %q (%v)", again, a, err)
	}
	// Another run already holds run_synth-c's first name.
	taken := filepath.Join(root, runTmpName("run_synth-c", 0))
	if err := os.Mkdir(taken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taken, runTmpRunFile), []byte("run_synth-other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := makeRunTmp(root, "run_synth-c")
	if err != nil || c != filepath.Join(root, runTmpName("run_synth-c", 1)) {
		t.Fatalf("collision: got %q (%v)", c, err)
	}
	if runTmpOwner(taken) != "run_synth-other" || runTmpOwner(c) != "run_synth-c" {
		t.Fatal("owners mixed up")
	}
	d := &Daemon{cfg: Config{StateDir: t.TempDir()}}
	if got := d.runTmpDirs(root, "run_synth-c"); len(got) != 1 || got[0] != c {
		t.Fatalf("runTmpDirs = %v", got)
	}
	// Every name taken: an error, never another run's folder.
	for n := 0; n < runTmpTries; n++ {
		p := filepath.Join(root, runTmpName("run_synth-full", n))
		os.Mkdir(p, 0o700)
	}
	if got, err := makeRunTmp(root, "run_synth-full"); err == nil {
		t.Fatalf("all names taken: got %q", got)
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

// Cleanup removes the folder of an archived session's ended run an hour
// after the archive (session.cleanup only marks it), of a session's latest
// run that ended over a day ago, and of a run a newer run of its session
// superseded once it ended over an hour ago; it keeps a live
// run's, a superseded run's within the hour, the session's current run's,
// one a process has open, folders under the root that are not Pyre's (no
// .run, no run by that name), and everything when open files cannot be
// listed. Legacy <state>/tmp/runs/<run_id> folders (rc99-rc103) follow the
// same rules.
func TestRunTmpCleanup(t *testing.T) {
	n := startNode(t, t.TempDir(), Config{})
	ctx := context.Background()
	now := time.Now()
	record := func(runID, sessionID, phase string, state contracts.RunState, ended time.Time, created time.Time) {
		t.Helper()
		run := contracts.Run{RunID: runID, SessionID: sessionID, Adapter: "mock", Harness: contracts.HarnessMock, State: state}
		if !ended.IsZero() {
			run.EndedAt = &ended
		}
		rec := runRecord{Session: contracts.Session{SessionID: sessionID, Harness: contracts.HarnessMock}, Phase: phase, Run: run, CreatedAt: created}
		if err := sqlitex.Tx(ctx, n.db, func(tx *sql.Tx) error { return putRunTx(ctx, tx, rec, now) }); err != nil {
			t.Fatal(err)
		}
	}
	scratch := func(dir string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	put := func(runID, sessionID, phase string, state contracts.RunState, ended time.Time, created time.Time) string {
		t.Helper()
		record(runID, sessionID, phase, state, ended, created)
		env := map[string]string{}
		n.d.setRunTmp(env, runID)
		if env["TMPDIR"] == "" || !strings.HasPrefix(env["TMPDIR"], filepath.Join(n.dir, "rt")+string(filepath.Separator)) {
			t.Fatalf("temp dir for %s = %q", runID, env["TMPDIR"])
		}
		return scratch(env["TMPDIR"])
	}
	legacy := func(runID, sessionID, phase string, state contracts.RunState, ended time.Time, created time.Time) string {
		t.Helper()
		record(runID, sessionID, phase, state, ended, created)
		dir := filepath.Join(n.dir, "tmp", "runs", runID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return scratch(dir)
	}
	old := now.Add(-48 * time.Hour)
	stale := put("run_synth-stale", "ses_synth-stale", phaseEnded, contracts.RunExited, old, old)
	recent := put("run_synth-recent", "ses_synth-recent", phaseEnded, contracts.RunExited, now.Add(-time.Hour), now.Add(-2*time.Hour))
	live := put("run_synth-live", "ses_synth-live", phaseLive, contracts.RunRunning, time.Time{}, old)
	busy := put("run_synth-busy", "ses_synth-busy", phaseEnded, contracts.RunExited, old, old)
	resumed := put("run_synth-first", "ses_synth-resumed", phaseEnded, contracts.RunExited, old, old)
	put("run_synth-second", "ses_synth-resumed", phaseLive, contracts.RunRunning, time.Time{}, now.Add(-time.Hour))
	// Superseded by an account switch 2 h ago (past the 1 h grace), by a
	// resume 30 min ago (within it), and one a detached job still uses; the
	// session's current run ended 2 h ago and stays.
	switched := put("run_synth-sw1", "ses_synth-switched", phaseEnded, contracts.RunExited, now.Add(-2*time.Hour), now.Add(-5*time.Hour))
	justResumed := put("run_synth-sw2", "ses_synth-switched", phaseEnded, contracts.RunExited, now.Add(-30*time.Minute), now.Add(-2*time.Hour))
	superBusy := put("run_synth-sw3", "ses_synth-switched", phaseEnded, contracts.RunFailed, now.Add(-2*time.Hour), now.Add(-90*time.Minute))
	current := put("run_synth-sw4", "ses_synth-switched", phaseEnded, contracts.RunExited, now.Add(-2*time.Hour), now.Add(-30*time.Minute))
	// Folders under the root that are not Pyre's: no .run file, a name
	// that is no run's, nothing changed for days.
	unknown := []string{filepath.Join(n.dir, "rt", "winbuild"), filepath.Join(n.dir, "rt", "abcdefghij")}
	for _, dir := range unknown {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		scratch(dir)
		for _, p := range []string{filepath.Join(dir, "scratch.txt"), dir} {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	oldStale := legacy("run_synth-oldstale", "ses_synth-oldstale", phaseEnded, contracts.RunExited, old, old)
	oldBusy := legacy("run_synth-oldbusy", "ses_synth-oldbusy", phaseEnded, contracts.RunExited, old, old)
	oldRecent := legacy("run_synth-oldrecent", "ses_synth-oldrecent", phaseEnded, contracts.RunExited, now.Add(-time.Hour), now.Add(-2*time.Hour))
	oldLive := legacy("run_synth-oldlive", "ses_synth-oldlive", phaseLive, contracts.RunRunning, time.Time{}, old)

	saved := runTmpOpenPaths
	t.Cleanup(func() { runTmpOpenPaths = saved })
	runTmpOpenPaths = func([]string) ([]string, bool) { return nil, false }
	if got := n.d.sweepRunTmp(ctx); got != 0 {
		t.Fatalf("removed %d with open files unknown", got)
	}
	runTmpOpenPaths = func(roots []string) ([]string, bool) {
		// An open file, and a detached job's working directory.
		return []string{filepath.Join(busy, "scratch.txt"), filepath.Join(oldBusy, "scratch.txt"), superBusy}, true
	}
	if got := n.d.sweepRunTmp(ctx); got != 4 {
		t.Fatalf("maintenance pass removed %d, want 4", got)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	for p, want := range map[string]bool{stale: false, recent: true, live: true, busy: true, resumed: false,
		switched: false, justResumed: true, superBusy: true, current: true, unknown[0]: true, unknown[1]: true,
		oldStale: false, oldBusy: true, oldRecent: true, oldLive: true} {
		if exists(p) != want {
			t.Errorf("%s exists=%v, want %v", p, !want, want)
		}
	}
	// Archived: the session's ended runs are marked, not removed (a job it
	// left running may still use its $TMPDIR); a live run's are not marked.
	for sid, want := range map[string]int{"ses_synth-recent": 1, "ses_synth-oldrecent": 1, "ses_synth-busy": 1, "ses_synth-oldbusy": 1,
		"ses_synth-live": 0, "ses_synth-oldlive": 0} {
		if got := n.d.markSessionRunTmp(ctx, sid); got != want {
			t.Errorf("mark %s: marked %d, want %d", sid, got, want)
		}
	}
	if got := n.d.markSessionRunTmp(ctx, "ses_synth-recent"); got != 0 {
		t.Errorf("marked again: %d (the first archive time stays)", got)
	}
	// Within the hour after the archive: every folder stays.
	if got := n.d.sweepRunTmp(ctx); got != 0 {
		t.Fatalf("removed %d within the archive grace", got)
	}
	for _, p := range []string{recent, oldRecent, busy, oldBusy, live, oldLive} {
		if !exists(p) {
			t.Fatalf("%s removed within the archive grace", p)
		}
	}
	if _, ok := runTmpArchivedAt(live); ok {
		t.Fatal("a live run's folder was marked archived")
	}
	// An hour after the archive: unused folders go; one a process holds
	// stays, and so does a live run's.
	past := []byte(now.Add(-runTmpGrace-time.Minute).UTC().Format(time.RFC3339) + "\n")
	for _, p := range []string{recent, oldRecent, busy, oldBusy} {
		if err := os.WriteFile(filepath.Join(p, runTmpArchivedFile), past, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := n.d.sweepRunTmp(ctx); got != 2 {
		t.Fatalf("removed %d after the archive grace, want 2", got)
	}
	for p, want := range map[string]bool{recent: false, oldRecent: false, busy: true, oldBusy: true, live: true, oldLive: true} {
		if exists(p) != want {
			t.Errorf("after the grace: %s exists=%v, want %v", p, !want, want)
		}
	}
	// Once nothing holds them, the busy folders go on the next pass (with
	// the superseded one a detached job held).
	runTmpOpenPaths = func([]string) ([]string, bool) { return nil, true }
	if got := n.d.sweepRunTmp(ctx); got != 3 || exists(busy) || exists(oldBusy) || exists(superBusy) {
		t.Fatalf("released folders: removed %d (busy exists %v, legacy %v, superseded %v)", got, exists(busy), exists(oldBusy), exists(superBusy))
	}
}

// A run's end starts a pass: the earlier run it superseded (ended over an
// hour ago) loses its folder; the run that just ended, the session's
// current one, keeps its own.
func TestRunTmpSweepAtRunEnd(t *testing.T) {
	saved := runTmpOpenPaths
	t.Cleanup(func() { runTmpOpenPaths = saved })
	runTmpOpenPaths = func([]string) ([]string, bool) { return nil, true }
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}})
	ctx := context.Background()
	now := time.Now()
	ended := now.Add(-2 * time.Hour)
	run := contracts.Run{RunID: "run_synth-end1", SessionID: "ses_synth-end", Adapter: "mock", Harness: contracts.HarnessMock, State: contracts.RunExited, EndedAt: &ended}
	rec := runRecord{Session: contracts.Session{SessionID: "ses_synth-end", Harness: contracts.HarnessMock}, Phase: phaseEnded, Run: run, CreatedAt: now.Add(-3 * time.Hour)}
	if err := sqlitex.Tx(ctx, n.db, func(tx *sql.Tx) error { return putRunTx(ctx, tx, rec, now) }); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	n.d.setRunTmp(env, "run_synth-end1")
	first := env["TMPDIR"]
	if first == "" {
		t.Fatal("no temp dir for the first run")
	}
	n.start(startCmd("ses_synth-end", "run_synth-end2", contracts.HarnessMock, t.TempDir()))
	dirs := n.d.runTmpDirs(n.d.runTmpRoot(), "run_synth-end2")
	if len(dirs) != 1 || dirs[0] == first {
		t.Fatalf("second run's temp dirs = %v (first run's %s)", dirs, first)
	}
	if res := n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_synth-end", RunID: "run_synth-end2"})); res.Error != nil {
		t.Fatalf("stop = %+v", res.Error)
	}
	waitFor(t, "superseded run's temp dir removed", func() bool { _, err := os.Stat(first); return os.IsNotExist(err) })
	if _, err := os.Stat(dirs[0]); err != nil {
		t.Fatalf("current run's temp dir: %v", err)
	}
}
