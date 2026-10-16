package tui

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Layout breakpoints. Full-screen terminals (~200x55 and wider) are the
// primary target; smaller sizes degrade to two panes and then a stack.
const (
	wideMin  = 170 // wide: the terminal pane gets the most room
	splitMin = 100 // two columns
	textMax  = 100 // comfortable reading width for prose and transcripts
)

// slowAfter is how long a load may run before the UI says the machine is
// slow rather than "loading".
const slowAfter = 3 * time.Second

// ---- Sessions: layout ----

// The right-hand pane is the selected session's live terminal
// (viewSessionTerm), as wide as the list allows; the one-line Needs-you
// strip sits under both panes at every width.
func (m *Model) viewSessions(w, h int) []string {
	var strip []string
	if m.footMode == footFull {
		strip = m.needsYouStrip(w, h)
	}
	bh := h - len(strip)
	term := m.viewSessionTerm
	var body []string
	switch {
	case m.tableShown(): // sessions_modes.go: the full-width table
		body = m.viewSessionTable(w, bh)
	case m.footMode == footSidebar:
		// The sidebar carries the status summary, toasts and key hints at
		// its bottom, so the terminal pane reaches the last row (footer.go).
		body = m.listDetailW(w, bh, m.sessionListWidth(w), m.sidebarList, term)
	case w >= splitMin:
		body = m.listDetailW(w, bh, m.sessionListWidth(w), m.viewSessionList, term)
	default:
		body = m.listDetail(w, bh, m.viewSessionList, m.viewSessionTerm)
	}
	return append(fit(body, w, bh), strip...)
}

// sessionCols are the fixed columns of a session row.
type sessionCols struct {
	harness bool
	machW   int // 0: no machine column (machine mode: the header names it)
	titleW  int
}

// sessionColumns lays out a row of width w; machW is the machine column's
// width (0 for none). Narrow lists drop the harness first, then the machine.
func sessionColumns(w, machW int) sessionCols {
	fixed := 2 + 12 + 1 + 1 + 4 + 1 // cursor status _ title _ age _
	if machW > 0 && w-fixed-(machW+1) < 12 {
		machW = 0
	}
	c := sessionCols{machW: machW}
	if machW > 0 {
		fixed += machW + 1
	}
	c.harness = w-fixed >= 31 // 52 columns without a machine column
	if c.harness {
		fixed += 8
	}
	c.titleW = max(10, w-fixed)
	return c
}

// machineColWidth is the width of the Sessions list's machine column: the
// longest listed machine name (7..12), or 0 in machine mode, where the
// group header names the machine.
func (m *Model) machineColWidth() int {
	if m.groupMode() == groupMachine {
		return 0
	}
	// Sized by every known machine, not the listed sessions, so the
	// columns stay put while sessions come and go.
	n := 0
	for _, nd := range m.nodes() {
		n = max(n, width(nd.Name))
	}
	if n == 0 {
		for _, s := range m.sessions() {
			n = max(n, width(s.NodeName))
		}
	}
	if n == 0 {
		return 0
	}
	return max(7, min(12, n))
}

// sessionListWidth sizes the sidenav from the terminal width alone (a
// fraction: 50 % (56..72 columns) below wideMin, 36 % (60..80) above, plus the usage column while it is
// shown), never from its content: titles changing (auto-titles, renames)
// or sessions coming and going never move the pane boundary.
func (m *Model) sessionListWidth(w int) int {
	lw := max(56, min(72, w*50/100))
	if w >= wideMin {
		lw = max(60, min(80, w*36/100))
	}
	if m.showUsage {
		lw += usageColW + 1
	}
	return min(lw, max(50, w-40))
}

// ---- Sessions: list ----

// sessionListTitle is the SESSION column's header: the grouping and, while
// filtering, how many match.
func (m *Model) sessionListTitle(n int) string {
	title := "SESSION" + m.groupTitleSuffix()
	if m.filter != "" {
		title += fmt.Sprintf(" · %d of %d", n, m.countSessions())
	}
	return title
}

func (m *Model) viewSessionList(w, h int) []string {
	vs := m.visibleSessions()
	c := sessionColumns(w, m.machineColWidth())
	if m.showUsage { // the memory/CPU column (stats.go)
		c.titleW = max(10, c.titleW-usageColW-1)
	}
	hdr := "  " + pad("STATUS", 12) + " " + pad(trunc(m.sessionListTitle(len(vs)), c.titleW), c.titleW)
	if c.machW > 0 {
		hdr += " " + pad("MACHINE", c.machW)
	}
	if c.harness {
		hdr += " " + pad("HARNESS", 7)
	}
	hdr += " " + padLeft("AGE", 4)
	if m.showUsage {
		hdr += " " + padLeft("MEM CPU", usageColW)
	}
	now := m.now()
	return m.groupedSessionRows(vs, w, h, hdr, func(s contracts.SessionView, sel bool) string {
		stt := listStatus(s, m.nodeOf(s))
		row := m.sessionCursor(sel, s) + m.statusCell(stt, 10) + " " + m.treeTitle(sel, s, c.titleW)
		if c.machW > 0 {
			row += " " + m.th.faint.Render(pad(trunc(orDash(s.NodeName), c.machW), c.machW))
		}
		if c.harness {
			row += " " + m.th.dim.Render(pad(string(s.Session.Harness), 7))
		}
		row += " " + m.th.dim.Render(padLeft(age(now, observedAt(s)), 4))
		if m.showUsage {
			row += " " + m.usageCell(s)
		}
		return row
	})
}

// groupedSessionRows lays out the Sessions list rows (sidenav and table):
// the column header hdr, group headers, folded groups, machine rows, the
// session rows drawn by row, and the tail (machines footer, ended toggle).
func (m *Model) groupedSessionRows(vs []contracts.SessionView, w, h int, hdr string, row func(s contracts.SessionView, sel bool) string) []string {
	st := m.loads["sessions"]
	out := []string{m.colHeader(hdr, w)}
	loaded := st != nil && st.have
	tail := m.sessionListTail(vs, w)
	if len(vs) == 0 {
		switch {
		case st == nil || (!st.have && st.inflight):
			out = append(out, m.th.dim.Render("  loading sessions…"))
		case !st.have && st.err != nil:
			out = append(out, m.th.tone(toneBad, "  ✗ could not load sessions: "+errText(st.err)))
		case m.filter != "":
			out = append(out, m.th.dim.Render("  no sessions match — esc clears the filter"))
		case loaded && m.countSessions() > 0:
			out = append(out, "", "  "+m.th.dim.Render("No live sessions — press ")+m.hintSeg(hint{"n", "to start one"}))
		default:
			out = append(out, "", "  No sessions yet — press "+m.hintSeg(hint{"n", "to start one"})+m.th.dim.Render(" (or ")+m.hintSeg(hint{"␣n", "anywhere"})+m.th.dim.Render(")"))
		}
		if loaded {
			out = append(out, tail...)
		}
		return out
	}
	byID := map[string]contracts.SessionView{}
	groups := make([]string, len(m.sessList.ids))
	for _, s := range vs {
		byID[s.Session.SessionID] = s
	}
	for i, id := range m.sessList.ids {
		groups[i] = m.rowGroup(id, byID) // grouping.go
	}
	isHdr := func(i int) bool {
		id := m.sessList.ids[i]
		_, mr := machineOfRow(id)
		_, gr := groupOfRow(id)
		return mr || gr
	}
	rows := h - 1 - len(tail)
	if rows < 3 {
		rows, tail = h-1, nil
	}
	for _, d := range groupedWindowHdr(groups, isHdr, m.sessList.idx, rows, &m.sessTop, true) {
		switch {
		case d.item == -2:
			out = append(out, "")
			continue
		case d.item < 0:
			out = append(out, m.groupHeader(d.group, w))
			continue
		}
		id := m.sessList.ids[d.item]
		sel := d.item == m.sessList.idx
		if key, ok := groupOfRow(id); ok {
			out = append(out, m.collapsedGroupRow(d.item, key, sel, w))
			continue
		}
		if name, ok := machineOfRow(id); ok {
			n := len(m.endedSessions(name, false))
			row := m.cursor(sel) + m.mark(zoneDef{kind: zoneGroup, name: name}, m.groupHeaderText(name, w-2)) + m.th.faint.Render(" · "+plural(n, "ended", "ended"))
			if sel {
				row += m.th.faint.Render(" · ") + m.th.key.Render("X") + " " + m.th.dim.Render("archive all")
			}
			out = append(out, m.markRow(&m.sessList, d.item, sel, trunc(row, w), w))
			continue
		}
		out = append(out, m.markRow(&m.sessList, d.item, sel, row(byID[id], sel), w))
	}
	return append(out, tail...)
}

// sessionListTail is drawn under the session rows: in machine mode one
// faint line per machine with nothing listed ("synth-x ● no live
// sessions"), in the other modes a machines line with every machine's
// health dot (machineDots); then the clickable ended-sessions toggle ("3
// ended — . show").
func (m *Model) sessionListTail(vs []contracts.SessionView, w int) []string {
	var lines []string
	if m.filter == "" && m.groupMode() != groupMachine {
		lines = append(lines, m.machineDots(vs, w)...)
	}
	if m.filter == "" && m.groupMode() == groupMachine {
		listed := map[string]bool{}
		for _, s := range vs {
			listed[s.NodeName] = true
		}
		for _, n := range m.nodes() {
			if listed[n.Name] || n.Connection.State == contracts.ConnRevoked {
				continue
			}
			ns := nodeStatus(n)
			line := "  " + m.th.dim.Render(n.Name) + " " + m.th.tone(ns.tone, ns.sym) + m.th.faint.Render(" · no live sessions")
			if n.Connection.State != contracts.ConnConnected {
				line += m.th.faint.Render(" · " + ns.label)
			}
			lines = append(lines, trunc(line, w))
		}
	}
	toggle := ""
	if n := m.hiddenEnded(); n > 0 {
		toggle = m.mark(zoneDef{kind: zoneHint, name: "."}, m.th.faint.Render(itoa(n)+" ended — ")+m.th.key.Render(".")+" "+m.th.dim.Render("show"))
	} else if m.showEnded {
		toggle = m.mark(zoneDef{kind: zoneHint, name: "."}, m.th.faint.Render("ended shown — ")+m.th.key.Render(".")+" "+m.th.dim.Render("hide"))
	}
	if toggle != "" {
		lines = append(lines, "  "+toggle)
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{""}, lines...)
}

// machineDots is the quiet machine footer outside machine mode, so an
// offline machine is noticed although no header names it. One tidy line
// (owner report, W-tuifix): "machines  server ● 5 · mac ● · pi ◌ offline",
// the live-session count only when > 0; when it does not fit, whole items
// give way to "+N" from the end, never a wrap or a cut mid-item.
func (m *Model) machineDots(vs []contracts.SessionView, w int) []string {
	live := map[string]int{}
	for _, s := range vs {
		if !sessionEnded(s) {
			live[s.NodeName]++
		}
	}
	var segs []string
	for _, n := range m.nodes() {
		if n.Connection.State == contracts.ConnRevoked {
			continue
		}
		ns := nodeStatus(n)
		seg := m.th.dim.Render(n.Name) + " " + m.th.tone(ns.tone, ns.sym)
		if n.Connection.State != contracts.ConnConnected {
			seg += " " + m.th.dim.Render(ns.label)
		} else if c := live[n.Name]; c > 0 {
			seg += " " + m.th.dim.Render(itoa(c))
		}
		segs = append(segs, seg)
	}
	if len(segs) == 0 {
		return nil
	}
	return []string{fitItems("  "+m.th.faint.Render("machines")+"  ", segs, m.th.faint.Render(" · "), w, m.th.faint.Render)}
}

// fitItems joins items after prefix on one line of at most w cells,
// dropping whole items from the end (replaced by "+N") rather than
// wrapping or cutting one.
func fitItems(prefix string, items []string, sep string, w int, faint func(...string) string) string {
	for k := len(items); k >= 0; k-- {
		line := prefix + strings.Join(items[:k], sep)
		if k < len(items) {
			more := "+" + itoa(len(items)-k)
			if k > 0 {
				line += sep
			}
			line += faint(more)
		}
		if width(line) <= w {
			return line
		}
	}
	return trunc(prefix, w)
}

// listStatus is the status shown in a list row. A session that is
// otherwise idle or working carries its attention flag instead: "! needs
// you" when it is blocked on you, "✉ message" when it only has a message
// for you (attention_flag.go). One signal per row, never a separate badge.
func listStatus(s contracts.SessionView, node *contracts.Node) status {
	st := sessionStatus(s, node)
	if st.tone == toneWarn || st.tone == toneBad || st.label == "offline" || st.label == "stale" {
		return st
	}
	level, reason := sessionAttention(s)
	if level == contracts.FlagNeedsYou && !sessionNeedsYou(s) {
		return reasonStatus(reason) // an ended session's failure: shown, not counted
	}
	if fs, ok := flagStatus(level); ok {
		return fs
	}
	return st
}

// statusCell renders a status in a fixed-width column with restrained
// colour: the glyph carries the tone; the word is coloured only when it
// needs attention (approval, failure) and is dim for inactive states.
func (m *Model) statusCell(st status, w int) string {
	word := pad(st.label, w)
	switch st.tone {
	case toneWarn, toneBad:
		word = m.th.tone(st.tone, word)
	case toneMuted:
		word = m.th.dim.Render(word)
	}
	return m.th.tone(st.tone, st.sym) + " " + word
}

func (m *Model) countSessions() int {
	n := 0
	for _, s := range m.sessions() {
		if !s.Session.Archived {
			n++
		}
	}
	return n
}

func (m *Model) nodeByName(name string) *contracts.Node {
	ns := m.nodes()
	for i := range ns {
		if ns[i].Name == name {
			return &ns[i]
		}
	}
	return nil
}

func observedAt(s contracts.SessionView) time.Time {
	if s.Run != nil && !s.Run.Freshness.ObservedAt.IsZero() {
		return s.Run.Freshness.ObservedAt
	}
	return s.Freshness.ObservedAt
}

// ---- Sessions: actions ----

// action is a session action with availability and the reason when not.
type action struct {
	key, label, reason string
}

func (m *Model) sessionActions(s contracts.SessionView) []action {
	var caps []string
	live := s.Run != nil && !s.Run.State.Terminal()
	if s.Run != nil {
		caps = s.Run.Capabilities
	}
	need := func(key, label string, capsAny ...string) action {
		a := action{key: key, label: label}
		if !live {
			a.reason = "no live run"
			return a
		}
		for _, c := range capsAny {
			if contracts.HasCap(caps, c) {
				return a
			}
		}
		a.reason = "not supported by " + harnessName(s)
		return a
	}
	acts := []action{
		need("enter", "attach", contracts.CapAttachNative, contracts.CapAttachWeb, contracts.CapAttachReadOnly),
		{key: "m", label: "message"}, // messages are stored and queued even when a run is busy or offline
		need("!", "interrupt", contracts.CapInterrupt),
		need("x", "stop", contracts.CapStop),
		need("", "steer", contracts.CapInputSteer),
	}
	if live && !contracts.HasCap(caps, contracts.CapInputQueue) && !contracts.HasCap(caps, contracts.CapInputSteer) {
		acts[1].reason = "stored only: " + harnessName(s) + " takes no queued input"
	}
	return acts
}

var lastObservedRe = regexp.MustCompile(`last observed (\w+)`)

// lastObserved is the last state seen before contact was lost, in words
// ("idle"), from the run state or the hub's reason ("…; last observed
// waiting_input"); "" when unknown.
func lastObserved(s contracts.SessionView) string {
	if s.Run == nil {
		return ""
	}
	st := s.Run.State
	if st == contracts.RunUnknown || st == "" {
		sm := lastObservedRe.FindStringSubmatch(s.Run.StateReason)
		if sm == nil {
			return ""
		}
		st = contracts.RunState(sm[1])
	}
	if st == contracts.RunRunning && s.Session.Harness == contracts.HarnessShell {
		return "running"
	}
	if l := runStatus(st).label; l != "unknown" {
		return l
	}
	return ""
}

// harnessName names the session's harness for people ("shell sessions").
func harnessName(s contracts.SessionView) string {
	switch s.Session.Harness {
	case contracts.HarnessShell:
		return "shell sessions"
	case "":
		return "this harness"
	}
	if s.Run == nil || s.Run.Adapter == "" {
		return titleCase(string(s.Session.Harness))
	}
	return titleCase(string(s.Session.Harness)) + " (" + s.Run.Adapter + ")"
}

func (m *Model) sessionHints() []hint {
	if m.focusedTerm() != nil {
		h := []hint{{m.unfocusLabel(), "back to the list"}}
		if sh := m.switchHint(); sh != "" {
			h = append(h, hint{sh, "switch session"})
		}
		if k := m.km.keysFor("global.switcher"); strings.HasPrefix(k, "alt+s") {
			h = append(h, hint{"alt+s", "find session"})
		}
		return h
	}
	if gh, ok := m.groupRowHints(); ok { // grouping.go
		return gh
	}
	if m.tableShown() {
		return m.tableHints() // sessions_modes.go
	}
	h := []hint{{"n", "new session"}} // always first: how to start one
	if _, ok := machineOfRow(m.sessList.sel); ok {
		return append(h, hint{"X", "archive all ended"}, hint{".", "hide ended"}, hint{"/", "filter"}, hint{"␣␣", "find"})
	}
	if s, ok := m.selectedSession(); ok {
		acts := map[string]bool{}
		for _, a := range m.sessionActions(s) {
			acts[a.label] = a.reason == ""
		}
		if acts["attach"] {
			if emb, _ := m.embeddable(s); emb {
				h = append(h, hint{"enter", "type"}, hint{"F", "full-screen"})
			} else {
				h = append(h, hint{"enter", "attach"})
			}
		}
		if _, ok := m.pendingRequest(s.Session.SessionID); ok {
			h = append(h, hint{"a", "answer"})
		}
		h = append(h, hint{"m", "message"}, hint{"i", "info"})
		if sessionEnded(s) {
			h = append(h, hint{"X", "archive"})
		}
	}
	return append(h, hint{"/", "filter"}, hint{"␣␣", "find"})
}

// ---- Sessions: detail ----

// machineRowDetail is the detail of a machine header row on the ended
// view: its ended sessions and what X does.
func (m *Model) machineRowDetail(name string, w int) []string {
	ss := m.endedSessions(name, false)
	out := []string{m.th.title.Render(trunc(orDash(name), w))}
	if nd := m.nodeByName(name); nd != nil {
		ns := nodeStatus(*nd)
		out = append(out, m.th.status(ns))
	}
	out = append(out, "", trunc(plural(len(ss), "ended session", "ended sessions")+m.th.dim.Render(" (exited or failed)"), w))
	for i, s := range ss {
		if i >= 12 {
			out = append(out, m.th.dim.Render(fmt.Sprintf("… and %d more", len(ss)-i)))
			break
		}
		st := sessionStatus(s, m.nodeOf(s))
		out = append(out, trunc(m.th.tone(st.tone, st.sym)+" "+iconTitle(s), w))
	}
	return append(out, "", trunc(m.th.key.Render("X")+" "+m.th.dim.Render("archives them all (confirms) · ")+m.th.key.Render(".")+" "+m.th.dim.Render("hides ended sessions"), w))
}

// failureLine says in plain words why a run failed: the hub's or node's
// reason without its technical prefix, else the exit code.
func (m *Model) failureLine(r *contracts.Run, now time.Time) string {
	reason := strings.TrimSpace(m.humanIDs(humanTimes(r.StateReason, now)))
	switch {
	case strings.HasPrefix(reason, "launch failed: "):
		reason = "Could not start: " + strings.TrimPrefix(reason, "launch failed: ")
	case reason != "":
		reason = "Failed: " + reason
	case r.ExitCode != nil:
		reason = fmt.Sprintf("Failed: the run exited with code %d", *r.ExitCode)
	default:
		reason = "Failed; no reason was reported"
	}
	if reason != "" && r.ExitCode != nil && !strings.Contains(reason, "code") {
		reason += fmt.Sprintf(" (exit code %d)", *r.ExitCode)
	}
	if !strings.HasSuffix(reason, ".") {
		reason += "."
	}
	return reason
}

// sessionDetail renders a session's status, facts and activity (finder
// preview).
func (m *Model) sessionDetail(s contracts.SessionView, w, h int) []string {
	out := m.sessionFacts(s, w)
	return append(out, m.activityLines(s, w, h-len(out))...)
}

// sessionFacts renders a session's status and facts (finder preview, the
// Sessions info overlay).
func (m *Model) sessionFacts(s contracts.SessionView, w int) []string {
	now := m.now()
	node := m.nodeOf(s)
	stt := sessionStatus(s, node)
	lw := 12 // the longest label ("Permissions", "Unavailable") plus a gap
	vw := w - lw
	out := []string{m.th.title.Render(trunc(iconTitle(s), w))}
	if s.Run != nil && s.Run.State == contracts.RunFailed {
		// A failed session leads with why, in plain words.
		for _, l := range wrap(m.failureLine(s.Run, now), min(w, textMax)) {
			out = append(out, m.th.tone(toneBad, l))
		}
	}
	head := m.doingStatus(s, stt) + m.th.dim.Render(" · ") + titleCase(string(s.Session.Harness))
	if s.NodeName != "" {
		head += m.th.dim.Render(" on ") + s.NodeName
	}
	head += m.th.dim.Render(" · " + ago(now, observedAt(s)))
	out = append(out, trunc(head, w))
	if g, ok := m.leadSignal(s.Session.SessionID); ok && g.kind != leadQuiet { // leads.go
		out = append(out, trunc(m.leadWords(s.Session.SessionID, g), w))
	}
	if s.Run != nil && (stt.label == "offline" || stt.label == "stale") {
		// One honest line: what is known, when, and that the run may live on.
		last := lastObserved(s)
		var line string
		if node != nil && node.Connection.State != contracts.ConnConnected {
			line = s.NodeName + " is offline"
			if node.Connection.LastSeenAt != nil {
				line += " (seen " + ago(now, *node.Connection.LastSeenAt) + ")"
			}
		} else {
			line = "No fresh report"
		}
		if last != "" {
			line += "; last observed " + last + " " + ago(now, s.Run.Freshness.ObservedAt)
		}
		for _, l := range wrap(line+".", min(w, textMax)) {
			out = append(out, m.th.dim.Render(l))
		}
		out = append(out, m.th.dim.Render("The run may still be alive."))
	} else if s.Run != nil && s.Run.StateReason != "" && s.Run.State != contracts.RunFailed {
		out = append(out, m.th.dim.Render(trunc(m.humanIDs(humanTimes(s.Run.StateReason, now)), w)))
	}
	out = append(out, "")
	proj := m.th.dim.Render("standalone")
	if s.Session.ProjectID != nil {
		proj = m.projectName(s.Session.ProjectID)
	}
	if s.Run != nil && s.Run.Checkout != nil {
		co := s.Run.Checkout
		d := " · " + string(co.Mode)
		if co.Branch != "" {
			d += " " + co.Branch
		}
		proj += m.th.dim.Render(d)
	}
	out = append(out, m.kvFit("Project", proj, lw, w)...)
	if lin := s.Session.Lineage; lin.ReparentedAt != nil { // W-reparent
		from := "the top level"
		if lin.ReparentedFrom != "" {
			from = m.sessionTitle(lin.ReparentedFrom)
		}
		if lin.ParentSessionID != "" {
			by := m.sessionTitle(lin.ParentSessionID)
			if !slices.Contains(s.Session.Tags, contracts.TagReportParent) {
				by += m.th.dim.Render(" · reports off")
			}
			out = append(out, m.kvFit("Reports to", by+m.th.dim.Render(" (reparented from "+from+")"), lw, w)...)
		} else {
			out = append(out, m.kvFit("Reports to", m.th.dim.Render("no parent (moved from "+from+")"), lw, w)...)
		}
	} else if parent := lin.ParentSessionID; parent != "" { // W-childnotify
		by := m.sessionTitle(parent)
		if slices.Contains(s.Session.Tags, contracts.TagReportParent) {
			by += m.th.dim.Render(" · reports to parent")
		}
		out = append(out, m.kvFit("Started by", by, lw, w)...)
	}
	if at := ownerActive(s); !at.IsZero() { // recently yours (yours.go)
		out = append(out, m.kvFit("You", "messaged or typed "+ago(now, at), lw, w)...)
	}
	if s.Run != nil {
		out = append(out, m.kvFit("Directory", tildePath(s.Run.Cwd), lw, w)...)
		if s.Session.Harness != contracts.HarnessShell {
			out = append(out, m.kvFit("Account", m.accountFact(s), lw, w)...)
			out = append(out, m.accountSwitchLines(s, lw, w)...)
			if contracts.HasCap(s.Run.Capabilities, contracts.CapPermissionsBypass) {
				out = append(out, m.kvFit("Permissions", m.th.tone(toneWarn, "bypass")+m.th.dim.Render(" · no approval prompts, no sandbox"), lw, w)...)
			}
		}
		run := s.Run.Adapter
		if s.Run.Runtime.Version != "" {
			run += " " + s.Run.Runtime.Version
		}
		if s.Run.ContinuationKind != "" {
			run += " · " + string(s.Run.ContinuationKind)
		}
		out = append(out, m.kvFit("Run", run+m.th.dim.Render("  "+s.Run.RunID), lw, w)...)
		if s.Run.Lease != nil && s.Run.Lease.Active(now) {
			holder := s.Run.Lease.Holder.Name
			if holder == "" {
				holder = s.Run.Lease.Holder.ID
			}
			out = append(out, m.kvWrap("Input", "leased by "+holder+" until "+s.Run.Lease.ExpiresAt.Format("15:04")+"; raw writes queue", func(ss ...string) string { return m.th.tone(toneWarn, strings.Join(ss, " ")) }, lw, w)...)
		}
		if n := len(s.Run.Subagents); n > 0 {
			out = append(out, m.kvFit("Subagents", plural(n, "native subagent", "native subagents")+m.th.dim.Render(" (not separately attachable)"), lw, w)...)
		}
	} else {
		out = append(out, m.kvl("Run", m.th.dim.Render("none"), lw))
	}
	// Only what cannot be done is worth a line, and only when the run is
	// gone; what can be done is in the footer keys.
	if live := s.Run != nil && !s.Run.State.Terminal(); !live {
		var gone []string
		reason := ""
		for _, a := range m.sessionActions(s) {
			if a.reason != "" {
				gone = append(gone, a.label)
				reason = a.reason
			}
		}
		if len(gone) > 0 {
			out = append(out, m.kvWrap("Unavailable", strings.Join(gone, ", ")+" — "+reason, m.th.dim.Render, lw, w)...)
		}
	}
	if _, pending := m.pendingRequest(s.Session.SessionID); !pending && s.Run != nil && s.Run.State == contracts.RunWaitingApproval {
		// Seen live: a remote approval expired and the harness fell back to
		// its own prompt. Nothing to answer here; the terminal is waiting.
		out = append(out, "", m.th.tone(toneWarn, "! Waiting on a prompt in its terminal")+m.th.dim.Render(" — enter types into it"))
	}
	if a, ok := m.pendingRequest(s.Session.SessionID); ok {
		out = append(out, "", m.th.tone(toneWarn, "! "+trunc(m.humanIDs(a.Title), vw))+m.th.dim.Render(" — a answers"))
	} else if sessionFlagged(s) {
		text := "needs you"
		if s.Flag != nil && s.Flag.Text != "" {
			text = m.humanIDs(s.Flag.Text)
		}
		out = append(out, "", m.th.tone(toneWarn, "! "+trunc(text, vw)))
	}
	if line := m.messageLine(s, w); line != "" {
		out = append(out, "", line)
	}
	return append(out, "")
}

// activityLines renders the latest transcript entries and, when the
// adapter offers one, a framed snapshot of the live terminal.
func (m *Model) activityLines(s contracts.SessionView, w, h int) []string {
	if h < 2 {
		return nil
	}
	if s.Run == nil {
		return []string{m.sectionTitle("Activity", w), m.th.dim.Render("No run yet.")}
	}
	if line, ok := m.failedRunLine(s, w); ok {
		return []string{m.sectionTitle("Activity", w), line}
	}
	if !contracts.HasCap(s.Run.Capabilities, contracts.CapTranscript) && !contracts.HasCap(s.Run.Capabilities, contracts.CapScreen) {
		return []string{m.sectionTitle("Activity", w), m.th.dim.Render("Transcript unavailable: the adapter offers neither transcript nor screen reads.")}
	}
	resp, st := get[contracts.ReadSessionResponse](m, "preview:"+s.Session.SessionID)
	if (st == nil || !st.have) && m.localCapture(s.Session.SessionID) != "" {
		// On this machine: its screen now; the transcript follows.
		resp, st = contracts.ReadSessionResponse{Screen: m.localCapture(s.Session.SessionID)}, &loadState{have: true, at: m.now()}
	}
	offline := false
	if n := m.nodeOf(s); n != nil && n.Connection.State != contracts.ConnConnected {
		offline = true
	}
	switch {
	case offline && (st == nil || !st.have):
		return []string{m.sectionTitle("Activity", w), m.th.dim.Render(trunc("No live preview while "+orDash(s.NodeName)+" is offline.", w))}
	case st != nil && !st.have && st.inflight && time.Since(st.started) >= slowAfter:
		// A slow or hung machine: say so instead of an endless "loading".
		line := fmt.Sprintf("waiting for %s… no answer for %s", orDash(s.NodeName), age(time.Now(), st.started))
		out := []string{m.sectionTitle("Activity", w), m.th.tone(toneWarn, trunc(line, w))}
		if st.err != nil {
			out = append(out, m.th.dim.Render(trunc("last try: "+m.humanIDs(errText(st.err))+"; retrying", w)))
		}
		return append(out, m.th.dim.Render(trunc("Navigation and other sessions are unaffected.", w)))
	case st == nil || (!st.have && st.inflight) || (!st.have && st.err == nil):
		if !m.showLoading(s, st) {
			return []string{m.sectionTitle("Activity", w)}
		}
		return []string{m.sectionTitle("Activity", w), m.th.dim.Render("loading preview…")}
	case !st.have:
		return []string{m.sectionTitle("Activity", w), m.th.tone(toneBad, trunc("✗ preview unavailable: "+errText(st.err), w))}
	}
	var notes []string
	if offline {
		notes = append(notes, m.th.dim.Render(trunc(orDash(s.NodeName)+" is offline; showing the preview from "+ago(m.now(), st.at), w)))
	} else if st.inflight && time.Since(st.started) >= slowAfter {
		notes = append(notes, m.th.tone(toneWarn, trunc(fmt.Sprintf("! waiting for %s… no answer for %s; showing the preview from %s", orDash(s.NodeName), age(time.Now(), st.started), ago(m.now(), st.at)), w)))
	} else if st.err != nil {
		notes = append(notes, m.th.tone(toneWarn, trunc("! refresh failed ("+errText(st.err)+"); showing preview from "+ago(m.now(), st.at), w)))
	}
	if resp.Gap {
		notes = append(notes, m.th.dim.Render("… earlier activity missing (gap)"))
	}
	// Terminal snapshot: bottom lines matter most.
	screen := compactScreen(resp.Screen)
	tw := min(w, textMax)
	var body []string
	for _, e := range shownEntries(resp.Entries) {
		body = append(body, m.transcriptEntry(e, tw)...)
	}
	frameH := 0
	avail := h - 1 - len(notes)
	if avail < 3 || (len(body) > 0 && avail < 7) {
		// No room for a whole frame (never draw half a box), or so little
		// that the transcript reads better on its own.
		screen = nil
	}
	if len(screen) > 0 {
		budget := avail
		if len(body) > 0 {
			budget = min(avail, max(5, avail*55/100))
			if len(body)+3 < avail-budget {
				budget = avail - len(body) - 2
			}
		}
		lines := min(len(screen), max(1, budget-2))
		screen = screen[len(screen)-lines:]
		frameH = len(screen) + 2
	}
	out := []string{m.sectionTitle("Activity", w)}
	out = append(out, notes...)
	room := h - len(out) - frameH
	if frameH > 0 && len(body) > 0 {
		room-- // blank line before the terminal
	}
	switch {
	case len(body) == 0 && frameH == 0:
		out = append(out, m.th.dim.Render("No activity captured yet."))
	case room > 0 && len(body) > room:
		body = body[len(body)-room:]
		fallthrough
	case room > 0:
		out = append(out, body...)
	}
	if frameH > 0 {
		if len(body) > 0 && room > 0 {
			out = append(out, "")
		}
		for i := range screen {
			screen[i] = m.th.term.Render(clip(screen[i], w-4))
		}
		out = append(out, m.frame("terminal", screen, w)...)
	}
	return out
}

// transcriptEntry renders one entry: a role column and wrapped text with a
// hanging indent.
func (m *Model) transcriptEntry(e contracts.TranscriptEntry, w int) []string {
	role, style := e.Kind, m.th.dim
	switch e.Kind {
	case "user":
		role, style = "user", m.th.accent
	case "assistant":
		role, style = "agent", m.th.bold
	case "pyre_message":
		role, style = "message", m.th.accent
	}
	raw := e.Text
	lead0 := ""
	if pm, ok := m.parsePyreMessage(raw); ok {
		// Delivered messages show who sent them, not the runtime envelope
		// ("[pyre-msg:msg_…] from owner:cli_… (…):").
		role, style = "message", m.th.accent
		lead0 = "from " + pm.from
		if pm.context != "" {
			lead0 += " · " + pm.context
		}
		lead0 += ": "
		raw = pm.body
	}
	txt := lead0 + strings.Join(strings.Fields(raw), " ")
	if e.Truncated {
		txt += " …"
	}
	const rw = 8
	lines := wrap(txt, max(10, w-rw))
	out := make([]string, len(lines))
	for i, l := range lines {
		lead := strings.Repeat(" ", rw)
		if i == 0 {
			lead = style.Render(pad(role, rw-1)) + " "
		}
		switch {
		case e.Kind == "system" || e.Kind == "tool":
			l = m.th.dim.Render(l)
		case i == 0 && lead0 != "" && strings.HasPrefix(l, lead0):
			l = m.th.dim.Render(lead0) + l[len(lead0):]
		}
		out[i] = lead + l
	}
	return out
}

// shownEntries drops transcript entries that carry no information for a
// reader: Claude's tool results arrive as a bare "tool result" placeholder
// right after the tool call they belong to.
func shownEntries(es []contracts.TranscriptEntry) []contracts.TranscriptEntry {
	out := make([]contracts.TranscriptEntry, 0, len(es))
	for _, e := range es {
		if e.Kind == "tool" && strings.TrimSpace(e.Text) == "tool result" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- Needs you ----

// needsYouStrip is the one-line summary under the Sessions panes: the
// sessions that need you (and how many messages are waiting), or a fleet
// summary when nothing does. It reads the sessions' flags.
func (m *Model) needsYouStrip(w, h int) []string {
	if h < 12 || !m.Loaded("sessions") {
		return nil
	}
	items := m.needsYouSessions()
	_, msgs := flagCounts(m.visibleSessions())
	msgNote := ""
	if msgs > 0 {
		msgNote = m.th.dim.Render(" · ") + m.th.tone(toneAccent, "✉") + m.th.dim.Render(" "+plural(msgs, "message", "messages"))
	}
	line := " "
	if len(items) == 0 {
		line += m.th.tone(toneGood, "✓") + " " + m.th.dim.Render("Nothing needs you") + msgNote + m.th.faint.Render("   ") + m.fleetSummary()
	} else {
		line += m.th.tone(toneWarn, "Needs you") + gap2
		more := msgNote
		for i, s := range items {
			_, reason := sessionAttention(s)
			st := reasonStatus(reason)
			seg := m.th.tone(st.tone, st.sym) + " " + trunc(iconTitle(s), 28)
			rest := len(items) - i - 1
			tail := ""
			if rest > 0 {
				tail = m.th.dim.Render(fmt.Sprintf("   +%d more", rest))
			}
			if width(line)+width(seg)+width(tail)+width(more) > w-1 {
				line += m.th.dim.Render(fmt.Sprintf("+%d more", len(items)-i))
				break
			}
			line += seg
			if rest > 0 {
				line += m.th.faint.Render("   ")
			}
		}
		if width(line)+width(more) <= w-1 {
			line += more
		}
	}
	return []string{"", trunc(line, w)}
}

// fleetSummary counts sessions by status: "● 5 working  ○ 3 idle …".
func (m *Model) fleetSummary() string {
	order := []string{"working", "running", "starting", "approval", "idle", "background", "offline", "stale", "failed"}
	counts := map[string]int{}
	syms := map[string]status{}
	for _, s := range m.visibleSessions() {
		st := sessionStatus(s, m.nodeOf(s))
		counts[st.label]++
		syms[st.label] = st
	}
	var segs []string
	for _, k := range order {
		if n := counts[k]; n > 0 {
			st := syms[k]
			segs = append(segs, m.th.tone(st.tone, st.sym)+" "+itoa(n)+" "+m.th.dim.Render(k))
		}
	}
	return strings.Join(segs, gap2)
}
