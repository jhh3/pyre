package tui

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Accounts view (␣a, :accounts): one section per provider with a summary
// line and a stacked mini-bar, then one row per account identity: a state
// chip, the label and kind, smooth 5-hour and weekly bars with the reset
// as relative time (absolute dim), and how many sessions use it. The
// selected row expands to its machines and by-machine readings, the
// sessions on it and the other windows. The owner's own login on a machine
// without Pyre accounts is one quiet line per provider. The shared meaning
// lives in internal/usage.

// usageCards derives the cards from the loaded accounts and machines.
func (m *Model) usageCards() []usage.Card {
	return usage.Cards(m.accounts(), m.nodes(), m.now())
}

// acctRow is one account identity: its logins (one card per machine, or a
// token/own-login account's single card) summarized by the best of them.
type acctRow struct {
	best  usage.Card
	cards []usage.Card
}

// rowRank orders login states: the best login of an account decides how
// its row reads (as usage.Pools counts it).
func rowRank(s usage.State) int {
	switch s {
	case usage.Healthy:
		return 0
	case usage.NearLimit:
		return 1
	case usage.AtLimit:
		return 2
	case usage.Unknown:
		return 3
	case usage.NeedsReauth:
		return 4
	}
	return 5
}

// accountRows groups the managed cards by account, ordered by provider and
// label (the selectable rows of the Accounts view).
func accountRows(cards []usage.Card) []acctRow {
	idx := map[string]int{}
	var rows []acctRow
	for _, c := range cards {
		if c.Account == nil {
			continue
		}
		id := c.Account.AccountID
		i, ok := idx[id]
		if !ok {
			idx[id] = len(rows)
			rows = append(rows, acctRow{best: c, cards: []usage.Card{c}})
			continue
		}
		r := &rows[i]
		r.cards = append(r.cards, c)
		br, cr := rowRank(r.best.State), rowRank(c.State)
		if cr < br || cr == br && c.Obs != nil && (r.best.Obs == nil || c.Obs.ObservedAt.After(r.best.Obs.ObservedAt)) {
			r.best = c
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].best, rows[j].best
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return strings.ToLower(a.Label()) < strings.ToLower(b.Label())
	})
	return rows
}

// windows is the row's 5-hour and weekly windows: the best login's, else
// the newest reading any of its logins has.
func (r acctRow) windows() (five, week *contracts.UsageWindow, obs *contracts.AccountObservation) {
	obs = r.best.Obs
	if obs == nil || len(obs.Windows) == 0 {
		for _, c := range r.cards {
			if c.Obs != nil && len(c.Obs.Windows) > 0 && (obs == nil || len(obs.Windows) == 0 || c.Obs.ObservedAt.After(obs.ObservedAt)) {
				obs = c.Obs
			}
		}
	}
	if obs == nil {
		return nil, nil, nil
	}
	sp := usage.Split(obs.Windows)
	return sp.FiveHour, sp.Weekly, obs
}

// assignments is every active assignment on the account, once per session.
func (r acctRow) assignments() []contracts.AccountAssignment {
	seen := map[string]bool{}
	var out []contracts.AccountAssignment
	for _, c := range r.cards {
		for _, a := range c.Assignments {
			if !seen[a.SessionID] {
				seen[a.SessionID] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// usageStatus is the chip of a login's state: glyph, word and tone.
func usageStatus(a usage.Assessment) status {
	switch a.State {
	case usage.Healthy:
		return status{"●", "ready", toneGood}
	case usage.NearLimit:
		return status{"◐", "near limit", toneWarn}
	case usage.AtLimit:
		return status{"✕", "at limit", toneBad}
	case usage.NeedsReauth:
		return status{"↻", "re-auth", toneBad}
	case usage.Disabled:
		return status{"–", "disabled", toneMuted}
	case usage.Ambient:
		return status{"○", "not managed", toneMuted}
	}
	return status{"○", "unknown", toneMuted}
}

// providerName is a provider as a section title.
func providerName(p contracts.Provider) string {
	switch p {
	case contracts.ProviderAnthropic:
		return "Anthropic"
	case contracts.ProviderOpenAI:
		return "OpenAI"
	}
	s := string(p)
	if s == "" {
		return "Other"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- bars ----

var eighths = [...]string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// barTone colors a used percent: calm accent below 60, amber to 85, red
// above.
func barTone(pct float64) tone {
	switch {
	case pct > 85:
		return toneBad
	case pct >= 60:
		return toneWarn
	}
	return toneAccent
}

// meter draws a smooth usage bar of n cells with partial blocks. The empty
// cells sit on a subtle track (256-color and truecolor), else faint ░.
// Last-known numbers are faint: they are not current capacity.
func (m *Model) meter(pct float64, n int, lastKnown bool) string {
	pct = max(0, min(100, pct))
	e := int(pct/100*float64(n*8) + 0.5)
	if e == 0 && pct > 0 {
		e = 1
	}
	full, part := e/8, e%8
	fill := strings.Repeat("█", full) + eighths[part]
	rest := n - full
	if part > 0 {
		rest--
	}
	st := m.th.tones[barTone(pct)]
	if lastKnown {
		st = m.th.faint
	}
	if m.th.hasTrack {
		bg := lipgloss.NewStyle().Background(m.th.track)
		out := ""
		if fill != "" {
			out = st.Background(m.th.track).Render(fill)
		}
		if rest > 0 {
			out += bg.Render(strings.Repeat(" ", rest))
		}
		return out
	}
	out := ""
	if fill != "" {
		out = st.Render(fill)
	}
	return out + m.th.faint.Render(strings.Repeat("░", max(0, rest)))
}

// miniBar is a provider's stacked pool bar: ready, near limit, at limit,
// re-auth and unknown accounts in proportion over n cells.
func (m *Model) miniBar(p usage.Pool, n int) string {
	type seg struct {
		count int
		tn    tone
		plain string
	}
	segs := []seg{{p.Ready, toneGood, "█"}, {p.Near, toneWarn, "▓"}, {p.AtLimit, toneBad, "▒"}, {p.Reauth, toneBad, "▒"}, {p.Unknown, toneMuted, "░"}}
	total := 0
	for _, s := range segs {
		total += s.count
	}
	if total == 0 {
		return ""
	}
	cells := make([]int, len(segs))
	used := 0
	for i, s := range segs {
		if s.count > 0 {
			cells[i] = max(1, s.count*n/total)
			used += cells[i]
		}
	}
	for used != n {
		// Grow the largest segment, or shrink it when the minimums overflow.
		big := 0
		for i := range cells {
			if cells[i] > cells[big] {
				big = i
			}
		}
		if used < n {
			cells[big]++
			used++
		} else if cells[big] > 1 {
			cells[big]--
			used--
		} else {
			break
		}
	}
	var b strings.Builder
	for i, s := range segs {
		if cells[i] == 0 {
			continue
		}
		if m.th.plain || m.th.mono {
			b.WriteString(strings.Repeat(s.plain, cells[i]))
			continue
		}
		st := m.th.tones[s.tn]
		if s.tn == toneMuted {
			st = m.th.faint
		}
		b.WriteString(st.Render(strings.Repeat("━", cells[i])))
	}
	return b.String()
}

// ---- layout ----

const (
	chipW  = 12 // "◐ near limit"
	kindW  = 10 // "own login"
	inUseW = 11 // "12 sessions"
	pctW   = 4
	relW   = 10 // "in 23h 59m"
	absW   = 12 // "Wed 11:20 PM"
	rowInd = 2 + chipW
)

// acctCols is the Accounts table layout for a width: the machines column
// and absolute reset times drop first, then the bars go under the name.
type acctCols struct {
	label, where, bar int
	abs, two          bool
}

func (c acctCols) winW() int {
	n := c.bar + 1 + pctW + 2 + relW
	if c.abs {
		n += 1 + absW
	}
	return n
}

func acctLayout(w int) acctCols {
	try := func(where int, abs bool, minBar int) (acctCols, bool) {
		c := acctCols{label: 22, where: where, abs: abs}
		rest := w - (2 + chipW + c.label + 1 + kindW + where + 2 + 2 + 2 + inUseW)
		c.bar = 0
		per := rest/2 - c.winW()
		if per < minBar {
			return c, false
		}
		c.bar = min(per, 24)
		c.label += min(8, max(0, rest-2*c.winW()))
		return c, true
	}
	if c, ok := try(14, true, 10); ok {
		return c
	}
	if c, ok := try(0, true, 10); ok {
		return c
	}
	if c, ok := try(0, false, 8); ok {
		return c
	}
	c := acctCols{label: 22, two: true}
	c.bar = max(4, min(16, (w-rowInd-3)/2-(5+1+pctW+2+relW)))
	return c
}

// ---- rows ----

// winCell renders one window: bar, % used, reset in relative time and
// (wide) the absolute time, faint.
func (m *Model) winCell(uw *contracts.UsageWindow, c acctCols, lastKnown bool) string {
	if uw == nil {
		return pad(m.th.faint.Render("not reported"), c.winW())
	}
	pct := padLeft(fmt.Sprintf("%.0f%%", uw.UsedPercent), pctW)
	switch {
	case lastKnown:
		pct = m.th.faint.Render(pct)
	case uw.UsedPercent >= 60:
		pct = m.th.tone(barTone(uw.UsedPercent), pct)
	}
	rel, abs := "", ""
	if uw.ResetAt != nil {
		now := m.now()
		if uw.ResetAt.After(now) {
			rel, abs = usage.In(now, *uw.ResetAt), usage.Clock(now, *uw.ResetAt)
		} else {
			rel = "reset passed"
		}
	}
	relS := m.th.dim.Render(pad(rel, relW))
	if uw.UsedPercent >= 100 && !lastKnown {
		relS = m.th.tone(toneBad, pad(rel, relW))
	}
	out := m.meter(uw.UsedPercent, c.bar, lastKnown) + " " + pct + "  " + relS
	if c.abs {
		out += " " + m.th.faint.Render(pad(abs, absW))
	}
	return out
}

// kindText is how the account is held: token, rota, login, own login.
func kindText(c usage.Card) string { return c.Kind() }

// whereText is the machines cell: "all machines", a machine, or "3 machines".
func (m *Model) whereText(r acctRow) string {
	if r.best.Collapsed() {
		return r.best.Machines
	}
	if len(r.cards) == 1 {
		if r.best.NodeID == "" {
			return "no machine yet"
		}
		return m.nodeName(r.best.NodeID)
	}
	return fmt.Sprintf("%d machines", len(r.cards))
}

// rowNote is what a row without usage bars says instead: why it is unknown,
// what re-auth needs, or that an own login's usage shows after a run.
func (m *Model) rowNote(c usage.Card) string {
	switch c.State {
	case usage.NeedsReauth:
		fix := "pyre accounts login"
		if c.Kind() == usage.KindToken {
			fix = "pyre accounts add-token"
		}
		return m.th.tone(toneBad, c.Why) + m.th.dim.Render(" · "+fix)
	case usage.Disabled:
		return m.th.dim.Render("disabled")
	case usage.AtLimit:
		if c.Until != nil {
			return m.th.tone(toneBad, "until "+usage.Clock(m.now(), *c.Until)) + m.th.dim.Render(" · "+usage.In(m.now(), *c.Until))
		}
		return m.th.tone(toneBad, c.Why) + m.th.dim.Render(" · reset time unknown")
	}
	if c.OnUse == usage.OwnUse && c.Obs == nil {
		return m.th.dim.Render("own login · usage shows after a Pyre run on it")
	}
	if c.Why != "" {
		return m.th.dim.Render(c.Why)
	}
	return m.th.dim.Render(string(c.OnUse))
}

// inUseText is the compact "2 sessions" count.
func (m *Model) inUseText(n int) string {
	if n == 0 {
		return ""
	}
	return m.th.accent.Render(plural(n, "session", "sessions"))
}

// rowLines renders one identity row (and, selected, its details).
func (m *Model) rowLines(r acctRow, sel bool, c acctCols, w, idx int) []string {
	b := r.best
	st := usageStatus(b.Assessment)
	word := pad(st.label, chipW-2)
	switch st.tone {
	case toneMuted:
		word = m.th.dim.Render(word)
	default:
		word = m.th.tone(st.tone, word)
	}
	chip := m.th.tone(st.tone, st.sym) + " " + word
	name := m.primary(sel, pad(trunc(b.Label(), c.label-1), c.label))
	kind := m.th.dim.Render(pad(kindText(b), kindW))
	head := m.cursor(sel) + chip + name + " " + kind
	five, week, obs := r.windows()
	bars := obs != nil && (five != nil || week != nil) && b.State != usage.Disabled && b.State != usage.NeedsReauth
	inUse := m.inUseText(len(r.assignments()))
	var out []string
	switch {
	case c.two:
		out = append(out, head+inUse)
		if bars {
			out = append(out, strings.Repeat(" ", rowInd)+m.th.dim.Render(pad("5h", 5))+m.winCell(five, c, b.LastKnown)+"   "+
				m.th.dim.Render(pad("week", 5))+m.winCell(week, c, b.LastKnown))
		} else {
			out = append(out, strings.Repeat(" ", rowInd)+m.rowNote(b))
		}
	default:
		if c.where > 0 {
			head += m.th.dim.Render(pad(trunc(m.whereText(r), c.where-2), c.where))
		}
		head += "  "
		if bars {
			head += m.winCell(five, c, b.LastKnown) + "  " + m.winCell(week, c, b.LastKnown) + "  "
		} else {
			head += pad(m.rowNote(b), 2*c.winW()+4)
		}
		out = append(out, head+inUse)
	}
	if sel {
		out = append(out, m.rowDetails(r, w)...)
	}
	for i := range out {
		out[i] = trunc(out[i], w)
	}
	out[0] = m.markRow(&m.mgmtList, idx, sel, out[0], w)
	return out
}

// rowDetails is the selected row's expansion: machines with by-machine
// readings, the sessions on it, the other windows and the reading's source.
func (m *Model) rowDetails(r acctRow, w int) []string {
	const lw = 10
	now := m.now()
	ind := strings.Repeat(" ", rowInd)
	vw := max(10, w-rowInd-lw)
	var out []string
	add := func(label, value string) {
		out = append(out, ind+m.kvl(label, trunc(value, vw), lw))
	}
	b := r.best
	reading := func(c usage.Card) string {
		var parts []string
		if c.Obs != nil {
			sp := usage.Split(c.Obs.Windows)
			if sp.FiveHour != nil {
				parts = append(parts, fmt.Sprintf("5h %.0f%%", sp.FiveHour.UsedPercent))
			}
			if sp.Weekly != nil {
				parts = append(parts, fmt.Sprintf("week %.0f%%", sp.Weekly.UsedPercent))
			}
			src := "observed " + ago(now, c.Obs.ObservedAt)
			if c.Obs.Source != "" {
				src += " via " + c.Obs.Source
			}
			parts = append(parts, src)
		}
		return strings.Join(parts, " · ")
	}
	if b.Collapsed() {
		v := b.Machines
		if s := reading(b); s != "" {
			v += m.th.dim.Render("  " + s)
		}
		add("machines", v)
	} else {
		for i, c := range r.cards {
			st := usageStatus(c.Assessment)
			name := m.nodeName(c.NodeID)
			if c.NodeID == "" {
				name = "no machine yet"
			}
			v := pad(name, 14)
			if n := m.node(c.NodeID); n != nil && n.Connection.State != contracts.ConnConnected {
				ns := nodeStatus(*n)
				v = pad(name+" "+m.th.tone(ns.tone, ns.sym), 14)
			}
			v += m.th.tone(st.tone, st.sym+" "+pad(st.label, chipW-2))
			if s := reading(c); s != "" {
				v += m.th.dim.Render(s)
			}
			label := ""
			if i == 0 {
				label = "machines"
			}
			add(label, v)
		}
	}
	for i, a := range r.assignments() {
		on := ""
		if a.NodeID != "" {
			on = " on " + m.nodeName(a.NodeID)
		}
		label := ""
		if i == 0 {
			label = "in use"
		}
		add(label, m.sessionTitle(a.SessionID)+m.th.dim.Render(on+" · since "+ago(now, a.AssignedAt)))
	}
	if _, _, obs := r.windows(); obs != nil {
		var extra []string
		sp := usage.Split(obs.Windows)
		for _, s := range sp.Scoped {
			extra = append(extra, fmt.Sprintf("%s %.0f%%", usage.ScopedLabel(s.Name), s.UsedPercent))
		}
		for _, o := range sp.Other {
			extra = append(extra, fmt.Sprintf("%s %.0f%%", windowLabel(o.Name), o.UsedPercent))
		}
		if sp.Extra != nil {
			extra = append(extra, fmt.Sprintf("extra usage %.0f%% (not a limit)", sp.Extra.UsedPercent))
		}
		if len(extra) > 0 {
			add("windows", m.th.dim.Render(strings.Join(extra, " · ")))
		}
	}
	if note := m.detailNote(b); note != "" {
		add("note", note)
	}
	return append(out, "")
}

// detailNote explains the state when the row alone does not: unknown and
// why, last-known numbers, usage read at use.
func (m *Model) detailNote(c usage.Card) string {
	var parts []string
	switch c.State {
	case usage.Unknown:
		parts = append(parts, c.Why)
		if c.LastKnown {
			parts = append(parts, "bars show last known usage")
		}
	case usage.AtLimit:
		if c.Until == nil {
			parts = append(parts, c.Why+" · reset time unknown")
		}
	case usage.NeedsReauth:
		return m.rowNote(c)
	case usage.Healthy, usage.NearLimit:
		switch c.OnUse {
		case "":
		case usage.OwnUse:
			parts = append(parts, "your own login")
			if c.Obs == nil {
				parts = append(parts, "usage shows after a Pyre run on it")
			}
		default:
			parts = append(parts, string(c.OnUse))
		}
		if c.OnUse != "" && c.LastKnown {
			parts = append(parts, "last known usage")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return m.th.dim.Render(strings.Join(parts, " · "))
}

// ---- sections ----

// poolTitle is a provider's summary line: "Anthropic · 7 ready · 1 at
// limit" (counts toned like their chips) and the stacked mini-bar.
func (m *Model) poolTitle(prov contracts.Provider, p usage.Pool, ok bool) string {
	sep := m.th.faint.Render(" · ")
	s := "  " + m.th.heading.Render(providerName(prov))
	if !ok {
		return s
	}
	parts := []string{fmt.Sprintf("%d ready", p.Ready)}
	if p.Tight() {
		parts[0] = m.th.tone(toneBad, parts[0])
	}
	if p.Near > 0 {
		parts = append(parts, m.th.tone(toneWarn, fmt.Sprintf("%d near limit", p.Near)))
	}
	if p.AtLimit > 0 {
		parts = append(parts, m.th.tone(toneBad, fmt.Sprintf("%d at limit", p.AtLimit)))
	}
	if p.Reauth > 0 {
		parts = append(parts, m.th.tone(toneBad, fmt.Sprintf("%d needs re-auth", p.Reauth)))
	}
	if p.Unknown > 0 {
		parts = append(parts, m.th.dim.Render(fmt.Sprintf("%d unknown", p.Unknown)))
	}
	if p.Tight() {
		parts = append(parts, m.th.tone(toneBad, "nothing available"))
	}
	s += sep + strings.Join(parts, sep)
	if bar := m.miniBar(p, 16); bar != "" {
		s += "   " + bar
	}
	return s
}

// acctColHeader is the faint column header under a provider's summary.
func (m *Model) acctColHeader(c acctCols) string {
	if c.two {
		return ""
	}
	h := strings.Repeat(" ", rowInd) + pad("account", c.label) + " " + pad("kind", kindW)
	if c.where > 0 {
		h += pad("machines", c.where)
	}
	h += "  " + pad("5-hour", c.winW()) + "  " + pad("weekly", c.winW()) + "  in use"
	return m.th.faint.Render(h)
}

func (m *Model) viewAccounts(w, h int) []string {
	all := m.usageCards()
	rows := accountRows(all)
	ambient := map[contracts.Provider][]string{}
	var cards []usage.Card
	for _, c := range all {
		if c.Account == nil {
			ambient[c.Provider] = append(ambient[c.Provider], c.NodeID)
			continue
		}
		cards = append(cards, c)
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.best.Account.AccountID
	}
	m.mgmtList.setIDs(ids)
	_, st := get[[]contracts.AccountView](m, "accounts")
	head := []string{m.mgmtHeader("Accounts", st), ""}
	if len(all) == 0 {
		return append(head, m.loadingOr(st, "no accounts configured — pyre accounts register"))
	}
	pools := map[contracts.Provider]usage.Pool{}
	for _, p := range usage.Pools(cards) {
		pools[p.Provider] = p
	}
	var provs []contracts.Provider
	for _, c := range all {
		if len(provs) == 0 || provs[len(provs)-1] != c.Provider {
			provs = append(provs, c.Provider)
		}
	}
	cols := acctLayout(w)
	var lines []string
	selTop, selEnd := 0, 0
	for pi, prov := range provs {
		if pi > 0 {
			lines = append(lines, "")
		}
		p, ok := pools[prov]
		lines = append(lines, trunc(m.poolTitle(prov, p, ok), w))
		started := false
		for i, r := range rows {
			if r.best.Provider != prov {
				continue
			}
			if !started {
				if ch := m.acctColHeader(cols); ch != "" {
					lines = append(lines, trunc(ch, w))
				}
				started = true
			}
			sel := i == m.mgmtList.idx
			rl := m.rowLines(r, sel, cols, w, i)
			if sel {
				selTop, selEnd = len(lines), len(lines)+len(rl)
			}
			lines = append(lines, rl...)
		}
		if ns := ambient[prov]; len(ns) > 0 {
			var names []string
			for _, id := range ns {
				names = append(names, m.nodeName(id))
			}
			text := "your own login on " + strings.Join(names, ", ") + " (not managed) · usage unknown; Pyre does not observe it"
			lines = append(lines, "")
			for i, l := range wrap(text, max(20, w-6)) {
				lead := "  " + m.th.dim.Render("○") + " "
				if i > 0 {
					lead = "    "
				}
				lines = append(lines, lead+m.th.dim.Render(l))
			}
		}
	}
	foot := "  Pyre keeps each machine on its own account below 80%, else the most 5-hour, then weekly headroom. Credentials never leave their machine."
	for _, c := range cards {
		if c.Kind() == usage.KindToken {
			foot = "  Pyre keeps each machine on its own account below 80%, else the most 5-hour, then weekly headroom. Logins stay on their machine; tokens stay encrypted on the hub."
			break
		}
	}
	if sw := m.recentSwitchLines(w); len(sw) > 0 { // activity.go
		lines = append(lines, "")
		lines = append(lines, sw...)
	}
	lines = append(lines, "", m.th.faint.Render(trunc(foot, w)))
	// Scroll so the selected row is in view (mgmtList.top is the line offset here).
	room := h - len(head)
	top := m.mgmtList.top
	if selTop-1 < top {
		top = max(0, selTop-1)
	}
	if selEnd > top+room {
		top = selEnd - room
	}
	top = max(0, min(top, len(lines)-room))
	m.mgmtList.top = top
	end := min(len(lines), top+room)
	return append(head, lines[top:end]...)
}

// ---- header pool summary ----

// poolSummary is the compact header indicator: "anthropic 3 ready · 1 at
// limit   openai 1 ready". The ready count is calm; the limit part is
// amber only when an account is near or at its limit, and the count red
// only when nothing is available. Clicking it opens Accounts.
func (m *Model) poolSummary() string {
	pools := usage.Pools(m.usageCards())
	if len(pools) == 0 {
		return ""
	}
	var parts []string
	for _, p := range pools {
		ready := fmt.Sprintf("%d ready", p.Ready)
		switch {
		case p.Tight():
			ready = m.th.tone(toneBad, ready)
		case p.Ready == 0:
			ready = m.th.dim.Render(ready)
		}
		s := m.th.dim.Render(string(p.Provider)+" ") + ready
		if bp := strings.Split(p.Brief(), " · "); len(bp) > 1 {
			s += m.th.faint.Render(" · ") + m.th.tone(toneWarn, bp[1])
		}
		parts = append(parts, s)
	}
	return m.mark(zoneDef{kind: zoneTab, view: viewAccounts}, strings.Join(parts, "   "))
}
