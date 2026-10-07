package sysstats

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration)    { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func write(t *testing.T, path, body string) { t.Helper(); mustWrite(t, path, body) }

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const synthMeminfo = `MemTotal:       32768000 kB
MemFree:         1000000 kB
MemAvailable:    8192000 kB
SwapTotal:       8388608 kB
SwapFree:        2097152 kB
`

func linuxSampler(t *testing.T, root string, clk *fakeClock) *Sampler {
	s := NewSampler(PathRole{Path: "/synthetic/home", Role: "home"}, PathRole{Path: "/synthetic/home/.local/share/pyre", Role: "data"}, PathRole{Path: "/synthetic/big", Role: "data"})
	s.GOOS, s.ProcRoot, s.Now, s.NumCPU = "linux", root, clk.now, func() int { return 8 }
	s.Statfs = func(p string) (FSInfo, error) {
		switch p {
		case "/synthetic/big":
			return FSInfo{Total: 4000 << 30, Avail: 2000 << 30, Dev: 2}, nil
		default:
			return FSInfo{Total: 100 << 30, Avail: 5 << 30, Dev: 1}, nil
		}
	}
	return s
}

func TestLinuxHostSample(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "loadavg"), "1.50 2.25 3.00 2/900 12345\n")
	write(t, filepath.Join(root, "meminfo"), synthMeminfo)
	write(t, filepath.Join(root, "pressure/memory"), "some avg10=12.50 avg60=3.00 avg300=1.00 total=123\nfull avg10=1.00 avg60=0.00 avg300=0.00 total=9\n")
	// user nice system idle iowait irq softirq steal guest guest_nice
	write(t, filepath.Join(root, "stat"), "cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3 4 5 6 7 8\n")
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := linuxSampler(t, root, clk)

	h := s.Sample(context.Background())
	if h.CPUPct != 0 {
		t.Fatalf("first sample has no CPU delta, got %v", h.CPUPct)
	}
	if h.Load1 != 1.5 || h.Load5 != 2.25 || h.Load15 != 3 || h.CPUCount != 8 {
		t.Fatalf("load/cpus: %+v", h)
	}
	if h.MemTotalMB != 32000 || h.MemAvailMB != 8000 || h.SwapTotalMB != 8192 || h.SwapUsedMB != 6144 {
		t.Fatalf("mem/swap: %+v", h)
	}
	if h.PSISome10 != 12.5 || h.Pressure != "warn" {
		t.Fatalf("pressure: %+v", h)
	}
	if len(h.Disks) != 2 || strings.Join(h.Disks[0].Roles, ",") != "home,data" || h.Disks[0].FreeMB != 5<<10 || h.Disks[1].TotalMB != 4000<<10 {
		t.Fatalf("disks: %+v", h.Disks)
	}

	// Within MinInterval: cached.
	write(t, filepath.Join(root, "stat"), "cpu  400 0 200 800 100 0 0 0 0 0\n")
	clk.add(time.Second)
	if h2 := s.Sample(context.Background()); h2.At != h.At {
		t.Fatalf("expected cached sample")
	}
	// After the interval: busy = (300+100) / (400+100+100) of the delta.
	clk.add(30 * time.Second)
	h3 := s.Sample(context.Background())
	if h3.CPUPct != 80 {
		t.Fatalf("cpu pct = %v, want 80 (busy 400 of 500 jiffies)", h3.CPUPct)
	}
	var r contracts.NodeResources
	h3.Apply(&r)
	if r.CPUPct != 80 || r.MemPressure != "warn" || r.SwapUsedMB != 6144 || len(r.Disks) != 2 {
		t.Fatalf("apply: %+v", r)
	}
	ws := Warnings(r)
	if len(ws) != 3 {
		t.Fatalf("warnings = %q, want low disk, pressure and swap", ws)
	}
}

func TestLinuxMissingFilesLeaveZero(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := linuxSampler(t, t.TempDir(), clk)
	s.Statfs = func(string) (FSInfo, error) { return FSInfo{}, errors.New("synthetic") }
	h := s.Sample(context.Background())
	if h.MemTotalMB != 0 || h.Pressure != "" || h.Disks != nil || h.CPUCount != 8 {
		t.Fatalf("%+v", h)
	}
}

func le(vals ...any) []byte {
	var b []byte
	for _, v := range vals {
		switch x := v.(type) {
		case uint64:
			b = binary.LittleEndian.AppendUint64(b, x)
		case uint32:
			b = binary.LittleEndian.AppendUint32(b, x)
		}
	}
	// syscall.Sysctl trims one trailing NUL.
	if len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

const synthVMStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                     4248.
Pages active:                                 217483.
Pages inactive:                               211822.
Pages speculative:                              4972.
Pages wired down:                             169567.
Pages purgeable:                                 265.
"Translation faults":                     4317460635.
File-backed pages:                            124316.
Anonymous pages:                              309961.
Pages occupied by compressor:                 400528.
`

const synthIostat = `      cpu    load average
 us sy id   1m   5m   15m
 18  8 74  5.08 8.00 6.94
 17 11 72  5.10 8.01 6.95
`

func TestDarwinHostSample(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewSampler(PathRole{Path: "/synthetic/Users/x", Role: "home"})
	s.GOOS, s.Now, s.NumCPU, s.AsyncCPU = "darwin", clk.now, func() int { return 10 }, false
	sysctls := map[string][]byte{
		"hw.memsize":                          le(uint64(16 << 30)),
		"vm.swapusage":                        le(uint64(9216<<20), uint64(1350<<20), uint64(7866<<20), uint32(16384), uint32(1)),
		"kern.memorystatus_vm_pressure_level": le(uint32(2)),
		"vm.loadavg":                          le(uint32(2048), uint32(4096), uint32(1024), uint32(0), uint64(2048)),
	}
	s.Sysctl = func(name string) ([]byte, error) {
		if b, ok := sysctls[name]; ok {
			return b, nil
		}
		return nil, errors.New("synthetic: no such sysctl")
	}
	iostat := synthIostat
	s.Exec = func(_ context.Context, name string, _ ...string) (string, error) {
		switch name {
		case "vm_stat":
			return synthVMStat, nil
		case "iostat":
			return iostat, nil
		}
		return "", errors.New("synthetic: unexpected exec " + name)
	}
	s.Statfs = func(string) (FSInfo, error) { return FSInfo{Total: 460 << 30, Avail: 120 << 30, Dev: 7}, nil }
	h := s.Sample(context.Background())
	// used = (309961-265 + 169567 + 400528) pages * 16 KiB = 13746 MiB.
	if h.MemTotalMB != 16384 || h.MemAvailMB != 16384-13746 {
		t.Fatalf("mem: total %d avail %d", h.MemTotalMB, h.MemAvailMB)
	}
	if h.SwapTotalMB != 9216 || h.SwapUsedMB != 7866 || h.Pressure != "warn" {
		t.Fatalf("swap/pressure: %+v", h)
	}
	if h.CPUPct != 28 || h.Load1 != 1 || h.Load5 != 2 || h.Load15 != 0.5 {
		t.Fatalf("cpu from iostat, load from vm.loadavg: %+v", h)
	}
	if len(h.Disks) != 1 || h.Disks[0].FreeMB != 120<<10 {
		t.Fatalf("disks: %+v", h.Disks)
	}
	// iostat unavailable: CPU unknown.
	iostat = ""
	clk.add(time.Minute)
	h = s.Sample(context.Background())
	if h.CPUPct != 0 || h.Load1 != 1 {
		t.Fatalf("no iostat: %+v", h)
	}
	// Async: the first sample starts iostat in the background and has no
	// CPU; once it completed, the next sample reports it without waiting.
	iostat = synthIostat
	s.AsyncCPU = true
	clk.add(time.Minute)
	if h = s.Sample(context.Background()); h.CPUPct != 0 {
		t.Fatalf("async first sample: %+v", h)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		done := !s.cpuAt.IsZero()
		s.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	clk.add(s.MinInterval)
	if h = s.Sample(context.Background()); h.CPUPct != 28 {
		t.Fatalf("async second sample: %+v", h)
	}
	var r contracts.NodeResources
	h.Apply(&r)
	if !SwapHeavy(r) || !PressureHigh(r) {
		t.Fatalf("want swap heavy + pressure high: %+v", r)
	}
}

func TestParsersEdgeCases(t *testing.T) {
	if _, _, _, _, ok := parseIostat(" us sy id 1m 5m 15m\n 18 8 74 5 8 6\n"); ok {
		t.Fatal("since-boot report alone must not count as a sample")
	}
	if got := DarwinPressure(4); got != "critical" {
		t.Fatal(got)
	}
	if PSILevel(0.2) != "normal" || PSILevel(45) != "critical" {
		t.Fatal("psi levels")
	}
	// Old vm_stat without "Anonymous pages": active + wired + compressor.
	old := "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages active: 100.\nPages wired down: 50.\nPages occupied by compressor: 10.\n"
	if used, ok := parseVMStatUsed(old); !ok || used != 160*4096 {
		t.Fatalf("old vm_stat used=%d ok=%v", used, ok)
	}
	if d, err := parsePSTime("1-02:03:04.50"); err != nil || d != (26*time.Hour+3*time.Minute+4500*time.Millisecond) {
		t.Fatalf("ps time %v %v", d, err)
	}
	if HumanMB(512) != "512M" || HumanMB(1229) != "1.2G" || HumanMB(15*1024) != "15G" {
		t.Fatal("human")
	}
	u := contracts.RunUsage{RSSMB: 1229, CPUPct: 35.4, Procs: 14, Top: &contracts.ProcUsage{Name: "node", RSSMB: 900}}
	if Compact(u) != "1.2G 35%" || Detail(u) != "1.2G rss · 35% cpu · 14 procs · top node 900M" {
		t.Fatalf("%q / %q", Compact(u), Detail(u))
	}
}

// procStat renders a synthetic /proc/<pid>/stat line.
func procStat(pid, ppid int, comm string, ut, st, cut, cst, rssPages int64) string {
	// pid (comm) state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime prio nice threads itreal start vsize rss
	return fmt.Sprintf("%d (%s) S %d 1 1 0 -1 0 0 0 0 0 %d %d %d %d 20 0 1 0 100 1000 %d 0 0\n", pid, comm, ppid, ut, st, cut, cst, rssPages)
}

func TestLinuxRunTree(t *testing.T) {
	root := t.TempDir()
	put := func(pid, ppid int, comm string, ut, st, cut, cst, rss int64) {
		write(t, filepath.Join(root, fmt.Sprint(pid), "stat"), procStat(pid, ppid, comm, ut, st, cut, cst, rss))
	}
	put(1, 0, "init", 0, 0, 0, 0, 10)
	put(100, 1, "tmux: server", 5, 5, 0, 0, 100)
	put(200, 100, "bash", 1, 1, 0, 0, 50)        // pane of run A
	put(201, 200, "claude", 100, 50, 0, 0, 1000) // harness
	put(202, 201, "node mcp) x", 10, 0, 0, 0, 300)
	put(300, 100, "zsh", 0, 0, 0, 0, 40) // pane of run B
	write(t, filepath.Join(root, "self", "stat"), "not a pid dir")
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewRunSampler()
	s.GOOS, s.ProcRoot, s.Now, s.PageSize, s.ClockTicks = "linux", root, clk.now, 4096, 100
	roots := []RunRoot{{RunID: "run_a", SessionID: "ses_a", PID: 200}, {RunID: "run_b", PID: 300}, {RunID: "run_gone", PID: 999}}
	us := s.Sample(context.Background(), roots)
	if len(us) != 2 {
		t.Fatalf("usage = %+v", us)
	}
	a := us[0]
	if a.RunID != "run_a" || a.Procs != 3 || a.RSSBytes != (50+1000+300)*4096 || a.HasCPU {
		t.Fatalf("run a first sample: %+v", a)
	}
	if a.Top == nil || a.Top.Name != "claude" || a.Top.PID != 201 {
		t.Fatalf("top = %+v", a.Top)
	}
	// 10 s later: the MCP child (10 jiffies) exited and was reaped by the
	// harness (cutime +10), and the harness used 200 more jiffies. The tree
	// delta is exactly the 200 new jiffies = 2 s CPU over 10 s = 20%.
	os.RemoveAll(filepath.Join(root, "202"))
	put(201, 200, "claude", 250, 100, 10, 0, 1100)
	clk.add(10 * time.Second)
	us = s.Sample(context.Background(), roots)
	if !us[0].HasCPU || us[0].CPUPct != 20 || us[0].Procs != 2 {
		t.Fatalf("run a second sample: %+v", us[0])
	}
	if !us[1].HasCPU || us[1].CPUPct != 0 {
		t.Fatalf("run b: %+v", us[1])
	}
	cs := Contract(us)
	if cs[0].RSSMB != 4.5 || cs[0].Top.Name != "claude" || cs[0].SessionID != "ses_a" {
		t.Fatalf("contract: %+v", cs[0])
	}
	// No roots: no scan, no state.
	if us := s.Sample(context.Background(), nil); us != nil {
		t.Fatal(us)
	}
}

func TestDarwinRunTreeFromPS(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewRunSampler()
	s.GOOS, s.Now = "darwin", clk.now
	out := `    1     0  13936  32:14.46 /sbin/launchd
  500     1   4000   0:01.00 /opt/homebrew/bin/tmux
  510   500   3000   0:00.50 -zsh
  511   510 900000   1:00.00 /Users/synthetic/.local/bin/claude
  512   511  50000   0:02.00 /Applications/Synthetic App.app/Contents/MacOS/helper
`
	s.Exec = func(_ context.Context, name string, args ...string) (string, error) {
		if name != "ps" || !strings.Contains(strings.Join(args, " "), "-S") {
			return "", errors.New("synthetic: unexpected")
		}
		return out, nil
	}
	us := s.Sample(context.Background(), []RunRoot{{RunID: "run_m", PID: 510}})
	if len(us) != 1 || us[0].Procs != 3 || us[0].RSSBytes != (3000+900000+50000)*1024 || us[0].Top.Name != "claude" {
		t.Fatalf("%+v", us)
	}
	out = strings.Replace(out, "1:00.00", "1:15.00", 1)
	clk.add(30 * time.Second)
	us = s.Sample(context.Background(), []RunRoot{{RunID: "run_m", PID: 510}})
	if us[0].CPUPct != 50 {
		t.Fatalf("cpu = %v, want 50 (15 s over 30 s)", us[0].CPUPct)
	}
	if p := parsePSTable(out)[512]; p.Name != "helper" {
		t.Fatalf("name with spaces: %+v", p)
	}
}

// TestRealSample is a light, read-only check on the host running the
// tests: one machine sample and one process-tree sample of this test
// process (no load generated).
func TestRealSample(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unsupported")
	}
	s := NewSampler(DefaultPaths(t.TempDir())...)
	h := s.Sample(context.Background())
	if h.MemTotalMB <= 0 || h.MemAvailMB <= 0 || h.MemAvailMB > h.MemTotalMB || len(h.Disks) == 0 || h.Disks[0].TotalMB <= 0 {
		t.Fatalf("real host sample: %+v", h)
	}
	if h.Pressure == "" {
		t.Logf("memory pressure unavailable on this host")
	}
	rs := NewRunSampler()
	us := rs.Sample(context.Background(), []RunRoot{{RunID: "run_self", PID: os.Getpid()}})
	if len(us) != 1 || us[0].RSSBytes <= 0 || us[0].Procs < 1 {
		t.Fatalf("self tree: %+v", us)
	}
	t.Logf("host: cpu %.1f%% load %.2f mem %d/%d MB swap %d/%d MB pressure %q disks %+v; self rss %d",
		h.CPUPct, h.Load1, h.MemAvailMB, h.MemTotalMB, h.SwapUsedMB, h.SwapTotalMB, h.Pressure, h.Disks, us[0].RSSBytes)
}

// TestRealProbe (opt-in: PYRE_STATS_PROBE=1) is the read-only real-host
// check: two machine samples 10 s apart (for CPU %) and the process trees
// of existing harness processes (claude/codex) sampled twice. It only
// reads /proc or runs ps/vm_stat/iostat; it starts no load.
func TestRealProbe(t *testing.T) {
	if os.Getenv("PYRE_STATS_PROBE") == "" {
		t.Skip("set PYRE_STATS_PROBE=1")
	}
	s := NewSampler(DefaultPaths("")...)
	s.MinInterval = 0
	s.AsyncCPU = false
	rs := NewRunSampler()
	rs.MinInterval = 0
	tab, err := rs.table(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var roots []RunRoot
	for _, p := range tab {
		parent, ok := tab[p.PPID]
		if (p.Name == "claude" || p.Name == "codex") && ok && p.PPID > 1 && parent.Name != "claude" && parent.Name != "codex" && len(roots) < 6 {
			roots = append(roots, RunRoot{RunID: fmt.Sprintf("pid%d-%s", p.PPID, p.Name), PID: p.PPID})
		}
	}
	start := time.Now()
	s.Sample(context.Background())
	rs.Sample(context.Background(), roots)
	time.Sleep(10 * time.Second)
	t0 := time.Now()
	h := s.Sample(context.Background())
	t1 := time.Now()
	us := rs.Sample(context.Background(), roots)
	t.Logf("cost: machine sample %s, process-tree sample (%d roots, full table) %s", t1.Sub(t0).Round(time.Microsecond), len(roots), time.Since(t1).Round(time.Microsecond))
	var r contracts.NodeResources
	h.Apply(&r)
	t.Logf("machine: cpu %.1f%% (%d cores) load %.2f %.2f %.2f mem used %d/%d MB swap %d/%d MB pressure %q psi10 %.2f",
		r.CPUPct, r.CPUCount, r.Load1, r.Load5, r.Load15, r.MemTotalMB-r.MemAvailMB, r.MemTotalMB, r.SwapUsedMB, r.SwapTotalMB, r.MemPressure, r.MemPressureSome10)
	for _, d := range r.Disks {
		t.Logf("disk %v: %d MB free of %d MB", d.Roles, d.FreeMB, d.TotalMB)
	}
	t.Logf("warnings: %q", Warnings(r))
	for _, u := range Contract(us) {
		t.Logf("tree %s: %s", u.RunID, Detail(u))
	}
	t.Logf("probe wall %s (incl. 10 s sleep)", time.Since(start).Round(time.Millisecond))
}
