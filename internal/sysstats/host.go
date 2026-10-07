// Package sysstats samples machine-level resources (CPU, memory, swap,
// memory pressure, disk) and per-run process-tree usage for node status
// reports. It uses no cgo and no extra dependencies: Linux reads /proc and
// statfs; macOS uses sysctl (syscall.Sysctl), statfs and, for what has no
// sysctl, one bounded vm_stat/iostat/ps invocation per sample.
//
// Cost is bounded: one sample per call, at most one per MinInterval (cached
// in between), and no goroutine per metric.
package sysstats

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Host is one machine sample. Zero values mean "not available".
type Host struct {
	CPUCount    int
	CPUPct      float64 // machine busy %, 0-100 across all cores
	Load1       float64
	Load5       float64
	Load15      float64
	MemTotalMB  int64
	MemAvailMB  int64
	SwapTotalMB int64
	SwapUsedMB  int64
	Pressure    string  // "normal" | "warn" | "critical" | ""
	PSISome10   float64 // Linux PSI memory some avg10
	Disks       []Disk
	At          time.Time
}

// Disk is free/total space of one filesystem.
type Disk struct {
	Path    string
	Roles   []string
	TotalMB int64
	FreeMB  int64
}

// PathRole names a directory whose filesystem is reported.
type PathRole struct {
	Path string
	Role string // "home", "data"
}

// FSInfo is the result of a statfs.
type FSInfo struct {
	Total, Avail uint64 // bytes
	Dev          uint64 // device id of the path (dedupes filesystems)
}

// Sampler takes machine samples. The zero value is not usable; use
// NewSampler. Function fields are injectable for tests.
type Sampler struct {
	GOOS        string
	ProcRoot    string // Linux /proc (tests: a fixture dir)
	Paths       []PathRole
	MinInterval time.Duration
	Now         func() time.Time
	NumCPU      func() int
	// Exec runs a command and returns stdout (bounded by its context).
	Exec func(ctx context.Context, name string, args ...string) (string, error)
	// Sysctl returns the raw value of a macOS sysctl (as syscall.Sysctl
	// does: one trailing NUL may be trimmed).
	Sysctl func(name string) ([]byte, error)
	Statfs func(path string) (FSInfo, error)
	// AsyncCPU (macOS) takes the 1 s CPU sample in the background.
	AsyncCPU bool

	mu       sync.Mutex
	last     Host
	haveLast bool
	cpuTotal uint64 // Linux /proc/stat jiffies at the last sample
	cpuIdle  uint64
	// macOS asynchronous CPU sample (AsyncCPU).
	cpuBusy bool
	cpuPct  float64
	cpuAt   time.Time
}

// NewSampler returns a sampler for this machine reporting the given paths'
// filesystems.
func NewSampler(paths ...PathRole) *Sampler {
	return &Sampler{GOOS: runtime.GOOS, ProcRoot: "/proc", Paths: paths, MinInterval: 10 * time.Second,
		Now: time.Now, NumCPU: runtime.NumCPU, Exec: execOut, Sysctl: sysctlRaw, Statfs: statfs, AsyncCPU: true}
}

// DefaultPaths is the home directory plus the Pyre data directory.
func DefaultPaths(dataDir string) []PathRole {
	var out []PathRole
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		out = append(out, PathRole{Path: h, Role: "home"})
	}
	if dataDir != "" {
		if abs, err := filepath.Abs(dataDir); err == nil {
			dataDir = abs
		}
		out = append(out, PathRole{Path: dataDir, Role: "data"})
	}
	return out
}

func execOut(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/bin:/usr/bin:/usr/sbin:/sbin"}
	b, err := cmd.Output()
	return string(b), err
}

// Last returns the most recent sample without sampling (ok false if none).
func (s *Sampler) Last() (Host, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.haveLast
}

// Sample returns a fresh sample, or the cached one when the previous
// sample is younger than MinInterval.
func (s *Sampler) Sample(ctx context.Context) Host {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if s.haveLast && now.Sub(s.last.At) < s.MinInterval {
		return s.last
	}
	h := Host{CPUCount: s.NumCPU(), At: now}
	switch s.GOOS {
	case "linux":
		s.sampleLinux(&h)
	case "darwin":
		s.sampleDarwin(ctx, &h)
	}
	h.Disks = s.disks()
	s.last, s.haveLast = h, true
	return h
}

func (s *Sampler) read(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.ProcRoot, rel))
	return string(b), err
}

func (s *Sampler) sampleLinux(h *Host) {
	if b, err := s.read("loadavg"); err == nil {
		h.Load1, h.Load5, h.Load15 = parseLoadavg(b)
	}
	if b, err := s.read("stat"); err == nil {
		if total, idle, ok := parseProcStat(b); ok {
			if s.cpuTotal > 0 && total > s.cpuTotal {
				dt, di := total-s.cpuTotal, idle-s.cpuIdle
				if idle < s.cpuIdle {
					di = 0
				}
				h.CPUPct = round1(100 * float64(dt-min(di, dt)) / float64(dt))
			}
			s.cpuTotal, s.cpuIdle = total, idle
		}
	}
	if b, err := s.read("meminfo"); err == nil {
		m := parseMeminfo(b)
		h.MemTotalMB = m["MemTotal"] / 1024
		h.MemAvailMB = m["MemAvailable"] / 1024
		h.SwapTotalMB = m["SwapTotal"] / 1024
		if st, ok := m["SwapTotal"]; ok {
			h.SwapUsedMB = (st - m["SwapFree"]) / 1024
		}
	}
	if b, err := s.read("pressure/memory"); err == nil {
		if some, ok := parsePSISome10(b); ok {
			h.PSISome10 = some
			h.Pressure = PSILevel(some)
		}
	}
}

func (s *Sampler) sampleDarwin(ctx context.Context, h *Host) {
	if b, err := s.Sysctl("hw.memsize"); err == nil {
		h.MemTotalMB = int64(le64(b, 0) >> 20)
	}
	if b, err := s.Sysctl("vm.swapusage"); err == nil {
		total, _, used := parseSwapUsage(b)
		h.SwapTotalMB, h.SwapUsedMB = int64(total>>20), int64(used>>20)
	}
	if b, err := s.Sysctl("kern.memorystatus_vm_pressure_level"); err == nil {
		h.Pressure = DarwinPressure(int(le32(b, 0)))
	}
	if out, err := s.Exec(ctx, "vm_stat"); err == nil && h.MemTotalMB > 0 {
		if used, ok := parseVMStatUsed(out); ok {
			h.MemAvailMB = max(h.MemTotalMB-used>>20, 0)
		}
	}
	if b, err := s.Sysctl("vm.loadavg"); err == nil {
		h.Load1, h.Load5, h.Load15 = parseLoadavgRaw(b)
	}
	// macOS has no CPU tick counters outside Mach host_statistics (cgo).
	// iostat's second report is a 1 s sample of the kernel's CPU ticks
	// (the first covers time since boot). AsyncCPU runs it off the caller's
	// path (one short-lived process at a time) and reports the latest
	// completed sample, so a status report never waits for it.
	if !s.AsyncCPU {
		if out, err := s.Exec(ctx, "iostat", "-n", "0", "-c", "2", "-w", "1"); err == nil {
			if busy, _, _, _, ok := parseIostat(out); ok {
				h.CPUPct = busy
			}
		}
		return
	}
	if !s.cpuBusy && (s.cpuAt.IsZero() || h.At.Sub(s.cpuAt) >= s.MinInterval) {
		s.cpuBusy = true
		go s.darwinCPU()
	}
	if !s.cpuAt.IsZero() && h.At.Sub(s.cpuAt) < 3*s.MinInterval+time.Minute {
		h.CPUPct = s.cpuPct
	}
}

func (s *Sampler) darwinCPU() {
	out, err := s.Exec(context.Background(), "iostat", "-n", "0", "-c", "2", "-w", "1")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cpuBusy = false
	if err != nil {
		return
	}
	if busy, _, _, _, ok := parseIostat(out); ok {
		s.cpuPct, s.cpuAt = busy, s.Now()
	}
}

func (s *Sampler) disks() []Disk {
	var out []Disk
	byDev := map[uint64]int{}
	for _, p := range s.Paths {
		fi, err := s.Statfs(p.Path)
		if err != nil || fi.Total == 0 {
			continue
		}
		if i, ok := byDev[fi.Dev]; ok {
			out[i].Roles = append(out[i].Roles, p.Role)
			continue
		}
		byDev[fi.Dev] = len(out)
		out = append(out, Disk{Path: p.Path, Roles: []string{p.Role}, TotalMB: int64(fi.Total >> 20), FreeMB: int64(fi.Avail >> 20)})
	}
	return out
}

// ---- parsers (platform independent; unit-tested with fixtures) ----

func parseLoadavg(s string) (l1, l5, l15 float64) {
	f := strings.Fields(s)
	if len(f) >= 3 {
		l1, _ = strconv.ParseFloat(f[0], 64)
		l5, _ = strconv.ParseFloat(f[1], 64)
		l15, _ = strconv.ParseFloat(f[2], 64)
	}
	return
}

// parseProcStat returns total and idle (idle+iowait) jiffies from the
// aggregate "cpu" line. guest/guest_nice are already part of user/nice.
func parseProcStat(s string) (total, idle uint64, ok bool) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		for i := 1; i < len(f) && i <= 8; i++ {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return 0, 0, false
			}
			total += v
			if i == 4 || i == 5 { // idle, iowait
				idle += v
			}
		}
		return total, idle, true
	}
	return 0, 0, false
}

// parseMeminfo returns /proc/meminfo values in kB.
func parseMeminfo(s string) map[string]int64 {
	m := map[string]int64{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
			m[k] = n
		}
	}
	return m
}

// parsePSISome10 reads "some avg10=" from /proc/pressure/memory.
func parsePSISome10(s string) (float64, bool) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "some" {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg10="); ok {
				x, err := strconv.ParseFloat(v, 64)
				return x, err == nil
			}
		}
	}
	return 0, false
}

// PSILevel maps Linux PSI memory "some avg10" to a pressure level.
func PSILevel(some10 float64) string {
	switch {
	case some10 >= 40:
		return "critical"
	case some10 >= 10:
		return "warn"
	default:
		return "normal"
	}
}

// DarwinPressure maps kern.memorystatus_vm_pressure_level (1 normal,
// 2 warn, 4 critical).
func DarwinPressure(level int) string {
	switch level {
	case 1:
		return "normal"
	case 2:
		return "warn"
	case 4:
		return "critical"
	}
	return ""
}

// parseVMStatUsed returns Activity Monitor's "Memory Used" in bytes: app
// memory (anonymous minus purgeable) + wired + compressor-occupied pages.
// Older vm_stat without "Anonymous pages" falls back to everything that is
// not free, inactive, speculative or purgeable.
func parseVMStatUsed(s string) (int64, bool) {
	page := int64(4096)
	vals := map[string]int64{}
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "page size of "); i >= 0 {
			if f := strings.Fields(line[i+len("page size of "):]); len(f) > 0 {
				if n, err := strconv.ParseInt(f[0], 10, 64); err == nil && n > 0 {
					page = n
				}
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
		if err != nil {
			continue
		}
		vals[strings.Trim(strings.TrimSpace(k), `"`)] = n
	}
	wired, okW := vals["Pages wired down"]
	if !okW {
		return 0, false
	}
	if anon, ok := vals["Anonymous pages"]; ok {
		app := max(anon-vals["Pages purgeable"], 0)
		return (app + wired + vals["Pages occupied by compressor"]) * page, true
	}
	used := vals["Pages active"] + wired + vals["Pages occupied by compressor"]
	return used * page, true
}

// parseIostat reads the last report line of `iostat -n 0 -c 2 -w 1`:
// "us sy id 1m 5m 15m".
func parseIostat(s string) (busy, l1, l5, l15 float64, ok bool) {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		f := strings.Fields(lines[i])
		if len(f) < 6 {
			continue
		}
		var v [6]float64
		bad := false
		for j := 0; j < 6; j++ {
			x, err := strconv.ParseFloat(f[len(f)-6+j], 64)
			if err != nil {
				bad = true
				break
			}
			v[j] = x
		}
		if bad {
			continue
		}
		if i < 2 { // header lines or the since-boot report only
			return 0, 0, 0, 0, false
		}
		return max(0, 100-v[2]), v[3], v[4], v[5], true
	}
	return 0, 0, 0, 0, false
}

// parseSwapUsage decodes struct xsw_usage {u64 total, avail, used; ...}.
func parseSwapUsage(b []byte) (total, avail, used uint64) {
	return le64(b, 0), le64(b, 8), le64(b, 16)
}

// parseLoadavgRaw decodes struct loadavg {u32 ldavg[3]; long fscale}.
func parseLoadavgRaw(b []byte) (l1, l5, l15 float64) {
	scale := float64(le64(b, 16))
	if scale == 0 {
		return
	}
	return round2(float64(le32(b, 0)) / scale), round2(float64(le32(b, 4)) / scale), round2(float64(le32(b, 8)) / scale)
}

// le64/le32 read little-endian integers, treating bytes past the end as
// zero (syscall.Sysctl trims one trailing NUL).
func le64(b []byte, off int) uint64 {
	var buf [8]byte
	if off < len(b) {
		copy(buf[:], b[off:])
	}
	return binary.LittleEndian.Uint64(buf[:])
}

func le32(b []byte, off int) uint32 {
	var buf [4]byte
	if off < len(b) {
		copy(buf[:], b[off:])
	}
	return binary.LittleEndian.Uint32(buf[:])
}

func round1(x float64) float64 { return float64(int64(x*10+0.5)) / 10 }
func round2(x float64) float64 { return float64(int64(x*100+0.5)) / 100 }
