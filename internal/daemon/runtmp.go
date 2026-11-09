package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/repoindex"
)

// Per-run temp directories (W-runtmp).
//
// /tmp on the owner's Linux nodes is a RAM-backed tmpfs with a per-user
// quota; agents' build outputs, review checkouts and tool caches filled it.
// Every run gets TMPDIR=$HOME/.ptmp/<id> (0700) on the node's data disk
// instead, unless its environment already sets TMPDIR (an environment
// profile, launch prep or the session's env file) or the owner turned it off
// (Config.RunTmpRoot "none"). Claude's own CLAUDE_CODE_TMPDIR is separate;
// its session folders are removed by the same rules (harnesstmp.go).
//
// The path is kept short (27 characters for /home/<4-char user>): Chromium,
// tmux, ssh ControlPath and gpg-agent put Unix sockets under $TMPDIR, and a
// socket path is limited to 108 bytes (104 on macOS). <id> is 10 lowercase
// base32 characters of a hash of the run ID (the next hash when that name
// belongs to another run); the folder's .run file holds the run ID, which is
// how cleanup maps a folder back to its run. Folders from rc99-rc103,
// <state>/tmp/runs/<run_id>, are still cleaned by the same rules.
//
// A run's folder is removed once nothing needs it: its session was archived
// over an hour ago (session.cleanup / node.sweep only mark the folders, with
// a .archived file holding the time; the maintenance pass removes them, so
// a job the session left running keeps its $TMPDIR files for that hour); or
// the run ended over an hour ago and a newer run of its session (a resume,
// continuation or account switch) superseded it; or the run is its
// session's latest on this node and ended more than a day ago. A resume
// never reuses an earlier run's folder (each run ID gets its own). A folder
// any process still has open (a file, its cwd or its executable) is kept,
// since a detached job may still use the old run's TMPDIR; so is every folder when this node cannot tell (no
// /proc, no lsof). A folder under the root with no .run file, whose name is
// no run's, is not Pyre's and is never touched. The maintenance pass runs
// every 10 minutes and after a run ends (at most once a minute).

const (
	runTmpNone = "none"
	// runTmpIdle: a session's latest run that ended this long ago has its
	// temp folder removed (so does a folder whose run has no record, after
	// this long with no change).
	runTmpIdle = 24 * time.Hour
	// runTmpGrace: a run a newer run of its session superseded has its
	// temp folder removed this long after it ended; an archived session's
	// run, this long after the archive.
	runTmpGrace = time.Hour
	// runTmpKickEvery bounds the passes that runs' ends start.
	runTmpKickEvery = time.Minute
	// runTmpExamineMax bounds the folders looked up per maintenance pass
	// (from a random offset, so a large backlog is reached over passes);
	// runTmpRemoveMax bounds the removals.
	runTmpExamineMax = 256
	runTmpRemoveMax  = 32
	// runTmpIDLen is the length of a folder name; runTmpTries bounds the
	// names tried for one run.
	runTmpIDLen = 10
	runTmpTries = 8
	// runTmpRunFile, inside each folder, holds the folder's run ID;
	// runTmpArchivedFile, when the session was archived (RFC 3339).
	runTmpRunFile      = ".run"
	runTmpArchivedFile = ".archived"
	// runTmpLongPath: a TMPDIR longer than this is logged (sockets under it
	// may not fit).
	runTmpLongPath = 40
)

// runTmpOpenPaths lists the paths under roots that this user's processes
// have open (files, working directories, executables). ok is false when the
// node cannot tell; nothing is removed then. Tests replace it.
var runTmpOpenPaths = openPathsUnder

// runTmpRoot is the directory holding per-run temp folders, or "" when runs
// keep the TMPDIR they inherit (runTmpRootFor).
func (d *Daemon) runTmpRoot() string { return runTmpRootFor(d.cfg, runtime.GOOS) }

// RunTmpRoot is runTmpRoot for a node configuration (the agent guide
// mentions $TMPDIR only when runs get their own).
func RunTmpRoot(cfg Config) string { return runTmpRootFor(cfg, runtime.GOOS) }

// runTmpRootFor is the root on goos: Config.RunTmpRoot when set, else
// $HOME/.ptmp, or <state>/rt when HOME is unknown or so long that
// <state>/rt is shorter (setRunTmp logs a TMPDIR over 40 characters). It is
// "" when turned off, on Windows (its TEMP is already on the user's disk
// and the node cannot tell which files are open there), or when the path
// has whitespace (many scripts expand $TMPDIR unquoted; on macOS that rules
// out the default state dir but not $HOME/.ptmp).
func runTmpRootFor(cfg Config, goos string) string {
	if cfg.RunTmpRoot == runTmpNone || goos == "windows" {
		return ""
	}
	if cfg.RunTmpRoot != "" {
		return usableRunTmpRoot(expandHome(cfg.RunTmpRoot))
	}
	home := usableRunTmpRoot(joinIfSet(cfg.UserHome, ".ptmp"))
	if home != "" && len(home)+1+runTmpIDLen <= runTmpLongPath {
		return home
	}
	if state := usableRunTmpRoot(joinIfSet(cfg.StateDir, "rt")); state != "" && (home == "" || len(state) < len(home)) {
		return state
	}
	return home
}

func joinIfSet(dir, name string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

func usableRunTmpRoot(root string) string {
	if !filepath.IsAbs(root) || strings.ContainsAny(root, " \t\n") {
		return ""
	}
	return filepath.Clean(root)
}

// legacyRunTmpRoot is where rc99-rc103 put run folders (<root>/<run_id>);
// the maintenance pass still sweeps it.
func (d *Daemon) legacyRunTmpRoot() string {
	if d.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(d.cfg.StateDir, "tmp", "runs")
}

var runTmpEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// runTmpName is the n-th folder name tried for runID.
func runTmpName(runID string, n int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", runID, n)))
	return runTmpEncoding.EncodeToString(h[:])[:runTmpIDLen]
}

// runTmpOwner is the run ID a folder's .run file names, or "".
func runTmpOwner(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, runTmpRunFile))
	if err != nil {
		return ""
	}
	if id := strings.TrimSpace(string(b)); safeRunName(id) {
		return id
	}
	return ""
}

func safeRunName(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}

func isPlainDir(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.IsDir()
}

// makeRunTmp creates (or finds) the run's folder under root: the first of
// its names that is free or already the run's.
func makeRunTmp(root, runID string) (string, error) {
	if !safeRunName(runID) {
		return "", fmt.Errorf("unusable run id %q", runID)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	for n := 0; n < runTmpTries; n++ {
		dir := filepath.Join(root, runTmpName(runID, n))
		err := os.Mkdir(dir, 0o700)
		if os.IsExist(err) {
			if runTmpOwner(dir) != runID {
				continue // another run's (or one being made): the next name
			}
			return dir, os.Chmod(dir, 0o700)
		}
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, runTmpRunFile), []byte(runID+"\n"), 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		return dir, nil
	}
	return "", fmt.Errorf("no free run temp name under %s after %d tries", root, runTmpTries)
}

// runTmpDirs are the existing temp folders of a run: its short folder under
// root and a legacy <root>/<run_id> or <state>/tmp/runs/<run_id>.
func (d *Daemon) runTmpDirs(root, runID string) []string {
	if !safeRunName(runID) {
		return nil
	}
	var dirs []string
	for n := 0; n < runTmpTries; n++ {
		if dir := filepath.Join(root, runTmpName(runID, n)); runTmpOwner(dir) == runID {
			dirs = append(dirs, dir)
			break
		}
	}
	for _, r := range []string{root, d.legacyRunTmpRoot()} {
		if r == "" {
			continue
		}
		if dir := filepath.Join(r, runID); isPlainDir(dir) && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// setRunTmp points the run's TMPDIR at its own folder, creating it, unless
// env already sets TMPDIR.
func (d *Daemon) setRunTmp(env map[string]string, runID string) {
	if _, set := env["TMPDIR"]; set {
		return
	}
	root := d.runTmpRoot()
	if root == "" {
		return
	}
	dir, err := makeRunTmp(root, runID)
	if err != nil {
		if d.cfg.Logger != nil {
			d.cfg.Logger.Warn("run temp dir unavailable; the run keeps the inherited TMPDIR", "run", runID, "err", err)
		}
		return
	}
	if len(dir) > runTmpLongPath && d.cfg.Logger != nil {
		d.cfg.Logger.Warn("run TMPDIR is long; Unix sockets under it may exceed the path limit", "run", runID, "dir", dir, "len", len(dir))
	}
	env["TMPDIR"] = dir
}

// markSessionRunTmp marks the temp folders of a session's ended runs as
// archived now (the session was archived): their $TMPDIR folders and their
// Claude session folders (harnesstmp.go). The maintenance pass removes them
// runTmpGrace later, unless a process still has them open: a detached job
// the session left running may still write to its $TMPDIR. A folder
// already marked keeps its first time. It returns the folders marked.
func (d *Daemon) markSessionRunTmp(ctx context.Context, sessionID string) int {
	recs, err := d.st.runs(ctx, `session_id=?`, sessionID)
	if err != nil {
		return 0
	}
	root := d.runTmpRoot()
	stamp := []byte(d.now().UTC().Format(time.RFC3339) + "\n")
	n := 0
	for _, r := range recs {
		if !d.runEnded(r) {
			continue
		}
		if root != "" {
			for _, dir := range d.runTmpDirs(root, r.Run.RunID) {
				if markArchived(dir, stamp) {
					n++
				}
			}
		}
		n += d.markHarnessTmp(r, stamp)
	}
	return n
}

// markArchived writes the folder's .archived file unless it has one.
func markArchived(dir string, stamp []byte) bool {
	f, err := os.OpenFile(filepath.Join(dir, runTmpArchivedFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false // already marked, or gone
	}
	_, werr := f.Write(stamp)
	return f.Close() == nil && werr == nil
}

// runTmpArchivedAt is when the folder's session was archived
// (markSessionRunTmp); ok is false when it is not marked.
func runTmpArchivedAt(dir string) (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(dir, runTmpArchivedFile))
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	return at, err == nil
}

// sweepRunTmp is the maintenance pass over the root and the legacy root:
// it removes the folders of runs whose session was archived over an hour
// ago, of superseded runs that ended over an hour ago, of sessions' latest
// runs that ended over a day ago, and folders whose .run (or, in the
// legacy root, name) is a run with no record that nothing touched for a
// day.
func (d *Daemon) sweepRunTmp(ctx context.Context) int {
	root := d.runTmpRoot()
	if root == "" {
		return 0
	}
	n := d.sweepRunTmpRoot(ctx, root)
	if legacy := d.legacyRunTmpRoot(); legacy != "" && legacy != root {
		n += d.sweepRunTmpRoot(ctx, legacy)
	}
	return n
}

func (d *Daemon) sweepRunTmpRoot(ctx context.Context, root string) int {
	ents, err := os.ReadDir(root)
	if err != nil || len(ents) == 0 {
		return 0
	}
	now := d.now()
	legacy := d.legacyRunTmpRoot()
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
		if dir == legacy {
			continue // a root configured as <state>/tmp: swept on its own
		}
		// A short folder names its run in .run; a legacy one is named
		// after it. Under the root, a folder with neither is not Pyre's.
		runID, named := runTmpOwner(dir), true
		if runID == "" {
			runID, named = e.Name(), root == legacy
		}
		if d.runTmpExpired(ctx, runID, dir, now, named) {
			dirs = append(dirs, dir)
		}
	}
	return d.removeRunTmpDirs(root, dirs)
}

// runTmpExpired reports whether the run's folder may go (before the
// open-files check). named is false for a folder outside the legacy root
// that names runID only by its own name: it may go only when runID has a
// record.
func (d *Daemon) runTmpExpired(ctx context.Context, runID, dir string, now time.Time, named bool) bool {
	rec, ok, err := d.st.run(ctx, runID)
	if err != nil {
		return false
	}
	if !ok {
		if !named {
			return false
		}
		// No record (a state dir restored without its database, a legacy
		// folder whose run is gone): only a folder nothing changed in for
		// a day.
		newest, _, walked := newestChange(dir)
		return walked && now.Sub(newest) >= runTmpIdle
	}
	if !d.runEnded(rec) {
		return false
	}
	ended := now.Sub(runEndedAt(rec))
	if ended < runTmpGrace {
		return false
	}
	if at, ok := runTmpArchivedAt(dir); ok && now.Sub(at) >= runTmpGrace {
		return true // its session was archived over an hour ago
	}
	latest, ok, err := d.st.latestRunForSession(ctx, rec.Session.SessionID)
	if err != nil {
		return false
	}
	if !ok || latest.Run.RunID != rec.Run.RunID {
		return true // superseded: the newer run has its own folder
	}
	return ended >= runTmpIdle // the session's current run
}

// kickRunTmpSweep asks the maintenance loop for a temp-folder pass: a run
// ended, so earlier runs it superseded may be due.
func (d *Daemon) kickRunTmpSweep() {
	select {
	case d.runTmpKick <- struct{}{}:
	default:
	}
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

// removeRunTmpDirs removes the folders (children of root) no process has
// open; with no way to tell, it removes none.
func (d *Daemon) removeRunTmpDirs(root string, dirs []string) int {
	if len(dirs) == 0 {
		return 0
	}
	// Processes report resolved paths: a root behind a symlink is matched
	// under both spellings.
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
	n, kept := 0, 0
	var freed int64
	for _, dir := range dirs {
		if inUse(dir, open) || inUse(filepath.Join(resolved, filepath.Base(dir)), open) {
			kept++
			continue
		}
		run, size := runTmpOwner(dir), dirBytes(dir)
		if run == "" {
			run = filepath.Base(dir) // legacy <root>/<run_id>
		}
		if err := os.RemoveAll(dir); err != nil {
			d.cfg.Logger.Warn("remove run temp dir", "dir", dir, "err", err)
			continue
		}
		d.cfg.Logger.Info("run temp dir removed", "dir", dir, "run", run, "bytes", size)
		n++
		freed += size
	}
	if n > 0 || kept > 0 {
		d.cfg.Logger.Info("run temp dirs removed", "count", n, "bytes_freed", freed, "kept_in_use", kept)
	}
	return n
}

// dirBytes is the total size of the regular files under dir.
func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
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
