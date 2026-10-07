package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

const (
	stepNode = iota
	stepHarness
	stepWhere
	stepCheckout
	stepBrowse
	stepConfirm
)

var stepNames = []string{"Machine", "Harness", "Directory or project", "Checkout", "Directory", "Review & start"}

type launchState struct {
	step       int
	hist       []int
	instance   string
	nodeID     string
	harness    contracts.Harness
	projectID  string
	checkout   *contracts.CheckoutRequest
	cwd        string
	browsePath string
	lists      map[int]*listState
	title      textinput.Model
	branch     textinput.Model
	editing    string // "", "title", "branch"
	starting   bool
	errLine    string
}

func (ls *launchState) list() *listState {
	l := ls.lists[ls.step]
	if l == nil {
		l = &listState{}
		ls.lists[ls.step] = l
	}
	return l
}

func (m *Model) openLaunch() tea.Cmd {
	ti := textinput.New()
	ti.Placeholder = "optional title"
	ti.CharLimit = 120
	ti.Prompt = ""
	br := textinput.New()
	br.Placeholder = "branch for the new worktree"
	br.CharLimit = 120
	br.Prompt = ""
	m.launch = &launchState{instance: contracts.NewID(contracts.PrefixOperation), lists: map[int]*listState{}, title: ti, branch: br}
	// Preselect the machine of the selected session: a common case.
	if s, ok := m.selectedSession(); ok && s.Run != nil {
		m.launch.list().sel = s.Run.NodeID
	}
	return m.push(viewLaunch)
}

func (m *Model) launchHints() []hint {
	ls := m.launch
	if ls == nil {
		return nil
	}
	if ls.editing != "" {
		return []hint{{"enter", "done"}, {"esc", "cancel edit"}}
	}
	if ls.step == stepConfirm {
		h := []hint{{"enter", "start"}, {"t", "title"}}
		if ls.checkout != nil && ls.checkout.Mode == contracts.CheckoutWorktree {
			h = append(h, hint{"b", "branch"})
		}
		return h
	}
	return []hint{{"enter", "choose"}, {"↑↓", "move"}}
}

// launchItems builds the choices for the current step.
func (m *Model) launchItems() []pickItem {
	ls := m.launch
	now := m.now()
	var items []pickItem
	switch ls.step {
	case stepNode:
		for _, n := range m.sortedNodes() {
			it := pickItem{id: n.NodeID, label: n.Name, detail: fmt.Sprintf("%s/%s · %s · runs %d", n.Platform, n.Arch, harnessSummary(n), n.Resources.ActiveRuns)}
			if n.Connection.State != contracts.ConnConnected {
				seen := "never seen"
				if n.Connection.LastSeenAt != nil {
					seen = "last seen " + ago(now, *n.Connection.LastSeenAt)
				}
				it.reason = "offline (" + seen + ")"
			} else if n.Resources.MaxRuns > 0 && n.Resources.ActiveRuns >= n.Resources.MaxRuns {
				it.reason = fmt.Sprintf("at capacity (%d/%d runs)", n.Resources.ActiveRuns, n.Resources.MaxRuns)
			}
			items = append(items, it)
		}
	case stepHarness:
		if n := m.node(ls.nodeID); n != nil {
			for _, h := range n.Harnesses {
				it := pickItem{id: string(h.Harness), label: titleCase(string(h.Harness)), detail: h.Version}
				if !h.Available {
					it.reason = orDash(h.Reason)
				}
				items = append(items, it)
			}
		}
	case stepWhere:
		items = append(items, pickItem{id: "dir", label: "Choose a directory on " + m.nodeName(ls.nodeID) + "…", detail: "no project needed"})
		for _, p := range m.visibleProjectsAll() {
			items = append(items, pickItem{id: "proj:" + p.ProjectID, label: "Project: " + p.Name, detail: plural(len(p.Repos), "repo", "repos")})
		}
	case stepCheckout:
		p := m.project(ls.projectID)
		if p != nil {
			for _, c := range p.Checkouts {
				if c.NodeID != ls.nodeID {
					continue
				}
				items = append(items, pickItem{id: "shared:" + c.Path + "|" + c.RepoID, label: "Shared checkout " + c.Path, detail: strings.TrimSpace(c.RepoID + " " + c.Branch)})
			}
			for _, r := range p.Repos {
				items = append(items, pickItem{id: "worktree:" + r.RepoID, label: "New worktree of " + r.Name, detail: "isolated; branch editable"})
			}
		}
		items = append(items, pickItem{id: "plain", label: "Choose a directory (plain) and associate the project", detail: "no git required"})
	case stepBrowse:
		res, st := get[contracts.DirListResult](m, m.dirKey())
		if !st.ok() {
			return nil
		}
		items = append(items, pickItem{id: "use", label: "[use] " + res.Path, detail: map[bool]string{true: "git repository", false: ""}[res.IsGit]})
		if res.Path != "/" {
			items = append(items, pickItem{id: "up", label: "..", detail: "parent directory"})
		}
		for _, d := range res.Dirs {
			items = append(items, pickItem{id: "d:" + d, label: d + "/"})
		}
	}
	return items
}

func (m *Model) visibleProjectsAll() []contracts.Project {
	saved := m.filter
	m.filter = ""
	ps := m.visibleProjects()
	m.filter = saved
	return ps
}

func (m *Model) dirKey() string {
	return "dir:" + m.launch.nodeID + ":" + m.launch.browsePath
}

func (m *Model) loadDir() tea.Cmd {
	ls := m.launch
	api, nid, p := m.api, ls.nodeID, ls.browsePath
	return m.ensure(m.dirKey(), 0, func(ctx context.Context) (any, error) {
		params, _ := json.Marshal(contracts.DirListParams{Path: p})
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

func (m *Model) accountKey() string {
	return "acctsel:" + m.launch.nodeID + ":" + string(m.launch.harness)
}

func (m *Model) loadAccountSelection() tea.Cmd {
	ls := m.launch
	prov := contracts.ProviderFor(ls.harness)
	if prov == "" {
		return nil
	}
	api, nid := m.api, ls.nodeID
	return m.ensure(m.accountKey(), 0, func(ctx context.Context) (any, error) {
		params, _ := json.Marshal(contracts.AccountSelectParams{Provider: prov, Policy: contracts.AccountPolicy{Mode: contracts.AccountEligible}})
		res, err := api.InvokeNodeAction(ctx, contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
			NodeID: nid, Action: contracts.ActionAccountSelect, Params: params, TimeoutSeconds: 5})
		if err != nil {
			return contracts.AccountSelection{}, err
		}
		if res.Error != nil {
			return contracts.AccountSelection{}, res.Error
		}
		var sel contracts.AccountSelection
		err = json.Unmarshal(res.Result, &sel)
		return sel, err
	})
}

func (m *Model) launchGo(step int) tea.Cmd {
	ls := m.launch
	ls.hist = append(ls.hist, ls.step)
	ls.step = step
	ls.errLine = ""
	switch step {
	case stepBrowse:
		return m.loadDir()
	case stepConfirm:
		return m.loadAccountSelection()
	}
	return nil
}

func (m *Model) launchBack() tea.Cmd {
	ls := m.launch
	if len(ls.hist) == 0 {
		m.launch = nil
		return m.goBack()
	}
	ls.step = ls.hist[len(ls.hist)-1]
	ls.hist = ls.hist[:len(ls.hist)-1]
	return nil
}

func (m *Model) keyLaunch(k tea.KeyPressMsg) tea.Cmd {
	ls := m.launch
	if ls == nil {
		return m.goBack()
	}
	key := k.String()
	if ls.editing != "" {
		in := &ls.title
		if ls.editing == "branch" {
			in = &ls.branch
		}
		switch key {
		case "enter", "esc":
			ls.editing = ""
			in.Blur()
			return nil
		}
		var cmd tea.Cmd
		*in, cmd = in.Update(k)
		return cmd
	}
	if key == "esc" || key == "q" {
		return m.launchBack()
	}
	if ls.step == stepConfirm {
		switch key {
		case "t":
			ls.editing = "title"
			return ls.title.Focus()
		case "b":
			if ls.checkout != nil && ls.checkout.Mode == contracts.CheckoutWorktree {
				ls.editing = "branch"
				return ls.branch.Focus()
			}
		case "enter":
			return m.startLaunch()
		}
		return nil
	}
	items := m.launchItems()
	l := ls.list()
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.id
	}
	l.setIDs(ids)
	if navKey(l, key, m.halfPage()) {
		return nil
	}
	if key != "enter" {
		return nil
	}
	var it pickItem
	for _, x := range items {
		if x.id == l.sel {
			it = x
		}
	}
	if it.id == "" {
		return nil
	}
	if it.reason != "" {
		return m.setToast(it.label+" unavailable: "+it.reason, toneWarn)
	}
	switch ls.step {
	case stepNode:
		ls.nodeID = it.id
		return m.launchGo(stepHarness)
	case stepHarness:
		ls.harness = contracts.Harness(it.id)
		return m.launchGo(stepWhere)
	case stepWhere:
		if it.id == "dir" {
			ls.projectID, ls.checkout = "", nil
			ls.browsePath = ""
			return m.launchGo(stepBrowse)
		}
		ls.projectID = strings.TrimPrefix(it.id, "proj:")
		return m.launchGo(stepCheckout)
	case stepCheckout:
		switch {
		case strings.HasPrefix(it.id, "shared:"):
			v := strings.TrimPrefix(it.id, "shared:")
			p, repo, _ := strings.Cut(v, "|")
			ls.checkout = &contracts.CheckoutRequest{Mode: contracts.CheckoutShared, Path: p, RepoID: repo}
			ls.cwd = p
			return m.launchGo(stepConfirm)
		case strings.HasPrefix(it.id, "worktree:"):
			ls.checkout = &contracts.CheckoutRequest{Mode: contracts.CheckoutWorktree, RepoID: strings.TrimPrefix(it.id, "worktree:")}
			ls.cwd = ""
			return m.launchGo(stepConfirm)
		default:
			ls.checkout = nil
			ls.browsePath = ""
			return m.launchGo(stepBrowse)
		}
	case stepBrowse:
		res, _ := get[contracts.DirListResult](m, m.dirKey())
		switch {
		case it.id == "use":
			ls.cwd = res.Path
			if ls.projectID != "" {
				ls.checkout = &contracts.CheckoutRequest{Mode: contracts.CheckoutPlain, Path: res.Path}
			}
			return m.launchGo(stepConfirm)
		case it.id == "up":
			ls.browsePath = path.Dir(res.Path)
		default:
			ls.browsePath = path.Join(res.Path, strings.TrimPrefix(it.id, "d:"))
		}
		l := ls.list()
		l.sel, l.idx, l.top = "", 0, 0
		return m.loadDir()
	}
	return nil
}

// startLaunch submits StartSession with one operation id per wizard, so a
// repeated Enter or a retry after a lost response cannot launch twice.
func (m *Model) startLaunch() tea.Cmd {
	ls := m.launch
	if ls.starting {
		return m.setToast("launch already submitted…", toneMuted)
	}
	req := contracts.StartSessionRequest{
		Mutation:      contracts.Mutation{OperationID: m.opID("launch:" + ls.instance)},
		NodeID:        ls.nodeID,
		Harness:       ls.harness,
		Title:         strings.TrimSpace(ls.title.Value()),
		AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible},
	}
	if ls.checkout != nil {
		co := *ls.checkout
		if co.Mode == contracts.CheckoutWorktree {
			co.Branch = strings.TrimSpace(ls.branch.Value())
		}
		req.Checkout = &co
	}
	if ls.cwd != "" && (ls.checkout == nil || ls.checkout.Mode != contracts.CheckoutWorktree) {
		req.Cwd = ls.cwd
	}
	if ls.projectID != "" {
		pid := ls.projectID
		req.ProjectID = &pid
		req.IncludeContext = true
		if p := m.project(pid); p != nil {
			req.EnvProfile = p.EnvProfile
		}
	}
	ls.starting = true
	ls.errLine = ""
	api := m.api
	return m.do(func(ctx context.Context) (any, error) { return api.StartSession(ctx, req) },
		func(m *Model, v any, err error) tea.Cmd {
			if m.launch != ls {
				return nil
			}
			ls.starting = false
			if err != nil {
				if contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) || retryable(err) {
					ls.errLine = "Outcome unknown (" + errText(err) + "). Enter retries with the same operation id; the hub will not start a second session."
				} else {
					ls.errLine = "Not started: " + errText(err)
				}
				return nil
			}
			resp := v.(contracts.StartSessionResponse)
			sid := resp.Session.Session.SessionID
			m.launch = nil
			m.sessList.sel = sid
			msg := "launch requested for " + orDash(resp.Session.Session.Title)
			if resp.CommandID != "" {
				msg += " (command " + shortID(resp.CommandID) + "; watch the run state for the outcome)"
			}
			m.view, m.back = viewSessions, nil
			m.applyFilter("")
			m.pendingFocus = sid
			return tea.Batch(m.setToast(msg, toneGood), m.reload("sessions", m.baseLoader("sessions")))
		})
}

func (m *Model) viewLaunch(w, h int) []string {
	ls := m.launch
	if ls == nil {
		return nil
	}
	out := []string{m.launchProgress(w), "", m.th.heading.Render(stepNames[ls.step]), ""}
	if ls.step == stepConfirm {
		return append(out, m.launchSummary(w)...)
	}
	items := m.launchItems()
	if ls.step == stepBrowse {
		_, st := get[contracts.DirListResult](m, m.dirKey())
		if !st.ok() {
			return append(out, m.loadingOr(st, ""))
		}
	}
	l := ls.list()
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.id
	}
	l.setIDs(ids)
	if len(items) == 0 {
		return append(out, m.th.dim.Render("  nothing to choose — esc to go back"))
	}
	cw := min(w, 120) // keep rows readable on full-screen terminals
	start, end := l.window(h - len(out))
	for i := start; i < end; i++ {
		it := items[i]
		sel := i == l.idx
		label := m.primary(sel, pad(it.label, min(44, cw/2)))
		right := m.th.dim.Render(it.detail)
		if it.reason != "" {
			label = m.th.dim.Render(pad(it.label, min(44, cw/2)))
			right = m.th.tone(toneWarn, "✗") + m.th.dim.Render(" unavailable — "+it.reason)
		}
		row := m.cursor(sel) + label + gap2 + right
		out = append(out, m.rowSel(sel, trunc(row, cw), cw))
	}
	return out
}

// launchProgress: "Launch  ✓ Machine synth-mac › ● Harness › Where › Review".
func (m *Model) launchProgress(w int) string {
	ls := m.launch
	cur := map[int]int{stepNode: 0, stepHarness: 1, stepWhere: 2, stepCheckout: 2, stepBrowse: 2, stepConfirm: 3}[ls.step]
	vals := []string{m.nodeName(ls.nodeID), string(ls.harness), "", ""}
	if ls.nodeID == "" {
		vals[0] = ""
	}
	switch {
	case ls.projectID != "":
		vals[2] = m.projectName(&ls.projectID)
	case ls.cwd != "":
		vals[2] = ls.cwd
	}
	var segs []string
	for i, name := range []string{"Machine", "Harness", "Where", "Review"} {
		switch {
		case i < cur:
			s := m.th.tone(toneGood, "✓") + " " + m.th.dim.Render(name)
			if vals[i] != "" {
				s += " " + trunc(vals[i], 28)
			}
			segs = append(segs, s)
		case i == cur:
			segs = append(segs, m.th.key.Render("● "+name))
		default:
			segs = append(segs, m.th.dim.Render("○ "+name))
		}
	}
	return trunc(m.th.title.Render("Launch session")+gap2+m.th.faint.Render("│")+gap2+strings.Join(segs, m.th.faint.Render("  ›  ")), w)
}

func (m *Model) launchSummary(w int) []string {
	ls := m.launch
	lw := 13
	out := []string{m.kvl("Machine", m.nodeName(ls.nodeID), lw), m.kvl("Harness", string(ls.harness), lw)}
	where := ls.cwd
	if ls.checkout != nil {
		switch ls.checkout.Mode {
		case contracts.CheckoutWorktree:
			b := strings.TrimSpace(ls.branch.Value())
			if b == "" {
				b = "(node default branch name)"
			}
			where = "new worktree of " + ls.checkout.RepoID + ", branch " + b
		case contracts.CheckoutShared:
			where = "shared checkout " + ls.checkout.Path + m.th.dim.Render(" (other sessions may use it)")
		}
	}
	out = append(out, m.kvl("Where", trunc(where, w-lw), lw))
	proj := "none (standalone)"
	if ls.projectID != "" {
		proj = m.projectName(&ls.projectID) + m.th.dim.Render(" · bounded brief included")
	}
	out = append(out, m.kvl("Project", proj, lw))
	acct := "not needed for a shell"
	ambient := false
	if contracts.ProviderFor(ls.harness) != "" {
		sel, st := get[contracts.AccountSelection](m, m.accountKey())
		switch {
		case !st.ok():
			acct = ansiStrip(m.loadingOr(st, ""))
		case sel.Outcome != "":
			acct = m.th.tone(toneWarn, "no eligible "+string(sel.Provider)+" account now ("+string(sel.Outcome)+"): "+sel.Reason)
		case sel.AccountID == "":
			ambient = true
			acct = ambientLabel(m.nodeName(ls.nodeID), sel.Reason)
		default:
			label := sel.AccountID
			if a := m.account(sel.AccountID); a != nil {
				label = a.Account.Label
			}
			acct = label + m.th.dim.Render(" — "+sel.Reason+" · observed "+ago(m.now(), sel.Freshness.ObservedAt))
		}
	}
	out = append(out, m.kvl("Account", trunc(acct, w-lw+30), lw))
	policy := "policy: eligible " + string(contracts.ProviderFor(ls.harness)) + " subscription; never crosses providers"
	if ambient {
		policy = "policy: node default account; no Pyre isolation or automatic fallback"
	}
	out = append(out, m.kvl("", m.th.dim.Render(policy), lw))
	env := "global default profile"
	if p := m.project(ls.projectID); p != nil && p.EnvProfile != "" {
		env = p.EnvProfile + " (project profile)"
	}
	out = append(out, m.kvl("Environment", env, lw))
	title := ls.title.Value()
	if ls.editing == "title" {
		title = ls.title.View()
	} else if title == "" {
		title = m.th.dim.Render("(automatic) — t to set")
	}
	out = append(out, m.kvl("Title", title, lw))
	if ls.editing == "branch" {
		out = append(out, m.kvl("Branch", ls.branch.View(), lw))
	}
	out = append(out, "")
	if ls.starting {
		out = append(out, m.th.dim.Render("starting…"))
	}
	if ls.errLine != "" {
		out = append(out, "")
		for _, l := range wrap(ls.errLine, w) {
			out = append(out, m.th.tone(toneWarn, l))
		}
	}
	return out
}

// ambientLabel describes a launch on the node's own login, e.g. "your own
// login on mac (this node has no Pyre anthropic accounts)".
func ambientLabel(node, reason string) string {
	why := reason
	if i := strings.Index(reason, "("); i >= 0 && strings.HasSuffix(reason, ")") {
		why = reason[i:]
	} else if why != "" {
		why = "(" + why + ")"
	}
	return strings.TrimSpace("your own login on " + node + " " + why)
}
