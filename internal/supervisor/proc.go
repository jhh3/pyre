package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/osutil"
	"github.com/jhh3/pyre/internal/sysstats"
)

// nativeTable converts the Win32 process table (sysstats) to rows.
func nativeTable(now time.Time) (map[int]Proc, error) {
	t, err := sysstats.NativeProcTable()
	if err != nil {
		return nil, err
	}
	m := make(map[int]Proc, len(t))
	for pid, p := range t {
		m[pid] = Proc{PID: p.PID, PPID: p.PPID, RSSKB: p.RSSBytes / 1024, CPUPct: sysstats.LifetimeCPUPct(p, now), Start: p.Start}
	}
	return m, nil
}

// Alive reports whether a process with pid exists (it may belong to another
// user, in which case signalling is not permitted but it is alive).
func Alive(pid int) bool {
	return osutil.ProcessAlive(pid)
}

// Proc is one row of the process table.
type Proc struct {
	PID    int
	PPID   int
	RSSKB  int64
	CPUPct float64
	// Start is the creation time where known (Windows); zero from ps.
	Start time.Time
}

// ProcessTable returns a snapshot of all processes (one bounded `ps`
// invocation on macOS and Linux; a Toolhelp snapshot on Windows, with
// CPUPct the lifetime average like procps pcpu).
func ProcessTable(ctx context.Context) (map[int]Proc, error) {
	if runtime.GOOS == "windows" {
		return nativeTable(time.Now())
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,rss=,pcpu=")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parsePS(out.Bytes()), nil
}

func parsePS(b []byte) map[int]Proc {
	m := map[int]Proc{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		rss, _ := strconv.ParseInt(f[2], 10, 64)
		cpu, _ := strconv.ParseFloat(strings.Replace(f[3], ",", ".", 1), 64)
		m[pid] = Proc{PID: pid, PPID: ppid, RSSKB: rss, CPUPct: cpu}
	}
	return m
}

// TreeUsage sums RSS and CPU over roots and all their descendants. Each
// process is counted once even if roots overlap.
func TreeUsage(table map[int]Proc, roots []int) (rssKB int64, cpuPct float64, count int) {
	children := map[int][]int{}
	for _, p := range table {
		if parent, ok := table[p.PPID]; ok && !parent.Start.IsZero() && !p.Start.IsZero() && p.Start.Before(parent.Start) {
			continue // Windows: stale parent PID reused by a newer process
		}
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	seen := map[int]bool{}
	stack := append([]int(nil), roots...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := table[pid]; ok {
			rssKB += p.RSSKB
			cpuPct += p.CPUPct
			count++
		}
		stack = append(stack, children[pid]...)
	}
	return rssKB, cpuPct, count
}

// SelfSampler measures the daemon's own CPU (getrusage deltas over wall
// time) and RSS. It is cheap and safe to call every heartbeat.
type SelfSampler struct {
	mu      sync.Mutex
	lastAt  time.Time
	lastCPU time.Duration
}

// Sample returns RSS in MB and CPU percent (of one core) since the previous
// call (0 on the first call).
func (s *SelfSampler) Sample(ctx context.Context) (rssMB float64, cpuPct float64) {
	s.mu.Lock()
	now := time.Now()
	cpu := cpuTime()
	if !s.lastAt.IsZero() {
		wall := now.Sub(s.lastAt)
		if wall > 0 {
			cpuPct = float64(cpu-s.lastCPU) / float64(wall) * 100
		}
	}
	s.lastAt, s.lastCPU = now, cpu
	s.mu.Unlock()
	return SelfRSSMB(ctx), cpuPct
}

// SelfRSSMB returns the current resident set size of this process in MB.
func SelfRSSMB(ctx context.Context) float64 {
	if runtime.GOOS == "windows" {
		_, rss, _ := sysstats.NativeSelf()
		return float64(rss) / (1 << 20)
	}
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/self/statm"); err == nil {
			f := strings.Fields(string(b))
			if len(f) >= 2 {
				pages, _ := strconv.ParseInt(f[1], 10, 64)
				return float64(pages*int64(os.Getpagesize())) / (1 << 20)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return float64(kb) / 1024
}
