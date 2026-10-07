package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/diagnostics"
	"github.com/jhh3/pyre/internal/messaging/delivery"
)

// ActionFunc handles a typed node action (contracts/actions.go). It
// returns a JSON-encodable result or a *contracts.Error.
type ActionFunc func(ctx context.Context, params json.RawMessage) (any, error)

type actionEntry struct {
	fn    ActionFunc
	async bool
}

// RegisterAction plugs in an integration action handler (WP10/12/13/14).
// It runs asynchronously: the command returns "delivered" at once and the
// final result is reported as a durable CommandResult, so a slow action
// never blocks the hub command stream. Registering an existing name
// replaces it.
func (d *Daemon) RegisterAction(name string, fn ActionFunc) {
	d.mu.Lock()
	d.actions[name] = actionEntry{fn: fn, async: true}
	d.mu.Unlock()
}

// RegisterSyncAction registers a quick action executed inline (bounded by
// CommandTimeout).
func (d *Daemon) RegisterSyncAction(name string, fn ActionFunc) {
	d.mu.Lock()
	d.actions[name] = actionEntry{fn: fn}
	d.mu.Unlock()
}

// Actions lists registered action names.
func (d *Daemon) Actions() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.actions))
	for k := range d.actions {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (d *Daemon) invokeAction(ctx context.Context, cmd contracts.NodeCommand, ar contracts.NodeActionRequest) (contracts.CommandResult, error) {
	d.mu.Lock()
	e, ok := d.actions[ar.Action]
	d.mu.Unlock()
	if !ok {
		if why := d.cfg.ActionsUnavailable; why != "" {
			return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported, "node action %q is not available on this node: %s", ar.Action, why)
		}
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported, "node action %q is not available on this node", ar.Action)
	}
	if !e.async {
		cctx, cancel := context.WithTimeout(ctx, d.cfg.CommandTimeout)
		defer cancel()
		v, err := e.fn(cctx, json.RawMessage(ar.Params))
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return okResult(contracts.OutcomeSucceeded, v)
	}
	select {
	case d.actSem <- struct{}{}:
	default:
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrCapacityExhausted, "too many node actions in progress (%d)", cap(d.actSem))
	}
	d.asyncWG.Add(1)
	go func() {
		defer d.asyncWG.Done()
		defer func() { <-d.actSem }()
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		res := contracts.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: contracts.OutcomeSucceeded}
		v, err := e.fn(actx, json.RawMessage(ar.Params))
		if err != nil {
			res = d.failResult(cmd, err)
		} else if b, merr := json.Marshal(v); merr != nil {
			res = d.failResult(cmd, merr)
		} else {
			res.Result = b
		}
		res.At = d.now()
		if _, err := d.rep.Enqueue(actx, contracts.ReportCommandResult, res); err != nil {
			d.noteErr("report action %s result: %v", ar.Action, err)
		}
	}()
	res, err := okResult(contracts.OutcomeDelivered, map[string]any{"action": ar.Action, "async": true})
	res.Interim = true // the final result follows as a second CommandResult
	return res, err
}

func (d *Daemon) registerBuiltinActions() {
	d.RegisterSyncAction(contracts.ActionInventory, d.actionInventory)
	d.RegisterSyncAction(contracts.ActionDirList, d.actionDirList)
	d.RegisterSyncAction(contracts.ActionDiagnostics, d.actionDiagnostics)
	d.RegisterSyncAction(contracts.ActionFsMkdir, d.actionFsMkdir) // W-qlpath
	d.registerRepoActions()                                        // W-quicklaunch
}

func (d *Daemon) actionInventory(ctx context.Context, _ json.RawMessage) (any, error) {
	home, _ := os.UserHomeDir()
	inv := contracts.NodeInventory{NodeID: d.nodeID, CollectedAt: d.now(), Harnesses: d.harnessInfo(ctx),
		Tools:        []contracts.ToolVersion{{Tool: "pyred", Version: buildinfo.Version, State: "active", Health: "ok", InstalledAt: d.start}},
		Extensions:   []contracts.ExtensionRef{},
		Drift:        []contracts.DriftItem{},
		Integrations: map[string]contracts.IntegrationStatus{}}
	for _, r := range []struct {
		h   contracts.Harness
		dir string
	}{{contracts.HarnessClaude, ".claude"}, {contracts.HarnessCodex, ".codex"}} {
		p := filepath.Join(home, r.dir)
		_, err := os.Stat(p)
		inv.ConfigRoots = append(inv.ConfigRoots, contracts.ConfigRoot{Harness: r.h, Path: p, Exists: err == nil})
	}
	if tm, err := exec.LookPath("tmux"); err == nil {
		v, _ := exec.CommandContext(ctx, tm, "-V").Output()
		inv.Tools = append(inv.Tools, contracts.ToolVersion{Tool: "tmux", Version: strings.TrimSpace(strings.TrimPrefix(string(v), "tmux ")), Path: tm, State: "active", Health: "unchecked"})
	}
	for _, name := range []string{"hive", "outfit", "rota"} {
		st := contracts.IntegrationStatus{Health: "unchecked"}
		if p, err := exec.LookPath(name); err == nil {
			st.Present, st.Path = true, p
		}
		inv.Integrations[name] = st
	}
	if d.cfg.Integrations != nil {
		for k, v := range d.cfg.Integrations(ctx) {
			inv.Integrations[k] = v
		}
	}
	return inv, nil
}

func (d *Daemon) actionDirList(ctx context.Context, raw json.RawMessage) (any, error) {
	var p contracts.DirListParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, contracts.Errorf(contracts.ErrInvalid, "dir.list params: %v", err)
		}
	}
	if p.Path == "" {
		p.Path = "~"
	}
	dir, err := validateDir(p.Path)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, contracts.Errorf(contracts.ErrInvalid, "cannot list %s: %v", dir, err)
	}
	res := contracts.DirListResult{Path: dir, Dirs: []string{}}
	for _, e := range ents {
		if e.Name() == ".git" {
			res.IsGit = true
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && st.IsDir() {
				res.Dirs = append(res.Dirs, e.Name())
			}
		}
		if len(res.Dirs) >= 1000 {
			break
		}
	}
	return res, nil
}

// AgentUsage separates agent/harness process resources from Pyre's own.
type AgentUsage struct {
	Processes int     `json:"processes"`
	RSSMB     float64 `json:"rss_mb"`
	CPUPct    float64 `json:"cpu_pct"`
}

// DiagnosticsResult is contracts.Diagnostics plus agent process usage.
type DiagnosticsResult struct {
	contracts.Diagnostics
	Agents AgentUsage `json:"agents"`
	Link   string     `json:"link"`
	// HubClockAheadMS is the hub link's conservative estimate of how far
	// the hub clock is ahead of this node's (negative: behind); absent when
	// standalone. Hub-issued deadlines are judged on the hub clock.
	HubClockAheadMS *int64 `json:"hub_clock_ahead_ms,omitempty"`
	// Runtime are pyred's own Go runtime counters (goroutines, heap, open
	// file descriptors) for leak checks in long soaks.
	Runtime diagnostics.RuntimeStats `json:"runtime"`
}

func (d *Daemon) actionDiagnostics(ctx context.Context, _ json.RawMessage) (any, error) {
	return d.Diagnostics(ctx), nil
}

// Diagnostics reports queues, limits, resources and recent errors.
func (d *Daemon) Diagnostics(ctx context.Context) DiagnosticsResult {
	queues := map[string]int{}
	if c, err := d.inbox.Counts(ctx); err == nil {
		for k, v := range c {
			queues["inbox_"+k] = v
		}
	}
	var pending int
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM daemon_outbox WHERE state='pending'`).Scan(&pending)
	queues["outbox_pending_local"] = pending
	if ol, ok := d.rep.(interface {
		OutboxLen(context.Context) (int, error)
	}); ok {
		if n, err := ol.OutboxLen(ctx); err == nil {
			queues["reports_pending"] = n
		}
	}
	rc := d.ReadCacheStats()
	queues["read_cache_pending"] = rc.Pending
	limits := map[string]int{"max_runs": d.cfg.MaxRuns, "poll_max_concurrent": d.cfg.Poll.MaxConcurrent,
		"poll_interval_ms": int(d.cfg.Poll.Interval.Milliseconds()), "poll_idle_max_ms": int(d.cfg.Poll.IdleMax.Milliseconds()),
		"max_actions": d.cfg.MaxActions, "max_outbox": d.cfg.MaxOutbox, "read_cache_queue": d.cfg.ReadCacheQueue,
		"delivery_max_concurrent": delivery.DefaultMaxConcurrent}
	if d.cfg.BackgroundQueues != nil {
		q, l := d.cfg.BackgroundQueues()
		for k, v := range q {
			queues[k] = v
		}
		for k, v := range l {
			limits[k] = v
		}
	}
	res, agents := d.resources(ctx)
	d.mu.Lock()
	errs := append([]string{}, d.errs...)
	d.mu.Unlock()
	link := "standalone"
	if _, ok := d.rep.(nopReporter); !ok {
		link = "disconnected"
		if d.rep.Connected() {
			link = "connected"
		}
	}
	var ahead *int64
	if _, ok := d.rep.(hubClocked); ok {
		ms := d.hubNow().Sub(d.now()).Milliseconds()
		ahead = &ms
	}
	return DiagnosticsResult{
		HubClockAheadMS: ahead,
		Diagnostics: contracts.Diagnostics{NodeID: d.nodeID, Uptime: int64(d.now().Sub(d.start).Seconds()), Queues: queues,
			Limits: limits, Resources: res, Errors: errs},
		Agents: agents, Link: link, Runtime: diagnostics.SampleRuntime(),
	}
}
