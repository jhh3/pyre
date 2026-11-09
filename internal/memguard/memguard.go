// Package memguard is pyred's low-memory watchdog (decision I66): when the
// node is nearly out of memory and swap, it stops the largest process that
// a Pyre run started, so one runaway tool command cannot stall every
// session on the machine.
//
// It is the safety net behind the per-run memory scopes (supervisor
// runscope.go): it also covers nodes without systemd user scopes (macOS,
// legacy units, no user bus) and a Codex app-server whose threads share one
// scope.
//
// Rules:
//   - Trigger (Linux): MemAvailable below AvailFrac of MemTotal AND swap
//     more than SwapFrac used (a machine without swap: the first alone).
//     macOS: the kernel's memory pressure level is critical AND available
//     memory (total minus Activity Monitor's "Memory Used") is below
//     AvailFrac; macOS swap grows on demand, so SwapFrac is not used.
//   - Candidates: this user's processes that descend from a Pyre run root
//     (the Pyre tmux server, whose direct children are pane processes, and
//     each live run's own process while it is a pane or names its run in
//     its own environment: a stale handle PID reused by another process is
//     never a root). Never a root or pane process (the
//     harness: Claude, a Codex TUI or app-server, agy, a shell run's
//     shell), never a process named like a harness or Pyre itself (claude,
//     codex, agy, tmux, pyre, pyred), never pyred's own PID. Processes that
//     left the run's tree (setsid -f, systemd-run --user --unit) are not
//     candidates: the owner's own processes are never touched.
//   - Victim: the candidate holding the most memory (RSS + swap), and only
//     if that is at least MinVictimBytes (default max(1 GiB, 5% of RAM)):
//     killing a small process would not relieve the node.
//   - Action: SIGTERM; after Grace (10 s), SIGKILL if it is still alive and
//     still holds a quarter of what it held. Linux signals through a pidfd
//     opened after re-checking the process's start time, so a reused PID is
//     never signalled.
//   - Rate limit: one victim at a time, Cooldown (30 s) after each, at most
//     MaxPerHour (10) per hour.
//   - Report: OnKill gets the outcome (the daemon audits it as
//     node.memory_guard_kill and sends the session a note).
package memguard

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/sysstats"
)

// Defaults.
const (
	DefaultAvailFrac  = 0.05
	DefaultSwapFrac   = 0.80
	DefaultGrace      = 10 * time.Second
	DefaultCooldown   = 30 * time.Second
	DefaultMaxPerHour = 10
	DefaultInterval   = 2 * time.Second
	// MinVictimFloor and MinVictimFrac set the default MinVictimBytes.
	MinVictimFloor = 1 << 30
	MinVictimFrac  = 0.05
)

// Config tunes the guard; zero fields take the defaults.
type Config struct {
	AvailFrac      float64
	SwapFrac       float64
	Grace          time.Duration
	Cooldown       time.Duration
	MaxPerHour     int
	Interval       time.Duration
	MinVictimBytes int64 // 0: max(MinVictimFloor, MinVictimFrac of RAM)
}

func (c Config) withDefaults() Config {
	if c.AvailFrac <= 0 {
		c.AvailFrac = DefaultAvailFrac
	}
	if c.SwapFrac <= 0 {
		c.SwapFrac = DefaultSwapFrac
	}
	if c.Grace <= 0 {
		c.Grace = DefaultGrace
	}
	if c.Cooldown <= 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.MaxPerHour <= 0 {
		c.MaxPerHour = DefaultMaxPerHour
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	return c
}

// Describe is a one-line summary of the trigger ("available < 5% and swap > 80% used").
func (c Config) Describe(goos string) string {
	c = c.withDefaults()
	if goos == "darwin" {
		return fmt.Sprintf("critical memory pressure and available < %s", pct(c.AvailFrac))
	}
	return fmt.Sprintf("available < %s and swap > %s used", pct(c.AvailFrac), pct(c.SwapFrac))
}

func pct(f float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", f*100), "0"), ".") + "%"
}

// Pressure is the trigger verdict for one memory sample.
type Pressure struct {
	Low    bool
	Detail string // "available 1.2G of 30.0G (3.9%), swap 7.1G of 8.0G (89%) used"
}

// Check applies the trigger to a sample. A sample without RAM figures is
// never low.
func (c Config) Check(goos string, h sysstats.Host) Pressure {
	c = c.withDefaults()
	if h.MemTotalMB <= 0 || h.MemAvailMB <= 0 {
		return Pressure{} // not sampled (no MemAvailable, vm_stat failed): never acts on a guess
	}
	availFrac := float64(h.MemAvailMB) / float64(h.MemTotalMB)
	d := fmt.Sprintf("available %s of %s (%.1f%%)", mb(h.MemAvailMB), mb(h.MemTotalMB), 100*availFrac)
	swapFrac := 0.0
	if h.SwapTotalMB > 0 {
		swapFrac = float64(h.SwapUsedMB) / float64(h.SwapTotalMB)
		d += fmt.Sprintf(", swap %s of %s (%.0f%%) used", mb(h.SwapUsedMB), mb(h.SwapTotalMB), 100*swapFrac)
	}
	if goos == "darwin" {
		if h.Pressure != "" {
			d += ", pressure " + h.Pressure
		}
		return Pressure{Low: h.Pressure == "critical" && availFrac < c.AvailFrac, Detail: d}
	}
	low := availFrac < c.AvailFrac && (h.SwapTotalMB <= 0 || swapFrac > c.SwapFrac)
	return Pressure{Low: low, Detail: d}
}

func mb(v int64) string { return sysstats.HumanMB(float64(v)) }

// Proc is one process-table row.
type Proc struct {
	PID, PPID int
	Name      string // comm
	Start     uint64 // Linux starttime (clock ticks since boot); 0 unknown
	RSS       int64  // bytes
	Swap      int64  // bytes (Linux VmSwap; 0 elsewhere)
	Zombie    bool   // Linux state Z/X: exited, holds nothing
}

// Mem is what the process holds (RSS + swap).
func (p Proc) Mem() int64 { return p.RSS + p.Swap }

// Roots are the process-tree roots of Pyre's runs on this node.
type Roots struct {
	// Hosts are session-host processes (the Pyre tmux server): their
	// direct children are pane processes (harnesses), protected.
	Hosts []int
	// Runs are live runs' own processes (pane PID, headless run PID),
	// protected, with the run they belong to.
	Runs []RunRoot
}

// RunRoot is one live run's root process.
type RunRoot struct {
	PID              int
	RunID, SessionID string
}

// protectedNames are process names (comm, which Linux truncates to 15
// bytes) never chosen: harnesses and Pyre itself.
var protectedNames = map[string]bool{"claude": true, "codex": true, "agy": true, "antigravity": true, "tmux": true, "tmux: server": true,
	"pyre": true, "pyred": true}

// Protected reports whether a process is never a victim by its name.
func Protected(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndexByte(n, '/'); i >= 0 {
		n = n[i+1:]
	}
	return protectedNames[n] || strings.HasPrefix(n, "codex-") || strings.HasPrefix(n, "pyre-")
}

// Candidate is a possible victim and the run root it descends from.
type Candidate struct {
	Proc
	Root RunRoot // zero when it descends only from a host root
}

// PickVictim returns the candidate holding the most memory, if it holds at
// least minBytes. self is pyred's PID.
func PickVictim(table map[int]Proc, roots Roots, self int, minBytes int64) (Candidate, bool) {
	children := map[int][]int{}
	for _, p := range table {
		if p.PID != p.PPID && p.PPID > 0 {
			children[p.PPID] = append(children[p.PPID], p.PID)
		}
	}
	protected := map[int]bool{self: true}
	runOf := map[int]RunRoot{}
	type item struct {
		pid  int
		root RunRoot
	}
	var stack []item
	for _, h := range roots.Hosts {
		if _, ok := table[h]; !ok || h <= 0 {
			continue
		}
		protected[h] = true
		for _, c := range children[h] {
			protected[c] = true // pane processes
		}
		stack = append(stack, item{pid: h})
	}
	for _, r := range roots.Runs {
		if _, ok := table[r.PID]; !ok || r.PID <= 0 {
			continue
		}
		protected[r.PID] = true
		runOf[r.PID] = r
	}
	// Run roots first, so a run's processes are attributed to it even when
	// its pane also hangs under a host root.
	for _, r := range roots.Runs {
		if _, ok := table[r.PID]; ok && r.PID > 0 {
			stack = append([]item{{pid: r.PID, root: r}}, stack...)
		}
	}
	seen := map[int]bool{}
	var best Candidate
	found := false
	for len(stack) > 0 {
		it := stack[0]
		stack = stack[1:]
		if seen[it.pid] {
			continue
		}
		seen[it.pid] = true
		p, ok := table[it.pid]
		if !ok {
			continue
		}
		root := it.root
		if r, ok := runOf[it.pid]; ok {
			root = r
		}
		if !protected[it.pid] && !Protected(p.Name) && p.Mem() >= minBytes {
			if !found || p.Mem() > best.Mem() || (p.Mem() == best.Mem() && p.PID < best.PID) {
				best, found = Candidate{Proc: p, Root: root}, true
			}
		}
		kids := append([]int(nil), children[it.pid]...)
		sort.Ints(kids)
		for _, c := range kids {
			stack = append(stack, item{pid: c, root: root})
		}
	}
	return best, found
}

// Kill is one guard action, reported when it is resolved.
type Kill struct {
	PID       int
	Name      string
	Command   string // command line (clipped; the caller redacts)
	RSS, Swap int64  // bytes when chosen
	RunID     string
	SessionID string
	Node      string // the pressure detail when chosen
	// Outcome: "exited after SIGTERM", "killed with SIGKILL after 10s",
	// "released its memory after SIGTERM", or "signal failed: …".
	Outcome  string
	SIGKILL  bool
	Chosen   time.Time
	Resolved time.Time
}

// OS is the guard's view of the machine (os_*.go; tests use fakes).
type OS interface {
	Memory(ctx context.Context) sysstats.Host
	Table(ctx context.Context) (map[int]Proc, error)
	// Lookup re-reads one process; ok false when it is gone or its start
	// time differs from p's (a reused PID).
	Lookup(p Proc) (Proc, bool)
	// Signal signals p only if it is still the same process.
	Signal(p Proc, sig syscall.Signal) error
	// Describe returns p's command line and the Pyre run and session its
	// environment names (PYRE_RUN_ID, PYRE_SESSION_ID), if any.
	Describe(p Proc) (cmd, runID, sessionID string)
	// RunEnv returns PYRE_RUN_ID from pid's own environment ("" if none or
	// unreadable): a run root is trusted only if it names its run, so a
	// root PID reused by another process is never a root.
	RunEnv(pid int) string
}

// Guard is the watchdog state. Use New; call Run, or Tick from a loop.
type Guard struct {
	cfg    Config
	goos   string
	os     OS
	roots  func(ctx context.Context) Roots
	onKill func(Kill)
	logf   func(format string, args ...any)
	now    func() time.Time
	self   int

	mu        sync.Mutex
	pending   *Kill
	pendProc  Proc
	deadline  time.Time
	lastEnd   time.Time
	kills     []time.Time
	lastQuiet time.Time // last "nothing to do" log while low
}

// Options wires a Guard.
type Options struct {
	Config Config
	GOOS   string // default runtime.GOOS
	OS     OS     // default the machine (NewOS)
	Roots  func(ctx context.Context) Roots
	OnKill func(Kill)
	Logf   func(format string, args ...any)
	Now    func() time.Time
	Self   int // pyred's PID (default os.Getpid)
}

// New returns a guard.
func New(o Options) *Guard {
	g := &Guard{cfg: o.Config.withDefaults(), goos: o.GOOS, os: o.OS, roots: o.Roots, onKill: o.OnKill, logf: o.Logf, now: o.Now, self: o.Self}
	if g.goos == "" {
		g.goos = runtime.GOOS
	}
	if g.os == nil {
		g.os = NewOS()
	}
	if g.roots == nil {
		g.roots = func(context.Context) Roots { return Roots{} }
	}
	if g.onKill == nil {
		g.onKill = func(Kill) {}
	}
	if g.logf == nil {
		g.logf = func(string, ...any) {}
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.self == 0 {
		g.self = selfPID()
	}
	return g
}

// Config returns the effective configuration.
func (g *Guard) Config() Config { return g.cfg }

// Run ticks every Interval until ctx ends.
func (g *Guard) Run(ctx context.Context) {
	t := time.NewTicker(g.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		g.Tick(ctx)
	}
}

// Tick runs one check: it resolves a pending victim, or samples memory and,
// when the node is low and the rate limit allows, signals a new victim.
func (g *Guard) Tick(ctx context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.pending != nil {
		g.resolve(now)
		return
	}
	host := g.os.Memory(ctx)
	pr := g.cfg.Check(g.goos, host)
	if !pr.Low {
		return
	}
	if !g.lastEnd.IsZero() && now.Sub(g.lastEnd) < g.cfg.Cooldown {
		return
	}
	recent := g.kills[:0]
	for _, t := range g.kills {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	g.kills = recent
	if len(g.kills) >= g.cfg.MaxPerHour {
		g.quiet(now, "memory guard: node low on memory (%s) but %d processes were already stopped in the last hour; not stopping more", pr.Detail, len(g.kills))
		return
	}
	table, err := g.os.Table(ctx)
	if err != nil {
		g.quiet(now, "memory guard: node low on memory (%s); process table: %v", pr.Detail, err)
		return
	}
	minBytes := g.cfg.MinVictimBytes
	if minBytes <= 0 {
		minBytes = max(MinVictimFloor, int64(MinVictimFrac*float64(host.MemTotalMB))<<20)
	}
	c, ok := PickVictim(table, g.trustedRoots(ctx, table), g.self, minBytes)
	if !ok {
		g.quiet(now, "memory guard: node low on memory (%s) but no process of a Pyre run holds %s or more; nothing stopped", pr.Detail, sysstats.HumanMB(float64(minBytes>>20)))
		return
	}
	cmd, runID, sessionID := g.os.Describe(c.Proc)
	if runID == "" && sessionID == "" {
		runID, sessionID = c.Root.RunID, c.Root.SessionID
	}
	k := &Kill{PID: c.PID, Name: c.Name, Command: cmd, RSS: c.RSS, Swap: c.Swap, RunID: runID, SessionID: sessionID, Node: pr.Detail, Chosen: now}
	g.kills = append(g.kills, now)
	if err := g.os.Signal(c.Proc, syscall.SIGTERM); err != nil {
		k.Outcome, k.Resolved = "signal failed: "+err.Error(), now
		g.lastEnd = now
		g.report(*k)
		return
	}
	g.logf("memory guard: node low on memory (%s); sent SIGTERM to pid %d (%s, %s rss, %s swap, session %s)", pr.Detail, c.PID, c.Name,
		sysstats.HumanMB(float64(c.RSS>>20)), sysstats.HumanMB(float64(c.Swap>>20)), orDash(sessionID))
	g.pending, g.pendProc, g.deadline = k, c.Proc, now.Add(g.cfg.Grace)
}

// trustedRoots drops run roots that are not provably the run: the PID a
// run handle recorded can be stale (a respawned pane, a run that died
// unseen) and reused by any process. A root is kept when it is a pane (a
// direct child of a host root) or its own environment names its run.
func (g *Guard) trustedRoots(ctx context.Context, table map[int]Proc) Roots {
	r := g.roots(ctx)
	hosts := map[int]bool{}
	for _, h := range r.Hosts {
		hosts[h] = true
	}
	kept := r.Runs[:0:0]
	for _, rr := range r.Runs {
		p, ok := table[rr.PID]
		if !ok {
			continue
		}
		if hosts[p.PPID] || (rr.RunID != "" && g.os.RunEnv(rr.PID) == rr.RunID) {
			kept = append(kept, rr)
		}
	}
	r.Runs = kept
	return r
}

// resolve finishes the pending victim: gone, or SIGKILL at the deadline.
func (g *Guard) resolve(now time.Time) {
	k := g.pending
	cur, alive := g.os.Lookup(g.pendProc)
	switch {
	case !alive:
		k.Outcome = "exited after SIGTERM"
	case now.Before(g.deadline):
		return
	case cur.Mem() < g.pendProc.Mem()/4:
		k.Outcome = "released its memory after SIGTERM (still running)"
	default:
		if err := g.os.Signal(g.pendProc, syscall.SIGKILL); err != nil {
			if _, still := g.os.Lookup(g.pendProc); !still {
				k.Outcome = "exited after SIGTERM"
				break
			}
			k.Outcome = "SIGKILL failed: " + err.Error()
			break
		}
		k.SIGKILL = true
		k.Outcome = fmt.Sprintf("killed with SIGKILL %s after SIGTERM", g.cfg.Grace)
	}
	k.Resolved = now
	g.pending = nil
	g.lastEnd = now
	g.report(*k)
}

func (g *Guard) report(k Kill) {
	g.logf("memory guard: pid %d (%s) %s", k.PID, k.Name, k.Outcome)
	g.onKill(k)
}

// quiet logs at most once a minute.
func (g *Guard) quiet(now time.Time, format string, args ...any) {
	if !g.lastQuiet.IsZero() && now.Sub(g.lastQuiet) < time.Minute {
		return
	}
	g.lastQuiet = now
	g.logf(format, args...)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Note is the system note a session gets after the guard stopped one of
// its processes. node is the node's display name.
func Note(k Kill, node, goos string) string {
	var b strings.Builder
	what := k.Name
	if c := strings.TrimSpace(k.Command); c != "" {
		what = c
	}
	if len(what) > 200 {
		what = what[:200] + "…"
	}
	fmt.Fprintf(&b, "[Pyre memory guard] Pyre stopped a process this session started, to keep %s responsive: `%s` (pid %d) held %s RSS",
		orDash(node), what, k.PID, sysstats.HumanMB(float64(k.RSS>>20)))
	if k.Swap > 0 {
		fmt.Fprintf(&b, " + %s swap", sysstats.HumanMB(float64(k.Swap>>20)))
	}
	fmt.Fprintf(&b, " while the machine was nearly out of memory (%s). It got SIGTERM", k.Node)
	if k.SIGKILL {
		b.WriteString(" and then SIGKILL")
	}
	fmt.Fprintf(&b, " (%s). Its output is likely incomplete; do not rerun it unchanged.\n", k.Outcome)
	if goos == "darwin" {
		b.WriteString("Reduce what it holds in memory (smaller inputs, streaming, fewer workers) before running it again; macOS has no per-command memory cap.")
	} else {
		b.WriteString("Run heavy jobs with their own memory cap, detached: `systemd-run --user --unit=<name> --same-dir --collect -E PYRE_SOCKET -E PYRE_SESSION_TOKEN -E PYRE_SESSION_ID -p MemoryMax=<about twice its real peak, e.g. 32G> -p MemorySwapMax=0 <command>` " +
			"(the job is OOM-killed at its cap instead of stalling the machine; a cap below its real peak only kills it; check it with `systemctl --user status <name>`), or reduce what it holds in memory.")
	}
	return b.String()
}
