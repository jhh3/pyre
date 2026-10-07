package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/sysstats"
)

// Machine and per-session resource stats (W-stats): the Machines view's
// bars and the Sessions list's optional memory/CPU column (u).

// usageColW is the width of the Sessions list's usage column ("1.2G 35%").
const usageColW = 9

// usageAction toggles the Sessions list's usage column.
func (m *Model) usageAction(id string) (tea.Cmd, bool) {
	if id != "sessions.usage" {
		return nil, false
	}
	m.showUsage = !m.showUsage
	return nil, true
}

// runUsage is the selected run's latest process-tree sample: from the hub's
// session view, else from its node's status report.
func (m *Model) runUsage(s contracts.SessionView) (contracts.RunUsage, bool) {
	if s.Run == nil || s.Run.State.Terminal() {
		return contracts.RunUsage{}, false
	}
	if s.Run.Usage != nil {
		return *s.Run.Usage, true
	}
	if n := m.nodeOf(s); n != nil && n.Connection.State == contracts.ConnConnected {
		return sysstats.FindRun(n.Resources, s.Run.RunID)
	}
	return contracts.RunUsage{}, false
}

// usageCell is the list column: "1.2G 35%", or blank when not sampled.
func (m *Model) usageCell(s contracts.SessionView) string {
	u, ok := m.runUsage(s)
	if !ok {
		return strings.Repeat(" ", usageColW)
	}
	return m.th.dim.Render(padLeft(sysstats.Compact(u), usageColW))
}

// machineStatsCell is the Machines list's compact stats: "cpu 35% mem 75%
// disk 5%"; warnings are amber.
func (m *Model) machineStatsCell(r contracts.NodeResources) string {
	if !hasMachineStats(r) {
		return ""
	}
	var parts []string
	if r.CPUPct > 0 {
		parts = append(parts, m.th.dim.Render(fmt.Sprintf("cpu %.0f%%", r.CPUPct)))
	}
	if r.MemTotalMB > 0 {
		s := fmt.Sprintf("mem %.0f%%", memUsedPct(r))
		if sysstats.PressureHigh(r) || sysstats.SwapHeavy(r) {
			parts = append(parts, m.th.tone(toneWarn, s+"!"))
		} else {
			parts = append(parts, m.th.dim.Render(s))
		}
	}
	if d, ok := lowestDisk(r.Disks); ok {
		s := fmt.Sprintf("disk %.0f%% free", 100*float64(d.FreeMB)/float64(d.TotalMB))
		if sysstats.DiskLow(d) {
			parts = append(parts, m.th.tone(toneWarn, s+"!"))
		} else {
			parts = append(parts, m.th.dim.Render(s))
		}
	}
	return strings.Join(parts, " ")
}

// hasMachineStats reports whether the node sent v1.8 machine stats (an
// older pyred sends only load and memory, already on the Resources line).
func hasMachineStats(r contracts.NodeResources) bool {
	return r.CPUPct > 0 || r.SwapTotalMB > 0 || r.MemPressure != "" || len(r.Disks) > 0
}

func memUsedPct(r contracts.NodeResources) float64 {
	if r.MemTotalMB <= 0 {
		return 0
	}
	return 100 * float64(r.MemTotalMB-r.MemAvailMB) / float64(r.MemTotalMB)
}

func lowestDisk(ds []contracts.DiskUsage) (contracts.DiskUsage, bool) {
	var best contracts.DiskUsage
	ok := false
	for _, d := range ds {
		if d.TotalMB <= 0 {
			continue
		}
		if !ok || float64(d.FreeMB)/float64(d.TotalMB) < float64(best.FreeMB)/float64(best.TotalMB) {
			best, ok = d, true
		}
	}
	return best, ok
}

// statBar draws a used-fraction bar, amber when warn.
func (m *Model) statBar(pct float64, n int, warn bool) string {
	pct = max(0, min(100, pct))
	fill := int(pct/100*float64(n) + 0.5)
	on, off := "━", "─"
	if m.th.plain || m.th.mono {
		on, off = "█", "░"
	}
	tn := toneGood
	if warn {
		tn = toneWarn
	}
	return m.th.tone(tn, strings.Repeat(on, fill)) + m.th.faint.Render(strings.Repeat(off, n-fill))
}

// machineStatsLines are the Machines detail's bars: CPU, memory (with
// pressure), swap, and each reported filesystem; then the live runs'
// usage. Nothing when the node reported no machine stats (older pyred).
func (m *Model) machineStatsLines(r contracts.NodeResources, w, lw int) []string {
	if !hasMachineStats(r) {
		return nil
	}
	barW := max(8, min(24, w-lw-30))
	row := func(label string, pct float64, warn bool, text string) string {
		t := m.th.dim.Render(text)
		if warn {
			t = m.th.tone(toneWarn, text)
		}
		return trunc(m.kvl(label, m.statBar(pct, barW, warn)+fmt.Sprintf(" %3.0f%%  ", pct)+t, lw), w)
	}
	var out []string
	if r.CPUPct > 0 {
		out = append(out, row("CPU", r.CPUPct, false, fmt.Sprintf("load %.1f %.1f %.1f · %d cores", r.Load1, r.Load5, r.Load15, r.CPUCount)))
	}
	if r.MemTotalMB > 0 {
		text := fmt.Sprintf("%s of %s", sysstats.HumanMB(float64(r.MemTotalMB-r.MemAvailMB)), sysstats.HumanMB(float64(r.MemTotalMB)))
		if r.MemPressure != "" {
			text += " · pressure " + r.MemPressure
			if r.MemPressureSome10 > 0 {
				text += fmt.Sprintf(" (psi %.1f%%)", r.MemPressureSome10)
			}
		}
		out = append(out, row("Memory", memUsedPct(r), sysstats.PressureHigh(r), text))
	}
	if r.SwapTotalMB > 0 {
		out = append(out, row("Swap", 100*float64(r.SwapUsedMB)/float64(r.SwapTotalMB), sysstats.SwapHeavy(r),
			fmt.Sprintf("%s of %s", sysstats.HumanMB(float64(r.SwapUsedMB)), sysstats.HumanMB(float64(r.SwapTotalMB)))))
	}
	for _, d := range r.Disks {
		if d.TotalMB <= 0 {
			continue
		}
		used := 100 * float64(d.TotalMB-d.FreeMB) / float64(d.TotalMB)
		out = append(out, row("Disk", used, sysstats.DiskLow(d),
			fmt.Sprintf("%s free of %s · %s", sysstats.HumanMB(float64(d.FreeMB)), sysstats.HumanMB(float64(d.TotalMB)), strings.Join(d.Roles, "+"))))
	}
	if ws := sysstats.Warnings(r); len(ws) > 0 {
		out = append(out, trunc(m.kvl("", m.th.tone(toneWarn, "! "+strings.Join(ws, " · ")), lw), w))
	}
	return out
}

// runUsageLines lists a machine's live runs by memory, titled by session.
func (m *Model) runUsageLines(r contracts.NodeResources, w int) []string {
	if len(r.Runs) == 0 {
		return nil
	}
	runs := append([]contracts.RunUsage(nil), r.Runs...)
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && runs[j].RSSMB > runs[j-1].RSSMB; j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
	out := []string{"", m.sectionTitle("Sessions by memory", w)}
	for _, u := range runs {
		name := u.SessionID
		if s, ok := m.session(u.SessionID); ok && s.Session.Title != "" {
			name = s.Session.Title
		}
		top := ""
		if u.Top != nil {
			top = fmt.Sprintf(" · top %s %s", u.Top.Name, sysstats.HumanMB(u.Top.RSSMB))
		}
		out = append(out, trunc(padLeft(sysstats.Compact(u), usageColW)+"  "+pad(trunc(name, 28), 28)+m.th.dim.Render(fmt.Sprintf(" %d procs%s", u.Procs, top)), w))
	}
	return out
}
