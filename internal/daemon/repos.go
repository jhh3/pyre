package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// Repo index and session worktree tracking (W-quicklaunch).
//
// The repo loop scans the configured roots in the background (bounded,
// paced, every Interval) and reports the node's repo metadata to the hub
// when it changes. The worktree loop lists the git worktrees of the
// repositories each live run touches and reports them per session when
// they change. Node actions create fresh worktrees for a launch, scan a
// session now, and remove a session's worktrees under the safety rules in
// repoindex.Remove (re-checked here, at removal time).

// RepoIndexConfig configures repo discovery and worktree tracking. It is
// off unless Enabled (pyred enables it; embedded and test daemons never
// scan the machine's home by accident).
type RepoIndexConfig struct {
	Enabled bool
	// Roots to scan; default repoindex.DefaultRoots (~/code, ~/src,
	// ~/Documents/code, ~/projects).
	Roots []string
	// Interval between scans (default 10m); WorktreeInterval between
	// session worktree checks (default 2m); StartDelay before the first
	// scan (default 20s, so a starting node settles first).
	Interval         time.Duration
	WorktreeInterval time.Duration
	StartDelay       time.Duration
}

type repoState struct {
	mu       sync.Mutex
	last     contracts.NodeRepoIndex
	lastHash string
	lastSent time.Time
	wtHash   map[string]string // session -> last reported worktree digest
	scanMu   sync.Mutex        // one discovery pass at a time
}

func (c RepoIndexConfig) withDefaults() RepoIndexConfig {
	if c.Interval <= 0 {
		c.Interval = 10 * time.Minute
	}
	if c.WorktreeInterval <= 0 {
		c.WorktreeInterval = 2 * time.Minute
	}
	if c.StartDelay == 0 {
		c.StartDelay = 20 * time.Second
	}
	return c
}

func (d *Daemon) repoState() *repoState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.repos == nil {
		d.repos = &repoState{wtHash: map[string]string{}}
	}
	return d.repos
}

func (d *Daemon) repoOptions() repoindex.Options {
	home, _ := os.UserHomeDir()
	return repoindex.Options{Roots: d.cfg.RepoIndex.Roots, Now: d.now,
		Exclude: []string{d.cfg.StateDir, filepath.Join(home, "pyre-work")}}
}

// reindex runs one discovery pass and reports it when it changed (or
// every 6 hours, so the hub's freshness stays meaningful).
func (d *Daemon) reindex(ctx context.Context) (contracts.NodeRepoIndex, error) {
	rs := d.repoState()
	rs.scanMu.Lock()
	defer rs.scanMu.Unlock()
	idx, err := repoindex.Discover(ctx, d.repoOptions())
	if err != nil {
		return idx, err
	}
	idx.NodeID = d.nodeID
	body := idx
	body.IndexedAt = time.Time{}
	b, _ := json.Marshal(body)
	h := hashToken(string(b))
	rs.mu.Lock()
	rs.last = idx
	send := h != rs.lastHash || d.now().Sub(rs.lastSent) > 6*time.Hour
	rs.mu.Unlock()
	if send {
		if _, err := d.rep.Enqueue(ctx, contracts.ReportRepos, idx); err != nil {
			return idx, err
		}
		rs.mu.Lock()
		rs.lastHash, rs.lastSent = h, d.now()
		rs.mu.Unlock()
	}
	return idx, nil
}

func (d *Daemon) repoLoop(ctx context.Context) {
	cfg := d.cfg.RepoIndex.withDefaults()
	if !cfg.Enabled {
		return
	}
	wait := cfg.StartDelay
	if wait < 0 {
		wait = 0
	}
	nextScan := d.now().Add(wait)
	nextWT := d.now().Add(wait)
	tick := time.NewTicker(min(cfg.WorktreeInterval, cfg.Interval, 30*time.Second))
	defer tick.Stop()
	for {
		now := d.now()
		if !now.Before(nextScan) {
			if _, err := d.reindex(ctx); err != nil && ctx.Err() == nil && !contracts.IsCode(err, contracts.ErrUnavailable) {
				d.noteErr("repo index: %v", err)
			}
			nextScan = d.now().Add(cfg.Interval)
		}
		if !now.Before(nextWT) {
			d.reportWorktrees(ctx)
			nextWT = d.now().Add(cfg.WorktreeInterval)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// scanInput describes a run's session for repoindex.Scan. The session's
// creation time is converted from the hub clock to the node clock.
func (d *Daemon) scanInput(rec runRecord) repoindex.ScanInput {
	in := repoindex.ScanInput{SessionID: rec.Session.SessionID, RunID: rec.Run.RunID, Cwd: rec.Run.Cwd}
	if rec.Run.Checkout != nil && rec.Run.Checkout.Path != "" {
		in.Cwd = rec.Run.Checkout.Path
	}
	since := rec.Run.StartedAt
	if !rec.Session.CreatedAt.IsZero() {
		s := rec.Session.CreatedAt.Add(d.now().Sub(d.hubNow()))
		if since.IsZero() || s.Before(since) {
			since = s
		}
	}
	in.Since = since
	return in
}

// reportWorktrees reports each live run's session worktrees when changed.
func (d *Daemon) reportWorktrees(ctx context.Context) {
	rs := d.repoState()
	live := map[string]bool{}
	for _, lr := range d.liveRuns() {
		rec, _ := lr.snapshot()
		if rec.Run.State.Terminal() || rec.Session.SessionID == "" {
			continue
		}
		live[rec.Session.SessionID] = true
		in := d.scanInput(rec)
		in.Check = true
		sw, err := repoindex.Scan(ctx, in)
		if err != nil || len(sw.Repos) == 0 {
			continue
		}
		sw.NodeID = d.nodeID
		b, _ := json.Marshal(sw)
		h := hashToken(string(b))
		rs.mu.Lock()
		same := rs.wtHash[sw.SessionID] == h
		rs.mu.Unlock()
		if same {
			continue
		}
		sw.ReportedAt = d.now()
		if _, err := d.rep.Enqueue(ctx, contracts.ReportSessionWorktrees, sw); err == nil {
			rs.mu.Lock()
			rs.wtHash[sw.SessionID] = h
			rs.mu.Unlock()
		}
		if ctx.Err() != nil {
			return
		}
	}
	rs.mu.Lock()
	for sid := range rs.wtHash {
		if !live[sid] {
			delete(rs.wtHash, sid)
		}
	}
	rs.mu.Unlock()
}

func (d *Daemon) registerRepoActions() {
	d.RegisterSyncAction(contracts.ActionWorktreeCreate, d.actionWorktreeCreate)
	d.RegisterSyncAction(contracts.ActionWorktreeScan, d.actionWorktreeScan)
	d.RegisterSyncAction(contracts.ActionWorktreeRemove, d.actionWorktreeRemove)
	d.RegisterAction(contracts.ActionRepoReindex, func(ctx context.Context, _ json.RawMessage) (any, error) { return d.reindex(ctx) })
}

func decodeAction[T any](name string, raw json.RawMessage) (T, error) {
	var p T
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, contracts.Errorf(contracts.ErrInvalid, "%s params: %v", name, err)
		}
	}
	return p, nil
}

func (d *Daemon) actionWorktreeCreate(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeAction[contracts.WorktreeCreateParams](contracts.ActionWorktreeCreate, raw)
	if err != nil {
		return nil, err
	}
	return repoindex.Create(ctx, p, d.now())
}

// sessionRun finds the session's latest run on this node.
func (d *Daemon) sessionRun(ctx context.Context, sessionID string) (runRecord, error) {
	if sessionID == "" {
		return runRecord{}, contracts.Errorf(contracts.ErrInvalid, "session_id is required")
	}
	rec, ok, err := d.st.latestRunForSession(ctx, sessionID)
	if err != nil {
		return runRecord{}, err
	}
	if !ok {
		return runRecord{}, contracts.Errorf(contracts.ErrNotFound, "session %s has no run on this node", sessionID).WithRef(contracts.KindSession, sessionID)
	}
	if lr, live := d.getLive(rec.Run.RunID); live {
		rec, _ = lr.snapshot()
	}
	return rec, nil
}

func (d *Daemon) actionWorktreeScan(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeAction[contracts.WorktreeScanParams](contracts.ActionWorktreeScan, raw)
	if err != nil {
		return nil, err
	}
	rec, err := d.sessionRun(ctx, p.SessionID)
	if err != nil {
		return nil, err
	}
	in := d.scanInput(rec)
	in.Check = true
	sw, err := repoindex.Scan(ctx, in)
	if err != nil {
		return nil, contracts.Errorf(contracts.ErrUnavailable, "scan worktrees: %v", err)
	}
	sw.NodeID, sw.ReportedAt = d.nodeID, d.now()
	return sw, nil
}

func (d *Daemon) actionWorktreeRemove(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeAction[contracts.WorktreeRemoveParams](contracts.ActionWorktreeRemove, raw)
	if err != nil {
		return nil, err
	}
	if len(p.Paths) == 0 || len(p.Paths) > 32 {
		return nil, contracts.Errorf(contracts.ErrInvalid, "name 1 to 32 worktree paths")
	}
	rec, err := d.sessionRun(ctx, p.SessionID)
	if err != nil {
		return nil, err
	}
	if lr, live := d.getLive(rec.Run.RunID); live {
		if r, _ := lr.snapshot(); !r.Run.State.Terminal() {
			return nil, contracts.Errorf(contracts.ErrConflict, "session %s is still running; stop it before removing its worktrees", p.SessionID)
		}
	}
	res := repoindex.Remove(ctx, d.scanInput(rec), p.Paths)
	d.cfg.Logger.Info("session worktrees removed", "session", p.SessionID, "results", len(res.Results))
	return res, nil
}
