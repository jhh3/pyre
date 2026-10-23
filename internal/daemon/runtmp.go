package daemon

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/repoindex"
)

// Per-run temp directories (W-runtmp).
//
// /tmp on the owner's Linux nodes is a RAM-backed tmpfs with a per-user
// quota; agents' build outputs, review checkouts and tool caches filled it.
// Every run gets TMPDIR=<state dir>/tmp/runs/<run_id> (0700) on the node's
// data disk instead, unless its environment already sets TMPDIR (an
// environment profile, launch prep or the session's env file) or the owner
// turned it off (Config.RunTmpRoot "none"). Claude's own CLAUDE_CODE_TMPDIR
// is separate and unchanged.
//
// A run's folder is removed once nothing needs it: its session was archived
// (session.cleanup / node.sweep), or it ended more than a day ago and its
// session has no newer run on this node still going. A folder any process
// still has open (a file, its cwd or its executable) is kept; so is every
// folder when this node cannot tell (no /proc, no lsof).

const (
	runTmpNone = "none"
	// runTmpIdle: a run that ended this long ago, with no newer run of its
	// session, has its temp folder removed.
	runTmpIdle = 24 * time.Hour
	// runTmpExamineMax bounds the folders looked up per maintenance pass
	// (from a random offset, so a large backlog is reached over passes);
	// runTmpRemoveMax bounds the removals.
	runTmpExamineMax = 256
	runTmpRemoveMax  = 32
)

// runTmpOpenPaths lists the paths under roots that this user's processes
// have open (files, working directories, executables). ok is false when the
// node cannot tell; nothing is removed then. Tests replace it.
var runTmpOpenPaths = openPathsUnder

// runTmpRoot is the directory holding per-run temp folders, or "" when runs
// keep the TMPDIR they inherit: turned off, no state dir, Windows (its
// TEMP is already on the user's disk and the node cannot tell which files
// are open there), or a path with whitespace (many scripts expand $TMPDIR
// unquoted; macOS's default state dir has a space).
func (d *Daemon) runTmpRoot() string { return RunTmpRoot(d.cfg) }

// RunTmpRoot is runTmpRoot for a node configuration (the agent guide
// mentions $TMPDIR only when runs get their own).
func RunTmpRoot(cfg Config) string {
	root := cfg.RunTmpRoot
	if root == runTmpNone || runtime.GOOS == "windows" {
		return ""
	}
	if root == "" {
		if cfg.StateDir == "" {
			return ""
		}
		root = filepath.Join(cfg.StateDir, "tmp", "runs")
	}
	root = expandHome(root)
	if !filepath.IsAbs(root) || strings.ContainsAny(root, " \t\n") {
		return ""
	}
	return filepath.Clean(root)
}

// runTmpDir is the run's temp folder, or "" (see runTmpRoot).
func (d *Daemon) runTmpDir(runID string) string {
	root := d.runTmpRoot()
	if root == "" || !safeRunName(runID) {
		return ""
	}
	return filepath.Join(root, runID)
}

func safeRunName(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}

// setRunTmp points the run's TMPDIR at its own folder, creating it, unless
// env already sets TMPDIR.
func (d *Daemon) setRunTmp(env map[string]string, runID string) {
	if _, set := env["TMPDIR"]; set {
		return
	}
	dir := d.runTmpDir(runID)
	if dir == "" {
		return
	}
	err := os.MkdirAll(filepath.Dir(dir), 0o700)
	if err == nil {
		err = os.Mkdir(dir, 0o700)
		if os.IsExist(err) {
			err = os.Chmod(dir, 0o700)
		}
	}
	if err != nil {
		if d.cfg.Logger != nil {
			d.cfg.Logger.Warn("run temp dir unavailable; the run keeps the inherited TMPDIR", "run", runID, "err", err)
		}
		return
	}
	env["TMPDIR"] = dir
}

// removeSessionRunTmp removes the temp folders of a session's ended runs
// (the session was archived). Folders still open are kept for the
// maintenance pass.
func (d *Daemon) removeSessionRunTmp(ctx context.Context, sessionID string) int {
	root := d.runTmpRoot()
	if root == "" {
		return 0
	}
	recs, err := d.st.runs(ctx, `session_id=?`, sessionID)
	if err != nil {
		return 0
	}
	var dirs []string
	for _, r := range recs {
		if !d.runEnded(r) {
			continue
		}
		if dir := d.runTmpDir(r.Run.RunID); dir != "" {
			if _, err := os.Lstat(dir); err == nil {
				dirs = append(dirs, dir)
			}
		}
	}
	return d.removeRunTmpDirs(root, dirs)
}

// sweepRunTmp is the maintenance pass: it removes the folders of runs that
// ended over a day ago with no newer run of their session still going, and
// folders no run record names that nothing touched for a day.
func (d *Daemon) sweepRunTmp(ctx context.Context) int {
	root := d.runTmpRoot()
	if root == "" {
		return 0
	}
	ents, err := os.ReadDir(root)
	if err != nil || len(ents) == 0 {
		return 0
	}
	now := d.now()
	// A random start: folders kept as in use never crowd out the rest.
	start := rand.IntN(len(ents))
	var dirs []string
	for i := 0; i < len(ents) && i < runTmpExamineMax && len(dirs) < runTmpRemoveMax; i++ {
		if ctx.Err() != nil {
			return 0
		}
		e := ents[(start+i)%len(ents)]
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || !safeRunName(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if d.runTmpExpired(ctx, e.Name(), dir, now) {
			dirs = append(dirs, dir)
		}
	}
	return d.removeRunTmpDirs(root, dirs)
}

// runTmpExpired reports whether the run's folder may go (before the
// open-files check).
func (d *Daemon) runTmpExpired(ctx context.Context, runID, dir string, now time.Time) bool {
	rec, ok, err := d.st.run(ctx, runID)
	if err != nil {
		return false
	}
	if !ok {
		// No record (a state dir restored without its database): only a
		// folder nothing changed in for a day.
		newest, _, walked := newestChange(dir)
		return walked && now.Sub(newest) >= runTmpIdle
	}
	if !d.runEnded(rec) || now.Sub(runEndedAt(rec)) < runTmpIdle {
		return false
	}
	latest, ok, err := d.st.latestRunForSession(ctx, rec.Session.SessionID)
	if err != nil || !ok {
		return err == nil
	}
	return latest.Run.RunID == rec.Run.RunID || d.runEnded(latest) && now.Sub(runEndedAt(latest)) >= runTmpIdle
}

// runEnded reports a run as over: its record is terminal (or was orphaned
// mid-launch) and it is not live in this daemon.
func (d *Daemon) runEnded(r runRecord) bool {
	if lr, live := d.getLive(r.Run.RunID); live {
		if cur, _ := lr.snapshot(); !cur.Run.State.Terminal() {
			return false
		}
		return true
	}
	return r.Phase == phaseEnded || r.Phase == phaseOrphaned || r.Run.State.Terminal()
}

func runEndedAt(r runRecord) time.Time {
	if r.Run.EndedAt != nil {
		return *r.Run.EndedAt
	}
	return r.CreatedAt
}

// removeRunTmpDirs removes the folders no process has open; with no way to
// tell, it removes none.
func (d *Daemon) removeRunTmpDirs(root string, dirs []string) int {
	if len(dirs) == 0 {
		return 0
	}
	// Processes report resolved paths: a state dir behind a symlink is
	// matched under both spellings.
	roots := []string{root}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return 0
	}
	if resolved != root {
		roots = append(roots, resolved)
	}
	open, ok := runTmpOpenPaths(roots)
	if !ok {
		d.cfg.Logger.Debug("run temp cleanup skipped: open files unknown", "dirs", len(dirs))
		return 0
	}
	n := 0
	for _, dir := range dirs {
		if inUse(dir, open) || inUse(filepath.Join(resolved, filepath.Base(dir)), open) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			d.cfg.Logger.Warn("remove run temp dir", "dir", dir, "err", err)
			continue
		}
		n++
	}
	if n > 0 {
		d.cfg.Logger.Info("run temp dirs removed", "count", n)
	}
	return n
}

func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if repoindex.Inside(p, r) {
			return true
		}
	}
	return false
}

func inUse(dir string, open []string) bool {
	for _, p := range open {
		if repoindex.Inside(p, dir) {
			return true
		}
	}
	return false
}
