//go:build linux

package memguard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/sysstats"
)

// writeProc writes one fake /proc/<pid> entry.
func writeProc(t *testing.T, root string, pid, ppid int, comm string, start uint64, rssKB, swapKB int64, cmdline string, env ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := []string{"S", strconv.Itoa(ppid)}
	for i := 2; i < 19; i++ {
		f = append(f, "0")
	}
	f = append(f, strconv.FormatUint(start, 10), "0", strconv.FormatInt(rssKB/4, 10), "0", "0")
	stat := fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(f, " "))
	status := fmt.Sprintf("Name:\t%s\nVmRSS:\t%d kB\nVmSwap:\t%d kB\n", comm, rssKB, swapKB)
	for name, body := range map[string]string{"stat": stat, "status": status,
		"cmdline": strings.ReplaceAll(cmdline, " ", "\x00") + "\x00", "environ": strings.Join(env, "\x00") + "\x00"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A fake /proc tree: the table, swap from status, attribution from the
// nearest ancestor's environment, and a start-time check on lookup.
func TestProcOSFakeTree(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 100, 1, "tmux: server", 10, 9000, 0, "tmux -S /x/pyre.sock")
	writeProc(t, root, 101, 100, "claude", 11, 2<<20, 0, "claude --resume", "PYRE_SESSION_ID=ses_synth-c1", "PYRE_RUN_ID=run_synth-c1")
	writeProc(t, root, 102, 101, "bash", 12, 4000, 0, "bash -c python3 -I measure.py") // environ unreadable below
	writeProc(t, root, 103, 102, "python3", 13, 23<<20, 1<<20, "python3 -I measure.py")
	if err := os.WriteFile(filepath.Join(root, "102", "environ"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte("MemTotal: 31457280 kB\nMemAvailable: 1258291 kB\nSwapTotal: 8388608 kB\nSwapFree: 838861 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := NewProcOS(root)
	tab, err := o.Table(context.Background())
	if err != nil || len(tab) != 4 {
		t.Fatalf("table %v %v", tab, err)
	}
	p := tab[103]
	if p.PPID != 102 || p.Name != "python3" || p.Start != 13 || p.RSS != 23<<30 || p.Swap != 1<<30 {
		t.Fatalf("row %+v", p)
	}
	c, ok := PickVictim(tab, Roots{Hosts: []int{100}}, 1, 1<<30)
	if !ok || c.PID != 103 {
		t.Fatalf("victim %+v", c)
	}
	cmd, run, sess := o.Describe(c.Proc)
	if cmd != "python3 -I measure.py" || run != "run_synth-c1" || sess != "ses_synth-c1" {
		t.Fatalf("describe %q %q %q", cmd, run, sess)
	}
	if _, ok := o.Lookup(Proc{PID: 103, Start: 99}); ok {
		t.Fatal("lookup ignored a different start time (reused pid)")
	}
	if _, ok := o.Lookup(p); !ok {
		t.Fatal("lookup lost the process")
	}
	if err := o.Signal(p, syscall.SIGTERM); err == nil {
		t.Fatal("signalled through a fake /proc")
	}
	if !(Config{}).Check("linux", o.Memory(context.Background())).Low {
		t.Fatalf("fake meminfo not low: %+v", o.Memory(context.Background()))
	}
}

// memOS is the real machine with a scripted memory reading.
type memOS struct {
	OS
	host sysstats.Host
}

func (m memOS) Memory(context.Context) sysstats.Host { return m.host }

// startTree starts a real process tree: host (a stand-in for the Pyre tmux
// server) -> pane (a stand-in for the harness) -> python3 holding 64 MiB,
// plus a small sleep. It returns the host and pane PIDs and the python PID.
func startTree(t *testing.T, ignoreTerm bool) (host, pane, py int) {
	t.Helper()
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	trap := ""
	if ignoreTerm {
		trap = "import signal; signal.signal(signal.SIGTERM, signal.SIG_IGN); "
	}
	pyScript := filepath.Join(dir, "hold.py")
	if err := os.WriteFile(pyScript, []byte(trap+"import time\na = b'x' * (64 << 20)\nopen('"+filepath.Join(dir, "ready")+"', 'w').close()\ntime.sleep(600)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paneSh := filepath.Join(dir, "pane.sh")
	if err := os.WriteFile(paneSh, []byte("#!/bin/sh\n"+pyBin+" -I "+pyScript+" &\nsleep 600 &\nwait\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	hostSh := filepath.Join(dir, "host.sh")
	if err := os.WriteFile(hostSh, []byte("#!/bin/sh\n/bin/sh "+paneSh+" &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", hostSh)
	cmd.Env = append(os.Environ(), "PYRE_SESSION_ID=ses_synth-tree", "PYRE_RUN_ID=run_synth-tree")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	host = cmd.Process.Pid
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			tab, _ := NewOS().Table(context.Background())
			for _, p := range tab {
				if p.PPID == host {
					pane = p.PID
				}
			}
			for _, p := range tab {
				if p.PPID == pane && p.Name == "python3" {
					py = p.PID
				}
			}
			if pane > 0 && py > 0 {
				return host, pane, py
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("process tree did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return err == nil && !strings.Contains(string(b), ") Z ")
}

// A real process tree: the guard picks the python3 holding memory (not the
// host or pane process, nor an unrelated process), SIGTERMs it through a
// pidfd, attributes it from its environment, and the pane and its other
// child survive.
func TestGuardRealTree(t *testing.T) {
	host, pane, py := startTree(t, false)
	var kills []Kill
	g := New(Options{GOOS: "linux", OS: memOS{OS: NewOS(), host: lowHost()}, Self: os.Getpid(),
		Config: Config{MinVictimBytes: 32 << 20},
		Roots:  func(context.Context) Roots { return Roots{Hosts: []int{host}} },
		OnKill: func(k Kill) { kills = append(kills, k) }})
	g.Tick(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for len(kills) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		g.Tick(context.Background())
	}
	if len(kills) != 1 {
		t.Fatalf("no resolved kill: %+v", kills)
	}
	k := kills[0]
	if k.PID != py || k.Outcome != "exited after SIGTERM" || k.SessionID != "ses_synth-tree" || k.RunID != "run_synth-tree" || !strings.Contains(k.Command, "hold.py") || k.RSS < 60<<20 {
		t.Fatalf("kill %+v (python %d)", k, py)
	}
	if alive(py) {
		t.Fatal("python3 still running")
	}
	if !alive(pane) || !alive(host) {
		t.Fatalf("the pane (%v) or host (%v) died", alive(pane), alive(host))
	}
}

// A victim that ignores SIGTERM gets SIGKILL after the grace.
func TestGuardRealTreeSIGKILL(t *testing.T) {
	host, pane, py := startTree(t, true)
	var kills []Kill
	g := New(Options{GOOS: "linux", OS: memOS{OS: NewOS(), host: lowHost()}, Self: os.Getpid(),
		Config: Config{MinVictimBytes: 32 << 20, Grace: 300 * time.Millisecond},
		Roots:  func(context.Context) Roots { return Roots{Hosts: []int{host}} },
		OnKill: func(k Kill) { kills = append(kills, k) }})
	deadline := time.Now().Add(5 * time.Second)
	for len(kills) == 0 && time.Now().Before(deadline) {
		g.Tick(context.Background())
		time.Sleep(50 * time.Millisecond)
	}
	if len(kills) != 1 || kills[0].PID != py || !kills[0].SIGKILL {
		t.Fatalf("kills %+v (python %d)", kills, py)
	}
	waitGone := time.Now().Add(2 * time.Second)
	for alive(py) && time.Now().Before(waitGone) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(py) || !alive(pane) {
		t.Fatalf("python alive %v, pane alive %v", alive(py), alive(pane))
	}
}

// A zombie (exited, not reaped) is gone for Lookup, so a SIGTERMed process
// that died is reported as exited, not as "released its memory".
func TestProcOSZombieIsGone(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 103, 1, "python3", 13, 23<<20, 0, "python3", "PYRE_RUN_ID=run_synth-z")
	o := NewProcOS(root)
	p, ok := o.Lookup(Proc{PID: 103, Start: 13})
	if !ok || o.RunEnv(103) != "run_synth-z" {
		t.Fatalf("live lookup %+v %v env %q", p, ok, o.RunEnv(103))
	}
	stat := filepath.Join(root, "103", "stat")
	b, _ := os.ReadFile(stat)
	if err := os.WriteFile(stat, []byte(strings.Replace(string(b), ") S ", ") Z ", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := o.Lookup(Proc{PID: 103, Start: 13}); ok {
		t.Fatal("zombie still alive for Lookup")
	}
}
