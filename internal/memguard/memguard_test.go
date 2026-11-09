package memguard

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/sysstats"
)

const gib = int64(1) << 30

// lowHost is a 30 GiB / 8 GiB-swap machine like office-box2 at the
// incident: 1.2 GiB available, 7.2 GiB of swap used.
func lowHost() sysstats.Host {
	return sysstats.Host{MemTotalMB: 30 * 1024, MemAvailMB: 1229, SwapTotalMB: 8 * 1024, SwapUsedMB: 7373}
}

func TestCheckThresholds(t *testing.T) {
	c := Config{}
	for _, tc := range []struct {
		name string
		goos string
		h    sysstats.Host
		low  bool
	}{
		{"incident", "linux", lowHost(), true},
		{"plenty available", "linux", sysstats.Host{MemTotalMB: 30720, MemAvailMB: 9000, SwapTotalMB: 8192, SwapUsedMB: 8000}, false},
		{"low but swap barely used", "linux", sysstats.Host{MemTotalMB: 30720, MemAvailMB: 1000, SwapTotalMB: 8192, SwapUsedMB: 2000}, false},
		{"swap exactly 80%", "linux", sysstats.Host{MemTotalMB: 30720, MemAvailMB: 1000, SwapTotalMB: 1000, SwapUsedMB: 800}, false},
		{"available exactly 5%", "linux", sysstats.Host{MemTotalMB: 1000, MemAvailMB: 50, SwapTotalMB: 1000, SwapUsedMB: 900}, false},
		{"no swap, low", "linux", sysstats.Host{MemTotalMB: 30720, MemAvailMB: 1000}, true},
		{"no sample", "linux", sysstats.Host{}, false},
		{"darwin critical and low", "darwin", sysstats.Host{MemTotalMB: 16384, MemAvailMB: 300, Pressure: "critical"}, true},
		{"darwin low but warn", "darwin", sysstats.Host{MemTotalMB: 16384, MemAvailMB: 300, Pressure: "warn", SwapTotalMB: 1024, SwapUsedMB: 1024}, false},
		{"darwin critical, memory unknown", "darwin", sysstats.Host{MemTotalMB: 16384, MemAvailMB: 0, Pressure: "critical"}, false},
		{"linux MemAvailable missing", "linux", sysstats.Host{MemTotalMB: 30720}, false},
		{"darwin critical, plenty", "darwin", sysstats.Host{MemTotalMB: 16384, MemAvailMB: 4000, Pressure: "critical"}, false},
	} {
		if got := c.Check(tc.goos, tc.h); got.Low != tc.low {
			t.Errorf("%s: low = %v (%s), want %v", tc.name, got.Low, got.Detail, tc.low)
		}
	}
	// Configured thresholds.
	c = Config{AvailFrac: 0.10, SwapFrac: 0.50}
	if !c.Check("linux", sysstats.Host{MemTotalMB: 1000, MemAvailMB: 90, SwapTotalMB: 1000, SwapUsedMB: 600}).Low {
		t.Fatal("10%/50% thresholds not applied")
	}
	if d := lowHost(); !strings.Contains(Config{}.Check("linux", d).Detail, "swap 7.2G of 8.0G (90%) used") {
		t.Fatalf("detail %q", Config{}.Check("linux", d).Detail)
	}
	if got := (Config{}).Describe("linux"); got != "available < 5% and swap > 80% used" {
		t.Fatalf("describe %q", got)
	}
}

// fakeTable is a node at the incident: pyred, the owner's own processes, a
// Pyre tmux server with a Claude pane whose Bash tool runs measure.py, a
// Codex app-server pane whose exec runs a build, and a shell run.
func fakeTable() map[int]Proc {
	ps := []Proc{
		{PID: 1, PPID: 0, Name: "systemd", RSS: 10 << 20},
		{PID: 50, PPID: 1, Name: "pyred", RSS: 80 << 20},
		// The owner's own heavy processes, outside every Pyre run.
		{PID: 60, PPID: 1, Name: "firefox", RSS: 40 * gib},
		{PID: 61, PPID: 1, Name: "tmux: server", RSS: 5 << 20}, // the owner's own tmux
		{PID: 62, PPID: 61, Name: "zsh", RSS: 5 << 20},
		{PID: 63, PPID: 62, Name: "python3", RSS: 30 * gib},
		// A detached job an agent started (setsid -f: reparented to systemd).
		{PID: 70, PPID: 1, Name: "render", RSS: 25 * gib},
		// Pyre's tmux server.
		{PID: 100, PPID: 1, Name: "tmux: server", RSS: 10 << 20},
		{PID: 101, PPID: 100, Name: "claude", RSS: 2 * gib}, // pane: harness
		{PID: 102, PPID: 101, Name: "bash", RSS: 4 << 20},
		{PID: 103, PPID: 102, Name: "python3", RSS: 23 * gib, Swap: gib}, // the runaway
		{PID: 104, PPID: 101, Name: "node", RSS: 3 * gib},                // an MCP server
		{PID: 105, PPID: 101, Name: "pyred", RSS: 20 << 20},              // hook
		{PID: 106, PPID: 102, Name: "claude", RSS: 9 * gib},              // a nested harness
		{PID: 110, PPID: 100, Name: "codex", RSS: 1 * gib},               // app-server pane
		{PID: 111, PPID: 110, Name: "zsh", RSS: 4 << 20},
		{PID: 112, PPID: 111, Name: "cargo", RSS: 5 * gib},
		{PID: 120, PPID: 100, Name: "zsh", RSS: 6 * gib}, // a shell run's shell (pane)
	}
	m := map[int]Proc{}
	for _, p := range ps {
		m[p.PID] = p
	}
	return m
}

func TestPickVictimLargestNonHarnessDescendant(t *testing.T) {
	tab := fakeTable()
	roots := Roots{Hosts: []int{100}, Runs: []RunRoot{{PID: 101, RunID: "run_c1", SessionID: "ses_c1"}, {PID: 120, RunID: "run_s1", SessionID: "ses_s1"}}}
	c, ok := PickVictim(tab, roots, 50, gib)
	if !ok || c.PID != 103 || c.Root.SessionID != "ses_c1" || c.Root.RunID != "run_c1" {
		t.Fatalf("victim %+v ok=%v, want pid 103 of ses_c1", c, ok)
	}
	// Without it, the next largest eligible process: the Codex exec's build
	// (5G), not the nested harness (9G) or the shell run's shell (6G).
	delete(tab, 103)
	if c, _ := PickVictim(tab, roots, 50, gib); c.PID != 112 || c.Root.SessionID != "" {
		t.Fatalf("second victim %+v, want the Codex exec 112 (no run root: app-server pane)", c)
	}
	delete(tab, 112)
	if c, _ := PickVictim(tab, roots, 50, gib); c.PID != 104 {
		t.Fatalf("third victim %+v, want the MCP server 104", c)
	}
	// Below the minimum nothing is chosen; harnesses, panes, pyred, tmux,
	// the owner's processes and detached jobs never are.
	delete(tab, 104)
	if c, ok := PickVictim(tab, roots, 50, gib); ok {
		t.Fatalf("picked %+v; only protected or outside processes are left", c)
	}
	// No roots: nothing is a Pyre process.
	if c, ok := PickVictim(fakeTable(), Roots{}, 50, 0); ok {
		t.Fatalf("picked %+v without roots", c)
	}
	// pyred's own PID is protected even under a root.
	tab = fakeTable()
	tab[50] = Proc{PID: 50, PPID: 102, Name: "worker", RSS: 90 * gib}
	if c, _ := PickVictim(tab, roots, 50, gib); c.PID != 103 {
		t.Fatalf("picked %+v; pyred's own pid must be protected", c)
	}
	// A run root without a host root (termhost, headless pyre run).
	tab = fakeTable()
	if c, _ := PickVictim(tab, Roots{Runs: []RunRoot{{PID: 110, RunID: "run_x", SessionID: "ses_x"}}}, 50, gib); c.PID != 112 || c.Root.SessionID != "ses_x" {
		t.Fatalf("run-root victim %+v", c)
	}
}

func TestProtectedNames(t *testing.T) {
	for _, n := range []string{"claude", "codex", "agy", "tmux", "tmux: server", "pyred", "pyre", "codex-linux-sandbox", "/usr/bin/claude"} {
		if !Protected(n) {
			t.Errorf("%q not protected", n)
		}
	}
	for _, n := range []string{"python3", "node", "cargo", "bash", "claudette-x"} {
		if Protected(n) {
			t.Errorf("%q protected", n)
		}
	}
}

// fakeOS is a scripted machine.
type fakeOS struct {
	mu      sync.Mutex
	host    sysstats.Host
	table   map[int]Proc
	signals []string
	// onTerm decides what SIGTERM does: "exit", "ignore", "release".
	onTerm  string
	sigErr  error
	env     map[int][2]string // pid -> run, session
	runEnv  map[int]string    // pid -> PYRE_RUN_ID of its own environment
	cmdline map[int]string
}

func (f *fakeOS) Memory(context.Context) sysstats.Host {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.host
}
func (f *fakeOS) Table(context.Context) (map[int]Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[int]Proc{}
	for k, v := range f.table {
		m[k] = v
	}
	return m, nil
}
func (f *fakeOS) Lookup(p Proc) (Proc, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.table[p.PID]
	if !ok || cur.Start != p.Start {
		return Proc{}, false
	}
	return cur, true
}
func (f *fakeOS) Signal(p Proc, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sigErr != nil {
		return f.sigErr
	}
	f.signals = append(f.signals, sig.String()+":"+strconv.Itoa(p.PID))
	switch {
	case sig == syscall.SIGKILL:
		delete(f.table, p.PID)
	case f.onTerm == "exit":
		delete(f.table, p.PID)
	case f.onTerm == "release":
		q := f.table[p.PID]
		q.RSS, q.Swap = 100<<20, 0
		f.table[p.PID] = q
	}
	return nil
}
func (f *fakeOS) RunEnv(pid int) string { return f.runEnv[pid] }
func (f *fakeOS) Describe(p Proc) (string, string, string) {
	e := f.env[p.PID]
	return f.cmdline[p.PID], e[0], e[1]
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func roots(context.Context) Roots {
	return Roots{Hosts: []int{100}, Runs: []RunRoot{{PID: 101, RunID: "run_c1", SessionID: "ses_c1"}}}
}
func newFake(onTerm string) *fakeOS {
	return &fakeOS{host: lowHost(), table: fakeTable(), onTerm: onTerm,
		env:     map[int][2]string{103: {"run_c1", "ses_c1"}},
		cmdline: map[int]string{103: "python3 -I measure.py"}}
}

func newGuard(f *fakeOS, c *clock, kills *[]Kill) *Guard {
	return New(Options{GOOS: "linux", OS: f, Roots: roots, Now: c.now, Self: 50, OnKill: func(k Kill) { *kills = append(*kills, k) }})
}

func TestGuardTermThenExit(t *testing.T) {
	f, c, kills := newFake("exit"), &clock{t: time.Unix(1_800_000_000, 0)}, []Kill{}
	g := newGuard(f, c, &kills)
	g.Tick(context.Background())
	if len(f.signals) != 1 || f.signals[0] != "terminated:103" {
		t.Fatalf("signals %v, want SIGTERM to 103", f.signals)
	}
	if len(kills) != 0 {
		t.Fatal("reported before resolution")
	}
	c.add(2 * time.Second)
	g.Tick(context.Background())
	if len(kills) != 1 {
		t.Fatalf("kills %v", kills)
	}
	k := kills[0]
	if k.PID != 103 || k.SessionID != "ses_c1" || k.RunID != "run_c1" || k.Command != "python3 -I measure.py" || k.RSS != 23*gib || k.Swap != gib ||
		k.Outcome != "exited after SIGTERM" || k.SIGKILL || !strings.Contains(k.Node, "available 1.2G of 30G") {
		t.Fatalf("kill %+v", k)
	}
	if len(f.signals) != 1 {
		t.Fatalf("SIGKILL sent to a process that exited: %v", f.signals)
	}
}

func TestGuardKillsAfterGrace(t *testing.T) {
	f, c, kills := newFake("ignore"), &clock{t: time.Unix(1_800_000_000, 0)}, []Kill{}
	g := newGuard(f, c, &kills)
	g.Tick(context.Background())
	for i := 0; i < 4; i++ { // 8 s: still within the grace
		c.add(2 * time.Second)
		g.Tick(context.Background())
	}
	if len(f.signals) != 1 || len(kills) != 0 {
		t.Fatalf("before the grace: signals %v kills %v", f.signals, kills)
	}
	c.add(2 * time.Second)
	g.Tick(context.Background())
	if len(f.signals) != 2 || f.signals[1] != "killed:103" || len(kills) != 1 || !kills[0].SIGKILL || kills[0].Outcome != "killed with SIGKILL 10s after SIGTERM" {
		t.Fatalf("after the grace: signals %v kills %+v", f.signals, kills)
	}
}

func TestGuardSparesProcessThatReleasedMemory(t *testing.T) {
	f, c, kills := newFake("release"), &clock{t: time.Unix(1_800_000_000, 0)}, []Kill{}
	g := newGuard(f, c, &kills)
	g.Tick(context.Background())
	c.add(11 * time.Second)
	g.Tick(context.Background())
	if len(f.signals) != 1 || len(kills) != 1 || kills[0].SIGKILL || !strings.Contains(kills[0].Outcome, "released its memory") {
		t.Fatalf("signals %v kills %+v", f.signals, kills)
	}
}

func TestGuardRateLimit(t *testing.T) {
	f, c, kills := newFake("exit"), &clock{t: time.Unix(1_800_000_000, 0)}, []Kill{}
	// Many runaways, all 20 GiB, under one run.
	for i := 0; i < 20; i++ {
		f.table[200+i] = Proc{PID: 200 + i, PPID: 102, Name: "python3", RSS: 20 * gib}
	}
	g := newGuard(f, c, &kills)
	tick := func() { g.Tick(context.Background()); c.add(2 * time.Second) }
	tick() // SIGTERM 103
	tick() // resolved (exited); cooldown starts
	n := len(f.signals)
	for i := 0; i < 14; i++ { // 28 s: inside the 30 s cooldown
		tick()
	}
	if len(f.signals) != n {
		t.Fatalf("signalled during the cooldown: %v", f.signals)
	}
	tick()
	if len(f.signals) != n+1 {
		t.Fatalf("no victim after the cooldown: %v", f.signals)
	}
	// Continuous pressure for the rest of the hour: at most MaxPerHour
	// victims (the first one leaves the window at t0+1h).
	for i := 0; i < 1700; i++ {
		tick()
	}
	if len(f.signals) != DefaultMaxPerHour {
		t.Fatalf("%d signals in an hour, want %d", len(f.signals), DefaultMaxPerHour)
	}
	// Pressure gone: nothing more even after the hour.
	f.host = sysstats.Host{MemTotalMB: 30720, MemAvailMB: 20000, SwapTotalMB: 8192}
	for i := 0; i < 1000; i++ {
		tick()
	}
	if len(f.signals) != DefaultMaxPerHour {
		t.Fatalf("signals without pressure: %d", len(f.signals))
	}
}

func TestGuardIdleWithoutPressureOrVictim(t *testing.T) {
	f, c, kills := newFake("exit"), &clock{t: time.Unix(1_800_000_000, 0)}, []Kill{}
	f.host = sysstats.Host{MemTotalMB: 30720, MemAvailMB: 10000, SwapTotalMB: 8192, SwapUsedMB: 8000}
	g := newGuard(f, c, &kills)
	g.Tick(context.Background())
	if len(f.signals) != 0 {
		t.Fatalf("signalled without pressure: %v", f.signals)
	}
	// Pressure, but the only big processes are the owner's: nothing.
	f.host = lowHost()
	for _, pid := range []int{103, 104, 106, 112} {
		delete(f.table, pid)
	}
	g.Tick(context.Background())
	if len(f.signals) != 0 {
		t.Fatalf("signalled an outside or protected process: %v", f.signals)
	}
	// A 1.2 GiB run process is under the default minimum on a 30 GiB node
	// (max(1 GiB, 5% of RAM) = 1.5 GiB): stopping it would not help.
	f.table[300] = Proc{PID: 300, PPID: 102, Name: "python3", RSS: 1229 << 20}
	g.Tick(context.Background())
	if len(f.signals) != 0 {
		t.Fatalf("signalled a process below the minimum: %v", f.signals)
	}
	f.table[300] = Proc{PID: 300, PPID: 102, Name: "python3", RSS: 1600 << 20}
	g.Tick(context.Background())
	if len(f.signals) != 1 || f.signals[0] != "terminated:300" {
		t.Fatalf("signals %v, want SIGTERM to 300", f.signals)
	}
}

func TestGuardSignalFailureIsReported(t *testing.T) {
	f, c, kills := newFake("exit"), &clock{t: time.Unix(1_800_000_000, 0)}, []Kill{}
	f.sigErr = errors.New("operation not permitted")
	g := newGuard(f, c, &kills)
	g.Tick(context.Background())
	if len(kills) != 1 || !strings.HasPrefix(kills[0].Outcome, "signal failed") {
		t.Fatalf("kills %+v", kills)
	}
}

func TestNoteText(t *testing.T) {
	k := Kill{PID: 103, Name: "python3", Command: "python3 -I measure.py", RSS: 23 * gib, Swap: gib, SessionID: "ses_c1",
		Node: "available 1.2G of 30.0G (3.9%), swap 7.2G of 8.0G (90%) used", Outcome: "killed with SIGKILL 10s after SIGTERM", SIGKILL: true}
	n := Note(k, "office-box2", "linux")
	for _, want := range []string{"[Pyre memory guard]", "keep office-box2 responsive", "`python3 -I measure.py` (pid 103)", "23G RSS + 1.0G swap",
		"available 1.2G of 30.0G", "SIGTERM and then SIGKILL", "do not rerun it unchanged", "systemd-run --user --unit=<name> --same-dir --collect -E PYRE_SOCKET -E PYRE_SESSION_TOKEN -E PYRE_SESSION_ID -p MemoryMax=<about twice its real peak, e.g. 32G> -p MemorySwapMax=0"} {
		if !strings.Contains(n, want) {
			t.Errorf("note lacks %q:\n%s", want, n)
		}
	}
	if m := Note(k, "laptop", "darwin"); strings.Contains(m, "systemd-run") || !strings.Contains(m, "macOS has no per-command memory cap") {
		t.Fatalf("darwin note:\n%s", m)
	}
}

// A run root that is neither a pane nor names its run in its own
// environment (a stale handle PID now reused by, say, the owner's browser)
// is dropped: its children are never candidates.
func TestGuardIgnoresUntrustedRunRoot(t *testing.T) {
	f, c, kills := newFake("exit"), &clock{t: time.Unix(1_800_000_000, 0)}, []Kill{}
	for _, pid := range []int{103, 104, 106, 112} {
		delete(f.table, pid)
	}
	// The owner's firefox (60) took the PID a run handle recorded; its
	// renderer holds 6 GiB.
	f.table[64] = Proc{PID: 64, PPID: 60, Name: "Web Content", RSS: 6 * gib}
	stale := func(context.Context) Roots {
		return Roots{Hosts: []int{100}, Runs: []RunRoot{{PID: 60, RunID: "run_old", SessionID: "ses_old"}}}
	}
	g := New(Options{GOOS: "linux", OS: f, Roots: stale, Now: c.now, Self: 50, OnKill: func(k Kill) { kills = append(kills, k) }})
	g.Tick(context.Background())
	if len(f.signals) != 0 {
		t.Fatalf("signalled under an untrusted root: %v", f.signals)
	}
	// The same root naming its run in its environment (a headless pyre
	// run's process) is trusted.
	f.runEnv = map[int]string{60: "run_old"}
	g.Tick(context.Background())
	if len(f.signals) != 1 || f.signals[0] != "terminated:64" {
		t.Fatalf("signals %v, want SIGTERM to 64 under the trusted root", f.signals)
	}
}
