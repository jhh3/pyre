package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// The Machine field of the new-session panel: the first choice, since the
// Where list, the folder picker and the agents all depend on it. Every node
// is one row with its load and agents; a disconnected one is greyed out and
// cannot be chosen.

// nsDefaultMachine is where the panel starts: this machine when it is a
// connected node, else the machine last used from the panel, else the
// least-loaded connected one.
func (m *Model) nsDefaultMachine(connected map[string]bool) string {
	if id := m.opts.ClientNodeID; id != "" && connected[id] {
		return id
	}
	if id := m.nsMem.Machine; id != "" && connected[id] {
		return id
	}
	var last nsPref
	for _, p := range m.nsMem.Repos {
		if connected[p.Machine] && p.At.After(last.At) {
			last = p
		}
	}
	if last.Machine != "" {
		return last.Machine
	}
	best, bestLoad := "", 0.0
	for _, n := range m.sortedNodes() {
		if !connected[n.NodeID] {
			continue
		}
		if l := nodeLoad(n.Resources); best == "" || l < bestLoad {
			best, bestLoad = n.NodeID, l
		}
	}
	if best != "" {
		return best
	}
	if ns := m.sortedNodes(); len(ns) > 0 {
		return ns[0].NodeID
	}
	return ""
}

// nodeLoad orders machines for the default: CPU busy (or load per core),
// half the memory in use, and the share of launch capacity taken (10 per
// run when unlimited).
func nodeLoad(r contracts.NodeResources) float64 {
	cpu := r.CPUPct
	if cpu == 0 && r.CPUCount > 0 {
		cpu = 100 * r.Load1 / float64(r.CPUCount)
	}
	runs := 10 * float64(r.ActiveRuns)
	if r.MaxRuns > 0 {
		runs = 100 * float64(r.ActiveRuns) / float64(r.MaxRuns)
	}
	return cpu + memUsedPct(r)/2 + runs
}

// nodeLoadText is a machine's load as the Machines list shows it: "1/16
// runs · cpu 7% · mem 40%" (load per core from an older pyred).
func nodeLoadText(r contracts.NodeResources) string {
	runs := strconv.Itoa(r.ActiveRuns)
	if r.MaxRuns > 0 {
		runs += "/" + strconv.Itoa(r.MaxRuns)
	}
	parts := []string{runs + " runs"}
	switch {
	case r.CPUPct > 0:
		parts = append(parts, fmt.Sprintf("cpu %.0f%%", r.CPUPct))
	case r.CPUCount > 0:
		parts = append(parts, fmt.Sprintf("load %.1f/%d", r.Load1, r.CPUCount))
	}
	if r.MemTotalMB > 0 {
		parts = append(parts, fmt.Sprintf("mem %.0f%%", memUsedPct(r)))
	}
	return strings.Join(parts, " · ")
}

// nsHarnessOrder is the Agent field's order.
var nsHarnessOrder = []contracts.Harness{contracts.HarnessClaude, contracts.HarnessCodex, contracts.HarnessAgy, contracts.HarnessShell}

// nodeAgents are the agents machine nodeID can run, in the Agent field's
// order. A node that reports no harnesses (unknown) offers Claude, Codex
// and Shell, as before machines reported them.
func (m *Model) nodeAgents(nodeID string) []contracts.Harness {
	n := m.node(nodeID)
	if n == nil || len(n.Harnesses) == 0 {
		return []contracts.Harness{contracts.HarnessClaude, contracts.HarnessCodex, contracts.HarnessShell}
	}
	var out []contracts.Harness
	for _, h := range nsHarnessOrder {
		if hi, ok := m.harnessOn(nodeID, h); ok && hi.Available {
			out = append(out, h)
		}
	}
	return out
}

func hasAgent(hs []contracts.Harness, h contracts.Harness) bool {
	for _, x := range hs {
		if x == h {
			return true
		}
	}
	return false
}

// harnessOn is what machine nodeID reports about harness h.
func (m *Model) harnessOn(nodeID string, h contracts.Harness) (contracts.HarnessInfo, bool) {
	n := m.node(nodeID)
	if n == nil {
		return contracts.HarnessInfo{}, false
	}
	for _, hi := range n.Harnesses {
		if hi.Harness == h {
			return hi, true
		}
	}
	return contracts.HarnessInfo{}, false
}

const nsMachineRows = 6

// machineLines renders the Machine field: one row per node (the chosen
// one marked ●), its load and agents; offline nodes faint.
func (q *nsPanel) machineLines(m *Model, p nsPlan, avail int) []string {
	if len(p.nodes) == 0 {
		return []string{m.th.dim.Render("no machines (pair one with pyre nodes pair)")}
	}
	nameW := 4
	for _, n := range p.nodes {
		nameW = max(nameW, min(20, width(m.nodeName(n.NodeID))))
	}
	loadW := 32
	focused := q.focus == nsMachine
	sel := 0
	for i, n := range p.nodes {
		if n.NodeID == p.nodeID {
			sel = i
		}
	}
	top := 0
	if len(p.nodes) > nsMachineRows {
		top = min(max(0, sel-nsMachineRows/2), len(p.nodes)-nsMachineRows)
	}
	var out []string
	for i := top; i < len(p.nodes) && i < top+nsMachineRows; i++ {
		n := p.nodes[i]
		on := n.NodeID == p.nodeID
		name := pad(trunc(m.nodeName(n.NodeID), nameW), nameW)
		var row string
		if !p.connected[n.NodeID] {
			why := "offline"
			if t := n.Connection.LastSeenAt; t != nil && !t.IsZero() {
				why += " · seen " + ago(m.now(), *t)
			}
			mark := "○ "
			if on {
				mark = "● "
			}
			row = m.th.faint.Render(mark + name + "  " + why)
		} else {
			var agents []string
			for _, h := range m.nodeAgents(n.NodeID) {
				agents = append(agents, string(h))
			}
			load := m.th.dim.Render(pad(trunc(nodeLoadText(n.Resources), loadW-1), loadW))
			ag := m.th.dim.Render(strings.Join(agents, " "))
			if on {
				row = m.th.key.Render("● "+name) + "  " + load + ag
			} else {
				row = m.th.dim.Render("○ ") + name + "  " + load + ag
			}
		}
		if on {
			switch {
			case p.nodeAuto && focused:
				row += "  " + m.th.dim.Render("auto · ↑↓")
			case p.nodeAuto:
				row += "  " + m.th.dim.Render("auto")
			}
		}
		row = m.mark(zoneDef{kind: zoneOverlay, name: "opt:" + strconv.Itoa(int(nsMachine)) + ":" + n.NodeID}, trunc(row, avail))
		out = append(out, m.rowSel(on && focused, row, avail))
	}
	if more := len(p.nodes) - (top + nsMachineRows); more > 0 {
		out = append(out, m.th.dim.Render("  ↓ "+strconv.Itoa(more)+" more machines"))
	}
	return out
}
