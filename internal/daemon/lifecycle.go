package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// Session cleanup and the nightly node sweep (W-lifecycle).
//
// The hub archives sessions that ended (a child that exited cleanly after
// its final report, a session archived when done, anything ended for a day
// at the nightly sweep) and then asks the session's node to tidy up:
//
//   - session.cleanup stops processes the session left running (found by
//     PYRE_SESSION_ID in their environment: background shells, watchers)
//     and removes the worktrees it created under repoindex's automatic
//     rules (clean, branch merged or pushed, nobody working in it);
//   - node.sweep does the same for the sessions the nightly sweep archived,
//     then removes orphan worktrees of the node's repositories (untouched
//     for a day, same rules) and harness temp folders of sessions untouched
//     for three days.
//
// Nothing that holds unsaved or unpushed work is removed; every kept item
// says why.

const (
	defaultWorktreeIdle = 24 * time.Hour
	defaultTmpIdle      = 72 * time.Hour
	// tmpWalkMax bounds the files visited per temp folder; a larger folder
	// is kept (counts as recent).
	tmpWalkMax = 200000
	// sweepRunLookback: repositories of runs started this recently are swept.
	sweepRunLookback = 30 * 24 * time.Hour
	// processGrace is how long leftover processes get to exit on SIGTERM.
	processGrace = 3 * time.Second
)

var uuidDirRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (d *Daemon) registerLifecycleActions() {
	d.RegisterAction(contracts.ActionSessionCleanup, d.actionSessionCleanup)
	d.RegisterAction(contracts.ActionNodeSweep, d.actionNodeSweep)
}

func (d *Daemon) actionSessionCleanup(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeAction[contracts.SessionCleanupParams](contracts.ActionSessionCleanup, raw)
	if err != nil {
		return nil, err
	}
	if len(p.SessionIDs) == 0 || len(p.SessionIDs) > 1000 {
		return nil, contracts.Errorf(contracts.ErrInvalid, "name 1 to 1000 sessions")
	}
	return contracts.SessionCleanupResult{Sessions: d.cleanSessions(ctx, p.SessionIDs)}, nil
}

func (d *Daemon) actionNodeSweep(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeAction[contracts.NodeSweepParams](contracts.ActionNodeSweep, raw)
	if err != nil {
		return nil, err
	}
	idle := time.Duration(p.WorktreeIdleSeconds) * time.Second
	if idle <= 0 {
		idle = defaultWorktreeIdle
	}
	tmpIdle := time.Duration(p.TmpIdleSeconds) * time.Second
	if tmpIdle <= 0 {
		tmpIdle = defaultTmpIdle
	}
	var res contracts.NodeSweepResult
	if len(p.SessionIDs) > 0 && !p.DryRun {
		res.Sessions = d.cleanSessions(ctx, p.SessionIDs)
	}
	inUse := d.worktreeInUse("")
	live := d.liveSessions()
	for _, id := range p.KeepSessions {
		live[id] = true
	}
	keep := func(sid string) bool { return live[sid] } // live here, or not archived on the hub
	for _, repo := range d.sweepRepos() {
		if ctx.Err() != nil {
			break
		}
		res.Worktrees = append(res.Worktrees, repoindex.SweepRepo(ctx, repo, idle, d.now(), keep, inUse, p.DryRun)...)
	}
	ids, keys := d.liveTmpKeys()
	for _, dir := range d.harnessTmpDirs() {
		n, b, err := sweepTmpDir(dir, tmpIdle, d.now(), ids, keys, p.DryRun)
		res.TmpDirsRemoved += n
		res.TmpBytesFreed += b
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
	}
	d.cfg.Logger.Info("node sweep", "sessions", len(res.Sessions), "worktrees", len(res.Worktrees), "tmp_dirs", res.TmpDirsRemoved, "dry_run", p.DryRun)
	return res, nil
}

// cleanSessions stops each ended session's leftover processes and removes
// the worktrees it created.
func (d *Daemon) cleanSessions(ctx context.Context, ids []string) []contracts.SessionCleanup {
	out := make([]contracts.SessionCleanup, 0, len(ids))
	for _, id := range ids {
		sc := contracts.SessionCleanup{SessionID: id}
		rec, err := d.sessionRun(ctx, id)
		if err != nil {
			sc.Error = err.Error()
			out = append(out, sc)
			continue
		}
		if lr, live := d.getLive(rec.Run.RunID); live {
			if r, _ := lr.snapshot(); !r.Run.State.Terminal() {
				sc.Error = "still running; nothing was touched"
				out = append(out, sc)
				continue
			}
		}
		sids, _ := d.st.sessionPanePIDs(ctx, id)
		if pids := sessionProcesses(id, sids); len(pids) > 0 {
			sc.ProcessesStopped = stopProcesses(pids, processGrace)
		}
		sc.Worktrees = repoindex.CleanSession(ctx, d.scanInput(rec), d.worktreeInUse(id))
		d.removeSessionRunTmp(ctx, id)
		if sc.ProcessesStopped > 0 || len(sc.Worktrees) > 0 {
			d.cfg.Logger.Info("session cleaned up", "session", id, "processes", sc.ProcessesStopped, "worktrees", len(sc.Worktrees))
		}
		out = append(out, sc)
	}
	return out
}

type liveCwd struct {
	session, title, cwd string
}

// worktreeInUse reports a worktree as in use when a live run (other than
// the session being cleaned up) or any process works inside it. Processes
// are listed again for every worktree, right before it is removed.
func (d *Daemon) worktreeInUse(cleaning string) repoindex.InUse {
	var runs []liveCwd
	for _, lr := range d.liveRuns() {
		rec, _ := lr.snapshot()
		if rec.Run.State.Terminal() || rec.Session.SessionID == "" || rec.Session.SessionID == cleaning {
			continue
		}
		runs = append(runs, liveCwd{session: rec.Session.SessionID, title: rec.Session.Title, cwd: d.scanInput(rec).Cwd})
	}
	return func(w contracts.Worktree, repo string, created time.Time) string {
		for _, r := range runs {
			if r.cwd != "" && repoindex.Inside(r.cwd, w.Path) {
				return fmt.Sprintf("live session %q (%s) works in it", r.title, r.session)
			}
		}
		for _, c := range processCwds() {
			if repoindex.Inside(c, w.Path) {
				return "a running process works in it (" + c + ")"
			}
		}
		return ""
	}
}

// liveSessions is the set of sessions with a live run on this node.
func (d *Daemon) liveSessions() map[string]bool {
	out := map[string]bool{}
	for _, lr := range d.liveRuns() {
		rec, _ := lr.snapshot()
		if !rec.Run.State.Terminal() && rec.Session.SessionID != "" {
			out[rec.Session.SessionID] = true
		}
	}
	return out
}

// sweepRepos lists the repositories the sweep looks at: the node's repo
// index (those with linked worktrees) plus the repositories runs on this
// node used recently (their main checkouts may sit outside the index roots).
func (d *Daemon) sweepRepos() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	rs := d.repoState()
	rs.mu.Lock()
	for _, r := range rs.last.Repos {
		if r.Worktrees > 0 {
			add(r.Path)
		}
	}
	rs.mu.Unlock()
	for _, lr := range d.liveRuns() {
		rec, _ := lr.snapshot()
		if r, ok := repoindex.FindRepo(d.scanInput(rec).Cwd); ok {
			add(r)
		}
	}
	cwds, _ := d.st.recentRunCwds(context.Background(), d.now().Add(-sweepRunLookback))
	for _, c := range cwds {
		if r, ok := repoindex.FindRepo(c); ok {
			add(r)
		}
	}
	return out
}

// harnessTmpDirs are the per-session temp roots runs use on this node
// (Claude's CLAUDE_CODE_TMPDIR under the state directory).
func (d *Daemon) harnessTmpDirs() []string {
	if len(d.cfg.SweepTmpDirs) > 0 {
		return d.cfg.SweepTmpDirs
	}
	if d.cfg.StateDir == "" {
		return nil
	}
	return []string{filepath.Join(d.cfg.StateDir, "tmp", "claude")}
}

// liveTmpKeys returns what marks a live run's temp folder: every harness
// session ID it used (its native ref and the IDs Claude reported at each
// SessionStart, which change on /clear) and its working directory's folder
// key (Claude names the parent folder after the cwd). A live session's temp
// folder is never removed, however old.
func (d *Daemon) liveTmpKeys() (ids, keys map[string]bool) {
	ids, keys = map[string]bool{}, map[string]bool{}
	for _, lr := range d.liveRuns() {
		rec, _ := lr.snapshot()
		if rec.Run.State.Terminal() {
			continue
		}
		if rec.Run.NativeRef != nil && rec.Run.NativeRef.ID != "" {
			ids[rec.Run.NativeRef.ID] = true
		}
		if h := rec.Handle; h != nil {
			if h.NativeRef != nil {
				ids[h.NativeRef.ID] = true
			}
			for _, id := range hookSessionIDs(filepath.Join(h.StateDir, "events.jsonl")) {
				ids[id] = true
			}
		}
		if cwd := d.scanInput(rec).Cwd; cwd != "" {
			keys[cwdKey(cwd)] = true
			if r, err := filepath.EvalSymlinks(cwd); err == nil {
				keys[cwdKey(r)] = true
			}
		}
	}
	return ids, keys
}

// cwdKey is Claude's folder name for a working directory (every character
// other than an ASCII letter or digit becomes '-').
func cwdKey(cwd string) string {
	b := []byte(cwd)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// hookSessionIDs reads the session IDs a Claude run's hooks reported
// (events.jsonl in its state dir; bounded).
func hookSessionIDs(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(io.LimitReader(f, 64<<20))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var rec struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && uuidDirRe.MatchString(rec.SessionID) && !seen[rec.SessionID] {
			seen[rec.SessionID] = true
			out = append(out, rec.SessionID)
		}
	}
	return out
}

// sweepTmpDir removes session folders (named by a harness session UUID, up
// to three levels below root: <cwd-key>/<uuid>) in which no file changed
// for idle, then the parents they leave empty. Folders of live sessions
// (live) and everything under a live run's cwd key (liveKeys) are kept.
func sweepTmpDir(root string, idle time.Duration, now time.Time, live, liveKeys map[string]bool, dryRun bool) (removed int, freed int64, err error) {
	if _, serr := os.Stat(root); serr != nil {
		return 0, 0, nil
	}
	var dirs []string
	var find func(dir string, depth int)
	find = func(dir string, depth int) {
		ents, rerr := os.ReadDir(dir)
		if rerr != nil {
			return
		}
		for _, e := range ents {
			if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if liveKeys[e.Name()] {
				continue // a live run works in this folder's cwd
			}
			if uuidDirRe.MatchString(e.Name()) {
				if !live[e.Name()] {
					dirs = append(dirs, p)
				}
				continue
			}
			if depth < 3 {
				find(p, depth+1)
			}
		}
	}
	find(root, 1)
	for _, dir := range dirs {
		newest, size, ok := newestChange(dir)
		if !ok || now.Sub(newest) < idle {
			continue
		}
		if !dryRun {
			if rerr := os.RemoveAll(dir); rerr != nil {
				err = rerr
				continue
			}
			removeEmptyParents(filepath.Dir(dir), root)
		}
		removed++
		freed += size
	}
	return removed, freed, err
}

// newestChange walks dir (bounded) for the newest file modification time
// (folder times change when files are deleted, so they do not count) and
// the total size. ok is false when the walk hit its bound or failed.
func newestChange(dir string) (time.Time, int64, bool) {
	var newest time.Time
	var size int64
	n, files := 0, false
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		n++
		if n > tmpWalkMax {
			return fs.SkipAll
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		if p == dir && newest.IsZero() {
			newest = info.ModTime() // a folder without files: its own time
		}
		if !e.IsDir() {
			if !files || info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			files = true
			if info.Mode().IsRegular() {
				size += info.Size()
			}
		}
		return nil
	})
	return newest, size, err == nil && n <= tmpWalkMax
}

func removeEmptyParents(dir, root string) {
	for dir != root && repoindex.Inside(dir, root) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
