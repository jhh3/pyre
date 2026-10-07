package sysstats

import (
	"fmt"
	"math"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Apply copies a machine sample into a status report's resources.
func (h Host) Apply(r *contracts.NodeResources) {
	if h.At.IsZero() {
		return
	}
	if h.CPUCount > 0 {
		r.CPUCount = h.CPUCount
	}
	r.CPUPct = h.CPUPct
	r.Load1, r.Load5, r.Load15 = h.Load1, h.Load5, h.Load15
	if h.MemTotalMB > 0 {
		r.MemTotalMB, r.MemAvailMB = h.MemTotalMB, h.MemAvailMB
	}
	r.SwapTotalMB, r.SwapUsedMB = h.SwapTotalMB, h.SwapUsedMB
	r.MemPressure, r.MemPressureSome10 = h.Pressure, h.PSISome10
	r.Disks = nil
	for _, d := range h.Disks {
		r.Disks = append(r.Disks, contracts.DiskUsage{Path: d.Path, Roles: append([]string(nil), d.Roles...), TotalMB: d.TotalMB, FreeMB: d.FreeMB})
	}
}

// Contract converts run samples to their wire form.
func Contract(us []RunUsage) []contracts.RunUsage {
	if len(us) == 0 {
		return nil
	}
	out := make([]contracts.RunUsage, 0, len(us))
	for _, u := range us {
		c := contracts.RunUsage{RunID: u.RunID, SessionID: u.SessionID, Procs: u.Procs, RSSMB: mb(u.RSSBytes), ObservedAt: u.At}
		if u.HasCPU {
			c.CPUPct = u.CPUPct
		}
		if u.Top != nil {
			c.Top = &contracts.ProcUsage{PID: u.Top.PID, Name: u.Top.Name, RSSMB: mb(u.Top.RSSBytes)}
		}
		out = append(out, c)
	}
	return out
}

func mb(b int64) float64 { return math.Round(float64(b)/(1<<20)*10) / 10 }

// FindRun returns the usage of runID in a node's resources.
func FindRun(r contracts.NodeResources, runID string) (contracts.RunUsage, bool) {
	for _, u := range r.Runs {
		if u.RunID == runID {
			return u, true
		}
	}
	return contracts.RunUsage{}, false
}

// Thresholds for warnings.
const (
	DiskWarnFreeFrac = 0.10 // free < 10% of the filesystem
	SwapWarnFrac     = 0.50 // swap used >= 50% of swap total ...
	SwapWarnMinMB    = 1024 // ... and at least 1 GiB
)

// DiskLow reports whether a filesystem has less than 10% free.
func DiskLow(d contracts.DiskUsage) bool {
	return d.TotalMB > 0 && float64(d.FreeMB) < DiskWarnFreeFrac*float64(d.TotalMB)
}

// SwapHeavy reports heavy swap use.
func SwapHeavy(r contracts.NodeResources) bool {
	return r.SwapTotalMB > 0 && r.SwapUsedMB >= SwapWarnMinMB && float64(r.SwapUsedMB) >= SwapWarnFrac*float64(r.SwapTotalMB)
}

// PressureHigh reports memory pressure at warn or critical.
func PressureHigh(r contracts.NodeResources) bool {
	return r.MemPressure == contracts.MemPressureWarn || r.MemPressure == contracts.MemPressureCritical
}

// Warnings lists short human warnings for a node's resources.
func Warnings(r contracts.NodeResources) []string {
	var w []string
	for _, d := range r.Disks {
		if DiskLow(d) {
			w = append(w, fmt.Sprintf("disk %s %s free (%.0f%%)", strings.Join(d.Roles, "+"), HumanMB(float64(d.FreeMB)), 100*float64(d.FreeMB)/float64(d.TotalMB)))
		}
	}
	if PressureHigh(r) {
		w = append(w, "memory pressure "+r.MemPressure)
	}
	if SwapHeavy(r) {
		w = append(w, fmt.Sprintf("swap %s of %s used", HumanMB(float64(r.SwapUsedMB)), HumanMB(float64(r.SwapTotalMB))))
	}
	return w
}

// HumanMB formats a MiB amount compactly: "820M", "1.2G", "15G".
func HumanMB(v float64) string {
	switch {
	case v >= 10*1024:
		return fmt.Sprintf("%.0fG", v/1024)
	case v >= 1024:
		return fmt.Sprintf("%.1fG", v/1024)
	default:
		return fmt.Sprintf("%.0fM", v)
	}
}

// Compact formats a run's usage as "1.2G 35%".
func Compact(u contracts.RunUsage) string {
	return fmt.Sprintf("%s %.0f%%", HumanMB(u.RSSMB), u.CPUPct)
}

// Detail formats a run's usage for an info line:
// "1.2G rss · 35% cpu · 14 procs · top node 900M".
func Detail(u contracts.RunUsage) string {
	parts := []string{HumanMB(u.RSSMB) + " rss"}
	parts = append(parts, fmt.Sprintf("%.0f%% cpu", u.CPUPct))
	parts = append(parts, fmt.Sprintf("%d procs", u.Procs))
	if u.Top != nil {
		parts = append(parts, fmt.Sprintf("top %s %s", u.Top.Name, HumanMB(u.Top.RSSMB)))
	}
	return strings.Join(parts, " · ")
}
