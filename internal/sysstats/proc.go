package sysstats

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Proc is one process-table row. CPU is cumulative user+system time
// INCLUDING reaped children (Linux cutime+cstime; macOS `ps -S`), so the
// sum over a tree stays continuous when a child exits and is waited for by
// a tree member: the tree's CPU delta then counts short-lived tool
// commands too.
type Proc struct {
	PID, PPID int
	RSSBytes  int64
	CPU       time.Duration
	Name      string
}

// RunRoot names a run and the PID of its tmux pane process.
type RunRoot struct {
	RunID, SessionID string
	PID              int
}

// RunUsage is one run's process-tree sample.
type RunUsage struct {
	RunID, SessionID string
	Procs            int
	RSSBytes         int64
	CPUPct           float64 // % of one core; HasCPU false until two samples exist
	HasCPU           bool
	Top              *Proc // largest process by RSS
	At               time.Time
}

// RunSampler samples run process trees and keeps the previous cumulative
// CPU per run for deltas.
type RunSampler struct {
	GOOS        string
	ProcRoot    string
	MinInterval time.Duration
	Now         func() time.Time
	Exec        func(ctx context.Context, name string, args ...string) (string, error)
	PageSize    int64
	ClockTicks  int64 // Linux USER_HZ

	mu   sync.Mutex
	prev map[string]treeMark
	last []RunUsage
	at   time.Time
	key  string
}

type treeMark struct {
	pid int
	cpu time.Duration
	at  time.Time
}

// NewRunSampler returns a sampler for this machine.
func NewRunSampler() *RunSampler {
	s := NewSampler()
	return &RunSampler{GOOS: s.GOOS, ProcRoot: "/proc", MinInterval: 10 * time.Second, Now: time.Now, Exec: execOut,
		PageSize: int64(os.Getpagesize()), ClockTicks: 100}
}

// Sample returns usage for each root whose process exists. With no roots
// it does nothing (no process scan). A call within MinInterval of the
// previous one for the same roots returns the cached result.
func (s *RunSampler) Sample(ctx context.Context, roots []RunRoot) []RunUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if len(roots) == 0 {
		s.prev, s.last, s.key = nil, nil, ""
		return nil
	}
	key := rootsKey(roots)
	if key == s.key && !s.at.IsZero() && now.Sub(s.at) < s.MinInterval {
		return s.last
	}
	table, err := s.table(ctx)
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, p := range table {
		if p.PID != p.PPID {
			children[p.PPID] = append(children[p.PPID], p.PID)
		}
	}
	next := map[string]treeMark{}
	var out []RunUsage
	for _, r := range roots {
		if _, ok := table[r.PID]; !ok || r.PID <= 0 {
			continue
		}
		u := RunUsage{RunID: r.RunID, SessionID: r.SessionID, At: now}
		var cpu time.Duration
		seen := map[int]bool{}
		stack := []int{r.PID}
		for len(stack) > 0 {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[pid] {
				continue
			}
			seen[pid] = true
			p, ok := table[pid]
			if !ok {
				continue
			}
			u.Procs++
			u.RSSBytes += p.RSSBytes
			cpu += p.CPU
			if u.Top == nil || p.RSSBytes > u.Top.RSSBytes {
				cp := p
				u.Top = &cp
			}
			stack = append(stack, children[pid]...)
		}
		if pm, ok := s.prev[r.RunID]; ok && pm.pid == r.PID {
			if wall := now.Sub(pm.at); wall > 0 {
				d := max(cpu-pm.cpu, 0)
				u.CPUPct, u.HasCPU = round1(100*float64(d)/float64(wall)), true
			}
		}
		next[r.RunID] = treeMark{pid: r.PID, cpu: cpu, at: now}
		out = append(out, u)
	}
	s.prev, s.last, s.at, s.key = next, out, now, key
	return out
}

func rootsKey(roots []RunRoot) string {
	var b strings.Builder
	for _, r := range roots {
		b.WriteString(r.RunID)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(r.PID))
		b.WriteByte(';')
	}
	return b.String()
}

func (s *RunSampler) table(ctx context.Context) (map[int]Proc, error) {
	if s.GOOS == "linux" {
		return readProcTable(s.ProcRoot, s.PageSize, s.ClockTicks)
	}
	out, err := s.Exec(ctx, "ps", "-A", "-S", "-o", "pid=,ppid=,rss=,time=,comm=")
	if err != nil && out == "" {
		return nil, err
	}
	return parsePSTable(out), nil
}

// readProcTable scans /proc/<pid>/stat once.
func readProcTable(root string, pageSize, ticks int64) (map[int]Proc, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if ticks <= 0 {
		ticks = 100
	}
	m := make(map[int]Proc, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			continue // exited meanwhile
		}
		if p, err := parseProcPIDStat(b, pageSize, ticks); err == nil && p.PID == pid {
			m[pid] = p
		}
	}
	return m, nil
}

// parseProcPIDStat parses /proc/<pid>/stat. Fields after the last ')':
// state(0) ppid(1) ... utime(11) stime(12) cutime(13) cstime(14) ...
// rss(21, pages).
func parseProcPIDStat(b []byte, pageSize, ticks int64) (Proc, error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return Proc{}, errors.New("sysstats: malformed stat")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b[:open])))
	if err != nil {
		return Proc{}, err
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 22 {
		return Proc{}, errors.New("sysstats: short stat")
	}
	n := func(i int) int64 { v, _ := strconv.ParseInt(f[i], 10, 64); return v }
	ppid := int(n(1))
	jiffies := n(11) + n(12) + n(13) + n(14)
	return Proc{PID: pid, PPID: ppid, RSSBytes: n(21) * pageSize, CPU: time.Duration(jiffies) * time.Second / time.Duration(ticks),
		Name: string(b[open+1 : end])}, nil
}

// parsePSTable parses `ps -A -S -o pid=,ppid=,rss=,time=,comm=` (rss in
// KiB; comm last and may contain spaces).
func parsePSTable(out string) map[int]Proc {
	m := map[int]Proc{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.ParseInt(f[2], 10, 64)
		cpu, err4 := parsePSTime(f[3])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		name := filepath.Base(strings.Join(f[4:], " "))
		m[pid] = Proc{PID: pid, PPID: ppid, RSSBytes: rss * 1024, CPU: cpu, Name: name}
	}
	return m
}

// parsePSTime parses ps TIME: [[dd-]hh:]mm:ss[.cc].
func parsePSTime(s string) (time.Duration, error) {
	var days int
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, err
		}
		days, s = d, s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, errors.New("sysstats: bad ps time")
	}
	secs, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, err
	}
	mins, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return 0, err
	}
	hours := 0
	if len(parts) == 3 {
		if hours, err = strconv.Atoi(parts[0]); err != nil {
			return 0, err
		}
	}
	total := float64(days*86400+hours*3600+mins*60) + secs
	return time.Duration(total * float64(time.Second)), nil
}
