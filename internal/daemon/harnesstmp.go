package daemon

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Harness temp folders of ended runs (W-runtmp).
//
// Claude keeps a session's scratchpad and background-task output under
// CLAUDE_CODE_TMPDIR (<state>/tmp/claude): <root>/claude-<uid>/<cwd key>/
// <session UUID>. Agents write renders and builds there, so a session's
// folder can hold many gigabytes. A folder whose UUID is a run's Claude
// session (the run's native ref) follows that run's $TMPDIR rules
// (runTmpExpired): it goes an hour after its session was archived, an hour
// after a newer run of its session superseded the run, or a day after the
// session's latest run ended. A UUID several runs share (a resume that kept
// the Claude session) goes only when every one of them is done, and a live
// run's UUID never does. Folders a process still has open stay. Folders no
// run names (the Claude session before a /clear, a nested claude a tool
// started) are left to the sweep at pyred start (untouched for a day, no
// live run working in that directory) and the nightly node sweep.

// harnessTmpStartIdle: the sweep at pyred start removes session folders
// untouched this long.
const harnessTmpStartIdle = 24 * time.Hour

// harnessSessionDirs lists the session folders under root by UUID, as
// sweepTmpDir finds them (up to three levels down).
func harnessSessionDirs(root string) map[string][]string {
	out := map[string][]string{}
	var find func(dir string, depth int)
	find = func(dir string, depth int) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if uuidDirRe.MatchString(e.Name()) {
				out[e.Name()] = append(out[e.Name()], p)
				continue
			}
			if depth < 3 {
				find(p, depth+1)
			}
		}
	}
	find(root, 1)
	return out
}

// sweepHarnessTmp is the maintenance pass over the harness temp roots: it
// removes the session folders of runs that are done by runTmpExpired's
// rules.
func (d *Daemon) sweepHarnessTmp(ctx context.Context) int {
	n := 0
	for _, root := range d.harnessTmpDirs() {
		dirs := harnessSessionDirs(root)
		if len(dirs) == 0 {
			continue
		}
		uuids := make([]string, 0, len(dirs))
		for id := range dirs {
			uuids = append(uuids, id)
		}
		owners, err := d.st.runsByNativeID(ctx, uuids)
		if err != nil {
			if ctx.Err() == nil {
				d.noteErr("harness temp owners: %v", err)
			}
			continue
		}
		live, _ := d.liveTmpKeys()
		now := d.now()
		var due []string
		for id, paths := range dirs {
			if ctx.Err() != nil {
				return n
			}
			runs := owners[id]
			if live[id] || len(runs) == 0 {
				continue
			}
			for _, p := range paths {
				if d.harnessTmpExpired(ctx, runs, p, now) {
					due = append(due, p)
				}
			}
		}
		n += d.removeHarnessTmpDirs(root, due)
	}
	return n
}

// harnessTmpExpired reports whether a session folder may go: every run
// that used its Claude session is done with it. An archive mark older than
// one of those runs is from an earlier archive of a session resumed since,
// and is dropped.
func (d *Daemon) harnessTmpExpired(ctx context.Context, runIDs []string, dir string, now time.Time) bool {
	if at, ok := runTmpArchivedAt(dir); ok {
		for _, id := range runIDs {
			if rec, ok, err := d.st.run(ctx, id); err == nil && ok && at.Before(runEndedAt(rec).Truncate(time.Second)) {
				_ = os.Remove(filepath.Join(dir, runTmpArchivedFile))
				break
			}
		}
	}
	for _, id := range runIDs {
		if !d.runTmpExpired(ctx, id, dir, now, true) {
			return false
		}
	}
	return true
}

// markHarnessTmp marks the session folders of a run's Claude session
// archived at stamp (markSessionRunTmp).
func (d *Daemon) markHarnessTmp(r runRecord, stamp []byte) int {
	if r.Run.NativeRef == nil || !uuidDirRe.MatchString(r.Run.NativeRef.ID) {
		return 0
	}
	n := 0
	for _, root := range d.harnessTmpDirs() {
		for _, dir := range harnessSessionDirs(root)[r.Run.NativeRef.ID] {
			if markArchived(dir, stamp) {
				n++
			}
		}
	}
	return n
}

// removeHarnessTmpDirs removes the session folders under root no process
// has open (with no way to tell, none), then the cwd-key folders they leave
// empty.
func (d *Daemon) removeHarnessTmpDirs(root string, dirs []string) int {
	if len(dirs) == 0 {
		return 0
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return 0
	}
	roots := []string{root}
	if resolved != root {
		roots = append(roots, resolved)
	}
	open, ok := runTmpOpenPaths(roots)
	if !ok {
		d.cfg.Logger.Debug("harness temp cleanup skipped: open files unknown", "dirs", len(dirs))
		return 0
	}
	n, kept := 0, 0
	var freed int64
	for _, dir := range dirs {
		if inUse(dir, open) || inUse(filepath.Join(resolved, strings.TrimPrefix(dir, root)), open) {
			kept++
			continue
		}
		size := dirBytes(dir)
		if err := os.RemoveAll(dir); err != nil {
			d.cfg.Logger.Warn("remove harness temp dir", "dir", dir, "err", err)
			continue
		}
		removeEmptyParents(filepath.Dir(dir), root)
		n++
		freed += size
	}
	if n > 0 || kept > 0 {
		d.cfg.Logger.Info("harness temp dirs removed", "count", n, "bytes_freed", freed, "kept_in_use", kept)
	}
	return n
}

// startupHarnessTmpSweep runs once at pyred start (after runs are
// recovered): session folders untouched for a day go, unless a live run
// uses that Claude session or works in that folder's directory.
func (d *Daemon) startupHarnessTmpSweep() {
	ids, keys := d.liveTmpKeys()
	for _, root := range d.harnessTmpDirs() {
		n, freed, err := sweepTmpDir(root, harnessTmpStartIdle, d.now(), ids, keys, false)
		if err != nil {
			d.cfg.Logger.Warn("startup harness temp sweep", "root", root, "err", err)
		}
		if n > 0 {
			d.cfg.Logger.Info("startup harness temp sweep", "root", root, "removed", n, "bytes_freed", freed)
		}
	}
}
