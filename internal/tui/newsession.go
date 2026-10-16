package tui

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// The new-session panel (n), W-newsession: a form, not a command line.
// Fields stacked vertically, each labeled: Where (a repository or folder,
// a type-to-filter list grouped Recent · On <machine> · Other machines ·
// GitHub · Folders), Machine, Agent, Workspace, Account (segmented pickers:
// every option visible, ←/→ or a click changes them), Prompt (multi-line)
// and Title. Tab/Shift-Tab (or ↑/↓ outside the list) move between fields;
// Enter moves on, and on the last field starts; ^s or ^⏎ starts from
// anywhere; Esc cancels.
//
// Each field keeps its own state: moving through the Where list never
// changes another field's value. A field the owner has not set shows its
// automatic value, marked "auto": the machine where the highlighted
// repository is (or was last used from this panel), the agent and
// workspace last used with it. A machine without the repository clones it
// first (repo.clone), shown in the panel while it runs.

const catalogKey = "repos:catalog"

type nsField int

const (
	nsWhere nsField = iota
	nsMachine
	nsAgent
	nsWorkspace
	nsAccount
	nsPrompt
	nsTitle
	nsFieldCount
)

var nsFieldLabels = [nsFieldCount]string{"Where", "Machine", "Agent", "Workspace", "Account", "Prompt", "Title"}

// Workspace choices.
const (
	wsMain = iota
	wsFresh
	wsBranch
)

var wsVals = []string{"main", "fresh", "branch"}

// Row kinds.
const (
	nsRepo = iota
	nsFolder
	nsCreate
	nsBrowseRow // "📁 Browse folders…": opens the folder browser
)

// nsRow is one entry of the Where list.
type nsRow struct {
	key    string // stable identity (the highlight survives refreshes)
	group  string
	kind   int
	name   string
	detail string // owner/name or path
	where  string // machines column
	note   string
	repo   *contracts.CatalogRepo // repository rows
	nodeID string                 // Recent rows: where it ran; folders: the machine listed
	path   string                 // folder rows
	pos    []int
}

// nsPref is what was last used with a repository.
type nsPref struct {
	Machine   string    `json:"machine,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Workspace string    `json:"workspace,omitempty"` // main | fresh
	At        time.Time `json:"at"`
}

// nsMemory is the panel's remembered choices (persisted in the TUI state file).
type nsMemory struct {
	Agent string            `json:"agent,omitempty"` // last agent overall
	Repos map[string]nsPref `json:"repos,omitempty"` // by catalog key
}

const nsMemMax = 200

func (n nsMemory) clone() nsMemory {
	out := nsMemory{Agent: n.Agent, Repos: make(map[string]nsPref, len(n.Repos))}
	for k, v := range n.Repos {
		out.Repos[k] = v
	}
	return out
}

func (n *nsMemory) remember(key string, p nsPref) {
	if n.Repos == nil {
		n.Repos = map[string]nsPref{}
	}
	n.Repos[key] = p
	for len(n.Repos) > nsMemMax {
		oldest := ""
		for k, v := range n.Repos {
			if oldest == "" || v.At.Before(n.Repos[oldest].At) {
				oldest = k
			}
		}
		delete(n.Repos, oldest)
	}
}

type nsPanel struct {
	instance string
	focus    nsField
	query    string // Where filter (or a path)
	hlKey    string // highlighted row ("" = the best match)
	extra    []contracts.CatalogRepo
	// Explicit choices ("" / unset = automatic).
	machine string
	agent   contracts.Harness
	ws      int
	wsSet   bool
	branch  string
	account string
	prompt  string
	title   string

	browse *nsBrowse // the inline folder browser, when open (newsession_browse.go)

	starting bool
	step     string
	stepAt   time.Time
	errLine  string
	cwd      string // this process's directory, for ./ and ../ paths
	home     string
}

type nsTickMsg struct{ q *nsPanel }

// QuickLaunchOpen reports that the new-session panel is open (tests).
func (m *Model) QuickLaunchOpen() bool {
	_, ok := m.overlay.(*nsPanel)
	return ok
}

func (m *Model) openQuickLaunch() tea.Cmd {
	q := &nsPanel{instance: contracts.NewID(contracts.PrefixOperation)}
	q.cwd, _ = os.Getwd()
	q.home, _ = os.UserHomeDir()
	m.overlay = q
	return q.load(m, q.plan(m))
}

// loadCatalog loads the global repo catalog; an older hub without
// repos.catalog falls back to repos.list (no GitHub part).
func (m *Model) loadCatalog(force bool) tea.Cmd {
	api, now := m.api, m.now()
	fn := func(ctx context.Context) (any, error) {
		if ca, ok := api.(contracts.RepoCatalogAPI); ok {
			cat, err := ca.RepoCatalog(ctx, contracts.RepoCatalogRequest{Limit: 2000})
			if err == nil {
				return cat, nil
			}
			if !contracts.IsCode(err, contracts.ErrUnsupported) {
				return nil, err
			}
		}
		r, ok := api.(contracts.ReposAPI)
		if !ok {
			return nil, contracts.Errorf(contracts.ErrUnsupported, "this hub has no repo index (upgrade pyre-hub); paths like ~/scratch still work")
		}
		list, err := r.ListRepos(ctx, contracts.ListReposRequest{Limit: 500})
		if err != nil {
			return nil, err
		}
		return contracts.RepoCatalog{Items: repoindex.Catalog(list.Items, nil, "", now), Nodes: list.Nodes}, nil
	}
	if force {
		return m.reload(catalogKey, fn)
	}
	return m.ensure(catalogKey, time.Minute, fn)
}

func (m *Model) catalog() ([]contracts.CatalogRepo, *loadState) {
	c, st := get[contracts.RepoCatalog](m, catalogKey)
	return c.Items, st
}

// nsHomeNode is the machine the panel starts on: this machine when it is a
// connected node, else the selected session's, else the first connected.
func (m *Model) nsHomeNode(connected map[string]bool) string {
	if id := m.opts.ClientNodeID; id != "" && connected[id] {
		return id
	}
	if s, ok := m.selectedSession(); ok && s.Run != nil && connected[s.Run.NodeID] {
		return s.Run.NodeID
	}
	ns := m.sortedNodes()
	for _, n := range ns {
		if connected[n.NodeID] {
			return n.NodeID
		}
	}
	if len(ns) > 0 {
		return ns[0].NodeID
	}
	return ""
}

func qlDirKey(nodeID, dir string) string { return "qldir:" + nodeID + ":" + dir }

// nsRecent lists where sessions ran lately: one row per machine and
// folder, most recent first, a repository's checkout named after it.
func (m *Model) nsRecent(items []contracts.CatalogRepo) []nsRow {
	type rc struct {
		s  contracts.SessionView
		at time.Time
	}
	var all []rc
	for _, s := range m.sessions() {
		if s.Run == nil || s.Run.Cwd == "" || s.Run.NodeID == "" {
			continue
		}
		all = append(all, rc{s, m.lastActivity(s)})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	byPath := map[string]int{}
	for i, c := range items {
		for _, e := range c.Present {
			byPath[e.NodeID+"\x00"+e.Path] = i
		}
	}
	seen := map[string]bool{}
	var out []nsRow
	for _, r := range all {
		k := r.s.Run.NodeID + "\x00" + r.s.Run.Cwd
		if seen[k] {
			continue
		}
		seen[k] = true
		p := tildePath(r.s.Run.Cwd)
		row := nsRow{key: "recent:" + k, group: "Recent", kind: nsFolder, name: path.Base(p), detail: p, path: p,
			nodeID: r.s.Run.NodeID, where: m.nodeName(r.s.Run.NodeID), note: "used " + ago(m.now(), r.at)}
		if p == "~" {
			row.name = "home"
		}
		if i, ok := byPath[k]; ok {
			row.kind, row.repo, row.name = nsRepo, &items[i], items[i].Name
		}
		out = append(out, row)
		if len(out) >= 40 {
			break
		}
	}
	return out
}

// whereQuery is the Where text as a filter or a path (a pasted, escaped
// or quoted path is unescaped).
func (q *nsPanel) whereQuery() string {
	s := strings.TrimLeft(q.query, " \t")
	if strings.ContainsAny(s, `\'"`) {
		t, rest, _ := repoindex.SplitTarget(s)
		s = strings.Join(append([]string{t}, rest...), " ")
	}
	return s
}

func (q *nsPanel) isPathMode(items []contracts.CatalogRepo) bool {
	s := strings.TrimSpace(q.whereQuery())
	if repoindex.IsPathSpec(s) {
		return true
	}
	if strings.EqualFold(s, "home") {
		for _, c := range items {
			if strings.EqualFold(c.Name, "home") {
				return false
			}
		}
		return true
	}
	return false
}

func catalogNote(m *Model, c contracts.CatalogRepo) string {
	switch {
	case c.LastUsedAt != nil:
		return "used " + ago(m.now(), *c.LastUsedAt)
	case c.ContributedAt != nil && c.Rank == contracts.CatalogRankContributed:
		return "contributed " + ago(m.now(), *c.ContributedAt)
	case len(c.Present) > 0 && !c.LastActivity.IsZero():
		return "active " + ago(m.now(), c.LastActivity)
	case c.PushedAt != nil:
		return "pushed " + ago(m.now(), *c.PushedAt)
	}
	return ""
}

// rows builds the Where list for the current text; base is the machine
// the groups and folder listings are relative to.
func (q *nsPanel) rows(m *Model, items []contracts.CatalogRepo, base string) (rows []nsRow, pathMode bool, dirNode, dirPath string) {
	query := strings.TrimSpace(q.whereQuery())
	if q.isPathMode(items) {
		return q.folderRows(m, items, base)
	}
	shown := map[string]bool{}
	limR := 3
	if query == "" {
		limR = 4
	}
	n := 0
	for _, r := range m.nsRecent(items) {
		if n >= limR {
			break
		}
		_, pos, ok := matchTerms(query, r.name, r.detail, r.where)
		if !ok {
			continue
		}
		r.pos = pos
		rows = append(rows, r)
		if r.repo != nil {
			shown[r.repo.Key] = true
		}
		n++
	}
	ranked := repoindex.RankCatalog(items, query)
	if repoindex.IsGitHubSpec(query) {
		found := false
		for _, c := range ranked {
			found = found || strings.EqualFold(c.FullName, query)
		}
		if !found {
			remote := repoindex.GitHubRemote(query)
			_, name, _ := strings.Cut(query, "/")
			ranked = append(ranked, contracts.CatalogRepo{Key: repoindex.CatalogKey(contracts.Repo{RemoteURL: remote}), Name: name,
				FullName: query, RemoteURL: remote, Present: []contracts.RepoEntry{}, GitHub: true})
		}
	}
	var here, other, gh []nsRow
	const capGroup = 60
	baseName := m.nodeName(base)
	for i := range ranked {
		c := &ranked[i]
		if shown[c.Key] {
			continue
		}
		row := nsRow{key: "repo:" + c.Key, kind: nsRepo, repo: c, name: c.Name, detail: c.FullName, note: catalogNote(m, *c)}
		_, row.pos, _ = matchTerms(query, c.Name)
		var names []string
		for _, e := range c.Present {
			names = append(names, e.NodeName)
		}
		if e, ok := repoindex.EntryOn(c.Present, base); ok {
			row.group = "On " + baseName
			if row.detail == "" {
				row.detail = tildePath(e.Path)
			}
			row.where = baseName
			if len(c.Present) > 1 {
				row.where += " +" + strconv.Itoa(len(c.Present)-1)
			}
			if len(here) < capGroup {
				here = append(here, row)
			}
			continue
		}
		if len(c.Present) > 0 {
			row.group = "Other machines"
			if row.detail == "" {
				row.detail = tildePath(c.Present[0].Path)
			}
			row.where = strings.Join(names, ", ")
			if len(other) < capGroup {
				other = append(other, row)
			}
			continue
		}
		row.group, row.where = "GitHub", "not cloned"
		if len(gh) < capGroup {
			gh = append(gh, row)
		}
	}
	rows = append(append(append(rows, here...), other...), gh...)
	if _, pos, ok := matchTerms(query, "home"); ok {
		rows = append(rows, nsRow{key: "home", group: "Folders", kind: nsFolder, name: "home", detail: "~", path: "~", pos: pos, note: "scratch · no project"})
	}
	return append(rows, browseRow(m, base)), false, "", ""
}

// folderRows: a path lists the machine's folders that complete it; a
// missing one is offered for creation.
func (q *nsPanel) folderRows(m *Model, items []contracts.CatalogRepo, base string) (rows []nsRow, pathMode bool, dirNode, dirPath string) {
	pth, dir := repoindex.NormPath(strings.TrimSpace(q.whereQuery()), q.cwd, q.home)
	parent, name := repoindex.SplitPath(pth, dir)
	res, st := get[contracts.DirListResult](m, qlDirKey(base, parent))
	loaded := st != nil && st.have && st.err == nil
	failed := st != nil && st.err != nil && !st.inflight
	exists := name == "" || strings.HasPrefix(name, ".")
	if loaded {
		for _, d := range res.Dirs {
			if d == name {
				exists = true
			}
		}
	}
	self := nsRow{key: "dir:" + pth, group: "Folders", kind: nsFolder, name: path.Base(pth), detail: pth, path: pth, nodeID: base, note: "folder"}
	switch {
	case pth == "~":
		self.name, self.note = "home", "scratch · no project"
	case failed || (loaded && !exists):
		self.kind, self.key, self.note = nsCreate, "create:"+pth, "new folder"
	case !loaded:
		self.note = "checking…"
	}
	var kids []nsRow
	if loaded {
		lname := strings.ToLower(name)
		for _, d := range res.Dirs {
			if (name == "" || strings.HasPrefix(strings.ToLower(d), lname)) && d != name {
				var pos []int
				for i := range []rune(name) {
					pos = append(pos, i)
				}
				p := repoindex.JoinPath(parent, d)
				kids = append(kids, nsRow{key: "dir:" + p, group: "Folders", kind: nsFolder, name: d, detail: p, path: p, nodeID: base, pos: pos, note: "folder"})
				if len(kids) >= 30 {
					break
				}
			}
		}
	}
	var rec []nsRow
	for _, r := range m.nsRecent(items) {
		if r.nodeID == base && strings.HasPrefix(strings.ToLower(r.path), strings.ToLower(pth)) && r.path != pth && len(rec) < 3 {
			rec = append(rec, r)
		}
	}
	if self.kind == nsCreate {
		rows = append(append(rows, kids...), self)
	} else {
		rows = append(append(rows, self), kids...)
	}
	return append(append(rows, rec...), browseRow(m, base)), true, base, parent
}

// browseRow is the always-present last row of the Where list.
func browseRow(m *Model, base string) nsRow {
	return nsRow{key: browseKey, group: "Folders", kind: nsBrowseRow, name: "📁 Browse folders…", detail: "any folder, or a new one", where: m.nodeName(base)}
}

// nsPlan is what the form says right now.
type nsPlan struct {
	rows      []nsRow
	sel       int
	target    *nsRow
	pathMode  bool
	dirNode   string
	dirPath   string
	base      string
	repos     []contracts.CatalogRepo
	folder    string
	create    bool
	nodeID    string
	nodeAuto  bool
	harness   contracts.Harness
	agentAuto bool
	ws        int
	wsAuto    bool
	multi     bool
	account   string
	clones    []contracts.CatalogRepo
	nodes     []contracts.Node
	connected map[string]bool
	problem   string
	problemAt nsField
}

func (q *nsPanel) plan(m *Model) nsPlan {
	var p nsPlan
	p.connected = map[string]bool{}
	for _, n := range m.sortedNodes() {
		p.connected[n.NodeID] = n.Connection.State == contracts.ConnConnected
	}
	for _, conn := range []bool{true, false} {
		for _, n := range m.sortedNodes() {
			if p.connected[n.NodeID] == conn {
				p.nodes = append(p.nodes, n)
			}
		}
	}
	p.base = q.machine
	if p.base == "" || m.node(p.base) == nil {
		p.base = m.nsHomeNode(p.connected)
	}
	items, _ := m.catalog()
	p.rows, p.pathMode, p.dirNode, p.dirPath = q.rows(m, items, p.base)
	if q.hlKey != "" {
		for i, r := range p.rows {
			if r.key == q.hlKey {
				p.sel = i
			}
		}
	} else {
		p.sel = bestRow(p.rows, strings.TrimSpace(q.whereQuery()))
	}
	if len(p.rows) > 0 {
		p.target = &p.rows[p.sel]
	}

	// What: repositories (the highlighted one plus any added with +), or a folder.
	seen := map[string]bool{}
	for _, c := range q.extra {
		if !seen[c.Key] {
			seen[c.Key] = true
			p.repos = append(p.repos, c)
		}
	}
	if t := p.target; t != nil && t.kind == nsRepo && !seen[t.repo.Key] {
		p.repos = append(p.repos, *t.repo)
	}
	p.multi = len(p.repos) > 1
	if t := p.target; t != nil && t.kind != nsRepo && t.kind != nsBrowseRow && len(q.extra) == 0 {
		p.folder, p.create = t.path, t.kind == nsCreate
	}
	key := ""
	if len(p.repos) > 0 {
		key = p.repos[0].Key
	}
	pref, hasPref := m.nsMem.Repos[key]

	// Machine.
	if q.machine != "" && m.node(q.machine) != nil {
		p.nodeID = q.machine
	} else {
		p.nodeAuto = true
		p.nodeID = q.autoMachine(m, p, pref, hasPref)
	}
	// Agent.
	if q.agent != "" {
		p.harness = q.agent
	} else {
		p.agentAuto, p.harness = true, contracts.HarnessClaude
		switch {
		case hasPref && pref.Agent != "":
			p.harness = contracts.Harness(pref.Agent)
		case m.nsMem.Agent != "":
			p.harness = contracts.Harness(m.nsMem.Agent)
		}
	}
	// Workspace.
	if q.wsSet {
		p.ws = q.ws
	} else {
		p.wsAuto, p.ws = true, wsMain
		if hasPref && pref.Workspace == "fresh" {
			p.ws = wsFresh
		}
	}
	if p.multi && p.ws == wsMain {
		p.ws = wsFresh
	}
	// Account: only one of the agent's provider.
	if a := m.account(q.account); a != nil && p.harness != contracts.HarnessShell && a.Account.Provider == contracts.ProviderFor(p.harness) {
		p.account = q.account
	}
	for _, r := range p.repos {
		if _, ok := repoindex.EntryOn(r.Present, p.nodeID); !ok {
			p.clones = append(p.clones, r)
		}
	}
	q.validate(m, &p, items)
	return p
}

// bestRow is the row a new filter highlights: the best-matching
// repository wherever its group is (the first on a tie), else the first.
func bestRow(rows []nsRow, query string) int {
	if query == "" {
		return 0
	}
	best, at := -1, 0
	for i, r := range rows {
		if r.repo == nil {
			continue
		}
		if s, ok := repoindex.CatalogScore(query, *r.repo); ok && s > best {
			best, at = s, i
		}
	}
	if best < 0 {
		// Nothing matches a name that is not a path: browse for a folder.
		for i, r := range rows {
			if r.kind == nsBrowseRow && !repoindex.IsPathSpec(query) {
				only := true
				for _, o := range rows {
					only = only && (o.kind == nsBrowseRow || o.key == "home")
				}
				if only {
					return i
				}
			}
		}
	}
	return at
}

// autoMachine: a Recent row's machine; the machine last used with the
// repository; the panel's machine when it has every repository; else the
// best machine that has them; else the panel's machine (it clones).
func (q *nsPanel) autoMachine(m *Model, p nsPlan, pref nsPref, hasPref bool) string {
	if t := p.target; t != nil && t.group == "Recent" && t.nodeID != "" && m.node(t.nodeID) != nil && len(q.extra) == 0 {
		return t.nodeID
	}
	if len(p.repos) == 0 {
		return p.base
	}
	if hasPref && pref.Machine != "" && p.connected[pref.Machine] {
		return pref.Machine
	}
	var groups [][]contracts.RepoEntry
	all := true
	for _, r := range p.repos {
		if _, ok := repoindex.EntryOn(r.Present, p.base); !ok {
			all = false
		}
		groups = append(groups, r.Present)
	}
	if all {
		return p.base
	}
	if ms := repoindex.Machines(groups, p.connected); len(ms) > 0 && p.connected[ms[0]] {
		return ms[0]
	}
	return p.base
}

func (q *nsPanel) validate(m *Model, p *nsPlan, items []contracts.CatalogRepo) {
	bad := func(f nsField, s string) {
		if p.problem == "" {
			p.problem, p.problemAt = s, f
		}
	}
	query := strings.TrimSpace(q.whereQuery())
	switch {
	case p.target == nil && len(q.extra) == 0:
		_, st := m.catalog()
		switch {
		case st == nil || (!st.have && st.err == nil):
			bad(nsWhere, "loading repositories…")
		case query != "":
			bad(nsWhere, "no repository matches “"+query+"” — owner/name clones one from GitHub; a folder? type it as ~/"+query)
		default:
			bad(nsWhere, "choose a repository or a folder")
		}
	case p.target != nil && p.target.kind == nsBrowseRow && len(q.extra) == 0:
		if query != "" && !p.pathMode {
			bad(nsWhere, "no repository matches “"+query+"”: Enter browses folders (or owner/name clones from GitHub)")
		} else {
			bad(nsWhere, "Enter opens the folder browser")
		}
	case p.target != nil && p.target.kind != nsRepo && len(q.extra) > 0:
		bad(nsWhere, "a folder cannot join repositories: highlight a repository, or remove the added ones")
	}
	switch {
	case p.nodeID == "":
		bad(nsMachine, "no machines (pair one with pyre nodes pair)")
	case !p.connected[p.nodeID]:
		bad(nsMachine, m.nodeName(p.nodeID)+" is offline")
	}
	for _, c := range p.clones {
		if c.RemoteURL == "" {
			bad(nsMachine, c.Name+" has no remote, so it cannot be cloned onto "+m.nodeName(p.nodeID)+": choose a machine that has it")
		}
	}
	if p.folder == "" && p.ws == wsBranch {
		b := strings.TrimSpace(q.branch)
		switch {
		case b == "":
			bad(nsWorkspace, "type the new branch name (Workspace)")
		case !repoindex.ValidBranch(b):
			bad(nsWorkspace, "“"+b+"” is not a valid branch name")
		}
	}
}

// load fetches what the plan needs: the catalog, and a folder listing.
func (q *nsPanel) load(m *Model, p nsPlan) tea.Cmd {
	cmds := []tea.Cmd{m.loadCatalog(false)}
	if p.pathMode && p.dirNode != "" {
		if n := m.node(p.dirNode); n != nil && n.Connection.State == contracts.ConnConnected {
			api, nid, dir := m.api, p.dirNode, p.dirPath
			cmds = append(cmds, m.ensure(qlDirKey(nid, dir), 15*time.Second, func(ctx context.Context) (any, error) {
				var out contracts.DirListResult
				err := nsInvoke(ctx, api, nid, contracts.NewID(contracts.PrefixOperation), contracts.ActionDirList, contracts.DirListParams{Path: dir}, &out, 5)
				return out, err
			}))
		}
	}
	return tea.Batch(cmds...)
}

func nsInvoke(ctx context.Context, api contracts.API, nodeID, op, action string, params, out any, timeout int) error {
	raw, _ := json.Marshal(params)
	res, err := api.InvokeNodeAction(ctx, contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: op},
		NodeID: nodeID, Action: action, Params: raw, TimeoutSeconds: timeout})
	if err != nil {
		return err
	}
	if res.Error != nil {
		return res.Error
	}
	if res.Outcome != contracts.OutcomeSucceeded {
		return contracts.Errorf(contracts.ErrOutcomeIndeterminate, "%s: %s", action, res.Outcome)
	}
	return json.Unmarshal(res.Result, out)
}

// ---- options of the segmented fields ----

type nsOpt struct {
	val, label string
	on         bool
	disabled   bool
}

func (q *nsPanel) options(m *Model, p nsPlan, f nsField) []nsOpt {
	var out []nsOpt
	switch f {
	case nsMachine:
		for _, n := range p.nodes {
			l := m.nodeName(n.NodeID)
			switch {
			case !p.connected[n.NodeID]:
				l += " · offline"
			case len(p.repos) == 1:
				if _, ok := repoindex.EntryOn(p.repos[0].Present, n.NodeID); ok {
					l += " · has it"
				} else {
					l += " · will clone"
				}
			case len(p.repos) > 1:
				miss := 0
				for _, r := range p.repos {
					if _, ok := repoindex.EntryOn(r.Present, n.NodeID); !ok {
						miss++
					}
				}
				if miss == 0 {
					l += " · has all"
				} else {
					l += " · clones " + strconv.Itoa(miss)
				}
			}
			out = append(out, nsOpt{val: n.NodeID, label: l, on: n.NodeID == p.nodeID})
		}
	case nsAgent:
		for _, h := range []contracts.Harness{contracts.HarnessClaude, contracts.HarnessCodex, contracts.HarnessShell} {
			out = append(out, nsOpt{val: string(h), label: titleCase(string(h)), on: h == p.harness})
		}
	case nsWorkspace:
		if p.folder != "" {
			return nil
		}
		b := "new branch"
		if p.ws == wsBranch {
			b += ": " + q.branch
		}
		out = []nsOpt{{val: "main", label: "main checkout", on: p.ws == wsMain, disabled: p.multi},
			{val: "fresh", label: "fresh worktree", on: p.ws == wsFresh}, {val: "branch", label: b, on: p.ws == wsBranch}}
	case nsAccount:
		if p.harness == contracts.HarnessShell {
			return nil
		}
		out = append(out, nsOpt{val: "", label: "auto", on: p.account == ""})
		prov := contracts.ProviderFor(p.harness)
		for _, a := range m.accounts() {
			if a.Account.Provider != prov || a.Account.Disabled {
				continue
			}
			l := a.Account.Label
			if l == "" {
				l = a.Account.AccountID
			}
			out = append(out, nsOpt{val: a.Account.AccountID, label: l, on: p.account == a.Account.AccountID})
		}
	}
	return out
}

// setOpt makes an explicit choice.
func (q *nsPanel) setOpt(f nsField, val string) {
	switch f {
	case nsMachine:
		q.machine = val
	case nsAgent:
		q.agent = contracts.Harness(val)
	case nsWorkspace:
		for i, v := range wsVals {
			if v == val {
				q.ws, q.wsSet = i, true
			}
		}
	case nsAccount:
		q.account = val
	}
}

// cycle moves a segmented field's choice by d (wrapping, skipping
// disabled options).
func (q *nsPanel) cycle(m *Model, p nsPlan, f nsField, d int) {
	opts := q.options(m, p, f)
	if len(opts) == 0 {
		return
	}
	cur := 0
	for i, o := range opts {
		if o.on {
			cur = i
		}
	}
	for n := 1; n <= len(opts); n++ {
		o := opts[((cur+d*n)%len(opts)+len(opts))%len(opts)]
		if !o.disabled {
			q.setOpt(f, o.val)
			return
		}
	}
}

func isSegmented(f nsField) bool {
	return f == nsMachine || f == nsAgent || f == nsWorkspace || f == nsAccount
}

// ---- keys, clicks, paste ----

func (q *nsPanel) hintsFor(m *Model) []hint {
	if q.browse != nil && q.focus == nsWhere {
		return q.browseHints()
	}
	tail := []hint{{"^s", "start"}, {"esc", "cancel"}, {"^a", "step-by-step"}}
	switch {
	case q.focus == nsWhere:
		return append([]hint{{"↑↓", "choose"}, {"tab", "next field"}, {"+", "add repo"}}, tail...)
	case isSegmented(q.focus):
		return append([]hint{{"←→", "change"}, {"↑↓", "field"}, {"enter", "next"}}, tail...)
	case q.focus == nsPrompt:
		return append([]hint{{"enter", "next"}, {"⇧⏎", "new line"}, {"↑↓", "field"}}, tail...)
	}
	return append([]hint{{"enter", "start"}, {"↑↓", "field"}}, tail...)
}

func (q *nsPanel) move(d int) {
	q.focus = nsField((int(q.focus) + d + int(nsFieldCount)) % int(nsFieldCount))
}

func (q *nsPanel) moveHighlight(p nsPlan, d int) {
	if len(p.rows) == 0 {
		return
	}
	i := min(max(p.sel+d, 0), len(p.rows)-1)
	q.hlKey = p.rows[i].key
}

func (q *nsPanel) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if q.starting {
		if key == "esc" || key == "ctrl+c" {
			m.overlay = nil // the launch continues; a toast reports it
		}
		return nil
	}
	if q.browse != nil && q.focus == nsWhere {
		cmd := q.browseKey(m, k)
		return tea.Batch(cmd, q.loadBrowse(m))
	}
	p := q.plan(m)
	switch key {
	case "esc", "ctrl+c":
		m.overlay = nil
		return nil
	case "ctrl+a":
		m.overlay = nil
		return m.openLaunch()
	case "ctrl+s", "ctrl+enter":
		return q.start(m, p)
	case "ctrl+o":
		q.cycle(m, p, nsMachine, 1)
	case "ctrl+t":
		q.cycle(m, p, nsAgent, 1)
	case "shift+tab":
		q.move(-1)
	case "tab":
		if q.focus != nsWhere || !q.complete(p) {
			q.move(1)
		}
	default:
		if cmd, done := q.fieldKey(m, p, k); done {
			return cmd
		}
	}
	q.errLine = ""
	return q.load(m, q.plan(m))
}

// fieldKey handles a key for the focused field; done means the key ended
// the update (a start).
func (q *nsPanel) fieldKey(m *Model, p nsPlan, k tea.KeyPressMsg) (tea.Cmd, bool) {
	key := k.String()
	switch q.focus {
	case nsWhere:
		switch key {
		case "up", "ctrl+p", "ctrl+k":
			q.moveHighlight(p, -1)
		case "down", "ctrl+n", "ctrl+j":
			q.moveHighlight(p, 1)
		case "pgup":
			q.moveHighlight(p, -8)
		case "pgdown":
			q.moveHighlight(p, 8)
		case "right":
			q.complete(p)
		case "enter":
			if p.target != nil && p.target.kind == nsBrowseRow {
				return q.openBrowse(m, p), true
			}
			if p.target == nil && len(q.extra) == 0 {
				q.errLine = p.problem
				return nil, true
			}
			q.move(1)
		case "+":
			if p.target != nil && p.target.kind == nsRepo && !p.pathMode {
				q.extra = append(q.extra, *p.target.repo)
				q.query, q.hlKey = "", ""
			} else if q.editText(&q.query, k) {
				q.hlKey = ""
			}
		case "backspace":
			if q.query == "" && len(q.extra) > 0 {
				q.extra = q.extra[:len(q.extra)-1]
				break
			}
			q.editText(&q.query, k)
			q.hlKey = ""
		default:
			if q.editText(&q.query, k) {
				q.hlKey = "" // a new filter: the best match
			}
		}
	case nsMachine, nsAgent, nsWorkspace, nsAccount:
		branchTyping := q.focus == nsWorkspace && p.ws == wsBranch
		switch key {
		case "left":
			q.cycle(m, p, q.focus, -1)
		case "right":
			q.cycle(m, p, q.focus, 1)
		case "space":
			if !branchTyping {
				q.cycle(m, p, q.focus, 1)
			}
		case "up":
			q.move(-1)
		case "down", "enter":
			q.move(1)
		default:
			if branchTyping {
				q.editText(&q.branch, k)
			}
		}
	case nsPrompt:
		switch key {
		case "enter":
			q.move(1)
		case "shift+enter", "alt+enter", "ctrl+j":
			q.prompt += "\n"
		case "up":
			q.move(-1)
		case "down":
			q.move(1)
		default:
			q.editText(&q.prompt, k)
		}
	case nsTitle:
		switch key {
		case "enter":
			return q.start(m, p), true
		case "up":
			q.move(-1)
		case "down":
			q.move(1)
		default:
			q.editText(&q.title, k)
		}
	}
	return nil, false
}

// editText applies an editing key to s; false when the key is not one.
func (q *nsPanel) editText(s *string, k tea.KeyPressMsg) bool {
	switch k.String() {
	case "backspace":
		if r := []rune(*s); len(r) > 0 {
			*s = string(r[:len(r)-1])
		}
	case "ctrl+w":
		t := strings.TrimRight(*s, " ")
		if i := strings.LastIndexAny(t, " /\n"); i >= 0 {
			*s = t[:i+1]
		} else {
			*s = ""
		}
	case "ctrl+u":
		*s = ""
	default:
		if k.Text == "" || k.Mod&^tea.ModShift != 0 {
			return false
		}
		*s += k.Text
	}
	return true
}

// complete fills in the highlighted folder (path mode): the next segment.
func (q *nsPanel) complete(p nsPlan) bool {
	t := p.target
	if !p.pathMode || t == nil || t.kind != nsFolder {
		return false
	}
	pth, dir := repoindex.NormPath(strings.TrimSpace(q.whereQuery()), q.cwd, q.home)
	if t.path == pth && dir {
		return false
	}
	next := t.path + "/"
	if t.path == "~" {
		next = "~/"
	}
	if next == q.query {
		return false
	}
	q.query, q.hlKey = next, ""
	return true
}

// wheel scrolls the Where list (whatever field has the keyboard).
func (q *nsPanel) wheel(m *Model, dir string) tea.Cmd {
	if q.starting {
		return nil
	}
	if q.browse != nil {
		return q.browseKey(m, keyFor(dir))
	}
	d := 1
	if dir == "up" {
		d = -1
	}
	q.moveHighlight(q.plan(m), d)
	return nil
}

func (q *nsPanel) click(m *Model, name string) tea.Cmd {
	if q.starting {
		return nil
	}
	kind, val, _ := strings.Cut(name, ":")
	switch kind {
	case "row":
		q.hlKey, q.focus = val, nsWhere
		if val == browseKey {
			return q.openBrowse(m, q.plan(m))
		}
	case "browse":
		return q.openBrowse(m, q.plan(m))
	case "brow":
		return q.browseClick(m, val)
	case "field":
		i, _ := strconv.Atoi(val)
		q.focus = nsField(i)
		if q.focus != nsWhere {
			q.browse = nil
		}
	case "opt":
		f, v, _ := strings.Cut(val, ":")
		i, _ := strconv.Atoi(f)
		q.focus = nsField(i)
		q.browse = nil
		for _, o := range q.options(m, q.plan(m), q.focus) {
			if o.val == v && !o.disabled {
				q.setOpt(q.focus, v)
			}
		}
	case "extra":
		i, _ := strconv.Atoi(val)
		if i >= 0 && i < len(q.extra) {
			q.extra = append(q.extra[:i:i], q.extra[i+1:]...)
		}
	case "start":
		return q.start(m, q.plan(m))
	case "cancel":
		m.overlay = nil
		return nil
	case "advanced":
		m.overlay = nil
		return m.openLaunch()
	}
	q.errLine = ""
	return q.load(m, q.plan(m))
}

func (q *nsPanel) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case nsTickMsg:
		if msg.q == q && q.starting {
			return q.tick()
		}
	case tea.PasteMsg:
		if q.starting {
			return nil
		}
		text := msg.Content
		if q.browse != nil && q.focus == nsWhere {
			line := strings.Join(strings.Fields(text), " ")
			if q.browse.naming {
				q.browse.name += line
			} else {
				q.browse.filter += line
				q.browseFirstMatch(m)
			}
			return q.loadBrowse(m)
		}
		switch q.focus {
		case nsPrompt:
			q.prompt += strings.ReplaceAll(text, "\r\n", "\n")
		case nsTitle:
			q.title += strings.Join(strings.Fields(text), " ")
		case nsWorkspace:
			q.branch += strings.TrimSpace(text)
		default:
			q.focus = nsWhere
			q.query += strings.NewReplacer("\r", "", "\n", "").Replace(text)
			q.hlKey = ""
		}
		return q.load(m, q.plan(m))
	}
	return nil
}

func (q *nsPanel) tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return nsTickMsg{q: q} })
}

// ---- start ----

// nsJob is one launch: folder creation or clones, a worktree, the session.
// Each step has its own stable operation id, so a retry after an unknown
// outcome never clones, creates or starts twice.
type nsJob struct {
	nodeID, nodeName string
	harness          contracts.Harness
	prompt, title    string
	folder           string
	create           bool
	repos            []contracts.CatalogRepo
	paths            []string
	ws               int
	branch           string
	account          string
	cwd              string
	cloned           []string
	wtBranch         string
}

func (m *Model) doLong(d time.Duration, fn func(ctx context.Context) (any, error), then func(m *Model, val any, err error) tea.Cmd) tea.Cmd {
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, d)
		defer cancel()
		v, err := fn(ctx)
		return resultMsg{val: v, err: err, then: then}
	}
}

func (q *nsPanel) start(m *Model, p nsPlan) tea.Cmd {
	if q.starting {
		return nil
	}
	if p.target != nil && p.target.kind == nsBrowseRow && len(q.extra) == 0 && p.problemAt == nsWhere {
		return q.openBrowse(m, p)
	}
	if p.problem != "" {
		q.errLine, q.focus = p.problem, p.problemAt
		return nil
	}
	j := &nsJob{nodeID: p.nodeID, nodeName: m.nodeName(p.nodeID), harness: p.harness, prompt: strings.TrimSpace(q.prompt),
		title: strings.TrimSpace(q.title), folder: p.folder, create: p.create, repos: p.repos, ws: p.ws,
		branch: strings.TrimSpace(q.branch), account: p.account}
	for _, r := range p.repos {
		e, _ := repoindex.EntryOn(r.Present, p.nodeID)
		j.paths = append(j.paths, e.Path)
	}
	q.starting, q.errLine = true, ""
	return tea.Batch(q.next(m, j), q.tick())
}

func (q *nsPanel) setStep(s string) {
	q.step, q.stepAt = s, time.Now()
}

func (q *nsPanel) fail(m *Model, err error) tea.Cmd {
	q.starting, q.step = false, ""
	if contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) || retryable(err) {
		q.errLine = "Outcome unknown (" + errText(err) + "). Starting again reuses the same operation ids; nothing happens twice."
	} else {
		q.errLine = "Not started: " + errText(err)
	}
	if m.overlay != q {
		return m.setToast("new session: "+q.errLine, toneBad)
	}
	return nil
}

func (q *nsPanel) next(m *Model, j *nsJob) tea.Cmd {
	api := m.api
	if j.folder != "" {
		if j.create && j.cwd == "" {
			q.setStep("creating " + j.folder + " on " + j.nodeName)
			op := m.opID("ns:" + q.instance + ":mkdir")
			return m.doLong(time.Minute, func(ctx context.Context) (any, error) {
				var mk contracts.FsMkdirResult
				err := nsInvoke(ctx, api, j.nodeID, op, contracts.ActionFsMkdir, contracts.FsMkdirParams{Path: j.folder}, &mk, 30)
				if err != nil && contracts.IsCode(err, contracts.ErrNotFound) && strings.Contains(errText(err), "unknown action") {
					err = contracts.Errorf(contracts.ErrUnsupported, "this machine's pyred cannot create folders yet (upgrade it, or create %s there)", j.folder)
				}
				return mk, err
			}, func(m *Model, v any, err error) tea.Cmd {
				if err != nil {
					return q.fail(m, err)
				}
				j.cwd = j.folder
				if mk := v.(contracts.FsMkdirResult); mk.Path != "" {
					j.cwd = mk.Path
				}
				return q.next(m, j)
			})
		}
		if j.cwd == "" {
			j.cwd = j.folder
		}
		return q.startSession(m, j)
	}
	for i, pth := range j.paths {
		if pth != "" {
			continue
		}
		r := j.repos[i]
		label := r.FullName
		if label == "" {
			label = r.Name
		}
		q.setStep("cloning " + label + " on " + j.nodeName + " (not there yet)")
		op := m.opID("ns:" + q.instance + ":clone:" + j.nodeID + ":" + r.Key)
		return m.doLong(11*time.Minute, func(ctx context.Context) (any, error) {
			return repoindex.CloneVia(ctx, api, j.nodeID, contracts.RepoCloneParams{Remote: r.RemoteURL}, op)
		}, func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				return q.fail(m, err)
			}
			res := v.(contracts.RepoCloneResult)
			j.paths[i] = res.Path
			if res.Cloned {
				j.cloned = append(j.cloned, label)
			}
			return q.next(m, j)
		})
	}
	if j.cwd == "" && j.ws == wsMain {
		j.cwd = j.paths[0]
	}
	if j.cwd == "" {
		q.setStep("creating a fresh worktree on " + j.nodeName)
		op := m.opID("ns:" + q.instance + ":wt")
		params := contracts.WorktreeCreateParams{Repos: j.paths, Slug: j.title}
		if params.Slug == "" {
			params.Slug = j.prompt
		}
		if j.ws == wsBranch {
			params.Branch = j.branch
		}
		return m.doLong(time.Minute, func(ctx context.Context) (any, error) {
			var wt contracts.WorktreeCreateResult
			err := nsInvoke(ctx, api, j.nodeID, op, contracts.ActionWorktreeCreate, params, &wt, 30)
			return wt, err
		}, func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				return q.fail(m, err)
			}
			wt := v.(contracts.WorktreeCreateResult)
			j.cwd, j.wtBranch = wt.Cwd, wt.Branch
			return q.next(m, j)
		})
	}
	return q.startSession(m, j)
}

func (q *nsPanel) startSession(m *Model, j *nsJob) tea.Cmd {
	api := m.api
	q.setStep("starting " + titleCase(string(j.harness)) + " on " + j.nodeName)
	op := m.opID("ns:" + q.instance + ":start")
	policy := contracts.AccountPolicy{Mode: contracts.AccountEligible}
	if j.account != "" {
		policy = contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: j.account}
	}
	req := contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: op}, NodeID: j.nodeID, Harness: j.harness,
		Cwd: j.cwd, Title: j.title, InitialPrompt: j.prompt, AccountPolicy: policy}
	return m.doLong(time.Minute, func(ctx context.Context) (any, error) {
		return api.StartSession(ctx, req)
	}, func(m *Model, v any, err error) tea.Cmd {
		if err != nil {
			return q.fail(m, err)
		}
		q.starting, q.step = false, ""
		resp := v.(contracts.StartSessionResponse)
		sid := resp.Session.Session.SessionID
		ws := "main"
		if j.ws != wsMain {
			ws = "fresh"
		}
		for _, r := range j.repos {
			m.nsMem.remember(r.Key, nsPref{Machine: j.nodeID, Agent: string(j.harness), Workspace: ws, At: m.now()})
		}
		m.nsMem.Agent = string(j.harness)
		if m.overlay == q {
			m.overlay = nil
		}
		m.sessList.sel = sid
		m.view, m.back = viewSessions, nil
		m.applyFilter("")
		m.pendingFocus = sid
		var names []string
		for _, r := range j.repos {
			names = append(names, r.Name)
		}
		where := strings.Join(names, "+") + " on " + j.nodeName
		switch {
		case j.folder != "" && j.create:
			where = j.folder + " (new folder) on " + j.nodeName
		case j.folder != "":
			where = j.folder + " on " + j.nodeName
		case j.ws != wsMain:
			where += " (fresh worktree " + j.wtBranch + ")"
		}
		if len(j.cloned) > 0 {
			where += ", cloned " + strings.Join(j.cloned, ", ")
		}
		cmds := []tea.Cmd{m.setToast("launch requested: "+string(j.harness)+" in "+where, toneGood),
			m.reload("sessions", m.baseLoader("sessions")), m.saveState()}
		if len(j.cloned) > 0 {
			cmds = append(cmds, m.loadCatalog(true))
		}
		return tea.Batch(cmds...)
	})
}

// ---- view ----

// summary says what starting does: "Claude on server · acme/atlas · fresh
// worktree pyre/fix-cache · account auto".
func (q *nsPanel) summary(m *Model, p nsPlan) string {
	if p.nodeID == "" || (p.folder == "" && len(p.repos) == 0) {
		return m.th.dim.Render("—")
	}
	sep := m.th.dim.Render(" · ")
	out := m.th.key.Render(titleCase(string(p.harness))) + " on " + m.th.key.Render(m.nodeName(p.nodeID)) + sep
	switch {
	case p.folder == "~":
		out += "~ (home, no project)" + sep + "folder"
	case p.folder != "" && p.create:
		out += p.folder + m.th.tone(toneWarn, " (new folder, created first)") + sep + "folder"
	case p.folder != "":
		out += p.folder + sep + "folder, no project"
	default:
		var names []string
		for _, r := range p.repos {
			n := r.FullName
			if n == "" {
				n = r.Name
			}
			names = append(names, n)
		}
		out += strings.Join(names, " + ")
		if len(p.clones) > 0 {
			out += m.th.tone(toneWarn, " (clone first)")
		}
		slug := repoindex.Slug(q.title)
		if slug == "" {
			slug = repoindex.Slug(q.prompt)
		}
		if slug == "" && p.multi {
			var ns []string
			for _, r := range p.repos {
				ns = append(ns, r.Name)
			}
			slug = repoindex.Slug(strings.Join(ns, " "))
		}
		if slug == "" {
			slug = "session-" + m.now().Format("0102-1504")
		}
		switch p.ws {
		case wsMain:
			out += sep + "main checkout"
		case wsFresh:
			out += sep + "fresh worktree pyre/" + slug
		case wsBranch:
			out += sep + "new branch " + strings.TrimSpace(q.branch)
		}
	}
	switch {
	case p.harness == contracts.HarnessShell:
		out += sep + "no account"
	case p.account != "":
		l := p.account
		if a := m.account(p.account); a != nil && a.Account.Label != "" {
			l = a.Account.Label
		}
		out += sep + "account " + l
	default:
		out += sep + "account auto"
	}
	return out
}

const nsLabelW = 13

func (q *nsPanel) label(m *Model, p nsPlan, f nsField) string {
	mk, st := "  ", m.th.label
	if q.focus == f {
		mk, st = m.th.marker.Render("›")+" ", m.th.key
	}
	s := mk + st.Render(nsFieldLabels[f])
	if p.problem != "" && p.problemAt == f && f != nsWhere {
		s += m.th.tone(toneWarn, " !")
	}
	return m.mark(zoneDef{kind: zoneOverlay, name: "field:" + strconv.Itoa(int(f))}, pad(s, nsLabelW))
}

// segLines renders a segmented picker, wrapped to width.
func (q *nsPanel) segLines(m *Model, p nsPlan, f nsField, avail int, auto bool, empty string) []string {
	opts := q.options(m, p, f)
	focused := q.focus == f
	var segs []string
	for _, o := range opts {
		var s string
		switch {
		case o.on:
			s = m.th.key.Render("● " + o.label)
			if o.val == "branch" && focused {
				s += m.th.accent.Render("▏")
			}
		case o.disabled:
			s = m.th.faint.Render("○ " + o.label)
		default:
			s = m.th.dim.Render("○ " + o.label)
		}
		segs = append(segs, m.mark(zoneDef{kind: zoneOverlay, name: "opt:" + strconv.Itoa(int(f)) + ":" + o.val}, s))
	}
	if len(segs) == 0 {
		segs = []string{m.th.dim.Render(empty)}
	}
	tail := ""
	switch {
	case auto && focused:
		tail = m.th.dim.Render("auto · ←→")
	case auto:
		tail = m.th.dim.Render("auto")
	case focused && len(opts) > 1:
		tail = m.th.dim.Render("←→")
	}
	if tail != "" {
		segs = append(segs, tail)
	}
	out := wrapJoin(segs, "   ", avail)
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func (q *nsPanel) view(m *Model, w, h int) []string {
	p := q.plan(m)
	bw := min(112, w-4)
	inner := bw - 4
	avail := max(10, inner-nsLabelW)
	indent := strings.Repeat(" ", nsLabelW)
	caret := m.th.accent.Render("▏")

	// Where: the input.
	in := q.query
	switch {
	case q.focus == nsWhere:
		in += caret
		if q.query == "" {
			in += m.th.dim.Render(trunc("repo, owner/name or path", avail-1))
		}
	case q.query == "" && p.target != nil:
		in = m.th.dim.Render("(" + p.target.name + ")")
	case q.query == "":
		in = m.th.dim.Render("type a repo, owner/name, or a path like ~/scratch")
	}
	if q.browse != nil {
		in = m.th.dim.Render("📁 ") + q.browse.path + m.th.dim.Render(" — browsing")
	}
	top := []string{q.label(m, p, nsWhere) + trunc(in, avail)}
	if q.browse == nil && len([]rune(strings.TrimSpace(q.query))) < 3 {
		top = append(top, indent+m.th.faint.Render("type a repo name, or a path like ~/scratch · ")+
			m.mark(zoneDef{kind: zoneOverlay, name: "browse"}, m.th.dim.Render("↓ Browse folders")))
	}
	if len(q.extra) > 0 {
		parts := []string{m.th.dim.Render("with")}
		for i, c := range q.extra {
			parts = append(parts, m.mark(zoneDef{kind: zoneOverlay, name: "extra:" + strconv.Itoa(i)}, m.th.key.Render(c.Name)+m.th.dim.Render(" ×")))
		}
		top = append(top, indent+trunc(strings.Join(parts, "  "), avail))
	}

	// The fields under the list.
	var bottom []string
	bottom = append(bottom, "")
	ml := q.segLines(m, p, nsMachine, avail, p.nodeAuto, "no machines")
	bottom = append(bottom, q.label(m, p, nsMachine)+ml[0])
	for _, l := range ml[1:] {
		bottom = append(bottom, indent+l)
	}
	bottom = append(bottom, q.label(m, p, nsAgent)+q.segLines(m, p, nsAgent, avail, p.agentAuto, "")[0])
	wl := q.segLines(m, p, nsWorkspace, avail, p.wsAuto && p.folder == "" && !p.multi, "— a folder: no git workspace")
	bottom = append(bottom, q.label(m, p, nsWorkspace)+wl[0])
	for _, l := range wl[1:] {
		bottom = append(bottom, indent+l)
	}
	al := q.segLines(m, p, nsAccount, avail, false, "— a shell needs no account")
	bottom = append(bottom, q.label(m, p, nsAccount)+al[0])
	for _, l := range al[1:] {
		bottom = append(bottom, indent+l)
	}
	// Prompt (multi-line: the last three lines show).
	pl := strings.Split(q.prompt, "\n")
	if len(pl) > 3 {
		pl = append([]string{m.th.dim.Render("…")}, pl[len(pl)-2:]...)
	}
	for i := range pl {
		pl[i] = trunc(pl[i], avail-1)
	}
	if q.focus == nsPrompt {
		pl[len(pl)-1] += caret
	}
	if q.prompt == "" {
		pl = []string{pl[0] + m.th.dim.Render("optional first prompt (shift+enter: new line)")}
	}
	bottom = append(bottom, q.label(m, p, nsPrompt)+pl[0])
	for _, l := range pl[1:] {
		bottom = append(bottom, indent+l)
	}
	tl := trunc(q.title, avail-1)
	if q.focus == nsTitle {
		tl += caret
	}
	if q.title == "" {
		tl += m.th.dim.Render("auto (named from the prompt)")
	}
	bottom = append(bottom, q.label(m, p, nsTitle)+tl)
	bottom = append(bottom, m.th.faint.Render(strings.Repeat("─", inner)))
	if b := q.browse; b != nil {
		sep := m.th.dim.Render(" · ")
		bottom = append(bottom, trunc(m.th.key.Render(titleCase(string(p.harness)))+" on "+m.th.key.Render(m.nodeName(b.node))+sep+b.path+m.th.dim.Render(" (browsing)")+sep+"folder", inner))
	} else {
		bottom = append(bottom, trunc(q.summary(m, p), inner))
	}
	start := m.mark(zoneDef{kind: zoneOverlay, name: "start"}, m.th.key.Render("[ Start ^s ]"))
	cancel := m.mark(zoneDef{kind: zoneOverlay, name: "cancel"}, m.th.dim.Render("[ Cancel ]"))
	buttons := start + "  " + cancel
	var status string
	switch {
	case q.starting:
		el := ""
		if !q.stepAt.IsZero() {
			if s := int(time.Since(q.stepAt).Seconds()); s >= 1 {
				el = " " + strconv.Itoa(s) + "s"
			}
		}
		status = m.th.key.Render("◌ " + q.step + "…" + el)
	case q.browse != nil:
		status = m.th.dim.Render("enter on ✓ Use this folder, or ^s, chooses " + q.browse.path + " · esc: back to the list")
	case q.errLine != "":
		status = m.th.tone(toneWarn, q.errLine)
	case p.problem != "":
		status = m.th.tone(toneWarn, "! "+p.problem)
	default:
		status = m.th.dim.Render("enter: next field · ^s or ^⏎: start · esc: cancel · ") + m.mark(zoneDef{kind: zoneOverlay, name: "advanced"}, m.th.dim.Render("^a step-by-step"))
	}
	room := inner - width(buttons) - 2
	if width(status) > room {
		ls := wrap(ansiStrip(status), inner)
		for i, l := range ls {
			if q.starting {
				ls[i] = m.th.key.Render(l)
			} else {
				ls[i] = m.th.tone(toneWarn, l)
			}
		}
		bottom = append(bottom, ls...)
		bottom = append(bottom, strings.Repeat(" ", max(0, inner-width(buttons)))+buttons)
	} else {
		bottom = append(bottom, status+strings.Repeat(" ", max(2, inner-width(status)-width(buttons)))+buttons)
	}

	// The list gets the rest (a stable height: the panel does not jump).
	listH := max(3, min(12, h-4-len(top)-len(bottom)))
	var list []string
	if q.browse != nil {
		list = q.browseLines(m, avail, listH)
	} else {
		list = q.listLines(m, p, avail, listH)
	}
	for i := range list {
		list[i] = indent + list[i]
	}
	lines := append(append(top, list...), bottom...)
	return m.box("New session", lines, bw, w, h)
}

// listLines renders the Where list, grouped, the highlighted row in sight.
func (q *nsPanel) listLines(m *Model, p nsPlan, width_, listH int) []string {
	var list []string
	selLine := 0
	group := ""
	nameW := min(26, max(12, width_/4))
	whereW := min(22, max(10, width_/5))
	noteW := 20
	detW := max(6, width_-2-nameW-whereW-noteW-3)
	pinned := ""
	for i, r := range p.rows {
		if r.kind == nsBrowseRow {
			// Always in sight: pinned under the list, never scrolled away.
			sel := i == p.sel
			row := m.cursor(sel) + pad(styleIf(sel, m.th.selText).Render(r.name), nameW) + " " +
				m.th.faint.Render(trunc(r.detail+" on "+r.where, width_-nameW-3))
			pinned = m.mark(zoneDef{kind: zoneOverlay, name: "row:" + r.key}, m.rowSel(sel && q.focus == nsWhere, trunc(row, width_), width_))
			continue
		}
		if r.group != group {
			group = r.group
			list = append(list, m.th.faint.Render(group))
		}
		sel := i == p.sel
		if sel {
			selLine = len(list)
		}
		name := r.name
		pos := r.pos
		if r.kind == nsCreate {
			name, pos = "create "+r.name, nil
		}
		note := m.th.dim.Render(trunc(r.note, noteW))
		if r.kind == nsCreate {
			note = m.th.tone(toneWarn, trunc(r.note, noteW))
		}
		whereS := r.where
		if whereS == "" && r.kind != nsRepo {
			whereS = m.nodeName(p.base)
		}
		row := m.cursor(sel) + pad(m.highlight(trunc(name, nameW-1), pos, styleIf(sel, m.th.selText)), nameW) + " " +
			m.th.faint.Render(pad(trunc(r.detail, detW-1), detW)) + " " +
			m.th.dim.Render(pad(trunc(whereS, whereW-1), whereW)) + " " + note
		list = append(list, m.mark(zoneDef{kind: zoneOverlay, name: "row:" + r.key}, m.rowSel(sel && q.focus == nsWhere, trunc(row, width_), width_)))
	}
	if !p.pathMode {
		items, st := m.catalog()
		var why string
		switch {
		case st != nil && st.err != nil && !st.have:
			why = errText(st.err)
		case st == nil || !st.have:
			why = "loading repositories…"
		case len(items) == 0:
			why = "no repositories yet (machines scan ~/code, ~/src, ~/Documents/code, ~/projects; GitHub through gh)"
		}
		if why != "" {
			list = append(list, m.th.dim.Render(trunc(why, width_)))
		}
		if strings.TrimSpace(q.query) == "" {
			list = append(list, m.th.faint.Render(trunc("type / or ~/ for any folder · owner/name for any GitHub repository", width_)))
		}
	}
	full := listH
	if pinned != "" {
		listH--
	}
	if len(list) > listH {
		top := 0
		if selLine >= listH-1 {
			top = selLine - listH + 2
		}
		top = min(top, len(list)-listH)
		below := len(list) - (top + listH)
		list = list[top : top+listH]
		if below > 0 {
			list[listH-1] = m.th.dim.Render("  ↓ " + strconv.Itoa(below+1) + " more")
		}
	}
	for len(list) < listH {
		list = append(list, "")
	}
	if pinned != "" {
		list = append(list, pinned)
	}
	return list[:full]
}
