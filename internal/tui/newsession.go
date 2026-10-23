package tui

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// The new-session panel (n), W-newsession: a form, not a command line.
// The machine comes first, since everything else depends on it: Machine
// (every node with its load and agents; offline ones greyed out), Where
// (that machine's repositories, then its recent folders, then "Clone a
// GitHub repo here…" and "📁 Browse folders…", a type-to-filter list),
// Agent (what the machine has), Model and Effort (its choices for the
// agent), Workspace, Account (segmented pickers: every option visible,
// ←/→ or a click changes them), Prompt (multi-line) and Title. Tab /
// Shift-Tab (or ↑/↓ outside the lists) move between fields; Enter moves
// on, and on the last field starts; ^s or ^⏎ starts from anywhere; Esc
// cancels. Typing on Machine filters Where.
//
// Each field keeps its own state: moving through a list never changes
// another field's value; changing the machine re-scopes Where and keeps
// its filter text. A field the owner has not set shows its automatic
// value, marked "auto": this machine (else the last one used, else the
// least loaded), the agent and workspace last used with the repository.
// A repository chosen from "Clone … here" is cloned onto the machine
// first (repo.clone), shown in the panel while it runs. A path typed in
// Where opens the folder picker there (newsession_picker.go).

const catalogKey = "repos:catalog"

type nsField int

const (
	nsMachine nsField = iota
	nsWhere
	nsAgent
	nsModel
	nsEffort
	nsWorkspace
	nsAccount
	nsPrompt
	nsTitle
	nsFieldCount
)

var nsFieldLabels = [nsFieldCount]string{"Machine", "Where", "Agent", "Model", "Effort", "Workspace", "Account", "Prompt", "Title"}

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
	nsBrowseRow // "📁 Browse folders…": opens the folder picker
	nsCloneRow  // "Clone a GitHub repo here…": lists repositories to clone
	nsSwitchRow // a repository on another machine: choosing it switches there
)

const (
	cloneKey    = "clone"
	nsPickedKey = "picked" // the folder chosen in the picker
)

// nsRow is one entry of the Where list.
type nsRow struct {
	key    string // stable identity (the highlight survives refreshes)
	group  string
	kind   int
	name   string
	detail string // a path, or owner/name
	where  string
	note   string
	repo   *contracts.CatalogRepo // repository rows
	path   string                 // folder rows
	nodeID string                 // switch rows: the machine that has it
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
	Machine string            `json:"machine,omitempty"` // last machine overall
	Agent   string            `json:"agent,omitempty"`   // last agent overall
	Repos   map[string]nsPref `json:"repos,omitempty"`   // by catalog key
}

const nsMemMax = 200

func (n nsMemory) clone() nsMemory {
	out := nsMemory{Machine: n.Machine, Agent: n.Agent, Repos: make(map[string]nsPref, len(n.Repos))}
	for k, v := range n.Repos {
		out.Repos[k] = v
	}
	return out
}

func (n nsMemory) empty() bool { return n.Machine == "" && n.Agent == "" && len(n.Repos) == 0 }

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
	query    string // Where filter
	hlKey    string // highlighted row ("" = the best match)
	extra    []contracts.CatalogRepo
	cloning  bool   // Where lists repositories to clone onto the machine
	folder   string // the folder chosen in the picker ("" = none)
	// folderNew: the chosen folder does not exist yet (created first).
	folderNew bool
	// Explicit choices ("" / unset = automatic).
	machine string
	agent   contracts.Harness
	ws      int
	wsSet   bool
	branch  string
	account string
	model   string // "" = the harness default
	effort  string // "" = the harness default
	prompt  string
	title   string

	autoNode string    // the automatic machine, fixed when the panel opens
	picker   *nsPicker // the folder picker, when open (newsession_picker.go)
	spinning bool      // a spinner tick is scheduled

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
	q := &nsPanel{instance: contracts.NewID(contracts.PrefixOperation), focus: nsMachine}
	q.cwd, _ = os.Getwd()
	q.home, _ = os.UserHomeDir()
	m.dropPickerCache(q.instance)
	m.overlay = q
	return q.load(m)
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

// nsRecent lists the folders sessions ran in lately on machine nodeID,
// most recent first; a repository's checkout (one of items) is a
// repository row.
func (m *Model) nsRecent(items []contracts.CatalogRepo, nodeID string) []nsRow {
	type rc struct {
		s  contracts.SessionView
		at time.Time
	}
	var all []rc
	for _, s := range m.sessions() {
		if s.Run == nil || s.Run.Cwd == "" || s.Run.NodeID != nodeID {
			continue
		}
		all = append(all, rc{s, m.lastActivity(s)})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	byPath := map[string]int{}
	for i, c := range items {
		for _, e := range c.Present {
			if e.NodeID == nodeID {
				byPath[e.Path] = i
			}
		}
	}
	seen := map[string]bool{}
	var out []nsRow
	for _, r := range all {
		if seen[r.s.Run.Cwd] {
			continue
		}
		seen[r.s.Run.Cwd] = true
		// The path stays as the node reported it (tildePath compresses any
		// /home/<user>, which on that node may not be its own home).
		p := tildePath(r.s.Run.Cwd)
		row := nsRow{key: "recent:" + r.s.Run.Cwd, kind: nsFolder, name: path.Base(p), detail: p, path: r.s.Run.Cwd,
			note: "used " + ago(m.now(), r.at)}
		if p == "~" {
			row.name = "home"
		}
		if i, ok := byPath[r.s.Run.Cwd]; ok {
			row.kind, row.repo, row.name = nsRepo, &items[i], items[i].Name
		}
		out = append(out, row)
		if len(out) >= 40 {
			break
		}
	}
	return out
}

// whereQuery is the Where text as a filter (a pasted, escaped or quoted
// path is unescaped).
func (q *nsPanel) whereQuery() string {
	s := strings.TrimLeft(q.query, " \t")
	if strings.ContainsAny(s, `\'"`) {
		t, rest, _ := repoindex.SplitTarget(s)
		s = strings.Join(append([]string{t}, rest...), " ")
	}
	return s
}

// pathTyped is the Where text when it is a path the folder picker should
// take ("" when not): ~…, /…, ./…, ../… or a drive path (C:\…). A lone
// "." or ".." waits for its slash, so ./x and ../x resolve as typed.
func (q *nsPanel) pathTyped() string {
	if raw := strings.TrimSpace(q.query); isDrive(raw) && len(raw) >= 3 {
		return raw // backslashes are separators here, not escapes
	}
	s := strings.TrimSpace(q.whereQuery())
	if s == "." || s == ".." || !repoindex.IsPathSpec(s) {
		return ""
	}
	return s
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

// rows builds the Where list on machine node for the current text: its
// repositories, its recent folders, then the Clone and Browse rows (last,
// pinned under the list). In clone mode: the repositories it lacks.
func (q *nsPanel) rows(m *Model, items []contracts.CatalogRepo, node string) []nsRow {
	query := strings.TrimSpace(q.whereQuery())
	if q.cloning {
		return q.cloneRows(m, items, node, query)
	}
	var rows []nsRow
	if q.folder != "" {
		r := nsRow{key: nsPickedKey, group: "Folder", kind: nsFolder, name: path.Base(q.folder), detail: q.folder, path: q.folder, note: "chosen"}
		if q.folder == "~" {
			r.name = "home"
		}
		if q.folderNew {
			r.kind, r.note = nsCreate, "new folder"
		}
		rows = append(rows, r)
	}
	nodeName := m.nodeName(node)
	shown := map[string]bool{}
	for _, c := range repoindex.RankCatalog(items, query) {
		e, ok := repoindex.EntryOn(c.Present, node)
		if !ok {
			continue
		}
		c := c
		row := nsRow{key: "repo:" + c.Key, group: "Repositories on " + nodeName, kind: nsRepo, repo: &c, name: c.Name,
			detail: tildePath(e.Path), where: c.FullName, note: catalogNote(m, c)}
		if row.where == "" {
			row.where = "no remote"
		}
		_, row.pos, _ = matchTerms(query, c.Name)
		shown[c.Key] = true
		rows = append(rows, row)
		if len(shown) >= 60 {
			break
		}
	}
	// A filter matching a repository only on other machines offers going
	// there (the machine last used with it, else the best that has it)
	// before cloning a second copy here.
	if query != "" {
		n := 0
		for _, c := range repoindex.RankCatalog(items, query) {
			if n >= 3 {
				break
			}
			if shown[c.Key] {
				continue
			}
			if _, ok := repoindex.EntryOn(c.Present, node); ok {
				continue
			}
			to := m.nsRepoMachine(c, node)
			if to == "" {
				continue
			}
			c := c
			e, _ := repoindex.EntryOn(c.Present, to)
			row := nsRow{key: "switch:" + c.Key, group: "On other machines", kind: nsSwitchRow, repo: &c, name: c.Name,
				detail: tildePath(e.Path), where: "on " + m.nodeName(to), note: "enter: switch there", nodeID: to}
			_, row.pos, _ = matchTerms(query, c.Name)
			rows = append(rows, row)
			n++
		}
	}
	lim := 5
	n := 0
	for _, r := range m.nsRecent(items, node) {
		if n >= lim {
			break
		}
		if r.repo != nil && shown[r.repo.Key] {
			continue
		}
		_, pos, ok := matchTerms(query, r.name, r.detail)
		if !ok {
			continue
		}
		r.group, r.pos = "Recent folders on "+nodeName, pos
		if r.repo != nil {
			r.where = r.repo.FullName
		}
		rows = append(rows, r)
		n++
	}
	if _, pos, ok := matchTerms(query, "home"); ok && !q.hasHome(rows) {
		rows = append(rows, nsRow{key: "home", group: "Recent folders on " + nodeName, kind: nsFolder, name: "home", detail: "~", path: "~", pos: pos, note: "scratch · no project"})
	}
	clone := nsRow{key: cloneKey, kind: nsCloneRow, name: "⇣ Clone a GitHub repo here…", detail: "any repository not on " + nodeName}
	if query != "" {
		if k := len(q.cloneCandidates(items, node, query)); k > 0 {
			clone.detail = strconv.Itoa(k) + " not on " + nodeName + " match “" + query + "”"
		}
	}
	browse := nsRow{key: browseKey, kind: nsBrowseRow, name: "📁 Browse folders…", detail: "any folder on " + nodeName + ", or a new one"}
	return append(rows, clone, browse)
}

// nsRepoMachine is the connected machine (not except) to go to for
// repository c: the one last used with it, else the first that has it.
func (m *Model) nsRepoMachine(c contracts.CatalogRepo, except string) string {
	ok := func(id string) bool {
		if id == "" || id == except {
			return false
		}
		if _, has := repoindex.EntryOn(c.Present, id); !has {
			return false
		}
		n := m.node(id)
		return n != nil && n.Connection.State == contracts.ConnConnected
	}
	if p, has := m.nsMem.Repos[c.Key]; has && ok(p.Machine) {
		return p.Machine
	}
	for _, e := range c.Present {
		if ok(e.NodeID) {
			return e.NodeID
		}
	}
	return ""
}

// switchTo chooses a switch row: its machine, the repository highlighted
// there (the filter kept).
func (q *nsPanel) switchTo(r nsRow) {
	q.setOpt(nsMachine, r.nodeID)
	q.hlKey = "repo:" + r.repo.Key
}

func (q *nsPanel) hasHome(rows []nsRow) bool {
	for _, r := range rows {
		if r.kind != nsRepo && r.detail == "~" {
			return true
		}
	}
	return false
}

// cloneCandidates are the repositories machine node lacks that it can
// clone (a remote), best match first; owner/name typed is one even when
// the catalog does not list it.
func (q *nsPanel) cloneCandidates(items []contracts.CatalogRepo, node, query string) []contracts.CatalogRepo {
	var out []contracts.CatalogRepo
	found := false
	for _, c := range repoindex.RankCatalog(items, query) {
		found = found || strings.EqualFold(c.FullName, query)
		if _, ok := repoindex.EntryOn(c.Present, node); ok || c.RemoteURL == "" {
			continue
		}
		out = append(out, c)
	}
	if repoindex.IsGitHubSpec(query) && !found {
		remote := repoindex.GitHubRemote(query)
		_, name, _ := strings.Cut(query, "/")
		out = append([]contracts.CatalogRepo{{Key: repoindex.CatalogKey(contracts.Repo{RemoteURL: remote}), Name: name,
			FullName: query, RemoteURL: remote, Present: []contracts.RepoEntry{}, GitHub: true}}, out...)
	}
	return out
}

// cloneRows is the Where list in clone mode.
func (q *nsPanel) cloneRows(m *Model, items []contracts.CatalogRepo, node, query string) []nsRow {
	var rows []nsRow
	group := "Clone onto " + m.nodeName(node)
	for _, c := range q.cloneCandidates(items, node, query) {
		c := c
		where := "GitHub"
		if len(c.Present) > 0 {
			var names []string
			for _, e := range c.Present {
				names = append(names, e.NodeName)
			}
			where = "on " + strings.Join(names, ", ")
		}
		detail := c.FullName
		if detail == "" {
			detail = c.RemoteURL
		}
		row := nsRow{key: "clone:" + c.Key, group: group, kind: nsRepo, repo: &c, name: c.Name, detail: detail, where: where, note: catalogNote(m, c)}
		_, row.pos, _ = matchTerms(query, c.Name)
		rows = append(rows, row)
		if len(rows) >= 60 {
			break
		}
	}
	return rows
}

// nsPlan is what the form says right now.
type nsPlan struct {
	rows      []nsRow
	sel       int
	target    *nsRow
	repos     []contracts.CatalogRepo
	folder    string
	create    bool
	nodeID    string
	nodeAuto  bool
	agents    []contracts.Harness // what the machine can run
	harness   contracts.Harness
	agentAuto bool
	ws        int
	wsAuto    bool
	multi     bool
	account   string
	model     string
	effort    string
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

	// Machine: first, everything below depends on it.
	if q.machine != "" && m.node(q.machine) != nil {
		p.nodeID = q.machine
	} else {
		if q.autoNode == "" || !p.connected[q.autoNode] {
			q.autoNode = m.nsDefaultMachine(p.connected)
		}
		p.nodeAuto, p.nodeID = true, q.autoNode
	}

	items, _ := m.catalog()
	p.rows = q.rows(m, items, p.nodeID)
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
	if t := p.target; t != nil && (t.kind == nsFolder || t.kind == nsCreate) && len(q.extra) == 0 {
		p.folder, p.create = t.path, t.kind == nsCreate
	}
	key := ""
	if len(p.repos) > 0 {
		key = p.repos[0].Key
	}
	pref, hasPref := m.nsMem.Repos[key]

	// Agent: only what the machine has.
	p.agents = m.nodeAgents(p.nodeID)
	if q.agent != "" && hasAgent(p.agents, q.agent) {
		p.harness = q.agent
	} else {
		p.agentAuto = true
		var cands []contracts.Harness
		if hasPref {
			cands = append(cands, contracts.Harness(pref.Agent))
		}
		for _, h := range append(cands, contracts.Harness(m.nsMem.Agent), contracts.HarnessClaude) {
			if h != "" && hasAgent(p.agents, h) {
				p.harness = h
				break
			}
		}
		if p.harness == "" && len(p.agents) > 0 {
			p.harness = p.agents[0]
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
	// Model and effort: only choices the machine lists for the agent.
	if hi, ok := m.harnessOn(p.nodeID, p.harness); ok && p.harness != contracts.HarnessShell {
		for _, mc := range hi.Models {
			if mc.ID == q.model {
				p.model = q.model
			}
		}
		for _, e := range hi.Efforts {
			if e == q.effort {
				p.effort = q.effort
			}
		}
	}
	for _, r := range p.repos {
		if _, ok := repoindex.EntryOn(r.Present, p.nodeID); !ok {
			p.clones = append(p.clones, r)
		}
	}
	q.validate(m, &p)
	return p
}

// bestRow is the row a new filter highlights: a name typed in full ("home"
// is the home folder, not every repository under /home); else the
// best-matching repository here (the first on a tie), then one on another
// machine (switching there), then a folder; with none, Clone when
// repositories elsewhere match, else Browse. No filter: the first row.
func bestRow(rows []nsRow, query string) int {
	if query == "" {
		return 0
	}
	for _, kinds := range [][]int{{nsRepo}, {nsSwitchRow}, {nsFolder, nsCreate}} {
		for i, r := range rows {
			if strings.EqualFold(r.name, query) && slices.Contains(kinds, r.kind) {
				return i
			}
		}
	}
	for _, kind := range []int{nsRepo, nsSwitchRow} {
		best, at := -1, 0
		for i, r := range rows {
			if r.kind != kind || r.repo == nil {
				continue
			}
			if s, ok := repoindex.CatalogScore(query, *r.repo); ok && s > best {
				best, at = s, i
			}
		}
		if best >= 0 {
			return at
		}
	}
	for i, r := range rows {
		if r.kind == nsFolder || r.kind == nsCreate {
			return i
		}
	}
	for i, r := range rows {
		if r.kind == nsCloneRow && !strings.HasPrefix(r.detail, "any ") {
			return i
		}
	}
	for i, r := range rows {
		if r.kind == nsBrowseRow {
			return i
		}
	}
	return 0
}

func (q *nsPanel) validate(m *Model, p *nsPlan) {
	bad := func(f nsField, s string) {
		if p.problem == "" {
			p.problem, p.problemAt = s, f
		}
	}
	switch {
	case p.nodeID == "":
		bad(nsMachine, "no machines (pair one with pyre nodes pair)")
	case !p.connected[p.nodeID]:
		bad(nsMachine, m.nodeName(p.nodeID)+" is offline: choose another machine")
	}
	query := strings.TrimSpace(q.whereQuery())
	name := m.nodeName(p.nodeID)
	switch t := p.target; {
	case t == nil && len(q.extra) == 0:
		_, st := m.catalog()
		switch {
		case q.cloning && query != "":
			bad(nsWhere, "nothing to clone matches “"+query+"”: type owner/name for any GitHub repository")
		case q.cloning:
			bad(nsWhere, "every repository is on "+name+" already")
		case st == nil || (!st.have && st.err == nil):
			bad(nsWhere, "loading repositories…")
		default:
			bad(nsWhere, "choose a repository or a folder")
		}
	case t != nil && t.kind == nsBrowseRow && len(q.extra) == 0:
		if query != "" {
			bad(nsWhere, "nothing on "+name+" matches “"+query+"”: Enter browses its folders")
		} else {
			bad(nsWhere, "Enter browses "+name+"'s folders")
		}
	case t != nil && t.kind == nsCloneRow && len(q.extra) == 0:
		if query != "" {
			bad(nsWhere, "no repository on "+name+" matches “"+query+"”: Enter lists the ones to clone here")
		} else {
			bad(nsWhere, "Enter lists repositories to clone onto "+name)
		}
	case t != nil && t.kind == nsSwitchRow && len(q.extra) == 0:
		bad(nsWhere, t.name+" is on "+m.nodeName(t.nodeID)+": Enter (or ^s) switches there")
	case t != nil && t.kind != nsRepo && len(q.extra) > 0:
		bad(nsWhere, "a folder cannot join repositories: highlight a repository, or remove the added ones")
	}
	if len(p.agents) == 0 && p.nodeID != "" {
		bad(nsAgent, name+" reports no agent it can run")
	}
	for _, c := range p.clones {
		if c.RemoteURL == "" {
			bad(nsWhere, c.Name+" has no remote, so it cannot be cloned onto "+name)
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

// load fetches what the panel needs: the catalog (and the picker's listing).
func (q *nsPanel) load(m *Model) tea.Cmd {
	return tea.Batch(m.loadCatalog(false), q.loadPicker(m))
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

// ---- options of the pickers ----

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
			out = append(out, nsOpt{val: n.NodeID, label: m.nodeName(n.NodeID), on: n.NodeID == p.nodeID, disabled: !p.connected[n.NodeID]})
		}
	case nsAgent:
		for _, h := range p.agents {
			out = append(out, nsOpt{val: string(h), label: titleCase(string(h)), on: h == p.harness})
		}
	case nsModel, nsEffort:
		if p.harness == contracts.HarnessShell {
			return nil
		}
		cur := p.model
		if f == nsEffort {
			cur = p.effort
		}
		out = append(out, nsOpt{val: "", label: "default", on: cur == ""})
		hi, _ := m.harnessOn(p.nodeID, p.harness)
		if f == nsModel {
			for _, mc := range hi.Models {
				out = append(out, nsOpt{val: mc.ID, label: mc.ID, on: cur == mc.ID})
			}
		} else {
			for _, e := range hi.Efforts {
				out = append(out, nsOpt{val: e, label: e, on: cur == e})
			}
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
		if cur := q.machine; val != cur && (cur != "" || val != q.autoNode) {
			// Where is the machine's: a folder chosen on another one, or
			// repositories added there, do not carry over; the filter does.
			q.folder, q.folderNew, q.extra, q.hlKey = "", false, nil, ""
		}
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
	case nsModel:
		q.model = val
	case nsEffort:
		q.effort = val
	}
}

// cycle moves a picker's choice by d (wrapping, skipping disabled
// options).
func (q *nsPanel) cycle(m *Model, p nsPlan, f nsField, d int) bool {
	return q.stepOpt(m, p, f, d, true)
}

// stepOpt is cycle; without wrap it stops at the ends (the machine list's
// arrows: ↓ past the last machine moves on to Where).
func (q *nsPanel) stepOpt(m *Model, p nsPlan, f nsField, d int, wrap bool) bool {
	opts := q.options(m, p, f)
	if len(opts) == 0 {
		return false
	}
	cur := 0
	for i, o := range opts {
		if o.on {
			cur = i
		}
	}
	for n := 1; n < len(opts)+1; n++ {
		i := cur + d*n
		if !wrap && (i < 0 || i >= len(opts)) {
			return false
		}
		o := opts[(i%len(opts)+len(opts))%len(opts)]
		if !o.disabled {
			if o.val == opts[cur].val {
				return false
			}
			q.setOpt(f, o.val)
			return true
		}
	}
	return false
}

func isSegmented(f nsField) bool {
	return f == nsAgent || f == nsWorkspace || f == nsAccount || f == nsModel || f == nsEffort
}

// ---- keys, clicks, paste ----

func (q *nsPanel) hintsFor(m *Model) []hint {
	if q.picker != nil && q.focus == nsWhere {
		return q.pickerHints()
	}
	tail := []hint{{"^s", "start"}, {"esc", "cancel"}, {"^a", "step-by-step"}}
	switch {
	case q.focus == nsMachine:
		return append([]hint{{"↑↓", "machine"}, {"enter", "next"}, {"type", "filter Where"}}, tail...)
	case q.focus == nsWhere && q.cloning:
		return append([]hint{{"↑↓", "choose"}, {"tab", "next field"}, {"esc", "back to " + "the list"}}, tail[:1]...)
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
	if q.picker != nil && q.focus == nsWhere {
		return q.pickerKeyMsg(m, k)
	}
	p := q.plan(m)
	switch key {
	case "esc":
		if q.cloning && q.focus == nsWhere {
			q.cloning, q.hlKey = false, ""
			break
		}
		m.overlay = nil
		return nil
	case "ctrl+c":
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
		q.move(1)
	default:
		if cmd, done := q.fieldKey(m, p, k); done {
			return cmd
		}
	}
	q.errLine = ""
	return q.load(m)
}

// fieldKey handles a key for the focused field; done means the key ended
// the update (a start, or the picker opening).
func (q *nsPanel) fieldKey(m *Model, p nsPlan, k tea.KeyPressMsg) (tea.Cmd, bool) {
	key := k.String()
	switch q.focus {
	case nsMachine:
		switch key {
		case "up", "left":
			q.stepOpt(m, p, nsMachine, -1, false)
		case "down", "right", "space":
			if !q.stepOpt(m, p, nsMachine, 1, false) && key == "down" {
				q.move(1)
			}
		case "enter":
			q.move(1)
		default:
			// Typing on Machine filters Where.
			if k.Text != "" && k.Mod&^tea.ModShift == 0 {
				q.focus = nsWhere
				return q.fieldKey(m, p, k)
			}
		}
	case nsWhere:
		switch key {
		case "up", "ctrl+p", "ctrl+k":
			if p.sel == 0 {
				q.move(-1)
			} else {
				q.moveHighlight(p, -1)
			}
		case "down", "ctrl+n", "ctrl+j":
			q.moveHighlight(p, 1)
		case "pgup":
			q.moveHighlight(p, -8)
		case "pgdown":
			q.moveHighlight(p, 8)
		case "enter":
			switch t := p.target; {
			case t != nil && t.kind == nsBrowseRow:
				return q.openPicker(m, p, ""), true
			case t != nil && t.kind == nsCloneRow:
				q.cloning, q.hlKey = true, ""
			case t != nil && t.kind == nsSwitchRow:
				q.switchTo(*t)
			case t == nil && len(q.extra) == 0:
				q.errLine = p.problem
				return nil, true
			default:
				q.move(1)
			}
		case "+":
			if t := p.target; t != nil && t.kind == nsRepo && !q.cloning {
				q.extra = append(q.extra, *t.repo)
				q.query, q.hlKey = "", ""
			} else if q.editText(&q.query, k) {
				q.hlKey = ""
			}
		case "backspace":
			if q.query == "" && len(q.extra) > 0 {
				q.extra = q.extra[:len(q.extra)-1]
				break
			}
			if q.query == "" && q.folder != "" {
				q.folder, q.folderNew, q.hlKey = "", false, ""
				break
			}
			q.editText(&q.query, k)
			q.hlKey = ""
		default:
			if q.editText(&q.query, k) {
				q.hlKey = "" // a new filter: the best match
				if s := q.pathTyped(); s != "" && !q.cloning {
					// A path: the folder picker takes it from here.
					q.query = ""
					return q.openPicker(m, p, s), true
				}
			}
		}
	case nsAgent, nsWorkspace, nsAccount, nsModel, nsEffort:
		branchTyping := q.focus == nsWorkspace && p.ws == wsBranch
		switch key {
		case "left":
			q.cycle(m, p, q.focus, -1)
		case "right":
			q.cycle(m, p, q.focus, 1)
		case "space":
			if !branchTyping {
				q.cycle(m, p, q.focus, 1)
			} else {
				q.editText(&q.branch, k)
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

// wheel scrolls the Where list (whatever field has the keyboard).
func (q *nsPanel) wheel(m *Model, dir string) tea.Cmd {
	if q.starting {
		return nil
	}
	if q.picker != nil {
		return q.pickerKeyMsg(m, keyFor(dir))
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
		switch {
		case val == browseKey:
			return q.openPicker(m, q.plan(m), "")
		case val == cloneKey:
			q.cloning, q.hlKey = true, ""
		case strings.HasPrefix(val, "switch:"):
			if p := q.plan(m); p.target != nil && p.target.kind == nsSwitchRow {
				q.switchTo(*p.target)
			}
		}
	case "pick":
		q.focus = nsWhere
		return q.pickerClick(m, val)
	case "field":
		i, _ := strconv.Atoi(val)
		q.focus = nsField(i)
		if q.focus != nsWhere {
			q.picker = nil
		}
	case "opt":
		f, v, _ := strings.Cut(val, ":")
		i, _ := strconv.Atoi(f)
		q.focus = nsField(i)
		q.picker = nil
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
	return q.load(m)
}

func (q *nsPanel) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case nsTickMsg:
		if msg.q == q && q.starting {
			return q.tick()
		}
	case nsSpinMsg:
		if msg.q == q {
			return q.onSpin(m)
		}
	case tea.PasteMsg:
		if q.starting {
			return nil
		}
		text := msg.Content
		if q.picker != nil && q.focus == nsWhere {
			q.pickerPaste(m, text)
			return q.loadPicker(m)
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
			if s := q.pathTyped(); s != "" && !q.cloning {
				q.query = ""
				return q.openPicker(m, q.plan(m), s)
			}
		}
		return q.load(m)
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
	model, effort    string
	cwd              string
	cloned           []string
	wtBranch         string
	wt               *contracts.WorktreeCreateResult // fresh worktrees (claimed for the session once it starts)
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
	if q.picker != nil {
		// Browsing: the picker's folder, never Where's highlighted row.
		return q.pickerStart(m)
	}
	if t := p.target; t != nil && t.kind == nsSwitchRow {
		q.switchTo(*t)
		if p = q.plan(m); p.target != nil && p.target.kind == nsSwitchRow {
			return q.load(m) // never loops: the repository is here now
		}
		return q.start(m, p)
	}
	if t := p.target; t != nil && len(q.extra) == 0 && p.problemAt == nsWhere && p.problem != "" {
		switch t.kind {
		case nsBrowseRow:
			return q.openPicker(m, p, "")
		case nsCloneRow:
			q.cloning, q.hlKey, q.focus = true, "", nsWhere
			return q.load(m)
		}
	}
	if p.problem != "" {
		q.errLine, q.focus = p.problem, p.problemAt
		return nil
	}
	j := &nsJob{nodeID: p.nodeID, nodeName: m.nodeName(p.nodeID), harness: p.harness, prompt: strings.TrimSpace(q.prompt),
		title: strings.TrimSpace(q.title), folder: p.folder, create: p.create, repos: p.repos, ws: p.ws,
		branch: strings.TrimSpace(q.branch), account: p.account, model: p.model, effort: p.effort}
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
		return m.doLong(3*time.Minute, func(ctx context.Context) (any, error) {
			return repoindex.CreateVia(ctx, api, j.nodeID, params, op) // fetches origin first
		}, func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				return q.fail(m, err)
			}
			wt := v.(contracts.WorktreeCreateResult)
			j.cwd, j.wtBranch, j.wt = wt.Cwd, wt.Branch, &wt
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
		policy = contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: j.account, Strict: true} // the owner's own pick
	}
	req := contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: op}, NodeID: j.nodeID, Harness: j.harness,
		Cwd: j.cwd, Title: j.title, InitialPrompt: j.prompt, AccountPolicy: policy, Args: nsModelArgs(j.model, j.effort)}
	claimOp := m.opID("ns:" + q.instance + ":claim")
	return m.doLong(time.Minute, func(ctx context.Context) (any, error) {
		resp, err := api.StartSession(ctx, req)
		if err == nil && j.wt != nil && j.wt.ClaimToken != "" {
			// W-lifecycle: the fresh worktrees now name their session, so its
			// automatic cleanup may remove them (best effort).
			params := contracts.WorktreeClaimParams{SessionID: resp.Session.Session.SessionID, Token: j.wt.ClaimToken}
			for _, w := range j.wt.Worktrees {
				params.Paths = append(params.Paths, w.Path)
			}
			var res contracts.WorktreeClaimResult
			_ = nsInvoke(ctx, api, j.nodeID, claimOp, contracts.ActionWorktreeClaim, params, &res, 10)
		}
		return resp, err
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
		m.nsMem.Agent, m.nsMem.Machine = string(j.harness), j.nodeID
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
	if p.model != "" || p.effort != "" {
		out += sep + strings.TrimSpace(p.model+" "+p.effort)
	}
	return out
}

// nsModelArgs is a launch's model and effort as the node's start args
// (daemon.ModelArg/EffortArg; nothing chosen: none, the harness default).
func nsModelArgs(model, effort string) []string {
	var args []string
	if model != "" {
		args = append(args, "--pyre-model="+model)
	}
	if effort != "" {
		args = append(args, "--pyre-effort="+effort)
	}
	return args
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
	field := func(out []string, f nsField, ls []string) []string {
		out = append(out, q.label(m, p, f)+ls[0])
		for _, l := range ls[1:] {
			out = append(out, indent+l)
		}
		return out
	}

	// Machine, then the Where input.
	top := field(nil, nsMachine, q.machineLines(m, p, avail))
	in := q.query
	switch {
	case q.picker != nil:
		in = q.pickerInput(m)
	case q.focus == nsWhere:
		in += caret
		if q.query == "" && q.cloning {
			in += m.th.dim.Render(trunc("search, or owner/name for any GitHub repository", avail-1))
		} else if q.query == "" {
			in += m.th.dim.Render(trunc("filter, or a path like ~/scratch", avail-1))
		}
	case q.query == "" && p.target != nil && p.target.kind != nsBrowseRow && p.target.kind != nsCloneRow:
		in = m.th.dim.Render("(" + p.target.name + ")")
	case q.query == "":
		in = m.th.dim.Render("type a repo, or a path like ~/scratch")
	}
	if q.cloning && q.picker == nil {
		in = m.th.dim.Render("clone: ") + in
	}
	top = append(top, q.label(m, p, nsWhere)+trunc(in, avail))
	if len(q.extra) > 0 {
		parts := []string{m.th.dim.Render("with")}
		for i, c := range q.extra {
			parts = append(parts, m.mark(zoneDef{kind: zoneOverlay, name: "extra:" + strconv.Itoa(i)}, m.th.key.Render(c.Name)+m.th.dim.Render(" ×")))
		}
		top = append(top, indent+trunc(strings.Join(parts, "  "), avail))
	}

	// The fields under the list.
	var bottom []string
	agentEmpty := "— " + m.nodeName(p.nodeID) + " has no agent it can run"
	bottom = field(bottom, nsAgent, q.segLines(m, p, nsAgent, avail, p.agentAuto, agentEmpty))
	for _, f := range []nsField{nsModel, nsEffort} {
		bottom = field(bottom, f, q.segLines(m, p, f, avail, false, "— a shell has no model"))
	}
	bottom = field(bottom, nsWorkspace, q.segLines(m, p, nsWorkspace, avail, p.wsAuto && p.folder == "" && !p.multi, "— a folder: no git workspace"))
	bottom = field(bottom, nsAccount, q.segLines(m, p, nsAccount, avail, false, "— a shell needs no account"))
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
	bottom = field(bottom, nsPrompt, pl)
	tl := trunc(q.title, avail-1)
	if q.focus == nsTitle {
		tl += caret
	}
	if q.title == "" {
		tl += m.th.dim.Render("auto (named from the prompt)")
	}
	bottom = append(bottom, q.label(m, p, nsTitle)+tl)
	bottom = append(bottom, m.th.faint.Render(strings.Repeat("─", inner)))
	if b := q.picker; b != nil {
		sep := m.th.dim.Render(" · ")
		dir, seg := pickerSplit(b.input)
		if seg != "" {
			dir = repoindex.JoinPath(dir, seg)
		}
		bottom = append(bottom, trunc(m.th.key.Render(titleCase(string(p.harness)))+" on "+m.th.key.Render(m.nodeName(b.node))+sep+dir+m.th.dim.Render(" (browsing)")+sep+"folder", inner))
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
	case q.picker != nil:
		status = m.th.dim.Render("enter on ✓ Use chooses the folder · ^s: use it and start · esc: back to the list")
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
	if q.picker != nil {
		list = q.pickerLines(m, avail, listH)
	} else {
		list = q.listLines(m, p, avail, listH)
	}
	for i := range list {
		list[i] = indent + list[i]
	}
	lines := append(append(top, list...), bottom...)
	return m.box("New session", lines, bw, w, h)
}

// listLines renders the Where list, grouped, the highlighted row in
// sight; the Clone and Browse rows are pinned under it.
func (q *nsPanel) listLines(m *Model, p nsPlan, width_, listH int) []string {
	var list, pinned []string
	selLine := 0
	group := ""
	nameW := min(26, max(12, width_/4))
	whereW := min(22, max(10, width_/5))
	noteW := 20
	detW := max(6, width_-2-nameW-whereW-noteW-3)
	focused := q.focus == nsWhere
	for i, r := range p.rows {
		sel := i == p.sel
		if r.kind == nsBrowseRow || r.kind == nsCloneRow {
			// Always in sight: pinned under the list, never scrolled away.
			row := m.cursor(sel) + pad(styleIf(sel, m.th.selText).Render(r.name), nameW+detW+1) + " " +
				m.th.faint.Render(trunc(r.detail, width_-nameW-detW-4))
			pinned = append(pinned, m.mark(zoneDef{kind: zoneOverlay, name: "row:" + r.key}, m.rowSel(sel && focused, trunc(row, width_), width_)))
			continue
		}
		if r.group != group {
			group = r.group
			list = append(list, m.th.faint.Render(group))
		}
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
		row := m.cursor(sel) + pad(m.highlight(trunc(name, nameW-1), pos, styleIf(sel, m.th.selText)), nameW) + " " +
			m.th.faint.Render(pad(trunc(r.detail, detW-1), detW)) + " " +
			m.th.dim.Render(pad(trunc(r.where, whereW-1), whereW)) + " " + note
		list = append(list, m.mark(zoneDef{kind: zoneOverlay, name: "row:" + r.key}, m.rowSel(sel && focused, trunc(row, width_), width_)))
	}
	items, st := m.catalog()
	var why string
	switch {
	case st != nil && st.err != nil && !st.have:
		why = errText(st.err)
	case st == nil || !st.have:
		why = "loading repositories…"
	case q.cloning && len(p.rows) == 0:
		why = "nothing to clone matches: type owner/name for any GitHub repository · esc: back"
	case !q.cloning && len(items) == 0:
		why = "no repositories yet (machines scan ~/code, ~/src, ~/Documents/code, ~/projects; GitHub through gh)"
	case !q.cloning && len(list) == 0 && strings.TrimSpace(q.query) == "":
		why = "no repositories or recent folders on " + m.nodeName(p.nodeID) + " yet"
	case !q.cloning && len(list) == 0:
		why = "nothing on " + m.nodeName(p.nodeID) + " (or another connected machine) matches"
	}
	if why != "" {
		list = append(list, m.th.dim.Render(trunc(why, width_)))
	}
	full := listH
	listH -= len(pinned)
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
	return append(list, pinned...)[:full]
}
