package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

func (m *Model) sortedNodes() []contracts.Node {
	ns := append([]contracts.Node(nil), m.nodes()...)
	sort.SliceStable(ns, func(i, j int) bool {
		a, b := strings.ToLower(ns[i].Name), strings.ToLower(ns[j].Name)
		if a != b {
			return a < b
		}
		return ns[i].NodeID < ns[j].NodeID
	})
	return ns
}

func (m *Model) keyManage(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if navKey(&m.mgmtList, key, m.halfPage()) {
		if m.view == viewEnvs {
			return tea.Batch(m.loadEnvDetail(false)...)
		}
		return nil
	}
	switch key {
	case "esc":
		return m.goBack()
	case "r":
		return m.refreshVisible(true)
	}
	return nil
}

func (m *Model) mgmtHeader(title string, st *loadState) string {
	h := m.th.faint.Render("  " + strings.ToUpper(title))
	if st != nil && st.have {
		h += m.th.faint.Render(" · updated " + ago(m.now(), st.at))
		if st.err != nil {
			h += m.th.tone(toneWarn, "  ! refresh failed: "+errText(st.err))
		}
	}
	return h
}

// ---- machines ----

func harnessSummary(n contracts.Node) string {
	var parts []string
	for _, h := range n.Harnesses {
		if h.Available {
			parts = append(parts, "✓"+string(h.Harness))
		} else {
			parts = append(parts, "✗"+string(h.Harness))
		}
	}
	return strings.Join(parts, " ")
}

func (m *Model) viewMachines(w, h int) []string {
	ns := m.sortedNodes()
	ids := make([]string, len(ns))
	for i, n := range ns {
		ids[i] = n.NodeID
	}
	m.mgmtList.setIDs(ids)
	_, st := get[[]contracts.Node](m, "nodes")
	now := m.now()
	list := func(w, h int) []string {
		out := []string{m.mgmtHeader("Machines", st)}
		if len(ns) == 0 {
			return append(out, m.loadingOr(st, "no enrolled machines — pyre node enroll"))
		}
		out = append(out, m.colHeader("  "+pad("STATUS", 10)+" "+pad("MACHINE", 14)+" "+padLeft("SEEN", 4)+"  HARNESSES", w))
		start, end := m.mgmtList.window(h - 2)
		for i := start; i < end; i++ {
			n := ns[i]
			sel := i == m.mgmtList.idx
			s := nodeStatus(n)
			cur := m.cursor(sel)
			seen := "—"
			if n.Connection.LastSeenAt != nil {
				seen = age(now, *n.Connection.LastSeenAt)
			}
			row := cur + m.statusCell(s, 8) + " " + m.primary(sel, pad(n.Name, 14)) + " " + m.th.dim.Render(padLeft(seen, 4)) + "  " + m.harnessChips(n)
			if st := m.machineStatsCell(n.Resources); st != "" && n.Connection.State == contracts.ConnConnected {
				row += "  " + st
			}
			out = append(out, m.markRow(&m.mgmtList, i, sel, trunc(row, w), w))
		}
		return out
	}
	detail := func(w, h int) []string {
		var n *contracts.Node
		for i := range ns {
			if ns[i].NodeID == m.mgmtList.sel {
				n = &ns[i]
			}
		}
		if n == nil {
			return nil
		}
		lw := 11
		s := nodeStatus(*n)
		out := []string{m.th.title.Render(n.Name) + "  " + m.th.status(s)}
		if n.Connection.State != contracts.ConnConnected {
			line := "offline does not mean idle: runs on this machine may still be alive"
			out = append(out, m.th.dim.Render(trunc(line, w)))
		}
		seen := "never"
		if n.Connection.LastSeenAt != nil {
			seen = ago(now, *n.Connection.LastSeenAt)
		}
		out = append(out, "", m.kvl("Last seen", seen, lw))
		if n.Connection.ConnectedSince != nil && n.Connection.State == contracts.ConnConnected {
			out = append(out, m.kvl("Connected", "since "+ago(now, *n.Connection.ConnectedSince), lw))
		}
		out = append(out, m.kvl("Platform", n.Platform+"/"+n.Arch, lw))
		if n.Hostname != "" {
			out = append(out, m.kvl("Hostname", n.Hostname, lw))
		}
		if n.SSHAlias != "" {
			out = append(out, m.kvl("SSH", "ssh "+n.SSHAlias, lw))
		}
		var vers []string
		for _, k := range sortedMapKeys(n.Versions) {
			vers = append(vers, k+" "+n.Versions[k])
		}
		out = append(out, m.kvl("Versions", trunc(strings.Join(vers, " · "), w-lw), lw))
		r := n.Resources
		res := fmt.Sprintf("load %.1f/%d cpu · mem %s free of %s · runs %d", r.Load1, r.CPUCount, humanBytes(r.MemAvailMB<<20), humanBytes(r.MemTotalMB<<20), r.ActiveRuns)
		if r.MaxRuns > 0 {
			res += "/" + itoa(r.MaxRuns)
		}
		out = append(out, m.kvl("Resources", trunc(res, w-lw), lw))
		out = append(out, m.machineStatsLines(r, w, lw)...)
		if !r.ObservedAt.IsZero() {
			out = append(out, m.kvl("", m.th.dim.Render(fmt.Sprintf("pyre overhead %.0f MiB, %.1f%% cpu · sampled %s", r.PyreRSSMB, r.PyreCPUPct, ago(now, r.ObservedAt))), lw))
		}
		out = append(out, "", m.sectionTitle("Harnesses", w))
		for _, hi := range n.Harnesses {
			if hi.Available {
				out = append(out, trunc(fmt.Sprintf("%s %-7s %s  %d capabilities", m.th.tone(toneGood, "✓"), hi.Harness, hi.Version, len(hi.Capabilities)), w))
			} else {
				out = append(out, trunc(fmt.Sprintf("%s %-7s unavailable: %s", m.th.tone(toneBad, "✗"), hi.Harness, hi.Reason), w))
			}
		}
		if n.Connection.State == contracts.ConnConnected {
			out = append(out, m.runUsageLines(r, w)...)
		}
		return out
	}
	return m.listDetail(w, h, list, detail)
}

func sortedMapKeys[V any](mp map[string]V) []string {
	ks := make([]string, 0, len(mp))
	for k := range mp {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ---- accounts ----

func windowLabel(name string) string {
	for _, p := range [][2]string{{"five_hour", "5h"}, {"seven_day", "7d"}} {
		if name == p[0] {
			return p[1]
		}
		// per-model windows from the real usage endpoint: "seven_day_opus"
		if rest, ok := strings.CutPrefix(name, p[0]+"_"); ok {
			return p[1] + " " + strings.ReplaceAll(rest, "_", " ")
		}
	}
	return strings.ReplaceAll(name, "_", " ")
}

// ---- connections ----

func (m *Model) viewConnections(w, h int) []string {
	cs, st := get[[]contracts.Connection](m, "connections")
	cs = append([]contracts.Connection(nil), cs...)
	sort.SliceStable(cs, func(i, j int) bool { return strings.ToLower(cs[i].Name) < strings.ToLower(cs[j].Name) })
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.ConnectionID
	}
	m.mgmtList.setIDs(ids)
	now := m.now()
	availSummary := func(c contracts.Connection) (int, int) {
		ok := 0
		for _, a := range c.Availability {
			if a.State == "verified" {
				ok++
			}
		}
		return ok, len(c.Availability)
	}
	list := func(w, h int) []string {
		out := []string{m.mgmtHeader("Connections", st)}
		if len(cs) == 0 {
			return append(out, m.loadingOr(st, "no named connections"))
		}
		start, end := m.mgmtList.window(h - 1)
		for i := start; i < end; i++ {
			c := cs[i]
			sel := i == m.mgmtList.idx
			ok, n := availSummary(c)
			s := status{"●", "verified", toneGood}
			if ok == 0 {
				s = status{"✗", "missing", toneBad}
			} else if ok < n {
				s = status{"◐", "partial", toneWarn}
			}
			cur := m.cursor(sel)
			row := cur + m.statusCell(s, 8) + " " + pad(c.Name, 20) + " " + pad(c.Service, 10) + fmt.Sprintf(" %d/%d machines", ok, n)
			out = append(out, m.markRow(&m.mgmtList, i, sel, trunc(row, w), w))
		}
		return out
	}
	detail := func(w, h int) []string {
		var c *contracts.Connection
		for i := range cs {
			if cs[i].ConnectionID == m.mgmtList.sel {
				c = &cs[i]
			}
		}
		if c == nil {
			return nil
		}
		lw := 10
		out := []string{m.th.title.Render(c.Name), m.kvl("Service", c.Service, lw), m.kvl("Modes", strings.Join(c.Modes, ", "), lw)}
		scope := func(xs []string, all string) string {
			if len(xs) == 0 {
				return all
			}
			return strings.Join(xs, ", ")
		}
		out = append(out, m.kvl("Machines", trunc(scope(c.Scope.Nodes, "all (owner-broad)"), w-lw), lw))
		out = append(out, m.kvl("Projects", trunc(scope(c.Scope.Projects, "all projects"), w-lw), lw))
		if len(c.Scope.Sessions) > 0 {
			out = append(out, m.kvl("Sessions", trunc(scope(c.Scope.Sessions, ""), w-lw), lw))
		}
		out = append(out, "", m.sectionTitle("Availability", w))
		seen := map[string]bool{}
		for _, a := range c.Availability {
			seen[a.NodeID] = true
			tn := toneGood
			sym := "●"
			if a.State != "verified" {
				tn, sym = toneWarn, "!"
			}
			line := fmt.Sprintf("%s %-10s %s %s · checked %s", sym, a.State, pad(m.nodeName(a.NodeID), 12), a.Mode, ago(now, a.VerifiedAt))
			out = append(out, m.th.tone(tn, trunc(line, w)))
			if a.State != "verified" {
				fix := "action: pyre connection grant " + c.Name + " --node " + m.nodeName(a.NodeID)
				if a.Detail != "" {
					fix = a.Detail + " — " + fix
				}
				out = append(out, m.th.dim.Render(trunc("  "+fix, w)))
			}
		}
		for _, n := range m.sortedNodes() {
			if !seen[n.NodeID] {
				out = append(out, m.th.dim.Render(trunc("· not checked on "+n.Name+" — pyre connection check "+c.Name+" --node "+n.Name, w)))
			}
		}
		return out
	}
	return m.listDetail(w, h, list, detail)
}

// ---- environments & updates ----

func (m *Model) loadEnvDetail(force bool) []tea.Cmd {
	ns := m.sortedNodes()
	ids := make([]string, len(ns))
	for i, n := range ns {
		ids[i] = n.NodeID
	}
	m.mgmtList.setIDs(ids)
	nid := m.mgmtList.sel
	if nid == "" {
		return nil
	}
	api := m.api
	action := func(name string, out any) loaderFn {
		return func(ctx context.Context) (any, error) {
			res, err := api.InvokeNodeAction(ctx, contracts.NodeActionRequest{
				Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
				NodeID:   nid, Action: name, TimeoutSeconds: 5,
			})
			if err != nil {
				return nil, err
			}
			if res.Error != nil {
				return nil, res.Error
			}
			if res.Outcome != contracts.OutcomeSucceeded {
				return nil, contracts.Errorf(contracts.ErrUnavailable, "node did not answer (%s); showing nothing rather than guessing", res.Outcome)
			}
			switch out.(type) {
			case []contracts.ToolVersion:
				var v []contracts.ToolVersion
				err = json.Unmarshal(res.Result, &v)
				return v, err
			default:
				var v []contracts.DriftItem
				err = json.Unmarshal(res.Result, &v)
				return v, err
			}
		}
	}
	ld := func(key string, fn loaderFn) tea.Cmd {
		if force {
			return m.reload(key, fn)
		}
		return m.ensure(key, 0, fn)
	}
	return []tea.Cmd{
		ld("env:"+nid+":tools", action(contracts.ActionUpdateList, []contracts.ToolVersion{})),
		ld("env:"+nid+":drift", action(contracts.ActionEnvDrift, []contracts.DriftItem{})),
	}
}

func toolTone(state, health string) tone {
	switch {
	case state == "failed" || strings.HasPrefix(health, "failed"):
		return toneBad
	case state == "staged":
		return toneAccent
	case state == "previous":
		return toneMuted
	}
	return toneNone
}

func (m *Model) viewEnvs(w, h int) []string {
	ns := m.sortedNodes()
	ids := make([]string, len(ns))
	for i, n := range ns {
		ids[i] = n.NodeID
	}
	m.mgmtList.setIDs(ids)
	now := m.now()
	list := func(w, h int) []string {
		verW := 7
		for _, n := range ns {
			verW = max(verW, min(24, width(n.Versions["pyred"])))
		}
		out := []string{m.colHeader("  "+pad("STATUS", 10)+" "+pad("MACHINE", 14)+" "+pad("PYRED", verW)+" DRIFT", w)}
		start, end := m.mgmtList.window(h - 1)
		for i := start; i < end; i++ {
			n := ns[i]
			sel := i == m.mgmtList.idx
			s := nodeStatus(n)
			drift := ""
			if d, st := get[[]contracts.DriftItem](m, "env:"+n.NodeID+":drift"); st.ok() {
				if len(d) > 0 {
					drift = fmt.Sprintf("%d drift", len(d))
				} else {
					drift = "no drift"
				}
			}
			cur := m.cursor(sel)
			row := cur + m.statusCell(s, 8) + " " + m.primary(sel, pad(n.Name, 14)) + " " + m.th.dim.Render(pad(n.Versions["pyred"], verW)) + " " + drift
			out = append(out, m.markRow(&m.mgmtList, i, sel, trunc(row, w), w))
		}
		return out
	}
	detail := func(w, h int) []string {
		nid := m.mgmtList.sel
		n := m.node(nid)
		if n == nil {
			return nil
		}
		out := []string{m.th.title.Render(n.Name) + "  " + m.th.status(nodeStatus(*n))}
		if n.Connection.State != contracts.ConnConnected {
			// Known offline: say so once instead of two red errors.
			out = append(out, "", m.th.dim.Render(trunc("Tool versions and configuration drift come from the machine itself;", w)),
				m.th.dim.Render(trunc("they show again when "+n.Name+" reconnects.", w)))
			return out
		}
		out = append(out, "", m.sectionTitle("Tool versions", w))
		tools, tst := get[[]contracts.ToolVersion](m, "env:"+nid+":tools")
		if !tst.ok() {
			out = append(out, m.loadingOr(tst, ""))
		} else {
			if tst.err != nil {
				out = append(out, m.th.tone(toneWarn, trunc("! refresh failed: "+errText(tst.err)+"; data from "+ago(now, tst.at), w)))
			}
			sort.SliceStable(tools, func(i, j int) bool {
				if tools[i].Tool != tools[j].Tool {
					return tools[i].Tool < tools[j].Tool
				}
				return tools[i].State < tools[j].State
			})
			for _, t := range tools {
				line := fmt.Sprintf("%-10s %-14s %-9s %s", t.Tool, t.Version, t.State, orDash(t.Health))
				if len(t.InUseByRuns) > 0 {
					line += fmt.Sprintf(" · pinned by %d run(s)", len(t.InUseByRuns))
				}
				out = append(out, m.th.tone(toolTone(t.State, t.Health), trunc(line, w)))
			}
			if len(tools) == 0 {
				out = append(out, m.th.dim.Render("no managed tools reported"))
			}
		}
		out = append(out, "", m.sectionTitle("Configuration drift", w))
		drift, dst := get[[]contracts.DriftItem](m, "env:"+nid+":drift")
		if !dst.ok() {
			out = append(out, m.loadingOr(dst, ""))
		} else {
			for _, d := range drift {
				line := d.Kind + "  " + d.Path
				if d.Key != "" {
					line += " [" + d.Key + "]"
				}
				out = append(out, m.th.tone(toneWarn, trunc("! "+line, w)))
				if d.Expected != "" || d.Actual != "" {
					out = append(out, m.th.dim.Render(trunc("  expected "+orDash(d.Expected)+" · actual "+orDash(d.Actual), w)))
				}
				if d.Detail != "" {
					out = append(out, m.th.dim.Render(trunc("  "+d.Detail, w)))
				}
			}
			if len(drift) == 0 {
				out = append(out, m.th.tone(toneGood, "● matches the applied environment"))
			}
		}
		out = append(out, "", m.th.dim.Render(trunc("Plan/apply/rollback: pyre env plan|apply --node "+n.Name+" · pyre update stage|rollback", w)))
		return out
	}
	return m.listDetail(w, h, list, detail)
}
