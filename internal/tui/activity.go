package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/audit"
	"github.com/jhh3/pyre/internal/contracts"
)

// Activity (␣l, :activity, a in Accounts): the owner-facing audit log —
// what Pyre did on its own and changes to fleet state, newest first. f
// cycles all · accounts · sessions · fleet; automatic entries carry ↻ and
// account switches (and limits with nowhere to switch) are amber so they
// stand out. The selected entry's who/what/why/where/when is in the detail
// pane. Hubs without audit.list say so instead of showing an empty log.

// activityFilters are the filter row's entries, in cycling order.
var activityFilters = []struct {
	name string
	kind contracts.AuditKind // "" = every kind
}{{"all", ""}, {"accounts", contracts.AuditAccount}, {"sessions", contracts.AuditSession}, {"fleet", contracts.AuditFleet}}

const (
	auditPageLimit = 200
	auditMaxPages  = 3
	// auditMaxAge bounds how old a shown log may get between periodic
	// refreshes; events invalidate it sooner (events.go: "audit:").
	auditMaxAge = 30 * time.Second
	autoGlyph   = "↻"
)

type activityState struct {
	filter int // index into activityFilters
}

// auditAPI is the hub's audit log, or nil when this hub does not offer it.
func (m *Model) auditAPI() contracts.AuditAPI {
	a, _ := m.api.(contracts.AuditAPI)
	return a
}

// auditUnsupported: the API lacks audit.list, or the hub answered that it
// does not have it (an older pyre-hub).
func (m *Model) auditUnsupported(st *loadState) bool {
	return m.auditAPI() == nil || (st != nil && !st.have && st.err != nil && contracts.IsCode(st.err, contracts.ErrUnsupported))
}

// auditLoader lists the newest entries of kinds (all when empty), following
// NextCursor for at most auditMaxPages pages.
func (m *Model) auditLoader(kinds ...contracts.AuditKind) loaderFn {
	a := m.auditAPI()
	if a == nil {
		return nil
	}
	return func(ctx context.Context) (any, error) {
		var out []contracts.AuditEntry
		cur := ""
		for i := 0; i < auditMaxPages; i++ {
			p, err := a.ListAudit(ctx, contracts.ListAuditRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: auditPageLimit}, Kinds: kinds})
			if err != nil {
				return nil, err
			}
			out = append(out, p.Items...)
			if p.NextCursor == "" || p.NextCursor == cur {
				break
			}
			cur = p.NextCursor
		}
		return out, nil
	}
}

func (m *Model) activityKey() string { return "audit:" + activityFilters[m.act.filter].name }

func (m *Model) loadAudit(key string, force bool, fn loaderFn) tea.Cmd {
	if fn == nil {
		return nil
	}
	if force {
		return m.reload(key, fn)
	}
	return m.ensure(key, auditMaxAge, fn)
}

// loadActivity loads the Activity view's current filter.
func (m *Model) loadActivity(force bool) tea.Cmd {
	f := activityFilters[m.act.filter]
	var kinds []contracts.AuditKind
	if f.kind != "" {
		kinds = append(kinds, f.kind)
	}
	return m.loadAudit(m.activityKey(), force, m.auditLoader(kinds...))
}

// loadRecentSwitches loads the account entries behind the Accounts view's
// RECENT SWITCHES section.
func (m *Model) loadRecentSwitches(force bool) tea.Cmd {
	return m.loadAudit("audit:switches", force, m.auditLoader(contracts.AuditAccount))
}

// openActivity opens Activity with a filter ("" keeps the current one).
// From a management view it pushes, so esc returns there.
func (m *Model) openActivity(filter string) tea.Cmd {
	for i, f := range activityFilters {
		if f.name == filter {
			m.act.filter = i
		}
	}
	if m.view == viewActivity {
		m.mgmtList = listState{}
		return m.loadActivity(false)
	}
	if m.view == viewAccounts || m.view == viewMachines || m.view == viewConnections || m.view == viewEnvs {
		m.mgmtList = listState{}
		m.focus = 0
		return m.push(viewActivity)
	}
	return m.openManage(viewActivity)
}

// activityFromManage is manage.activity: Accounts opens the account
// entries, Machines the fleet ones, anything else everything.
func (m *Model) activityFromManage() tea.Cmd {
	switch m.view {
	case viewAccounts:
		return m.openActivity("accounts")
	case viewMachines:
		return m.openActivity("fleet")
	case viewConnections, viewEnvs:
		return m.openActivity("all")
	}
	return nil
}

// cycleActivityFilter is manage.filter (f) in Activity.
func (m *Model) cycleActivityFilter() tea.Cmd {
	if m.view != viewActivity {
		return nil
	}
	m.act.filter = (m.act.filter + 1) % len(activityFilters)
	m.mgmtList = listState{}
	m.detailOff = 0
	return m.loadActivity(false)
}

func (m *Model) keyActivity(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if navKey(&m.mgmtList, key, m.halfPage()) {
		return nil
	}
	switch key {
	case "esc":
		return m.goBack()
	case "r":
		return m.refreshVisible(true)
	case "f":
		return m.cycleActivityFilter()
	}
	return nil
}

// isSwitch: an automatic account switch, or a limit with no account to
// switch to — the entries the owner looks for first.
func isSwitch(e contracts.AuditEntry) bool {
	return e.Action == contracts.AuditAccountSwitched || e.Action == contracts.AuditAccountWaiting
}

func auditSummary(e contracts.AuditEntry) string {
	if e.Summary != "" {
		return e.Summary
	}
	return audit.Summarize(e)
}

func auditKey(e contracts.AuditEntry) string {
	if e.AuditID != 0 {
		return strconv.FormatInt(e.AuditID, 10)
	}
	return e.At.Format(time.RFC3339Nano) + "|" + e.Action + "|" + e.SessionID + "|" + e.NodeID
}

// filterCounts renders "all 23 · accounts 9 · sessions 8 · fleet 6" with
// the active filter marked. Counts come from the active filter's list and,
// for the others, from the unfiltered log once it has loaded ("+" when a
// list was capped).
func (m *Model) filterCounts() string {
	count := func(es []contracts.AuditEntry) string {
		s := itoa(len(es))
		if len(es) >= auditPageLimit*auditMaxPages {
			s += "+"
		}
		return s
	}
	allEntries, allSt := get[[]contracts.AuditEntry](m, "audit:all")
	var parts []string
	for i, f := range activityFilters {
		label := f.name
		if es, st := get[[]contracts.AuditEntry](m, "audit:"+f.name); st.ok() {
			label += " " + count(es)
		} else if allSt.ok() && len(allEntries) < auditPageLimit*auditMaxPages {
			var es []contracts.AuditEntry
			for _, e := range allEntries {
				if e.Kind == f.kind || (f.kind == contracts.AuditFleet && filterIndex(e.Kind) == len(activityFilters)-1) {
					es = append(es, e)
				}
			}
			label += " " + count(es)
		}
		parts = append(parts, m.plainTab(label, i == m.act.filter))
	}
	return strings.Join(parts, " ")
}

func filterIndex(k contracts.AuditKind) int {
	for i, f := range activityFilters {
		if f.kind == k && k != "" {
			return i
		}
	}
	return len(activityFilters) - 1 // unknown kinds count as fleet
}

func (m *Model) viewActivity(w, h int) []string {
	key := m.activityKey()
	entries, st := get[[]contracts.AuditEntry](m, key)
	head := []string{m.mgmtHeader("Activity", st), " " + m.filterCounts() + "  " + m.th.key.Render("f") + m.th.dim.Render(" cycles")}
	if m.auditUnsupported(st) {
		return append(head, "", m.th.dim.Render("  this hub does not offer the activity log; upgrade pyre-hub"))
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = auditKey(e)
	}
	m.mgmtList.setIDs(ids)
	now := m.now()
	list := func(w, h int) []string {
		if len(entries) == 0 {
			return []string{m.loadingOr(st, "nothing recorded yet")}
		}
		whenW, whereW := 5, 0
		start, end := m.mgmtList.window(h)
		for _, e := range entries[start:end] {
			whenW = max(whenW, width(audit.When(e.At, now)))
			whereW = max(whereW, min(14, width(audit.Where(e))))
		}
		var out []string
		for i := start; i < end; i++ {
			e := entries[i]
			sel := i == m.mgmtList.idx
			mark := " "
			if e.Automatic {
				mark = m.th.accent.Render(autoGlyph)
			}
			lead := m.th.dim.Render(pad(audit.When(e.At, now), whenW))
			if whereW > 0 {
				lead += " " + m.th.dim.Render(pad(trunc(audit.Where(e), whereW), whereW))
			}
			sum := auditSummary(e)
			room := max(8, w-2-width(lead)-3)
			switch {
			case isSwitch(e):
				sum = m.th.tone(toneWarn, trunc(sum, room))
			default:
				sum = m.primary(sel, trunc(sum, room))
			}
			row := m.cursor(sel) + lead + " " + mark + " " + sum
			out = append(out, m.markRow(&m.mgmtList, i, sel, trunc(row, w), w))
		}
		return out
	}
	detail := func(w, h int) []string {
		i := m.mgmtList.idx
		if len(entries) == 0 || i < 0 || i >= len(entries) {
			return nil
		}
		return m.auditDetail(entries[i], w)
	}
	body := m.activityPanes(w, h-len(head), len(entries), list, detail)
	return append(head, body...)
}

// activityPanes: side by side when there is room for the summaries (≥ 150
// columns), else the list (n rows) above the selected entry's detail, which
// gets at least 12 lines (or half the height) and whatever the list leaves.
func (m *Model) activityPanes(w, h, n int, list, detail func(w, h int) []string) []string {
	if w >= 150 {
		return m.listDetailW(w, h, w*60/100, list, detail)
	}
	dh := min(12, max(4, h/2))
	lh := max(2, min(max(n, 1), h-dh-1))
	dh = h - lh - 1
	out := m.markPane("list", fit(list(w, lh), w, lh))
	rule := m.th.faint.Render(strings.Repeat("─", w))
	if m.focus == 1 {
		rule = m.th.accent.Render(strings.Repeat("━", w))
	}
	out = append(out, rule)
	return append(out, m.markPane("detail", fit(m.scrolled(detail, w, dh), w, dh))...)
}

// auditActor names who did it.
func auditActor(e contracts.AuditEntry) string {
	a := e.Actor
	var s string
	switch a.Kind {
	case "owner":
		s = "owner"
		if a.Name != "" && a.Name != "owner" {
			s += " (" + a.Name + ")"
		}
	case "session":
		s = "session"
		if a.Name != "" {
			s += " '" + a.Name + "'"
		}
		if a.ID != "" {
			s += " " + a.ID
		}
	case "node":
		s = "machine " + orDash(a.Name)
	case "pyre", "":
		s = "Pyre"
	default:
		s = a.Kind + " " + a.Name
	}
	if e.Automatic {
		s += " · automatic (nobody asked)"
	}
	return s
}

func auditWithID(label, id string) string {
	switch {
	case label == "":
		return id
	case id == "" || id == label:
		return label
	}
	return label + "  " + id
}

func auditWithNote(s, note string) string {
	if note == "" {
		return s
	}
	return s + " (" + note + ")"
}

// auditDetail is the selected entry: the summary, then who/what/why/
// where/when and the IDs it refers to.
func (m *Model) auditDetail(e contracts.AuditEntry, w int) []string {
	const lw = 9
	style := m.th.title.Render
	if isSwitch(e) {
		style = func(s ...string) string { return m.th.tone(toneWarn, strings.Join(s, "")) }
	}
	var out []string
	for _, l := range wrap(auditSummary(e), max(10, w)) {
		out = append(out, style(l))
	}
	out = append(out, "")
	now := m.now()
	when := e.At.Local().Format("Mon Jan 2 2006 15:04:05")
	out = append(out, m.kvFit("When", when+m.th.dim.Render(" · "+ago(now, e.At)), lw, w)...)
	if where := audit.Where(e); where != "" {
		out = append(out, m.kvFit("Where", auditWithID(where, e.NodeID), lw, w)...)
	}
	out = append(out, m.kvFit("Who", auditActor(e), lw, w)...)
	out = append(out, m.kvFit("What", e.Action+m.th.dim.Render(" · "+string(e.Kind)), lw, w)...)
	if e.Reason != "" {
		out = append(out, m.kvWrap("Why", e.Reason, m.th.dim.Render, lw, w)...)
	}
	if e.SessionID != "" {
		t := e.SessionTitle
		if t != "" {
			t = "'" + t + "'"
		}
		out = append(out, m.kvFit("Session", auditWithID(t, e.SessionID), lw, w)...)
	}
	if e.RunID != "" {
		out = append(out, m.kvFit("Run", e.RunID, lw, w)...)
	}
	if e.AccountID != "" && e.AccountID != e.FromAccountID {
		out = append(out, m.kvFit("Account", auditWithID(e.AccountLabel, e.AccountID), lw, w)...)
	}
	if e.From != "" || e.FromAccountID != "" || e.FromNote != "" {
		out = append(out, m.kvFit("From", auditWithNote(auditWithID(e.From, e.FromAccountID), e.FromNote), lw, w)...)
	}
	if e.To != "" || e.ToAccountID != "" || e.ToNote != "" {
		out = append(out, m.kvFit("To", auditWithNote(auditWithID(e.To, e.ToAccountID), e.ToNote), lw, w)...)
	}
	return out
}

// recentSwitchLines is the Accounts view's Recent switches section: the
// last three automatic switches (or limits with nowhere to switch). It is
// omitted when the hub has no audit log (or it has not loaded).
func (m *Model) recentSwitchLines(w int) []string {
	entries, st := get[[]contracts.AuditEntry](m, "audit:switches")
	if m.auditAPI() == nil || !st.ok() {
		return nil
	}
	out := []string{m.th.heading.Render("  Recent switches") + m.th.faint.Render(" · ") + m.th.key.Render("a") + m.th.faint.Render(" all activity")}
	now := m.now()
	n := 0
	for _, e := range entries {
		if !isSwitch(e) {
			continue
		}
		out = append(out, "    "+m.th.tone(toneWarn, autoGlyph)+" "+m.th.dim.Render(trunc(audit.Line(e, now), max(10, w-6))))
		if n++; n == 3 {
			break
		}
	}
	if n == 0 {
		out = append(out, m.th.faint.Render("    no automatic account switches yet"))
	}
	return out
}
