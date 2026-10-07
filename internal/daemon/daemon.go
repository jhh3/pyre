// Package daemon is the Pyre node service core (pyred): it owns local run
// supervision and state, executes hub commands, observes runs and reports
// durable observations, delivers inbox messages through the delivery
// engine, and serves the local API socket with hub-outage fallbacks.
//
// Ownership rules (architecture §3): the executing node is authoritative
// for running processes; a hub outage never stops local runs; a restarted
// daemon recovers runs from persisted RunHandles and never launches a
// second process for a run.
package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/delivery"
	"github.com/jhh3/pyre/internal/messaging/inbox"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/internal/sysstats"
)

// Daemon is the node service.
type Daemon struct {
	cfg    Config
	db     *sql.DB
	ownDB  bool
	st     *store
	inbox  *inbox.Store
	engine *delivery.Engine
	nodeID string
	start  time.Time
	self   supervisor.SelfSampler
	rep    Reporter
	// Machine and per-run resource samplers (sysstats); each takes at most
	// one sample per interval, cached in between.
	hostStats *sysstats.Sampler
	runStats  *sysstats.RunSampler

	adapters  map[string]contracts.RuntimeAdapter // by adapter name
	byHarness map[contracts.Harness]contracts.RuntimeAdapter

	mu        sync.Mutex
	live      map[string]*liveRun // run ID -> live/recovering run
	probes    map[contracts.Harness]contracts.ProbeResult
	probedAt  time.Time
	actions   map[string]actionEntry
	upstreams map[string]contracts.API
	errs      []string

	actSem     chan struct{}
	pollKick   chan struct{}
	pollSem    chan struct{}
	outboxKick chan struct{}
	wg         sync.WaitGroup
	asyncWG    sync.WaitGroup // launches and async actions (finish even on shutdown)
	cancel     context.CancelFunc

	cacheWrites atomic.Uint64 // read cache inserts (prune cadence)
	cache       cacheWriter   // background read cache writer
	hubLinkDown atomic.Bool   // hub link known down: proxied calls do not retry
	search      *searchState  // transcript search index (search.go); nil if disabled
	repos       *repoState    // repo index and session worktrees (repos.go)
}

// liveRun is the in-memory state of a run the daemon supervises.
type liveRun struct {
	mu        sync.Mutex
	rec       runRecord
	adapter   contracts.RuntimeAdapter
	recovered bool
	cursor    string
	nextPoll  time.Time
	interval  time.Duration
	polling   bool
	attached  bool

	lastActivity string
	activityAt   time.Time
	lastLimitAt  time.Time

	// stamp is the adapter's change stamp taken when the last observation
	// started; observedAt is when that observation started.
	stamp      string
	observedAt time.Time
}

func (lr *liveRun) snapshot() (runRecord, bool) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.rec, lr.attached
}

// New opens local state and prepares the daemon. Call Start (or Run).
func New(cfg Config) (*Daemon, error) {
	cfg.defaults()
	if cfg.StateDir == "" {
		return nil, errors.New("daemon: StateDir is required")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(cfg.StateDir, 0o700)
	d := &Daemon{cfg: cfg, start: cfg.Clock.Now(), live: map[string]*liveRun{}, actions: map[string]actionEntry{},
		adapters: map[string]contracts.RuntimeAdapter{}, byHarness: map[contracts.Harness]contracts.RuntimeAdapter{},
		upstreams: map[string]contracts.API{}, hostStats: sysstats.NewSampler(sysstats.DefaultPaths(cfg.StateDir)...), runStats: sysstats.NewRunSampler(), actSem: make(chan struct{}, cfg.MaxActions), pollSem: make(chan struct{}, cfg.Poll.MaxConcurrent), pollKick: make(chan struct{}, 1), outboxKick: make(chan struct{}, 1)}
	db := cfg.DB
	if db == nil {
		path := filepath.Join(cfg.StateDir, "pyred.db")
		var err error
		if db, err = sqlitex.Open(path); err != nil {
			return nil, err
		}
		d.ownDB = true
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			_ = os.Chmod(p, 0o600)
		}
	}
	d.db = db
	d.st = &store{db: db}
	ctx := context.Background()
	if err := sqlitex.Migrate(ctx, db, append(append([]sqlitex.Migration{}, Migrations...), ReadCacheMigrations...)...); err != nil {
		d.closeDB()
		return nil, err
	}
	ib, err := inbox.Open(ctx, db, inbox.Limits{})
	if err != nil {
		d.closeDB()
		return nil, err
	}
	d.inbox = ib
	if err := d.initNodeID(ctx); err != nil {
		d.closeDB()
		return nil, err
	}
	for _, a := range cfg.Adapters {
		d.adapters[a.Name()] = a
		if _, ok := d.byHarness[a.Harness()]; !ok {
			d.byHarness[a.Harness()] = a
		}
	}
	d.rep = cfg.Reporter
	if d.rep == nil {
		d.rep = nopReporter{}
	}
	d.engine = delivery.New(delivery.Config{NodeID: d.nodeID, Inbox: ib, Runs: (*deliveryRuns)(d), Reporter: d.rep,
		Clock: cfg.Clock, HubClock: hubClock{d}, Logger: cfg.Logger.With("component", "delivery"), OnAccepted: d.pollSoon})
	d.registerBuiltinActions()
	d.initSearch()
	return d, nil
}

func (d *Daemon) closeDB() {
	if d.ownDB && d.db != nil {
		d.db.Close()
	}
}

func (d *Daemon) initNodeID(ctx context.Context) error {
	stored, err := d.st.meta(ctx, "node_id")
	if err != nil {
		return err
	}
	switch {
	case d.cfg.NodeID != "":
		d.nodeID = d.cfg.NodeID
		if stored != d.cfg.NodeID {
			if err := d.st.setMeta(ctx, "node_id", d.cfg.NodeID); err != nil {
				return err
			}
		}
	case stored != "":
		d.nodeID = stored
	default:
		d.nodeID = contracts.NewID(contracts.PrefixNode)
		if err := d.st.setMeta(ctx, "node_id", d.nodeID); err != nil {
			return err
		}
	}
	return nil
}

// NodeID returns this node's ID.
func (d *Daemon) NodeID() string { return d.nodeID }

// DB returns the node database (for the NodeLink, namespace "nodelink").
func (d *Daemon) DB() *sql.DB { return d.db }

// SetReporter attaches the hub link after construction (the NodeLink needs
// the daemon's Hello and HandleCommand, and the daemon needs the link).
// Call before Start.
func (d *Daemon) SetReporter(r Reporter) {
	if r == nil {
		r = nopReporter{}
	}
	d.rep = r
	d.engine = delivery.New(delivery.Config{NodeID: d.nodeID, Inbox: d.inbox, Runs: (*deliveryRuns)(d), Reporter: r,
		Clock: d.cfg.Clock, HubClock: hubClock{d}, Logger: d.cfg.Logger.With("component", "delivery"), OnAccepted: d.pollSoon})
}

// SocketPath returns the local API socket path.
func (d *Daemon) SocketPath() string {
	if d.cfg.SocketPath != "" {
		return d.cfg.SocketPath
	}
	return filepath.Join(d.cfg.StateDir, "pyred.sock")
}

func (d *Daemon) now() time.Time { return d.cfg.Clock.Now() }

// hubClocked is implemented by a Reporter that estimates the hub's clock
// (*transport.NodeLink). Hub-issued deadlines (input leases) are judged on
// it, so a node with a skewed clock neither drops nor prolongs them.
type hubClocked interface{ HubNow() time.Time }

// hubNow is the hub clock estimate, or the node clock when standalone.
func (d *Daemon) hubNow() time.Time {
	if hc, ok := d.rep.(hubClocked); ok {
		return hc.HubNow()
	}
	return d.now()
}

// hubClock adapts hubNow to contracts.Clock (delivery engine lease checks).
type hubClock struct{ d *Daemon }

func (c hubClock) Now() time.Time { return c.d.hubNow() }

func (d *Daemon) noteErr(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	d.cfg.Logger.Warn(msg)
	d.mu.Lock()
	d.errs = append(d.errs, d.now().Format(time.RFC3339)+" "+redact(msg))
	if len(d.errs) > 50 {
		d.errs = d.errs[len(d.errs)-50:]
	}
	d.mu.Unlock()
}

// Start recovers runs and delivery attempts, then starts the background
// loops (delivery, observation, heartbeats, outbox flush). It returns once
// recovery is complete.
func (d *Daemon) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	if err := d.recoverRuns(ctx); err != nil {
		cancel()
		return err
	}
	if err := d.engine.Recover(ctx); err != nil {
		cancel()
		return err
	}
	d.refreshProbes(ctx)
	loops := []func(context.Context){d.engine.Run, d.pollLoop, d.heartbeatLoop, d.outboxLoop, d.maintenanceLoop, d.searchLoop, d.repoLoop}
	for _, p := range d.cfg.Periodic {
		p := p
		loops = append(loops, func(ctx context.Context) { d.periodicLoop(ctx, p) })
	}
	for _, f := range loops {
		f := f
		d.wg.Add(1)
		go func() { defer d.wg.Done(); f(ctx) }()
	}
	return nil
}

// Close stops background loops and closes local state. Runs keep running.
func (d *Daemon) Close() error {
	if d.cancel != nil {
		d.cancel()
	}
	d.wg.Wait()
	d.asyncWG.Wait()
	d.cache.mu.Lock()
	d.cache.closed = true // a late read cannot start a writer after Wait
	d.cache.mu.Unlock()
	d.cache.wg.Wait()
	d.closeDB()
	return nil
}

// recoverRuns reloads supervised runs. Live runs are re-attached through
// adapter.Recover (never Launch); runs interrupted mid-launch become
// orphaned/unknown and are never relaunched.
func (d *Daemon) recoverRuns(ctx context.Context) error {
	recs, err := d.st.runs(ctx, `phase IN (?,?)`, phaseLive, phaseLaunching)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.Phase == phaseLaunching || rec.Handle == nil {
			rec.Phase = phaseOrphaned
			rec.Run.State = contracts.RunUnknown
			rec.Run.StateReason = "pyred stopped during launch before a run handle was recorded; the process may exist under " + d.runDir(rec.Run.RunID) + " and is not relaunched"
			rec.Run.Freshness = d.freshness(true, "launch outcome unknown")
			if err := d.persistAndReport(ctx, rec); err != nil {
				return err
			}
			continue
		}
		a := d.adapters[rec.Run.Adapter]
		lr := &liveRun{rec: rec, adapter: a, interval: d.cfg.Poll.Interval}
		if a == nil {
			rec.Run.State = contracts.RunUnknown
			rec.Run.StateReason = "adapter " + rec.Run.Adapter + " is not available in this pyred build"
			rec.Run.Freshness = d.freshness(true, "adapter unavailable")
			lr.rec = rec
			if err := d.persistAndReport(ctx, rec); err != nil {
				return err
			}
			d.addLive(lr)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, d.cfg.Poll.Timeout)
		h, rerr := a.Recover(rctx, *rec.Handle)
		cancel()
		switch {
		case rerr == nil:
			lr.recovered = true
			lr.rec.Handle = &h
			d.cfg.Logger.Info("recovered run", "run", rec.Run.RunID, "session", rec.Run.SessionID)
			// Provider-less harnesses (shell) and runs launched without an
			// assignment (ambient login) have nothing to re-validate.
			needsCheck := contracts.ProviderFor(rec.Run.Harness) != "" && rec.Run.AccountID != ""
			if err := d.checkAssignmentIf(ctx, needsCheck, rec.Run.RunID); err != nil {
				// Keep supervising (never kill on a bookkeeping error) but
				// make the problem visible on the run.
				d.noteErr("assignment check for recovered run %s: %v", rec.Run.RunID, err)
				if lr.rec.Run.AdapterData == nil {
					lr.rec.Run.AdapterData = map[string]string{}
				}
				lr.rec.Run.AdapterData["pyre.account_check"] = redact(contracts.AsError(err).Message)
			}
		case contracts.IsCode(rerr, contracts.ErrNotFound):
			d.endRun(ctx, lr, contracts.RunExited, "run process not found after pyred restart (exit not observed)", nil)
			continue
		default:
			lr.rec.Run.State = contracts.RunUnknown
			lr.rec.Run.StateReason = "recover failed: " + rerr.Error()
			lr.rec.Run.Freshness = d.freshness(true, "recover failed; retrying")
			if err := d.persistAndReport(ctx, lr.rec); err != nil {
				return err
			}
		}
		d.addLive(lr)
	}
	return nil
}

func (d *Daemon) addLive(lr *liveRun) {
	d.mu.Lock()
	d.live[lr.rec.Run.RunID] = lr
	d.mu.Unlock()
	select {
	case d.pollKick <- struct{}{}:
	default:
	}
}

func (d *Daemon) getLive(runID string) (*liveRun, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	lr, ok := d.live[runID]
	return lr, ok
}

func (d *Daemon) liveRuns() []*liveRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.live))
	for id := range d.live {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*liveRun, 0, len(ids))
	for _, id := range ids {
		out = append(out, d.live[id])
	}
	return out
}

func (d *Daemon) freshness(stale bool, reason string) contracts.Freshness {
	return contracts.Freshness{ObservedAt: d.now(), Source: "node:" + d.nodeID, Stale: stale, Reason: reason}
}

func (d *Daemon) runDir(runID string) string { return filepath.Join(d.cfg.StateDir, "runs", runID) }

// persistAndReport stores a run record and queues a durable run report in
// one transaction.
func (d *Daemon) persistAndReport(ctx context.Context, rec runRecord) error {
	rec.Fingerprint = fingerprint(rec.Run)
	err := sqlitex.Tx(ctx, d.db, func(tx *sql.Tx) error {
		if err := putRunTx(ctx, tx, rec, d.now()); err != nil {
			return err
		}
		_, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportRun, d.now(), rec.Run)
		return err
	})
	if err == nil {
		d.rep.Kick()
	}
	return err
}

// endRun records a terminal state, withdraws pending approvals, and stops
// supervision.
func (d *Daemon) endRun(ctx context.Context, lr *liveRun, state contracts.RunState, reason string, exit *int) {
	d.forgetRunTitle(lr.rec.Run.RunID) // W-names
	lr.mu.Lock()
	now := d.now()
	lr.rec.Phase = phaseEnded
	lr.rec.Run.State = state
	lr.rec.Run.StateReason = reason
	lr.rec.Run.Freshness = d.freshness(false, "")
	if lr.rec.Run.EndedAt == nil {
		lr.rec.Run.EndedAt = &now
	}
	if exit != nil {
		lr.rec.Run.ExitCode = exit
	}
	lr.rec.Run.PendingApprovalIDs = nil
	lr.rec.Run.Activity = nil
	rec := lr.rec
	lr.mu.Unlock()
	err := sqlitex.Tx(ctx, d.db, func(tx *sql.Tx) error {
		pend, err := pendingApprovals(ctx, tx, rec.Run.RunID)
		if err != nil {
			return err
		}
		for _, a := range pend {
			a.State = contracts.ApprovalWithdrawn
			if err := putApprovalTx(ctx, tx, a, now); err != nil {
				return err
			}
			if _, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportApproval, now, a); err != nil {
				return err
			}
		}
		rec.Fingerprint = fingerprint(rec.Run)
		if err := putRunTx(ctx, tx, rec, now); err != nil {
			return err
		}
		_, err = d.rep.EnqueueTx(ctx, tx, contracts.ReportRun, now, rec.Run)
		return err
	})
	if err != nil {
		d.noteErr("record end of run %s: %v", rec.Run.RunID, err)
	}
	d.rep.Kick()
	d.mu.Lock()
	delete(d.live, rec.Run.RunID)
	d.mu.Unlock()
	if d.cfg.OnRunEnded != nil {
		ev := RunEnded{Run: rec.Run, Session: rec.Session}
		if rec.Handle != nil {
			ev.ProfileDir = rec.Handle.Data[HandleProfileDir]
		}
		d.cfg.OnRunEnded(ev)
	}
	d.releaseAccount(rec.Run.RunID)
	d.engine.Kick(rec.Run.SessionID)
}

// refreshProbes probes adapters (bounded: at most once per 5 minutes).
func (d *Daemon) refreshProbes(ctx context.Context) map[contracts.Harness]contracts.ProbeResult {
	d.mu.Lock()
	if d.probes != nil && d.now().Sub(d.probedAt) < 5*time.Minute {
		p := d.probes
		d.mu.Unlock()
		return p
	}
	d.mu.Unlock()
	out := map[contracts.Harness]contracts.ProbeResult{}
	for _, a := range d.cfg.Adapters {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		pr, err := a.Probe(pctx)
		cancel()
		if err != nil {
			pr = contracts.ProbeResult{Harness: a.Harness(), Adapter: a.Name(), Available: false, Reason: redact(err.Error())}
		}
		if _, ok := out[a.Harness()]; !ok || (!out[a.Harness()].Available && pr.Available) {
			out[a.Harness()] = pr
		}
	}
	d.mu.Lock()
	d.probes, d.probedAt = out, d.now()
	d.mu.Unlock()
	return out
}

func (d *Daemon) harnessInfo(ctx context.Context) []contracts.HarnessInfo {
	probes := d.refreshProbes(ctx)
	var out []contracts.HarnessInfo
	d.mu.Lock()
	at := d.probedAt
	d.mu.Unlock()
	for h, p := range probes {
		hi := contracts.HarnessInfo{Harness: h, Version: p.Version, Executable: p.Executable, Available: p.Available,
			Reason: p.Reason, Capabilities: p.Capabilities, ProbedAt: at}
		if d.cfg.OwnLogin != nil && p.Available {
			hi.Login = d.cfg.OwnLogin(h)
		}
		out = append(out, hi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Harness < out[j].Harness })
	return out
}

func (d *Daemon) versions() map[string]string {
	return map[string]string{"pyred": buildinfo.Version, "protocol": fmt.Sprint(contracts.ProtocolVersion), "go": runtime.Version()}
}

func (d *Daemon) capabilities() []string {
	caps := []string{"local_api", "worktree_checkout", "delivery.inbox"}
	if d.cfg.SSHAlias != "" {
		caps = append(caps, "attach.ssh_tmux")
	}
	return caps
}

// Resources samples host load, the daemon's own overhead and the agent
// processes' usage (sent to the hub in hello and status reports).
func (d *Daemon) Resources(ctx context.Context) contracts.NodeResources {
	r, _ := d.resources(ctx)
	return r
}

// resources is Resources plus the agent process count for diagnostics.
func (d *Daemon) resources(ctx context.Context) (contracts.NodeResources, AgentUsage) {
	h := d.hostStats.Sample(ctx)
	rss, cpu := d.self.Sample(ctx)
	active := d.activeRuns()
	agents := d.agentUsage(ctx)
	r := contracts.NodeResources{ActiveRuns: active, MaxRuns: d.cfg.MaxRuns, PyreRSSMB: rss, PyreCPUPct: cpu,
		AgentRSSMB: math.Round(agents.RSSMB*10) / 10, AgentCPUPct: math.Round(agents.CPUPct*10) / 10, ObservedAt: d.now()}
	h.Apply(&r)
	r.Runs = sysstats.Contract(d.runStats.Sample(ctx, d.runRoots(ctx)))
	return r, agents
}

// runRoots lists each live run's tmux pane process (the root of its
// process tree), asking tmux only when the launch did not record it.
func (d *Daemon) runRoots(ctx context.Context) []sysstats.RunRoot {
	var out []sysstats.RunRoot
	for _, lr := range d.liveRuns() {
		rec, _ := lr.snapshot()
		if rec.Run.State.Terminal() || rec.Handle == nil {
			continue
		}
		pid := rec.Handle.PID
		if pid <= 0 && rec.Handle.TmuxSocket != "" && rec.Handle.TmuxTarget != "" {
			if p, err := (supervisor.Tmux{Socket: rec.Handle.TmuxSocket, Timeout: 3 * time.Second}).Pane(ctx, rec.Handle.TmuxTarget); err == nil && !p.Dead {
				pid = p.PID
			}
		}
		if pid > 0 {
			out = append(out, sysstats.RunRoot{RunID: rec.Run.RunID, SessionID: rec.Run.SessionID, PID: pid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
}

// activeRuns counts live runs that have not ended.
func (d *Daemon) activeRuns() int {
	active := 0
	for _, lr := range d.liveRuns() {
		rec, _ := lr.snapshot()
		if !rec.Run.State.Terminal() {
			active++
		}
	}
	return active
}

// agentUsage sums RSS and CPU over the supervised harness processes: every
// process under the tmux servers that host this node's live runs (one
// `ps` snapshot; each server is asked for its PID once).
func (d *Daemon) agentUsage(ctx context.Context) AgentUsage {
	var roots []int
	seen := map[string]bool{}
	for _, lr := range d.liveRuns() {
		rec, _ := lr.snapshot()
		if rec.Handle == nil || rec.Handle.TmuxSocket == "" || seen[rec.Handle.TmuxSocket] {
			continue
		}
		seen[rec.Handle.TmuxSocket] = true
		if pid, err := (supervisor.Tmux{Socket: rec.Handle.TmuxSocket, Timeout: 3 * time.Second}).ServerPID(ctx); err == nil {
			roots = append(roots, pid)
		}
	}
	if len(roots) == 0 {
		return AgentUsage{}
	}
	tab, err := supervisor.ProcessTable(ctx)
	if err != nil {
		return AgentUsage{}
	}
	rss, cpu, n := supervisor.TreeUsage(tab, roots)
	return AgentUsage{Processes: n, RSSMB: float64(rss) / 1024, CPUPct: cpu}
}

// Hello builds the dynamic NodeHello fields for the hub link, including
// LiveRuns for reconciliation.
func (d *Daemon) Hello(ctx context.Context) (contracts.NodeHello, error) {
	host, _ := os.Hostname()
	recs, err := d.st.runs(ctx, `phase IN (?,?,?)`, phaseLive, phaseLaunching, phaseOrphaned)
	if err != nil {
		return contracts.NodeHello{}, err
	}
	live := make([]contracts.Run, 0, len(recs))
	for _, r := range recs {
		if lr, ok := d.getLive(r.Run.RunID); ok {
			r, _ = lr.snapshot()
		}
		live = append(live, r.Run)
	}
	return contracts.NodeHello{NodeID: d.nodeID, Protocol: contracts.ProtocolVersion, MinProtocol: contracts.MinProtocolVersion,
		PyredVersion: buildinfo.Version, Hostname: host, Platform: runtime.GOOS, Arch: runtime.GOARCH,
		Harnesses: d.harnessInfo(ctx), Versions: d.versions(), Capabilities: d.capabilities(),
		Resources: d.Resources(ctx), LiveRuns: live}, nil
}

func (d *Daemon) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(d.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		if d.rep.Connected() {
			st := contracts.NodeStatusReport{Resources: d.Resources(ctx), Harnesses: d.harnessInfo(ctx), Versions: d.versions()}
			if err := d.rep.SendStatus(ctx, st); err != nil && !contracts.IsCode(err, contracts.ErrUnavailable) && ctx.Err() == nil {
				d.noteErr("status heartbeat: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Daemon) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := d.st.pruneSecrets(ctx); err != nil && ctx.Err() == nil {
			d.noteErr("prune secrets: %v", err)
		}
		if _, err := d.db.ExecContext(ctx, `DELETE FROM daemon_outbox WHERE state != 'pending' AND updated_at < ?`, ftime(d.now().Add(-7*24*time.Hour))); err != nil && ctx.Err() == nil {
			d.noteErr("prune outbox: %v", err)
		}
	}
}

// periodicLoop emits an integration report when its payload changes.
func (d *Daemon) periodicLoop(ctx context.Context, p PeriodicReport) {
	iv := p.Interval
	if iv < time.Minute {
		iv = time.Minute
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	last := ""
	for {
		v, err := p.Collect(ctx)
		if err != nil && ctx.Err() == nil {
			if contracts.IsCode(err, contracts.ErrUnavailable) {
				d.cfg.Logger.Debug("periodic report source unavailable", "kind", p.Kind, "err", err)
			} else {
				d.noteErr("periodic %s report: %v", p.Kind, err)
			}
		} else if err == nil {
			b, _ := json.Marshal(v)
			if h := hashToken(string(b)); h != last {
				if _, err := d.rep.Enqueue(ctx, p.Kind, json.RawMessage(b)); err == nil {
					last = h
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// nopReporter is used when no hub is configured: reports are dropped
// (local state remains authoritative and readable).
type nopReporter struct{}

func (nopReporter) EnqueueTx(context.Context, *sql.Tx, contracts.ReportKind, time.Time, any) (uint64, error) {
	return 0, nil
}
func (nopReporter) Kick() {}
func (nopReporter) Enqueue(context.Context, contracts.ReportKind, any) (uint64, error) {
	return 0, nil
}
func (nopReporter) SendStatus(context.Context, contracts.NodeStatusReport) error {
	return contracts.Errorf(contracts.ErrUnavailable, "no hub configured")
}
func (nopReporter) Connected() bool { return false }

// deliveryRuns adapts the daemon to delivery.Runs.
type deliveryRuns Daemon

func (r *deliveryRuns) target(lr *liveRun) delivery.Target {
	rec, attached := lr.snapshot()
	t := delivery.Target{Run: rec.Run, Adapter: lr.adapter, Attached: attached}
	if rec.Handle != nil {
		t.Handle = *rec.Handle
	}
	if !lr.recovered {
		t.Adapter = nil // not controllable until recovered
	}
	return t
}

func (r *deliveryRuns) Current(ctx context.Context, sessionID string) (delivery.Target, bool) {
	d := (*Daemon)(r)
	rec, ok, err := d.st.latestRunForSession(ctx, sessionID)
	if err != nil || !ok {
		return delivery.Target{}, false
	}
	if lr, ok := d.getLive(rec.Run.RunID); ok {
		return r.target(lr), true
	}
	return delivery.Target{Run: rec.Run}, true
}

func (r *deliveryRuns) Get(ctx context.Context, runID string) (delivery.Target, bool) {
	d := (*Daemon)(r)
	if lr, ok := d.getLive(runID); ok {
		return r.target(lr), true
	}
	rec, ok, err := d.st.run(ctx, runID)
	if err != nil || !ok {
		return delivery.Target{}, false
	}
	return delivery.Target{Run: rec.Run}, true
}
