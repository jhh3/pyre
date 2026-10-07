package tui

import (
	"context"
	"encoding/json"
	"fmt"
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

// The new-session panel (n): a centered panel with one input, the same
// grammar as `pyre new` — `<repo>[+<repo>…] | <path>  [@machine]
// [harness] [first prompt…]` — and a live suggestion list grouped as
// Recent (where sessions ran lately), Repos (the fleet's repo index,
// loaded once per open) and Folders (a path typed as /…, ~/…, ./… or ../…
// completes against the chosen machine's folders; a missing one is offered
// for creation; ~ or "home" is the home folder, a projectless scratch
// session). Under the list a target line says exactly what Enter does, and
// harness, machine and fresh-worktree are visible chips (^t, ^o, ^f; clicks
// work too). Tab completes the selected suggestion, then moves to the
// optional prompt field; Enter starts from anywhere; ^a opens the
// step-by-step launch. A path with spaces is quoted ('~/My Docs') or
// escaped (~/My\ Docs, as a terminal pastes a dragged folder); ^l makes the
// whole line one folder path, spaces and all (the prompt then goes in the
// prompt field).

const reposKey = "repos:index"

const (
	qlRepo = iota
	qlFolder
	qlCreate
)

type quickBar struct {
	instance  string
	query     string // the target line
	prompt    string // the optional first-prompt field
	field     int    // 0 target, 1 prompt
	sel       int    // selected suggestion row
	token     string // what the selection was made for (reset on change)
	node      string // machine chosen with ^o or a click ("" = default)
	harness   contracts.Harness
	fresh     bool
	wholePath bool // ^l: the whole target line is one folder path
	starting  bool
	errLine   string
	cwd       string // this process's directory, for ./ and ../ paths
	home      string // this machine's home, to write those paths with ~
}

// qlRow is one suggestion.
type qlRow struct {
	group  string // Recent, Repos, Folders
	kind   int
	name   string
	path   string // tilde form where possible
	nodeID string // machine it belongs to (recent rows, folders)
	repos  []contracts.RepoEntry
	pos    []int
	note   string
}

// QuickLaunchOpen reports that the new-session panel is open (tests).
func (m *Model) QuickLaunchOpen() bool {
	_, ok := m.overlay.(*quickBar)
	return ok
}

func (m *Model) openQuickLaunch() tea.Cmd {
	q := &quickBar{instance: contracts.NewID(contracts.PrefixOperation), harness: contracts.HarnessClaude}
	q.cwd, _ = os.Getwd()
	q.home, _ = os.UserHomeDir()
	m.overlay = q
	r, ok := m.api.(contracts.ReposAPI)
	if !ok {
		return nil
	}
	return m.ensure(reposKey, time.Minute, func(ctx context.Context) (any, error) {
		return r.ListRepos(ctx, contracts.ListReposRequest{Limit: 500})
	})
}

func (q *quickBar) hints() []hint {
	return []hint{{"enter", "start"}, {"tab", "complete/prompt"}, {"^t", "harness"}, {"^o", "machine"}, {"^f", "fresh"}, {"^l", "path"}, {"^a", "step-by-step"}, {"esc", "close"}}
}

// qlPlan is what Enter would do right now.
type qlPlan struct {
	pathMode bool
	tokens   []string                // repo tokens as typed
	groups   [][]contracts.RepoEntry // chosen group per token (repo target)
	active   string                  // the token being completed
	rows     []qlRow
	target   *qlRow
	folder   string // folder target (tilde or absolute)
	create   bool   // the folder is created first
	machines []string
	nodeID   string
	typedAt  string
	harness  contracts.Harness
	prompt   string
	fresh    bool
	problem  string
	dirNode  string // folder listing to load (path mode)
	dirPath  string
}

func (m *Model) quickIndex() ([]contracts.RepoEntry, *loadState) {
	l, st := get[contracts.RepoList](m, reposKey)
	return l.Items, st
}

func qlDirKey(nodeID, dir string) string { return "qldir:" + nodeID + ":" + dir }

// qlDefaultNode is where a folder launch goes unless chosen: the selected
// session's machine when connected, else the first connected machine.
func (m *Model) qlDefaultNode(connected map[string]bool) string {
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

// qlRecent lists where sessions ran lately: one row per machine and
// folder, most recent first, a repository's main checkout named after it.
func (m *Model) qlRecent(groups [][]contracts.RepoEntry) []qlRow {
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
	byPath := map[string][]contracts.RepoEntry{}
	for _, g := range groups {
		for _, e := range g {
			byPath[e.NodeID+"\x00"+e.Path] = g
		}
	}
	seen := map[string]bool{}
	var out []qlRow
	for _, r := range all {
		k := r.s.Run.NodeID + "\x00" + r.s.Run.Cwd
		if seen[k] {
			continue
		}
		seen[k] = true
		p := tildePath(r.s.Run.Cwd)
		row := qlRow{group: "Recent", kind: qlFolder, name: path.Base(p), path: p, nodeID: r.s.Run.NodeID, note: "used " + ago(m.now(), r.at)}
		if p == "~" {
			row.name = "home"
		}
		if g, ok := byPath[k]; ok {
			row.kind, row.repos, row.name = qlRepo, g, g[0].Name
		}
		out = append(out, row)
		if len(out) >= 40 {
			break
		}
	}
	return out
}

func (q *quickBar) plan(m *Model) qlPlan {
	var p qlPlan
	all, _ := m.quickIndex()
	var fields []string
	if target, rest, _ := repoindex.SplitTarget(q.query); target != "" || len(rest) > 0 {
		fields = append([]string{target}, rest...)
	}
	connected := map[string]bool{}
	for _, n := range m.sortedNodes() {
		connected[n.NodeID] = n.Connection.State == contracts.ConnConnected
	}
	if q.wholePath {
		p.pathMode = true
	} else if len(fields) > 0 {
		f := fields[0]
		p.pathMode = repoindex.IsPathSpec(f)
		if strings.EqualFold(f, "home") {
			p.pathMode = true
			for _, e := range all {
				if strings.EqualFold(e.Name, "home") {
					p.pathMode = false
				}
			}
		}
	}
	var repos []string
	var harness, inline string
	if q.wholePath {
		p.active = wholeLinePath(q.query)
	} else if p.pathMode {
		_, p.typedAt, harness, inline = repoindex.ParseLine(append([]string{"_"}, fields[1:]...))
		p.active = fields[0]
	} else {
		repos, p.typedAt, harness, inline = repoindex.ParseLine(fields)
		if len(repos) == 0 {
			repos = []string{""} // empty line: recent places first; enter starts the selected one
		}
		p.tokens = repos
		p.active = repos[len(repos)-1]
	}
	p.prompt = strings.TrimSpace(inline + " " + strings.TrimSpace(q.prompt))
	p.harness = q.harness
	if harness != "" {
		p.harness = contracts.Harness(harness)
	}
	tok := fmt.Sprint(p.pathMode, len(p.tokens), p.active)
	if tok != q.token {
		q.token, q.sel = tok, 0
	}
	allGroups := repoindex.Group(all)

	// Machine for folders: typed @name, then a chip choice, then the default.
	var nodeOrder []string
	for _, n := range m.sortedNodes() {
		if connected[n.NodeID] {
			nodeOrder = append(nodeOrder, n.NodeID)
		}
	}
	for _, n := range m.sortedNodes() {
		if !connected[n.NodeID] {
			nodeOrder = append(nodeOrder, n.NodeID)
		}
	}
	pickNode := func(machines []string, pref string) (string, string) {
		want := q.node
		if p.typedAt != "" {
			want = ""
			for _, id := range machines {
				if strings.HasPrefix(strings.ToLower(m.nodeName(id)), strings.ToLower(p.typedAt)) {
					want = id
					break
				}
			}
			if want == "" {
				return "", "not on a machine called “" + p.typedAt + "”"
			}
		}
		if want == "" {
			want = pref
		}
		for _, id := range machines {
			if id == want {
				return id, ""
			}
		}
		if len(machines) == 0 {
			return "", "no machines"
		}
		return machines[0], ""
	}

	// Suggestions.
	if p.pathMode {
		nodeID, problem := pickNode(nodeOrder, m.qlDefaultNode(connected))
		pth, dir := repoindex.NormPath(p.active, q.cwd, q.home)
		parent, name := repoindex.SplitPath(pth, dir)
		p.dirNode, p.dirPath = nodeID, parent
		res, st := get[contracts.DirListResult](m, qlDirKey(nodeID, parent))
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
		home := pth == "~"
		self := qlRow{group: "Folders", kind: qlFolder, name: path.Base(pth), path: pth, nodeID: nodeID, note: "folder"}
		if home {
			self.name, self.note = "home", "scratch · no project"
		}
		switch {
		case home:
		case failed || (loaded && !exists):
			// A listing that fails means the folder (or its parent) is
			// missing; creation makes the parents too.
			self.kind, self.note = qlCreate, "new folder"
		case !loaded:
			self.note = "checking…"
		}
		var kids []qlRow
		if loaded {
			lname := strings.ToLower(name)
			for _, d := range res.Dirs {
				if (name == "" || strings.HasPrefix(strings.ToLower(d), lname)) && d != name {
					var pos []int
					for i := range []rune(name) {
						pos = append(pos, i)
					}
					kids = append(kids, qlRow{group: "Folders", kind: qlFolder, name: d, path: repoindex.JoinPath(parent, d), nodeID: nodeID, pos: pos, note: "folder"})
					if len(kids) >= 7 {
						break
					}
				}
			}
		}
		// Recent places on that machine under the typed path.
		var rec []qlRow
		for _, r := range m.qlRecent(allGroups) {
			if r.nodeID == nodeID && strings.HasPrefix(strings.ToLower(r.path), strings.ToLower(pth)) && r.path != pth && len(rec) < 3 {
				rec = append(rec, r)
			}
		}
		// The typed folder first when it exists; the offer to create it
		// after the folders that complete it; recent places last.
		if self.kind == qlCreate {
			p.rows = append(append(p.rows, kids...), self)
		} else {
			p.rows = append(append(p.rows, self), kids...)
		}
		p.rows = append(p.rows, rec...)
		if problem != "" {
			p.problem = problem
		}
	} else {
		single := len(p.tokens) == 1
		used := map[string]bool{}
		if single {
			n := 0
			for _, r := range m.qlRecent(allGroups) {
				if n >= 3 {
					break
				}
				_, pos, ok := matchTerms(p.active, r.name, r.path, m.nodeName(r.nodeID))
				if !ok {
					continue
				}
				r.pos = pos
				if r.repos != nil {
					used[r.repos[0].Name+"\x00"+r.repos[0].Path] = true
				}
				p.rows = append(p.rows, r)
				n++
			}
		}
		lim := 6
		switch {
		case !single:
			lim = 8
		case p.active == "":
			lim = 4 // room for Recent and the home folder
		}
		for _, g := range repoindex.Group(repoindex.Rank(p.active, all)) {
			if used[g[0].Name+"\x00"+g[0].Path] {
				continue
			}
			if lim--; lim < 0 {
				break
			}
			_, pos, _ := matchTerms(p.active, g[0].Name)
			note := "active " + ago(m.now(), g[0].LastActivity)
			for _, e := range g {
				if e.LastUsedAt != nil {
					note = "used " + ago(m.now(), *e.LastUsedAt)
					break
				}
			}
			p.rows = append(p.rows, qlRow{group: "Repos", kind: qlRepo, name: g[0].Name, path: tildePath(g[0].Path), repos: g, pos: pos, note: note})
		}
		if single && p.active == "" {
			p.rows = append(p.rows, qlRow{group: "Folders", kind: qlFolder, name: "home", path: "~", note: "scratch · no project"})
		}
	}
	if q.sel >= len(p.rows) {
		q.sel = max(0, len(p.rows)-1)
	}
	if len(p.rows) > 0 {
		p.target = &p.rows[q.sel]
	}

	// Folder target.
	if p.pathMode || (p.target != nil && p.target.kind != qlRepo) {
		if p.target == nil {
			if p.problem == "" {
				p.problem = "type a repo, or a path like ~/scratch"
			}
			return p
		}
		p.folder, p.create = p.target.path, p.target.kind == qlCreate
		p.machines = nodeOrder
		if p.pathMode {
			p.nodeID = p.dirNode
		} else {
			id, problem := pickNode(nodeOrder, p.target.nodeID)
			if p.target.nodeID == "" && q.node == "" && p.typedAt == "" {
				id = m.qlDefaultNode(connected)
			}
			p.nodeID = id
			if problem != "" {
				p.problem = problem
				return p
			}
		}
		if p.problem == "" && p.nodeID != "" && !connected[p.nodeID] {
			p.problem = m.nodeName(p.nodeID) + " is offline"
		}
		return p
	}

	// Repository target(s).
	var chosen [][]contracts.RepoEntry
	for i, t := range p.tokens {
		var g []contracts.RepoEntry
		if i == len(p.tokens)-1 {
			if p.target != nil {
				g = p.target.repos
			}
		} else if gs := repoindex.Group(repoindex.Rank(t, all)); len(gs) > 0 && t != "" {
			g = gs[0]
		}
		p.groups = append(p.groups, g)
		if g == nil {
			if len(all) > 0 && t != "" && p.problem == "" {
				p.problem = "no repository matches “" + t + "” (a folder? type it as a path like ~/" + t + ")"
			}
			continue
		}
		chosen = append(chosen, g)
	}
	p.fresh = q.fresh || len(chosen) > 1
	if len(chosen) == 0 {
		if p.problem == "" {
			p.problem = "type a repo, or a path like ~/scratch"
		}
		return p
	}
	if p.problem != "" {
		return p
	}
	p.machines = repoindex.Machines(chosen, connected)
	if len(p.machines) == 0 {
		p.problem = "no single machine has all of these repositories"
		return p
	}
	pref := ""
	if p.target != nil && p.target.group == "Recent" {
		pref = p.target.nodeID
	}
	id, problem := pickNode(p.machines, pref)
	if problem != "" {
		p.problem = problem
		return p
	}
	p.nodeID = id
	if !connected[p.nodeID] {
		p.problem = m.nodeName(p.nodeID) + " is offline"
	}
	return p
}

// wholeLinePath reads the line as one folder path (^l): spaces kept, a
// name that is not a path spec taken as a folder under home.
func wholeLinePath(line string) string {
	s := strings.TrimLeft(line, " \t")
	switch {
	case s == "":
		return "~/"
	case repoindex.IsPathSpec(s), s == "home":
		return s
	}
	return "~/" + s
}

// load fetches the folder listing the plan needs (path mode).
func (q *quickBar) load(m *Model, p qlPlan) tea.Cmd {
	if !p.pathMode || p.dirNode == "" {
		return nil
	}
	if n := m.node(p.dirNode); n == nil || n.Connection.State != contracts.ConnConnected {
		return nil
	}
	api, nid, dir := m.api, p.dirNode, p.dirPath
	return m.ensure(qlDirKey(nid, dir), 15*time.Second, func(ctx context.Context) (any, error) {
		params, _ := json.Marshal(contracts.DirListParams{Path: dir})
		res, err := api.InvokeNodeAction(ctx, contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
			NodeID: nid, Action: contracts.ActionDirList, Params: params, TimeoutSeconds: 5})
		if err != nil {
			return contracts.DirListResult{}, err
		}
		if res.Error != nil {
			return contracts.DirListResult{}, res.Error
		}
		if res.Outcome != contracts.OutcomeSucceeded {
			return contracts.DirListResult{}, contracts.Errorf(contracts.ErrUnavailable, "machine did not answer (%s)", res.Outcome)
		}
		var out contracts.DirListResult
		err = json.Unmarshal(res.Result, &out)
		return out, err
	})
}

// complete replaces the first word with the selected suggestion; false
// when that changes nothing (Tab then moves to the prompt field).
func (q *quickBar) complete(p qlPlan) bool {
	if p.target == nil {
		return false
	}
	if _, rest, open := repoindex.SplitTarget(q.query); !q.wholePath && (!open || len(rest) > 0) {
		return false // the target is finished: words after it are machine, harness, prompt
	}
	t := p.target
	quote := repoindex.QuoteTarget
	if q.wholePath {
		quote = func(s string) string { return s }
	}
	var next string
	switch {
	case p.pathMode && t.kind == qlFolder:
		pth, dir := repoindex.NormPath(p.active, q.cwd, q.home)
		if t.path == pth && dir {
			return false
		}
		next = quote(t.path) + "/"
		if t.path == "~" {
			next = "~/"
		}
	case p.pathMode:
		return false
	case t.kind == qlRepo:
		head := ""
		if i := strings.LastIndexByte(q.query, '+'); i >= 0 {
			head = q.query[:i+1]
		}
		next = head + t.repos[0].Name + " "
	default:
		next = quote(t.path) + " "
	}
	if next == q.query {
		return false
	}
	q.query = next
	return true
}

func (q *quickBar) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	if q.starting {
		if k.String() == "esc" {
			m.overlay = nil
		}
		return nil
	}
	p := q.plan(m)
	switch k.String() {
	case "esc", "ctrl+c":
		m.overlay = nil
		return nil
	case "ctrl+a":
		m.overlay = nil
		return m.openLaunch()
	case "enter":
		return q.start(m, p)
	case "tab", "shift+tab":
		if q.field == 1 {
			q.field = 0
		} else if k.String() == "shift+tab" || !q.complete(p) {
			q.field = 1
		}
	case "down", "ctrl+n", "ctrl+j":
		if q.sel < len(p.rows)-1 {
			q.sel++
		}
		q.node = ""
	case "up", "ctrl+p", "ctrl+k":
		if q.sel > 0 {
			q.sel--
		}
		q.node = ""
	case "right":
		if q.field == 0 {
			q.complete(p)
		}
	case "ctrl+o":
		q.cycleNode(p)
	case "ctrl+t":
		q.cycleHarness()
	case "ctrl+f":
		q.fresh = !q.fresh
	case "ctrl+l":
		q.toggleWholePath()
	case "backspace":
		q.edit(func(s string) string {
			if r := []rune(s); len(r) > 0 {
				return string(r[:len(r)-1])
			}
			return s
		})
	case "ctrl+w":
		q.edit(func(s string) string {
			t := strings.TrimRight(s, " ")
			if i := strings.LastIndexAny(t, " +/"); i >= 0 {
				return t[:i+1]
			}
			return ""
		})
	case "ctrl+u":
		q.edit(func(string) string { return "" })
	default:
		if k.Text == "" || k.Mod&^tea.ModShift != 0 {
			return nil
		}
		q.edit(func(s string) string { return s + k.Text })
	}
	q.errLine = ""
	return q.load(m, q.plan(m))
}

func (q *quickBar) edit(f func(string) string) {
	if q.field == 1 {
		q.prompt = f(q.prompt)
	} else {
		q.query = f(q.query)
	}
}

// toggleWholePath switches ^l: the line is read as one folder path, or
// again as `target [@machine] [harness] [prompt]`. The typed text is
// carried over (unquoted one way, quoted the other).
func (q *quickBar) toggleWholePath() {
	q.wholePath = !q.wholePath
	if q.field != 0 {
		return
	}
	if q.wholePath {
		if target, rest, _ := repoindex.SplitTarget(q.query); len(rest) == 0 {
			q.query = target
		}
		return
	}
	if strings.ContainsAny(strings.TrimSpace(q.query), " \t") {
		q.query = repoindex.QuoteTarget(strings.TrimSpace(q.query))
	}
}

func (q *quickBar) cycleNode(p qlPlan) {
	if len(p.machines) < 2 {
		return
	}
	i := 0
	for j, id := range p.machines {
		if id == p.nodeID {
			i = j
		}
	}
	q.node = p.machines[(i+1)%len(p.machines)]
}

var qlHarnesses = []contracts.Harness{contracts.HarnessClaude, contracts.HarnessCodex, contracts.HarnessShell}

func (q *quickBar) cycleHarness() {
	for i, h := range qlHarnesses {
		if h == q.harness {
			q.harness = qlHarnesses[(i+1)%len(qlHarnesses)]
			return
		}
	}
	q.harness = contracts.HarnessClaude
}

// click handles a click inside the panel: a row selects (a second click
// starts), a chip toggles, a field takes the keyboard.
func (q *quickBar) click(m *Model, name string) tea.Cmd {
	if q.starting {
		return nil
	}
	kind, val, _ := strings.Cut(name, ":")
	switch kind {
	case "row":
		i, _ := strconv.Atoi(val)
		if i == q.sel {
			return q.start(m, q.plan(m))
		}
		q.sel, q.node = i, ""
	case "node":
		q.node = val
	case "harness":
		q.harness = contracts.Harness(val)
	case "fresh":
		q.fresh = !q.fresh
	case "field":
		q.field, _ = strconv.Atoi(val)
	case "advanced":
		m.overlay = nil
		return m.openLaunch()
	}
	q.errLine = ""
	return q.load(m, q.plan(m))
}

func (q *quickBar) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	if p, ok := msg.(tea.PasteMsg); ok {
		text := strings.Join(strings.Fields(p.Content), " ")
		if q.wholePath && q.field == 0 {
			// A path keeps its spaces; only line breaks go.
			text = strings.NewReplacer("\r", "", "\n", "").Replace(p.Content)
		}
		q.edit(func(s string) string { return s + text })
		return q.load(m, q.plan(m))
	}
	return nil
}

// start creates the folder or worktrees when needed, then starts the
// session. One operation id per panel and step means a repeated Enter or a
// retry after a lost response never creates or starts twice.
func (q *quickBar) start(m *Model, p qlPlan) tea.Cmd {
	if p.problem != "" {
		q.errLine = p.problem
		return nil
	}
	var paths, names []string
	if p.folder == "" {
		for _, g := range p.groups {
			e, _ := repoindex.EntryOn(g, p.nodeID)
			paths = append(paths, e.Path)
			names = append(names, e.Name)
		}
	}
	q.starting, q.errLine = true, ""
	api := m.api
	wtOp, mkOp, startOp := m.opID("qlwt:"+q.instance), m.opID("qlmk:"+q.instance), m.opID("ql:"+q.instance)
	harness, prompt, nodeID, fresh, folder, create := p.harness, p.prompt, p.nodeID, p.fresh && p.folder == "", p.folder, p.create
	invoke := func(ctx context.Context, op, action string, params any, out any) error {
		raw, _ := json.Marshal(params)
		res, err := api.InvokeNodeAction(ctx, contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: op},
			NodeID: nodeID, Action: action, Params: raw, TimeoutSeconds: 30})
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
	return m.do(func(ctx context.Context) (any, error) {
		cwd := folder
		switch {
		case folder != "" && create:
			var mk contracts.FsMkdirResult
			if err := invoke(ctx, mkOp, contracts.ActionFsMkdir, contracts.FsMkdirParams{Path: folder}, &mk); err != nil {
				if contracts.IsCode(err, contracts.ErrNotFound) && strings.Contains(errText(err), "unknown action") {
					return nil, contracts.Errorf(contracts.ErrUnsupported, "this machine's pyred cannot create folders yet (upgrade it, or create %s there)", folder)
				}
				return nil, err
			}
			if mk.Path != "" {
				cwd = mk.Path
			}
		case folder == "":
			cwd = paths[0]
			if fresh {
				var wt contracts.WorktreeCreateResult
				if err := invoke(ctx, wtOp, contracts.ActionWorktreeCreate, contracts.WorktreeCreateParams{Repos: paths, Slug: prompt}, &wt); err != nil {
					return nil, err
				}
				cwd = wt.Cwd
			}
		}
		return api.StartSession(ctx, contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: startOp}, NodeID: nodeID,
			Harness: harness, Cwd: cwd, InitialPrompt: prompt, AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}})
	}, func(m *Model, v any, err error) tea.Cmd {
		if m.overlay != q {
			return nil
		}
		q.starting = false
		if err != nil {
			if contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) || retryable(err) {
				q.errLine = "Outcome unknown (" + errText(err) + "). Enter retries with the same operation id; nothing starts twice."
			} else {
				q.errLine = "Not started: " + errText(err)
			}
			return nil
		}
		resp := v.(contracts.StartSessionResponse)
		sid := resp.Session.Session.SessionID
		m.overlay = nil
		m.sessList.sel = sid
		m.view, m.back = viewSessions, nil
		m.applyFilter("")
		m.pendingFocus = sid
		where := strings.Join(names, "+") + " on " + m.nodeName(nodeID)
		switch {
		case folder != "" && create:
			where = folder + " (new folder) on " + m.nodeName(nodeID)
		case folder != "":
			where = folder + " on " + m.nodeName(nodeID)
		case fresh:
			where += " (fresh worktree)"
		}
		return tea.Batch(m.setToast("launch requested: "+string(harness)+" in "+where, toneGood), m.reload("sessions", m.baseLoader("sessions")))
	})
}

// targetLine says what Enter does: "Claude on server · ~/code/tundra · main checkout".
func (q *quickBar) targetLine(m *Model, p qlPlan) string {
	if p.nodeID == "" || (p.folder == "" && len(p.groups) == 0) {
		return m.th.dim.Render("—")
	}
	sep := m.th.dim.Render(" · ")
	head := m.th.key.Render(titleCase(string(p.harness))) + " on " + m.th.key.Render(m.nodeName(p.nodeID))
	if p.folder != "" {
		switch {
		case p.create:
			return head + sep + p.folder + sep + m.th.tone(toneWarn, "new folder, created on start")
		case p.folder == "~":
			return head + sep + "~" + sep + "home folder · scratch session, no project"
		}
		return head + sep + p.folder + sep + "folder, no project"
	}
	var names []string
	var first string
	for _, g := range p.groups {
		if e, ok := repoindex.EntryOn(g, p.nodeID); ok {
			names = append(names, e.Name)
			if first == "" {
				first = tildePath(e.Path)
			}
		}
	}
	if len(names) == 0 {
		return m.th.dim.Render("—")
	}
	slug := repoindex.Slug(p.prompt)
	if slug == "" && len(names) > 1 {
		slug = repoindex.Slug(strings.Join(names, " "))
	}
	if slug == "" {
		slug = "session-" + m.now().Format("0102-1504")
	}
	switch {
	case len(names) > 1:
		return head + sep + "fresh worktrees of " + strings.Join(names, " + ") + sep + "branch pyre/" + slug + m.th.dim.Render(" in ~/pyre-work/"+slug+"/")
	case p.fresh:
		return head + sep + first + sep + "fresh worktree on branch pyre/" + slug
	}
	return head + sep + first + sep + "main checkout" + m.th.dim.Render(" (the agent decides about worktrees)")
}

func (q *quickBar) chip(m *Model, zone, label string, on, enabled bool) string {
	s := "○ " + label
	switch {
	case on:
		s = m.th.key.Render("● " + label)
	case !enabled:
		s = m.th.faint.Render(s)
	default:
		s = m.th.dim.Render(s)
	}
	return m.mark(zoneDef{kind: zoneOverlay, name: zone}, s)
}

func (q *quickBar) view(m *Model, w, h int) []string {
	p := q.plan(m)
	bw := min(104, w-4)
	inner := bw - 4
	mark := func(name, s string) string { return m.mark(zoneDef{kind: zoneOverlay, name: name}, s) }

	// The input.
	caret := m.th.accent.Render("▏")
	prefix := ""
	if q.wholePath {
		prefix = m.th.tone(toneWarn, "path") + " "
	}
	in := prefix + m.th.dim.Render("›") + " " + q.query
	if q.field == 0 {
		in = prefix + m.th.key.Render("›") + " " + q.query + caret
	}
	switch {
	case q.query == "" && q.wholePath:
		in += m.th.dim.Render(trunc("the whole line is one folder path, spaces and all (^l back)", max(0, inner-9)))
	case q.query == "":
		in += m.th.dim.Render(trunc("type a repo, or a path like ~/scratch   [@machine] [claude|codex|shell] [prompt…]", max(0, inner-4)))
	}
	lines := []string{mark("field:0", pad(in, inner)), m.th.faint.Render(strings.Repeat("─", inner))}

	// Suggestions, grouped.
	listH := max(4, min(13, h-19))
	var list []string
	all, st := m.quickIndex()
	_, offered := m.api.(contracts.ReposAPI)
	machW, noteW := 16, 22
	nameW := 22
	pathW := max(8, inner-2-nameW-machW-noteW-3)
	group := ""
	for i, r := range p.rows {
		if r.group != group {
			group = r.group
			list = append(list, m.th.label.Render(group))
		}
		sel := i == q.sel
		mach := m.nodeName(r.nodeID)
		if r.kind == qlRepo && r.group == "Repos" {
			mach = r.repos[0].NodeName
			if len(r.repos) > 1 {
				mach += fmt.Sprintf(" +%d", len(r.repos)-1)
			}
		}
		if r.nodeID == "" && r.kind != qlRepo {
			mach = m.nodeName(p.nodeID)
		}
		name := r.name
		if r.kind == qlCreate {
			name = "create " + r.name
		}
		note := m.th.dim.Render(trunc(r.note, noteW))
		if r.kind == qlCreate {
			note = m.th.tone(toneWarn, trunc(r.note, noteW))
		}
		pos := r.pos
		if r.kind == qlCreate {
			pos = nil
		}
		row := m.cursor(sel) + pad(m.highlight(trunc(name, nameW-1), pos, styleIf(sel, m.th.selText)), nameW) + " " +
			m.th.faint.Render(pad(trunc(r.path, pathW-1), pathW)) + " " +
			m.th.dim.Render(pad(trunc(mach, machW-1), machW)) + " " + note
		list = append(list, mark("row:"+strconv.Itoa(i), m.rowSel(sel, trunc(row, inner), inner)))
	}
	if !p.pathMode {
		var why string
		switch {
		case !offered:
			why = "this hub has no repo index (upgrade pyre-hub); paths like ~/scratch still work"
		case !st.ok():
			why = strings.TrimSpace(ansiStrip(m.loadingOr(st, "")))
		case len(all) == 0:
			why = "no repositories indexed yet (machines scan ~/code, ~/src, ~/Documents/code, ~/projects)"
		case len(p.rows) == 0:
			why = "no repository matches “" + p.active + "” — a folder? type it as a path like ~/" + p.active
		}
		if why != "" {
			list = append(list, m.th.dim.Render("  "+trunc(why, inner-2)))
		}
	}
	if len(list) > listH {
		// Keep the selection in sight (group headers count as lines).
		top, selLine := 0, 0
		cnt, g := -1, ""
		for i, r := range p.rows {
			if r.group != g {
				g = r.group
				cnt++
			}
			cnt++
			if i == q.sel {
				selLine = cnt
			}
		}
		if selLine >= listH {
			top = selLine - listH + 1
		}
		list = list[top:min(len(list), top+listH)]
	}
	for len(list) < listH {
		list = append(list, "") // a stable height: the panel does not jump while typing
	}
	lines = append(lines, list...)
	lines = append(lines, m.th.faint.Render(strings.Repeat("─", inner)))

	// What Enter does, then the chips and the prompt.
	lines = append(lines, trunc(q.targetLine(m, p), inner), "")
	lw := 13
	label := func(l, key string) string {
		return m.th.label.Render(l) + strings.Repeat(" ", max(1, lw-width(l)-width(key)-1)) + m.th.dim.Render(key) + " "
	}
	var hs []string
	for _, hh := range qlHarnesses {
		hs = append(hs, q.chip(m, "harness:"+string(hh), string(hh), hh == p.harness, true))
	}
	lines = append(lines, label("Harness", "^t")+strings.Join(hs, "  "))
	var ms []string
	for _, id := range p.machines {
		l := m.nodeName(id)
		if n := m.node(id); n != nil && n.Connection.State != contracts.ConnConnected {
			l += " (offline)"
		}
		ms = append(ms, q.chip(m, "node:"+id, l, id == p.nodeID, true))
	}
	if len(ms) == 0 {
		ms = []string{m.th.dim.Render("—")}
	}
	lines = append(lines, trunc(label("Machine", "^o")+strings.Join(ms, "  "), inner))
	repoTarget := p.folder == "" && len(p.groups) > 0
	fl := "fresh worktree"
	switch {
	case len(p.groups) > 1:
		fl = "fresh worktrees (always, for several repos)"
	case !repoTarget:
		fl = "fresh worktree (repositories only)"
	}
	lines = append(lines, label("Worktree", "^f")+q.chip(m, "fresh", fl, repoTarget && p.fresh, repoTarget))
	pr := q.prompt
	switch {
	case q.field == 1:
		pr = m.th.key.Render("›") + " " + pr + caret
	case pr == "" && p.prompt != "":
		pr = m.th.dim.Render("› ") + p.prompt + m.th.dim.Render("  (from the line)")
	case pr == "":
		pr = m.th.dim.Render("› optional first prompt")
	default:
		pr = m.th.dim.Render("›") + " " + pr
	}
	lines = append(lines, mark("field:1", pad(label("Prompt", "tab")+trunc(pr, inner-lw-1), inner)))
	lines = append(lines, "")
	switch {
	case q.starting:
		lines = append(lines, m.th.dim.Render("starting…"))
	case q.errLine != "":
		for _, l := range wrap(q.errLine, inner) {
			lines = append(lines, m.th.tone(toneWarn, l))
		}
	default:
		lines = append(lines, m.th.dim.Render("type a repo, or a path like ~/scratch · enter starts · ")+mark("advanced", m.th.dim.Render("^a step-by-step")))
	}
	return m.box("New session", lines, bw, w, h)
}
