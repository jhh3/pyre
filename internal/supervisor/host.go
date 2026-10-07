package supervisor

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// HostStats is a replaceable host resource observation.
type HostStats struct {
	CPUCount   int
	Load1      float64
	MemTotalMB int64
	MemAvailMB int64
}

// Host samples host-wide load and memory. Failures leave fields zero (an
// unknown value, never an assertion of idleness).
func Host(ctx context.Context) HostStats {
	h := HostStats{CPUCount: runtime.NumCPU()}
	switch runtime.GOOS {
	case "linux":
		if b, err := os.ReadFile("/proc/loadavg"); err == nil {
			if f := strings.Fields(string(b)); len(f) > 0 {
				h.Load1, _ = strconv.ParseFloat(f[0], 64)
			}
		}
		if f, err := os.Open("/proc/meminfo"); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				k, v, ok := strings.Cut(sc.Text(), ":")
				if !ok {
					continue
				}
				fs := strings.Fields(v)
				if len(fs) == 0 {
					continue
				}
				kb, _ := strconv.ParseInt(fs[0], 10, 64)
				switch k {
				case "MemTotal":
					h.MemTotalMB = kb / 1024
				case "MemAvailable":
					h.MemAvailMB = kb / 1024
				}
			}
			f.Close()
		}
	case "darwin":
		if s, err := cmdOut(ctx, "sysctl", "-n", "vm.loadavg"); err == nil {
			f := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
			if len(f) > 0 {
				h.Load1, _ = strconv.ParseFloat(f[0], 64)
			}
		}
		if s, err := cmdOut(ctx, "sysctl", "-n", "hw.memsize"); err == nil {
			b, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			h.MemTotalMB = b >> 20
		}
		if s, err := cmdOut(ctx, "vm_stat"); err == nil {
			h.MemAvailMB = parseVMStat(s) >> 20
		}
	}
	return h
}

var pageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVMStat returns available bytes (free + inactive + speculative +
// purgeable pages) from macOS vm_stat output.
func parseVMStat(s string) int64 {
	page := int64(4096)
	if m := pageSizeRe.FindStringSubmatch(s); m != nil {
		page, _ = strconv.ParseInt(m[1], 10, 64)
	}
	var pages int64
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
			pages += n
		}
	}
	return pages * page
}

func cmdOut(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, args...).Output()
	return string(b), err
}
