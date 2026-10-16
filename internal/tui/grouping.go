package tui

import (
	"path"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Session grouping. The Sessions list groups rows by project (the default:
// projects by name, sessions in creation order inside, "No project" last), by machine, by repository (derived from the run's directory) or not
// at all (a flat list ordered by what needs you, then recent activity). gb
// cycles the mode, :group sets it; an explicit choice is remembered in the
// state file. Group headers are quiet: a faint name, a count, a status
// summary (project) or health (machine) and an amber dot when something in
// the group needs you. Outside machine mode each row names its machine and
// a footer line keeps every machine's health dot in view.
// za, enter on a collapsed group, or a click on a header folds a group; a
// folded group is one selectable row. Pinned sessions (pinning.go) sit in
// a "Pinned" group on top in every mode.

type groupMode string

const (
	groupMachine   groupMode = "machine"
	groupRepo      groupMode = "repo"
	groupProject   groupMode = "project"
	groupAttention groupMode = "attention" // needs you, message for you, working, idle, ended (sort.go)
	groupNone      groupMode = "none"
)

// groupModes is the gb cycle, starting from the default.
var groupModes = []groupMode{groupProject, groupAttention, groupMachine, groupRepo, groupNone}

// groupStateVersion marks state files whose "group" is an explicit choice.
// Before version 2 machine was the default and was never written, so an
// older file without a group (or with "machine") has made no choice and
// gets the new default, project.
const groupStateVersion = 2

const (
	// pinnedGroup is the group key of the Pinned group (sorts first).
	pinnedGroup = "\x00pinned"
	// noGroup is the key of sessions without a repo or project; it sorts
	// last and still gets a header.
	noGroup = "\x7f"
	// groupRowPrefix marks a folded group's row in the Sessions list ids.
	groupRowPrefix = "@group:"
)

// groupingState is the Sessions list's grouping (Model.grouping).
type groupingState struct {
	mode      groupMode                                   // "" until chosen: the default (project)
	collapsed map[string]bool                             // mode + ":" + key
	counts    map[string]int                              // sessions per group (last sync)
	needs     map[string]bool                             // groups with a session that needs you
	leads     map[string][]string                         // group → its top-level rows (lead signals, leads.go)
	sort      sortMode                                    // order inside groups ("" = name; sort.go)
	kinds     map[string]map[contracts.AttentionKind]bool // open attention per session (last sort)
	trees     map[string]bool                             // parents whose children are folded (tree.go)
}

func groupRowID(key string) string { return groupRowPrefix + key }

func groupOfRow(id string) (string, bool) {
	if !strings.HasPrefix(id, groupRowPrefix) {
		return "", false
	}
	return strings.TrimPrefix(id, groupRowPrefix), true
}

func parseGroupMode(s string) (groupMode, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "flat", "off":
		s = "none"
	case "machines", "node", "host":
		s = "machine"
	case "repos", "repository", "dir", "directory":
		s = "repo"
	case "projects":
		s = "project"
	case "needs", "needs-you", "needsyou", "status", "state":
		s = "attention"
	}
	for _, g := range groupModes {
		if string(g) == s {
			return g, true
		}
	}
	return "", false
}

func (m *Model) groupMode() groupMode {
	if m.grouping.mode == "" {
		return groupProject
	}
	return m.grouping.mode
}

// ---- derivation ----

// repoFromPath names the repository a directory belongs to: the
// directory's own name, except that a worktree maps to its repository
// (<repo>/.claude/worktrees/x, <repo>/.worktrees/x, and Pyre's own
// <repo>-worktrees/x), and a home directory reads "~". "" when unknown.
func repoFromPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = "/home/~" + strings.TrimPrefix(p, "~")
	}
	p = path.Clean(p)
	if p == "/" || p == "." {
		return ""
	}
	for _, mk := range []string{"/.claude/worktrees/", "/.worktrees/", "/.git/worktrees/"} {
		if i := strings.Index(p+"/", mk); i > 0 && i+len(mk) < len(p)+1 {
			return repoFromPath(p[:i])
		}
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if r := strings.TrimSuffix(parts[i], "-worktrees"); r != parts[i] && r != "" {
			return r
		}
	}
	if (len(parts) == 2 && (parts[0] == "home" || parts[0] == "Users")) || p == "/root" {
		return "~"
	}
	return parts[len(parts)-1]
}

// sessionRepo is the repository of s's current run ("" without a run).
func sessionRepo(s contracts.SessionView) string {
	if s.Run == nil {
		return ""
	}
	if co := s.Run.Checkout; co != nil && co.Path != "" {
		if r := repoFromPath(co.Path); r != "" {
			return r
		}
	}
	return repoFromPath(s.Run.Cwd)
}

// groupKey is s's group in the current mode, ignoring pins.
func (m *Model) groupKey(s contracts.SessionView) string {
	switch m.groupMode() {
	case groupRepo:
		if r := sessionRepo(s); r != "" {
			return r
		}
		return noGroup
	case groupProject:
		if s.Session.ProjectID != nil && *s.Session.ProjectID != "" {
			return *s.Session.ProjectID
		}
		return noGroup
	case groupAttention:
		return attnGroupKeys[m.attentionClass(s, m.grouping.kinds)]
	case groupNone:
		return ""
	}
	return s.NodeName
}

// sessionGroup is s's group in the list: Pinned, else its mode group.
func (m *Model) sessionGroup(s contracts.SessionView) string {
	if m.pinned(s) { // pinned, or under a pinned parent (tree.go)
		return pinnedGroup
	}
	return m.groupKey(s)
}

// groupLabel names a group for people.
func (m *Model) groupLabel(key string) string {
	switch {
	case key == pinnedGroup:
		return "Pinned"
	case m.groupMode() == groupNone:
		return "Others"
	case m.groupMode() == groupAttention:
		return attnGroupLabels[key]
	case key == noGroup && m.groupMode() == groupProject:
		return "No project"
	case key == noGroup:
		return "no directory"
	case m.groupMode() == groupProject:
		return m.projectName(&key)
	case key == "":
		return "no machine"
	}
	return key
}

// ---- ordering ----

// sortGrouped orders sessions for the list: Pinned first (by pin time),
// then the mode's groups — projects, machines and repos by name — with
// unknowns last; inside a project rows list in creation order (owner
// request 2026-10-13: nothing reorders while you watch — a new session
// appends, a rename or a run start moves nothing); inside other groups by
// machine, then title; the flat list by needs-you then recent activity.
func (m *Model) sortGrouped(ss []contracts.SessionView) {
	sortSessionsByMachine(ss)
	mode := m.groupMode()
	m.grouping.kinds = m.openAttnKinds()
	srt := m.grouping.sort
	if srt == "" && mode == groupNone {
		srt = sortAttention // the flat list: needs you, then recent activity
	}
	type keyed struct {
		pin     bool
		at      int64
		created string // project mode, name sort: creation order (time-sortable session ID)
		group   string
		class   attnClass // attention sort
		needs   bool
		act     time.Time
	}
	ks := make(map[string]keyed, len(ss))
	for _, s := range ss {
		k := keyed{}
		k.pin, k.at = m.pinState(s)
		if !k.pin {
			k.group = strings.ToLower(m.groupLabel(m.groupKey(s))) + "\x00" + m.groupKey(s)
			if g := m.groupKey(s); g == noGroup || (mode == groupMachine && g == "") {
				k.group = "￿" // unknown groups last
			} else if mode == groupProject && (srt == "" || srt == sortName) {
				k.created = s.Session.SessionID
			}
			if mode == groupAttention {
				k.group = m.groupKey(s) // the classes in their fixed order
			}
			switch srt {
			case sortAttention:
				k.class = m.attentionClass(s, m.grouping.kinds)
				k.needs = k.class <= attnMessage
				if mode == groupNone {
					k.act = m.lastActivity(s)
				}
			case sortActivity:
				k.act = m.lastActivity(s)
			case sortYours:
				k.act = ownerActive(s) // never: last, by machine and title
			}
		}
		ks[s.Session.SessionID] = k
	}
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ks[ss[i].Session.SessionID], ks[ss[j].Session.SessionID]
		if a.pin != b.pin {
			return a.pin
		}
		if a.pin {
			if (a.at == 0) != (b.at == 0) {
				return a.at != 0
			}
			return a.at < b.at
		}
		if a.group != b.group {
			return a.group < b.group
		}
		if a.created != b.created {
			return a.created < b.created
		}
		if a.class != b.class {
			return a.class < b.class
		}
		if !a.act.Equal(b.act) {
			return a.act.After(b.act)
		}
		return false
	})
	// Children under their parents, in the same order (tree.go).
	groups := make([]string, len(ss))
	for i, s := range ss {
		groups[i] = m.sessionGroup(s)
	}
	ordered, _ := treeLayout(ss, groups)
	copy(ss, ordered)
}

// lastActivity is the latest known activity of s: a state change the Grid
// saw, else when its run started, else when it was created.
func (m *Model) lastActivity(s contracts.SessionView) time.Time {
	t := s.Session.CreatedAt
	if s.Run != nil && s.Run.StartedAt.After(t) {
		t = s.Run.StartedAt
	}
	if a, ok := m.grid.active[s.Session.SessionID]; ok && a.After(t) {
		t = a
	}
	return t
}

// ---- rows ----

func (m *Model) groupCollapsed(key string) bool {
	return m.filter == "" && m.grouping.collapsed[string(m.groupMode())+":"+key]
}

// groupedIDs lays the visible sessions out as list rows: a folded group is
// one row; in machine mode with ended sessions shown, each machine with
// ended sessions gets its selectable header row (X archives them).
func (m *Model) groupedIDs(vs []contracts.SessionView) []string {
	g := &m.grouping
	g.counts, g.needs = map[string]int{}, map[string]bool{}
	g.leads = map[string][]string{}
	keys := make([]string, len(vs))
	for i, s := range vs {
		k := m.sessionGroup(s)
		keys[i] = k
		g.counts[k]++
		if gridNeedsYou(s, m.nodeOf(s)) {
			g.needs[k] = true
		}
	}
	m.layoutTrees(vs, keys) // tree.go
	for i, s := range vs {
		if r := m.tree.rows[s.Session.SessionID]; r.depth == 0 && r.hint == "" {
			g.leads[keys[i]] = append(g.leads[keys[i]], s.Session.SessionID)
		}
	}
	ids := make([]string, 0, len(vs))
	for i, s := range vs {
		k := keys[i]
		if m.treeHidden(s.Session.SessionID) {
			continue // under a folded parent
		}
		first := i == 0 || keys[i-1] != k
		if m.groupCollapsed(k) {
			if first {
				ids = append(ids, groupRowID(k))
			}
			continue
		}
		if first && k != pinnedGroup && m.groupMode() == groupMachine && m.showEnded && m.filter == "" && len(m.endedSessions(s.NodeName, false)) > 0 {
			ids = append(ids, machineRowID(s.NodeName))
		}
		ids = append(ids, s.Session.SessionID)
	}
	return ids
}

// rowGroup is the group a list row belongs to.
func (m *Model) rowGroup(id string, byID map[string]contracts.SessionView) string {
	if k, ok := groupOfRow(id); ok {
		return k
	}
	if name, ok := machineOfRow(id); ok {
		return name
	}
	return m.sessionGroup(byID[id])
}

// groupHeaderText is a header's content in w columns: the faint name, the
// count, then the project's lead summary or the machine's health, and an
// amber dot when the group needs you (outside project mode, where the
// summary leads with what needs you). A project whose leads are all quiet
// reads faint.
func (m *Model) groupHeaderText(key string, w int) string {
	h := m.th.dim.Render(m.groupLabel(key)) + " " + m.th.faint.Render(itoa(m.grouping.counts[key]))
	if m.groupMode() == groupProject && key != pinnedGroup {
		segs, quiet := m.projectLeadSummary(key, m.grouping.leads[key]) // leads.go
		if quiet {
			return m.th.faint.Render(m.groupLabel(key) + " " + itoa(m.grouping.counts[key]))
		}
		h = m.groupLabel(key) + " " + m.th.faint.Render(itoa(m.grouping.counts[key]))
		for _, seg := range segs {
			if width(h)+width(seg) > w {
				break
			}
			h += seg
		}
		return h
	}
	if m.groupMode() == groupMachine && key != pinnedGroup {
		if nd := m.nodeByName(key); nd != nil {
			ns := nodeStatus(*nd)
			h += " " + m.th.tone(ns.tone, ns.sym)
			if nd.Connection.State != contracts.ConnConnected {
				meta := ns.label
				if nd.Connection.LastSeenAt != nil {
					meta += " · seen " + ago(m.now(), *nd.Connection.LastSeenAt)
				}
				h += " " + m.th.dim.Render(meta)
			}
		}
	}
	if m.grouping.needs[key] && m.groupMode() != groupAttention { // there the label says it
		h += " " + m.th.tone(toneWarn, "•")
	}
	return h
}

// groupHeader is an (expanded) group's header line; a click folds it.
func (m *Model) groupHeader(key string, w int) string {
	return m.mark(zoneDef{kind: zoneGroup, name: key}, trunc("  "+m.groupHeaderText(key, w-2), w))
}

// collapsedGroupRow is a folded group's selectable row.
func (m *Model) collapsedGroupRow(i int, key string, sel bool, w int) string {
	row := m.cursor(sel) + m.mark(zoneDef{kind: zoneGroup, name: key}, m.th.faint.Render("▸ ")+m.groupHeaderText(key, w-4))
	return m.markRow(&m.sessList, i, sel, trunc(row, w), w)
}

// groupTitleSuffix names a non-default grouping in the list's column
// header ("SESSION · by repo").
func (m *Model) groupTitleSuffix() string {
	if mode := m.groupMode(); mode != groupProject {
		if mode == groupNone {
			return " · flat"
		}
		return " · by " + string(mode)
	}
	return ""
}

// ---- actions ----

// toggleGroup folds or unfolds a group and keeps the cursor on it: on the
// folded row, or on the group's first session when it opens.
func (m *Model) toggleGroup(key string) tea.Cmd {
	if m.grouping.collapsed == nil {
		m.grouping.collapsed = map[string]bool{}
	}
	ck := string(m.groupMode()) + ":" + key
	fold := !m.grouping.collapsed[ck]
	if fold {
		m.grouping.collapsed[ck] = true
	} else {
		delete(m.grouping.collapsed, ck)
	}
	m.syncSessions()
	if fold {
		m.sessList.selectID(groupRowID(key))
	} else {
		for _, s := range m.visibleSessions() {
			if m.sessionGroup(s) == key {
				if !m.sessList.selectID(s.Session.SessionID) {
					continue
				}
				break
			}
		}
	}
	return tea.Batch(m.saveState(), m.schedulePreview())
}

// foldSelected folds the selected row's group (za).
func (m *Model) foldSelected() tea.Cmd {
	id := m.sessList.sel
	if id == "" {
		return nil
	}
	if k, ok := groupOfRow(id); ok {
		return m.toggleGroup(k)
	}
	if m.filter != "" {
		return m.setToast("groups stay open while filtering", toneMuted)
	}
	if m.tree.rows[id].kids > 0 || m.grouping.trees[id] { // a parent folds its children (tree.go)
		return m.toggleTree(id)
	}
	byID := map[string]contracts.SessionView{}
	if s, ok := m.session(id); ok {
		byID[id] = s
	}
	k := m.rowGroup(id, byID)
	if k == "" && m.groupMode() == groupNone {
		return m.setToast("the flat list has no groups — gb groups it", toneMuted)
	}
	return m.toggleGroup(k)
}

// setGroupMode switches the grouping and remembers it.
func (m *Model) setGroupMode(g groupMode) tea.Cmd {
	m.grouping.mode = g
	m.syncSessions()
	label := "grouped by " + string(g)
	if g == groupNone {
		label = "flat list: needs you first, then recent activity"
	}
	return tea.Batch(m.saveState(), m.setToast(label, toneMuted), m.schedulePreview())
}

// cycleGroupMode moves to the next grouping (gb).
func (m *Model) cycleGroupMode() tea.Cmd {
	cur := m.groupMode()
	for i, g := range groupModes {
		if g == cur {
			return m.setGroupMode(groupModes[(i+1)%len(groupModes)])
		}
	}
	return m.setGroupMode(groupModes[0])
}

// groupingAction runs the grouping and pinning keymap actions.
func (m *Model) groupingAction(id string) (tea.Cmd, bool) {
	switch id {
	case "sessions.group":
		return m.cycleGroupMode(), true
	case "sessions.fold":
		return m.foldSelected(), true
	case "sessions.sort":
		return m.cycleSortMode(), true // sort.go
	case "sessions.pin":
		return m.pinSelected(), true
	}
	return nil, false
}

// groupRowKey handles keys on a folded group row (enter opens it).
func (m *Model) groupRowKey(key string) (tea.Cmd, bool) {
	k, ok := groupOfRow(m.sessList.sel)
	if !ok {
		return nil, false
	}
	switch key {
	case "enter", "l", "right", "o":
		return m.toggleGroup(k), true
	}
	return nil, false
}

// groupRowHints are the footer keys on a folded group row.
func (m *Model) groupRowHints() ([]hint, bool) {
	if _, ok := groupOfRow(m.sessList.sel); !ok {
		return nil, false
	}
	if m.projectArchiveKey(m.sessList.sel) != "" { // W-archive2
		return []hint{{"enter", "expand"}, {"X", "archive project"}, {"n", "new session"}, {"gb", "group by"}, {"/", "filter"}, {"␣␣", "find"}}, true
	}
	return []hint{{"n", "new session"}, {"enter", "expand"}, {"gb", "group by"}, {"/", "filter"}, {"␣␣", "find"}}, true
}

// groupCommand is :group [project|attention|machine|repo|none]; no
// argument cycles.
func (m *Model) groupCommand() command {
	return command{name: "group", aliases: []string{"groupby"}, args: "[mode]", desc: "group Sessions by project, attention, machine, repo or none (no mode: next)",
		run: func(m *Model, args string) tea.Cmd {
			if strings.TrimSpace(args) == "" {
				return m.cycleGroupMode()
			}
			g, ok := parseGroupMode(args)
			if !ok {
				return m.setToast("group by project, attention, machine, repo or none", toneWarn)
			}
			return m.setGroupMode(g)
		}}
}

// ---- state ----

func (g *groupingState) load(st tuiState) {
	g.sort, _ = parseSortMode(st.Sort)
	if st.Sort == "" {
		g.sort = ""
	}
	g.mode = ""
	if mode, ok := parseGroupMode(st.Group); ok && (st.GroupVersion >= groupStateVersion || mode != groupMachine) {
		g.mode = mode // an explicit choice (older files only wrote non-default modes)
	}
	g.collapsed = map[string]bool{}
	for _, k := range st.Collapsed {
		g.collapsed[k] = true
	}
	g.trees = map[string]bool{}
	for _, id := range st.FoldedTrees {
		g.trees[id] = true
	}
}

func (g *groupingState) save(st *tuiState) {
	if g.mode != "" { // an explicit choice, even of the default
		st.Group, st.GroupVersion = string(g.mode), groupStateVersion
	}
	if g.sort != "" {
		st.Sort = string(g.sort)
	}
	st.Collapsed = nil
	for k := range g.collapsed {
		st.Collapsed = append(st.Collapsed, k)
	}
	sort.Strings(st.Collapsed)
	st.FoldedTrees = nil
	for id := range g.trees {
		st.FoldedTrees = append(st.FoldedTrees, id)
	}
	sort.Strings(st.FoldedTrees)
}

// ---- accessors for tests ----

// SessionRows are the Sessions list row ids in order (sessions, folded
// group rows "@group:<key>", machine rows "@machine:<name>").
func (m *Model) SessionRows() []string { return append([]string(nil), m.sessList.ids...) }

// GridOrder is the session ids of the Grid's current page, in tile order.
func (m *Model) GridOrder() []string { return append([]string(nil), m.grid.list.ids...) }

// GroupMode is the Sessions list grouping ("project", "machine", …).
func (m *Model) GroupMode() string { return string(m.groupMode()) }

// groupRowDetail is the right-hand pane on a folded group row: what is
// inside and how to open it.
func (m *Model) groupRowDetail(key string, w int) []string {
	out := []string{m.th.title.Render(trunc(m.groupLabel(key), w)), ""}
	var ss []contracts.SessionView
	for _, s := range m.visibleSessions() {
		if m.sessionGroup(s) == key {
			ss = append(ss, s)
		}
	}
	out = append(out, m.th.dim.Render(plural(len(ss), "session", "sessions")+" folded"))
	for i, s := range ss {
		if i >= 20 {
			out = append(out, m.th.dim.Render("… and "+itoa(len(ss)-i)+" more"))
			break
		}
		st := listStatus(s, m.nodeOf(s))
		out = append(out, trunc(m.th.tone(st.tone, st.sym)+" "+iconTitle(s)+m.th.faint.Render("  "+st.label), w))
	}
	return append(out, "", trunc(m.th.key.Render("enter")+" "+m.th.dim.Render("or a click expands · ")+m.th.key.Render("gb")+" "+m.th.dim.Render("changes the grouping"), w))
}
