package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProcSample is one resource observation of a process (or the sum over a
// process tree). CPU is cumulative user+system time; compare two samples
// with CPUPercent to get utilisation.
type ProcSample struct {
	PID      int           `json:"pid"`
	Procs    int           `json:"procs"` // processes included (1 unless a tree)
	RSSBytes int64         `json:"rss_bytes"`
	CPU      time.Duration `json:"cpu"`
	At       time.Time     `json:"at"`
}

// RSSMB returns RSS in MiB.
func (s ProcSample) RSSMB() float64 { return float64(s.RSSBytes) / (1 << 20) }

// CPUPercent returns the CPU utilisation between two samples of the same
// process(es), as a percentage of one core (200 = two cores busy).
func CPUPercent(a, b ProcSample) float64 {
	wall := b.At.Sub(a.At)
	if wall <= 0 {
		return 0
	}
	return 100 * float64(b.CPU-a.CPU) / float64(wall)
}

// SampleSelf samples the current process: CPU from getrusage (exact), RSS
// from /proc on Linux or ps elsewhere.
func SampleSelf() (ProcSample, error) {
	now := time.Now()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return ProcSample{}, err
	}
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	s := ProcSample{PID: os.Getpid(), Procs: 1, CPU: cpu, At: now}
	if rss, err := linuxRSS(os.Getpid()); err == nil {
		s.RSSBytes = rss
		return s, nil
	}
	p, err := SampleProcess(os.Getpid())
	if err != nil {
		return s, err
	}
	s.RSSBytes = p.RSSBytes
	return s, nil
}

// SampleProcess samples one process by PID.
func SampleProcess(pid int) (ProcSample, error) {
	if runtime.GOOS == "linux" {
		if s, err := linuxSample(pid); err == nil {
			return s, nil
		}
	}
	rows, err := psTable(context.Background(), pid)
	if err != nil {
		return ProcSample{}, err
	}
	for _, r := range rows {
		if r.pid == pid {
			return ProcSample{PID: pid, Procs: 1, RSSBytes: r.rss, CPU: r.cpu, At: time.Now()}, nil
		}
	}
	return ProcSample{}, fmt.Errorf("diagnostics: process %d not found", pid)
}

// SampleTree sums a process and all of its descendants (e.g. a daemon plus
// helper processes, but callers measuring Pyre overhead should exclude agent
// runtimes by sampling the daemon PID alone).
func SampleTree(pid int) (ProcSample, error) {
	rows, err := psTable(context.Background(), 0)
	if err != nil {
		return ProcSample{}, err
	}
	children := map[int][]psRow{}
	var root *psRow
	for i := range rows {
		children[rows[i].ppid] = append(children[rows[i].ppid], rows[i])
		if rows[i].pid == pid {
			root = &rows[i]
		}
	}
	if root == nil {
		return ProcSample{}, fmt.Errorf("diagnostics: process %d not found", pid)
	}
	out := ProcSample{PID: pid, At: time.Now()}
	queue := []psRow{*root}
	seen := map[int]bool{}
	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]
		if seen[r.pid] {
			continue
		}
		seen[r.pid] = true
		out.Procs++
		out.RSSBytes += r.rss
		out.CPU += r.cpu
		queue = append(queue, children[r.pid]...)
	}
	return out, nil
}

type psRow struct {
	pid, ppid int
	rss       int64
	cpu       time.Duration
}

// psTable runs ps (portable across macOS and Linux procps). pid 0 = all.
func psTable(ctx context.Context, pid int) ([]psRow, error) {
	args := []string{"-o", "pid=,ppid=,rss=,time="}
	if pid > 0 {
		args = append(args, "-p", strconv.Itoa(pid))
	} else {
		args = append([]string{"-A"}, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/bin:/usr/bin"}
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("diagnostics: ps: %w", err)
	}
	var rows []psRow
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		p, err1 := strconv.Atoi(f[0])
		pp, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.ParseInt(f[2], 10, 64)
		cpu, err4 := parsePSTime(f[3])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		rows = append(rows, psRow{pid: p, ppid: pp, rss: rss * 1024, cpu: cpu})
	}
	return rows, nil
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
		return 0, fmt.Errorf("bad ps time %q", s)
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

func linuxRSS(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, errors.New("diagnostics: short statm")
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * int64(os.Getpagesize()), nil
}

// linuxClockTicks is USER_HZ, 100 on every mainstream Linux configuration.
const linuxClockTicks = 100

func linuxSample(pid int) (ProcSample, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ProcSample{}, err
	}
	// comm may contain spaces; fields resume after the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return ProcSample{}, errors.New("diagnostics: malformed stat")
	}
	f := strings.Fields(string(b[i+1:]))
	// after ')': state(0) ppid(1) ... utime(11) stime(12)
	if len(f) < 13 {
		return ProcSample{}, errors.New("diagnostics: short stat")
	}
	ut, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	rss, err := linuxRSS(pid)
	if err != nil {
		return ProcSample{}, err
	}
	cpu := time.Duration(ut+st) * time.Second / linuxClockTicks
	return ProcSample{PID: pid, Procs: 1, RSSBytes: rss, CPU: cpu, At: time.Now()}, nil
}

// HostResources is a best-effort machine summary (zero fields when a value
// cannot be read on this platform).
type HostResources struct {
	CPUCount   int
	Load1      float64
	MemTotalMB int64
	MemAvailMB int64
}

// SampleHost reads load and memory from /proc (Linux) or sysctl/vm_stat
// (macOS). It never fails; unreadable values stay zero.
func SampleHost() HostResources {
	h := HostResources{CPUCount: runtime.NumCPU()}
	switch runtime.GOOS {
	case "linux":
		if b, err := os.ReadFile("/proc/loadavg"); err == nil {
			if f := strings.Fields(string(b)); len(f) > 0 {
				h.Load1, _ = strconv.ParseFloat(f[0], 64)
			}
		}
		if b, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				if len(f) < 2 {
					continue
				}
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				switch f[0] {
				case "MemTotal:":
					h.MemTotalMB = kb / 1024
				case "MemAvailable:":
					h.MemAvailMB = kb / 1024
				}
			}
		}
	case "darwin":
		if out, err := run("sysctl", "-n", "vm.loadavg"); err == nil {
			// "{ 1.23 1.10 0.98 }"
			f := strings.Fields(strings.Trim(strings.TrimSpace(out), "{}"))
			if len(f) > 0 {
				h.Load1, _ = strconv.ParseFloat(f[0], 64)
			}
		}
		if out, err := run("sysctl", "-n", "hw.memsize"); err == nil {
			n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
			h.MemTotalMB = n >> 20
		}
		if out, err := run("vm_stat"); err == nil {
			h.MemAvailMB = parseVMStat(out)
		}
	}
	return h
}

func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/bin:/usr/bin:/usr/sbin:/sbin"}
	out, err := cmd.Output()
	return string(out), err
}

// parseVMStat estimates available memory (free + inactive + speculative
// pages) from macOS vm_stat output.
func parseVMStat(out string) int64 {
	page := int64(4096)
	var pages int64
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "page size of") {
			f := strings.Fields(line)
			for i, w := range f {
				if w == "of" && i+1 < len(f) {
					if n, err := strconv.ParseInt(f[i+1], 10, 64); err == nil {
						page = n
					}
				}
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages free", "Pages inactive", "Pages speculative":
			n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
			pages += n
		}
	}
	return pages * page >> 20
}
