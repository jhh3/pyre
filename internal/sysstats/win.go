package sysstats

import (
	"encoding/binary"
	"time"
)

// WinRaw is one raw Windows machine reading (sys_windows.go fills it from
// GlobalMemoryStatusEx, GetSystemTimes and the pagefile list). It is kept
// OS-neutral so the arithmetic is unit-tested on every platform.
type WinRaw struct {
	MemTotal, MemAvail uint64 // physical memory, bytes
	// System times (100 ns units). Kernel time includes idle time.
	Idle, Kernel, User uint64
	// Pagefiles (swap): total and in-use bytes; HavePagefile false when the
	// pagefile list could not be read (swap then stays "not available").
	PageTotal, PageInUse uint64
	HavePagefile         bool
}

// sampleWindows applies a raw reading. Windows has no load average and no
// memory-pressure level comparable to macOS/Linux PSI, so Load* and
// Pressure stay zero/"" (contract: not sampled on this platform).
func (s *Sampler) sampleWindows(h *Host) {
	if s.Win == nil {
		return
	}
	r, err := s.Win()
	if err != nil {
		return
	}
	h.MemTotalMB = int64(r.MemTotal >> 20)
	h.MemAvailMB = int64(r.MemAvail >> 20)
	if r.HavePagefile {
		h.SwapTotalMB = int64(r.PageTotal >> 20)
		h.SwapUsedMB = int64(min(r.PageInUse, r.PageTotal) >> 20)
	}
	total := r.Kernel + r.User
	if total > 0 {
		if s.cpuTotal > 0 && total > s.cpuTotal {
			dt := total - s.cpuTotal
			di := uint64(0)
			if r.Idle >= s.cpuIdle {
				di = r.Idle - s.cpuIdle
			}
			h.CPUPct = round1(100 * float64(dt-min(di, dt)) / float64(dt))
		}
		s.cpuTotal, s.cpuIdle = total, r.Idle
	}
}

// parsePageFiles walks a SYSTEM_PAGEFILE_INFORMATION list as returned by
// NtQuerySystemInformation(SystemPageFileInformation): each entry starts
// with ULONG NextEntryOffset, TotalSize, TotalInUse, PeakUsage (sizes in
// pages). It returns the summed total and in-use page counts.
func parsePageFiles(buf []byte) (totalPages, inUsePages uint64) {
	off := 0
	for n := 0; n < 64 && off+16 <= len(buf); n++ {
		next := binary.LittleEndian.Uint32(buf[off:])
		totalPages += uint64(binary.LittleEndian.Uint32(buf[off+4:]))
		inUsePages += uint64(binary.LittleEndian.Uint32(buf[off+8:]))
		if next == 0 {
			break
		}
		off += int(next)
	}
	return totalPages, inUsePages
}

// filetimeDuration converts a Windows 100 ns tick count to a duration.
func filetimeDuration(ticks uint64) time.Duration { return time.Duration(ticks) * 100 }

// LifetimeCPUPct is cumulative CPU over the process lifetime as a percent
// of one core (the way procps `ps -o pcpu` reports it), used where only one
// sample is available.
func LifetimeCPUPct(p Proc, now time.Time) float64 {
	if p.Start.IsZero() {
		return 0
	}
	wall := now.Sub(p.Start)
	if wall <= 0 {
		return 0
	}
	return round1(100 * float64(p.CPU) / float64(wall))
}

// ParentLink reports whether child may be counted as a descendant of
// parent. Windows recycles PIDs and keeps a stale parent PID after the
// parent exits, so a child that started before its supposed parent is not
// its child. Without start times (Unix tables) every link is kept.
func ParentLink(parent, child Proc) bool {
	if parent.Start.IsZero() || child.Start.IsZero() {
		return true
	}
	return !child.Start.Before(parent.Start)
}
