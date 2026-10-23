package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runfail"
	"github.com/jhh3/pyre/internal/sysstats"
)

// viewSessionTerm is the Sessions view's right-hand pane: one compact
// header line, a "✉ message for you" line while the session has one (its
// messages show here, not in an inbox), then the session's live terminal
// for the rest of the pane. Everything else
// (project, directory, run, capabilities, delivery facts) is behind i.
func (m *Model) viewSessionTerm(w, h int) []string {
	if name, ok := machineOfRow(m.sessList.sel); ok {
		return m.machineRowDetail(name, w) // a machine header row (ended view)
	}
	if key, ok := groupOfRow(m.sessList.sel); ok {
		return m.groupRowDetail(key, w) // a folded group (grouping.go)
	}
	s, ok := m.selectedSession()
	if !ok {
		return []string{"", m.th.dim.Render("No session selected")}
	}
	focused := m.focusedTerm() != nil
	// One header line; lease, embed and attention state are a faint hint
	// at its right edge, never extra lines.
	out := []string{m.termHeader(s, w, focused)}
	if line := m.messageLine(s, w); line != "" && h >= 8 {
		out = append(out, line)
	} else if line := m.dismissLine(s, w); line != "" && h >= 8 {
		out = append(out, line)
	}
	th := h - len(out)
	if th < 1 {
		return out
	}
	tm := m.terms()
	tm.paneW, tm.paneH = w, th
	tm.shown.Store(s.Session.SessionID) // this terminal's output schedules frames (termPoke)
	tv := tm.views[s.Session.SessionID]
	if tv != nil && s.Run != nil && tv.runID == s.Run.RunID {
		if st, _, drawn := tv.status(); drawn && st != termUnavailable {
			// No stream (being reopened, or closed): the last screen at
			// once, dimmed until the live one draws over it.
			live, _ := tv.live()
			rows := tv.render(w, th, focused && live, m.th.plain || !live)
			for i, r := range rows {
				if !live {
					r = m.th.dim.Render(r)
				}
				rows[i] = m.mark(zoneDef{kind: zoneTerm, idx: i}, r) // mouse input goes to the terminal
			}
			return append(out, rows...)
		}
	}
	return append(out, m.termFallback(s, tv, w, th)...)
}

// termHeader: title · status · harness on machine · account · bypass, and
// at the right edge the state of the embedded terminal (faint).
func (m *Model) termHeader(s contracts.SessionView, w int, focused bool) string {
	sep := m.th.dim.Render(" · ")
	stt := sessionStatus(s, m.nodeOf(s))
	right := ""
	if focused {
		right = m.th.accent.Render("typing") + m.th.faint.Render(" · "+m.unfocusLabel()+" back to list")
	} else if hint := m.termHint(s); hint != "" {
		right = m.th.faint.Render(hint)
	}
	harness := titleCase(string(s.Session.Harness))
	where := harness
	if s.NodeName != "" {
		where += m.th.dim.Render(" on ") + s.NodeName
	}
	acct, bypass := "", ""
	if s.Run != nil && s.Session.Harness != contracts.HarnessShell {
		acct = m.th.dim.Render("ambient login")
		if s.Run.AccountID != "" {
			acct = m.accountHeadline(s)
		}
		if contracts.HasCap(s.Run.Capabilities, contracts.CapPermissionsBypass) {
			bypass = m.th.tone(toneWarn, "bypass")
		}
	}
	room := w
	if right != "" {
		room = w - width(right) - 2
	}
	// The title keeps priority: facts drop from the right (account, then
	// the machine) before the title is shortened. bypass is never dropped.
	title := m.markTitle(m.th.title.Render(trunc(iconTitle(s), 40))) // click: rename
	doing := m.doingStatus(s, stt)
	var left string
	for _, segs := range [][]string{
		{title, doing, where, acct, bypass},
		{title, doing, where, bypass},
		{title, doing, harness, bypass},
		{title, doing, bypass},
	} {
		left = joinNonEmpty(sep, segs...)
		if width(left) <= room {
			break
		}
	}
	left = trunc(left, room)
	if right == "" {
		return left
	}
	return left + strings.Repeat(" ", max(1, w-width(left)-width(right))) + right
}

// termHint is the short, faint state at the header's right edge: what
// matters most right now about typing into this pane ("" when nothing).
func (m *Model) termHint(s contracts.SessionView) string {
	tv := m.terms().views[s.Session.SessionID]
	if tv != nil {
		switch {
		case tv.takeoverOffered():
			return "typing on " + tv.askTakeover + " · enter to take over"
		case tv.focusNote != "":
			return tv.focusNote
		}
	}
	if _, ok := m.pendingRequest(s.Session.SessionID); ok {
		return "needs you · a answers"
	}
	switch level, _ := sessionAttention(s); {
	case level == contracts.FlagNeedsYou:
		return "needs you"
	case level == contracts.FlagMessage && m.messageLine(s, 200) == "":
		return "✉ message for you"
	}
	if s.Run != nil && s.Run.Lease != nil && s.Run.Lease.Active(m.now()) && (tv == nil || tv.leaseID != s.Run.Lease.LeaseID) {
		if s.Run.Lease.Holder.Kind == contracts.PrincipalOwner {
			return "typing elsewhere · enter to take over"
		}
		return "typing: " + holderName(s.Run.Lease.Holder)
	}
	if tv == nil {
		return ""
	}
	if live, _ := tv.live(); live {
		return "live · enter to type"
	}
	switch st, _, drawn := tv.status(); {
	case st == termStarting:
		return "starting…"
	case drawn && (st == termEnded || st == termIdle):
		return "disconnected · last screen"
	case drawn && st == termConnecting:
		return "last screen · connecting…"
	}
	return ""
}

// termFallback fills the pane when there is no live screen: why, and the
// last captured screen when the adapter offers one.
func (m *Model) termFallback(s contracts.SessionView, tv *termView, w, h int) []string {
	var head []string
	ok, why := m.embeddable(s)
	now := m.now()
	switch {
	case ok && tv != nil:
		st, _, _ := tv.status()
		switch st {
		case termFailed: // the reason was shown once (toast); the pane stays calm
			head = append(head, m.th.dim.Render(trunc("couldn't connect to "+orDash(s.NodeName)+" — retrying", w)))
		case termUnavailable:
			note := "read-only snapshot"
			if tv.note != "" {
				note += " · " + tv.note
			}
			head = append(head, m.th.dim.Render(trunc(note, w)))
		case termStarting:
			head = append(head, m.th.dim.Render(trunc("starting…", w)))
		case termEnded:
			head = append(head, m.th.dim.Render(trunc("reconnecting…", w)))
		default:
			head = append(head, m.th.dim.Render(trunc("connecting to "+orDash(s.NodeName)+"…", w)))
		}
	case ok:
		head = append(head, m.th.dim.Render(trunc("connecting to "+orDash(s.NodeName)+"…", w)))
	case s.Run != nil && (sessionStatus(s, m.nodeOf(s)).label == "offline" || sessionStatus(s, m.nodeOf(s)).label == "stale"):
		node := m.nodeOf(s)
		line := "No fresh report"
		if node != nil && node.Connection.State != contracts.ConnConnected {
			line = s.NodeName + " is offline"
			if node.Connection.LastSeenAt != nil {
				line += " (seen " + ago(now, *node.Connection.LastSeenAt) + ")"
			}
		}
		if last := lastObserved(s); last != "" {
			line += "; last observed " + last + " " + ago(now, s.Run.Freshness.ObservedAt)
		}
		for _, l := range wrap(line+".", min(w, textMax)) {
			head = append(head, m.th.dim.Render(l))
		}
		head = append(head, m.th.dim.Render("The run may still be alive."))
	case s.Run != nil && s.Run.State == contracts.RunFailed:
		// Why it failed, in plain words (not a persistent red line).
		for _, l := range wrap(m.failureLine(s.Run, now), min(w, textMax)) {
			head = append(head, l)
		}
	default:
		head = append(head, m.th.dim.Render(trunc(why, w)))
	}
	screen := m.snapshotScreen(s)
	room := h - len(head) - 1
	if len(screen) == 0 && ok {
		return append(head, m.snapshotLoading(s, w)...)
	}
	if len(screen) == 0 || room < 1 {
		return head
	}
	if len(screen) > room {
		screen = screen[len(screen)-room:]
	}
	out := append(head, "")
	for _, l := range screen {
		out = append(out, m.th.term.Render(clip(l, w)))
	}
	return out
}

// snapshotScreen is the last captured screen of s from the preview load
// (screen-capable adapters), compacted; nil when there is none.
func (m *Model) snapshotScreen(s contracts.SessionView) []string {
	resp, st := get[contracts.ReadSessionResponse](m, "preview:"+s.Session.SessionID)
	if st == nil || !st.have {
		return nil
	}
	return compactScreen(resp.Screen)
}

// snapshotLoading says how the last-screen load is doing while there is
// none: loading, or waiting for a machine that does not answer (named,
// with how long), so a hung machine never reads as "loading" forever.
func (m *Model) snapshotLoading(s contracts.SessionView, w int) []string {
	if line, ok := m.failedRunLine(s, w); ok {
		return []string{"", line}
	}
	if s.Run == nil || (!contracts.HasCap(s.Run.Capabilities, contracts.CapTranscript) && !contracts.HasCap(s.Run.Capabilities, contracts.CapScreen)) {
		return nil
	}
	_, st := get[contracts.ReadSessionResponse](m, "preview:"+s.Session.SessionID)
	switch {
	case st != nil && !st.have && st.inflight && time.Since(st.started) >= slowAfter:
		line := fmt.Sprintf("waiting for %s… no answer for %s", orDash(s.NodeName), age(time.Now(), st.started))
		out := []string{"", m.th.tone(toneWarn, trunc(line, w))}
		if st.err != nil {
			out = append(out, m.th.dim.Render(trunc("last try: "+m.humanIDs(errText(st.err))+"; retrying", w)))
		}
		return append(out, m.th.dim.Render(trunc("Navigation and other sessions are unaffected.", w)))
	case st == nil || !st.have && (st.inflight || st.err == nil):
		if !m.showLoading(s, st) {
			return nil // a local capture is coming, or the read just started
		}
		return []string{"", m.th.dim.Render("loading preview…")}
	case !st.have:
		return []string{"", m.th.tone(toneBad, trunc("✗ preview unavailable: "+errText(st.err), w))}
	}
	return nil
}

// showLoading reports whether a preview still loading says so: never
// for a session on this machine (its local capture paints instead), and
// only once the read has run loadingAfter with nothing to show.
func (m *Model) showLoading(s contracts.SessionView, st *loadState) bool {
	if emb, _ := m.embeddable(s); emb {
		if _, ok := m.localFor(s); ok {
			return false
		}
	}
	return st != nil && st.inflight && time.Since(st.started) >= loadingAfter
}

// ---- info overlay (i) ----

// infoOverlay shows what the compact header leaves out: project,
// directory, account details, run and version, capabilities and how input
// and messages reach the session right now.
type infoOverlay struct{ sid string }

func (o *infoOverlay) hints() []hint { return []hint{{"esc", "close"}} }

func (o *infoOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc", "i", "q", "enter":
		m.overlay = nil
	}
	return nil
}

func (o *infoOverlay) view(m *Model, w, h int) []string {
	s, ok := m.session(o.sid)
	if !ok {
		m.overlay = nil
		return nil
	}
	bw := min(96, w-4)
	inner := bw - 4
	lines := m.sessionFacts(s, inner)
	lw := 11
	vw := inner - lw
	add := func(label, value string) {
		for i, l := range wrap(value, max(10, vw)) {
			if i == 0 {
				lines = append(lines, m.kvl(label, l, lw))
			} else {
				lines = append(lines, strings.Repeat(" ", lw)+l)
			}
		}
	}
	lines = append(lines, "")
	add("Session", s.Session.SessionID)
	if s.Run != nil {
		add("Run ID", s.Run.RunID)
		if len(s.Run.Capabilities) > 0 {
			add("Supports", strings.Join(s.Run.Capabilities, ", "))
		}
	}
	if u, ok := m.runUsage(s); ok { // stats.go
		add("Usage", sysstats.Detail(u))
	}
	add("Terminal", m.termFacts(s))
	add("Messages", m.deliveryFacts(s))
	lines = append(lines, m.worktreeInfoLines(s, inner)...) // W-quicklaunch
	return m.box(iconTitle(s), lines, bw, w, h)
}

// termFacts says how the pane reaches the terminal right now.
func (m *Model) termFacts(s contracts.SessionView) string {
	tv := m.terms().views[s.Session.SessionID]
	if tv == nil {
		if ok, why := m.embeddable(s); !ok {
			return why
		}
		return "not attached yet"
	}
	live, mode := tv.live()
	st, errS, _ := tv.status()
	switch {
	case live && mode == termInteractive && tv.leaseIdle:
		return "embedded, interactive; the input lease was released while idle and is taken back on the next key"
	case live && mode == termInteractive:
		return "embedded, interactive (writable tmux client, input lease " + tv.leaseID + "); " + m.unfocusLabel() + " returns to the list"
	case live:
		return "embedded, read-only (tmux attach -r: watching takes no input lease); enter, a click or tab to type"
	case st == termFailed:
		return "could not embed: " + errS
	case st == termStarting:
		return "the run is starting; the terminal attaches as soon as it is live"
	case st == termUnavailable:
		return "no embeddable terminal from here (" + orDash(tv.note) + "); showing the last captured screen"
	}
	return "disconnected; reconnects while selected"
}

// deliveryFacts says what happens to a message sent to s right now.
func (m *Model) deliveryFacts(s contracts.SessionView) string {
	if s.Run == nil || s.Run.State.Terminal() {
		return "stored; delivered when the session runs again (if it resumes)"
	}
	if tv := m.terms().views[s.Session.SessionID]; tv != nil && tv.leaseID != "" {
		return "you hold the input lease: Pyre does not paste into the terminal while you type; messages queue and the doorbell tells the agent"
	}
	if s.Run.Lease != nil && s.Run.Lease.Active(m.now()) {
		return "a human holds the input lease: messages queue; the doorbell tells the agent"
	}
	return "delivered into the terminal when the agent is idle (stored, then runtime acceptance is reported separately)"
}

// failedRunLine: a failed run with nothing to preview (it never started, so
// its machine has no transcript: "not_found: no run of this session")
// shows why it failed in plain words instead of the read error.
func (m *Model) failedRunLine(s contracts.SessionView, w int) (string, bool) {
	if s.Run == nil || s.Run.State != contracts.RunFailed || strings.TrimSpace(s.Run.StateReason) == "" {
		return "", false
	}
	_, st := get[contracts.ReadSessionResponse](m, "preview:"+s.Session.SessionID)
	noCaps := !contracts.HasCap(s.Run.Capabilities, contracts.CapTranscript) && !contracts.HasCap(s.Run.Capabilities, contracts.CapScreen)
	notFound := st != nil && !st.have && st.err != nil &&
		(contracts.IsCode(st.err, contracts.ErrNotFound) || strings.Contains(st.err.Error(), "no run of this session"))
	if !noCaps && !notFound {
		return "", false
	}
	text := runfail.Plain(s.Run.StateReason, s.Run.ExitCode != nil, runfail.Names{Account: m.accountLabel, Node: s.NodeName})
	return m.th.tone(toneBad, trunc("✗ "+m.humanIDs(text), w)), true
}
