package tui

import (
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// command is one ":" command (Neovim cmdline / Claude Code slash style).
type command struct {
	name    string
	aliases []string
	args    string // argument hint
	arg     string // "session", "project" or "" — drives argument completion
	desc    string
	run     func(m *Model, args string) tea.Cmd
}

func (m *Model) commands() []command {
	sess := func(f func(m *Model, s contracts.SessionView) tea.Cmd) func(m *Model, args string) tea.Cmd {
		return func(m *Model, args string) tea.Cmd {
			s, ok := m.sessionArg(args)
			if !ok {
				return m.setToast("no session matches "+strconv.Quote(args), toneWarn)
			}
			m.focusSessionQuiet(s.Session.SessionID)
			return f(m, s)
		}
	}
	nav := func(id string) func(m *Model, args string) tea.Cmd {
		return func(m *Model, args string) tea.Cmd { return m.runAction(id) }
	}
	return []command{
		{name: "attach", arg: "session", args: "[session]", desc: "attach to a session's terminal (detach: ctrl+b d)",
			run: sess(func(m *Model, s contracts.SessionView) tea.Cmd { return m.attach(s) })},
		{name: "send", aliases: []string{"msg", "message"}, args: "[text]", desc: "message the selected session (no text: open the composer)",
			run: func(m *Model, args string) tea.Cmd {
				s, ok := m.selectedSession()
				if !ok {
					return m.setToast("select a session first", toneWarn)
				}
				cmd := m.openCompose(s)
				if strings.TrimSpace(args) != "" {
					c := m.overlay.(*composeOverlay)
					c.ta.SetValue(args)
					return tea.Batch(cmd, c.send(m))
				}
				return cmd
			}},
		{name: "approve", aliases: []string{"answer"}, args: "[1-9]", desc: "answer the selected session's pending request",
			run: func(m *Model, args string) tea.Cmd {
				if n, err := strconv.Atoi(strings.TrimSpace(args)); err == nil {
					return m.answerSelectedWith(n)
				}
				return m.answerSelected()
			}},
		{name: "interrupt", arg: "session", args: "[session]", desc: "interrupt the current turn (confirms)",
			run: sess(func(m *Model, s contracts.SessionView) tea.Cmd { return m.confirmSessionCommand(s, "interrupt") })},
		{name: "stop", arg: "session", args: "[session]", desc: "stop the run (confirms)",
			run: sess(func(m *Model, s contracts.SessionView) tea.Cmd { return m.confirmSessionCommand(s, "stop") })},
		{name: "archive", arg: "session", args: "[session]", desc: "archive a session (live: stop + archive, confirms)",
			run: sess(func(m *Model, s contracts.SessionView) tea.Cmd { return m.archiveSession(s) })},
		m.renameCommand(),
		m.iconCommand(),                                                    // W-archive2
		m.reparentCommand(),                                                // W-reparent
		m.pinCommand(), m.groupCommand(), m.sortCommand(), m.viewCommand(), // pinning.go, grouping.go, sessions_modes.go
		{name: "archived", desc: "list archived sessions (u restores)", run: func(m *Model, args string) tea.Cmd { return m.openArchived() }},
		{name: "ended", desc: "show / hide ended sessions in the list", run: func(m *Model, args string) tea.Cmd { return m.toggleEnded() }},
		{name: "read", aliases: []string{"transcript"}, arg: "session", args: "[session]", desc: "read the transcript full-screen",
			run: sess(func(m *Model, s contracts.SessionView) tea.Cmd { return m.openReader(s) })},
		{name: "history", arg: "session", args: "[session]", desc: "message history with delivery facts",
			run: sess(func(m *Model, s contracts.SessionView) tea.Cmd {
				m.curMsgSess = s.Session.SessionID
				return m.push(viewMessages)
			})},
		{name: "yank", aliases: []string{"copy"}, arg: "session", args: "[session]", desc: "copy a session ID to the clipboard",
			run: sess(func(m *Model, s contracts.SessionView) tea.Cmd {
				return m.copyText(s.Session.SessionID, "copied session ID "+s.Session.SessionID)
			})},
		{name: "project", arg: "project", args: "[project]", desc: "open a project (default: the selected session's)",
			run: func(m *Model, args string) tea.Cmd {
				if strings.TrimSpace(args) == "" {
					if s, ok := m.selectedSession(); ok && s.Session.ProjectID != nil {
						return m.openProject(*s.Session.ProjectID)
					}
					return m.goTop(viewProjects)
				}
				if p, ok := m.projectArg(args); ok {
					return m.openProject(p.ProjectID)
				}
				return m.setToast("no project matches "+strconv.Quote(args), toneWarn)
			}},
		{name: "new", aliases: []string{"launch"}, desc: "launch a new session", run: nav("global.new")},
		{name: "find", aliases: []string{"search"}, args: "[query]", desc: "find anything",
			run: func(m *Model, args string) tea.Cmd {
				cmd := m.openFinder()
				if f, ok := m.overlay.(*finderOverlay); ok {
					f.query = strings.TrimSpace(args)
				}
				return cmd
			}},
		{name: "filter", args: "<text>", desc: "filter the current list",
			run: func(m *Model, args string) tea.Cmd {
				if !m.view.topLevel() {
					m.view, m.back = viewSessions, nil
				}
				m.applyFilter(args)
				return m.schedulePreview()
			}},
		{name: "sessions", desc: "go to Sessions", run: nav("global.sessions")},
		{name: "projects", desc: "go to Projects", run: nav("global.projects")},
		{name: "grid", desc: "toggle the Grid inside Sessions: every live session as a live tile", run: nav("global.grid")},
		{name: "inbox", aliases: []string{"asks", "web"}, desc: "open the web inbox for the selection (its lead's or project's asks); copies the link over ssh", run: nav("global.inbox")},
		{name: "digest", aliases: []string{"away"}, desc: "while you were away: what each agent did since you last looked", run: nav("global.digest")},
		{name: "machines", aliases: []string{"nodes"}, desc: "connection, versions, resources, harnesses", run: nav("global.machines")},
		{name: "accounts", desc: "eligibility, usage windows, assignments", run: nav("global.accounts")},
		{name: "connections", desc: "verified availability per machine", run: nav("global.connections")},
		{name: "environments", aliases: []string{"envs", "updates"}, desc: "tool versions, drift, staged/rollback", run: nav("global.environments")},
		{name: "activity", aliases: []string{"audit", "log"}, desc: "activity log: account switches, sessions, machines", run: nav("global.activity")},
		{name: "refresh", desc: "reload everything now", run: nav("global.refresh")},
		{name: "help", aliases: []string{"keys"}, desc: "keys & help (searchable)", run: nav("global.help")},
		{name: "quit", aliases: []string{"q", "qa"}, desc: "quit Pyre (sessions keep running)",
			run: func(m *Model, args string) tea.Cmd { return tea.Quit }},
	}
}

// focusSessionQuiet selects a session in the Sessions list without
// switching views.
func (m *Model) focusSessionQuiet(id string) {
	if !m.sessList.selectID(id) {
		m.applyFilter("")
		m.sessList.selectID(id)
	}
}

// sessionArg resolves an optional session argument: empty = selection,
// otherwise the best fuzzy match on title or ID.
func (m *Model) sessionArg(args string) (contracts.SessionView, bool) {
	args = strings.TrimSpace(args)
	if args == "" {
		return m.selectedSession()
	}
	best, bestScore, found := contracts.SessionView{}, 0, false
	for _, s := range m.sessions() {
		if s.Session.Archived {
			continue
		}
		if s.Session.SessionID == args {
			return s, true
		}
		if sc, _, ok := matchTerms(args, s.Session.Title, s.Session.SessionID, s.NodeName); ok && (!found || sc > bestScore) {
			best, bestScore, found = s, sc, true
		}
	}
	return best, found
}

func (m *Model) projectArg(args string) (contracts.Project, bool) {
	best, bestScore, found := contracts.Project{}, 0, false
	for _, p := range m.projects() {
		if p.Archived {
			continue
		}
		if sc, _, ok := matchTerms(args, p.Name, p.ProjectID); ok && (!found || sc > bestScore) {
			best, bestScore, found = p, sc, true
		}
	}
	return best, found
}

// ---- overlay ----

type completion struct {
	insert string // replaces the word being completed
	label  string
	desc   string
	pos    []int
}

type cmdlineOverlay struct {
	text    string
	sel     int
	histIdx int // -1: editing; otherwise index into m.cmdHist from the end
	draft   string
	jk      jkState
}

func (m *Model) openCmdline(initial string) tea.Cmd {
	m.overlay = &cmdlineOverlay{text: initial, histIdx: -1}
	return nil
}

func (c *cmdlineOverlay) hints() []hint {
	return []hint{{"enter", "run"}, {"tab", "complete"}, {"↑↓", "history"}, {"esc", "cancel"}}
}

// completions lists matches for the word under the cursor: command names
// for the first word, sessions/projects for a command's argument.
func (c *cmdlineOverlay) completions(m *Model) []completion {
	name, arg, hasArg := strings.Cut(c.text, " ")
	cmds := m.commands()
	if !hasArg {
		q := strings.ToLower(name)
		type sc struct {
			c     completion
			score int
		}
		var out []sc
		for _, cmd := range cmds {
			best, pos, ok := fuzzyMatch(q, cmd.name, true)
			label := cmd.name
			for _, a := range cmd.aliases {
				if s, p, ok2 := fuzzyMatch(q, a, true); ok2 && (!ok || s > best) && q != "" {
					best, pos, ok, label = s, p, true, a
				}
			}
			if !ok {
				continue
			}
			disp := label
			if label != cmd.name {
				disp = label + " → " + cmd.name
				pos = pos[:0:0]
			}
			if cmd.args != "" {
				disp += " " + cmd.args
			}
			out = append(out, sc{completion{insert: cmd.name, label: disp, desc: cmd.desc, pos: pos}, best})
		}
		if q != "" {
			sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
		}
		res := make([]completion, len(out))
		for i, o := range out {
			res[i] = o.c
		}
		return res
	}
	var cmd *command
	for i := range cmds {
		if cmds[i].name == strings.ToLower(name) || contains(cmds[i].aliases, strings.ToLower(name)) {
			cmd = &cmds[i]
		}
	}
	if cmd == nil || cmd.arg == "" {
		return nil
	}
	type sc struct {
		c     completion
		score int
	}
	var out []sc
	switch cmd.arg {
	case "session":
		for _, s := range m.visibleSessions() {
			score, pos, ok := matchTerms(arg, s.Session.Title, s.NodeName, s.Session.SessionID)
			if !ok {
				continue
			}
			st := sessionStatus(s, m.nodeOf(s))
			out = append(out, sc{completion{insert: s.Session.Title, label: s.Session.Title, desc: st.String() + " · " + s.NodeName, pos: pos}, score})
		}
	case "project":
		for _, p := range m.visibleProjectsAll() {
			score, pos, ok := matchTerms(arg, p.Name, p.ProjectID)
			if !ok {
				continue
			}
			out = append(out, sc{completion{insert: p.Name, label: p.Name, desc: plural(len(p.Repos), "repo", "repos"), pos: pos}, score})
		}
	}
	if strings.TrimSpace(arg) != "" {
		sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	} else if cmd.arg == "session" {
		// No argument typed yet: the selected session is the likely target.
		if sel, ok := m.selectedSession(); ok {
			sort.SliceStable(out, func(i, j int) bool {
				return out[i].c.label == sel.Session.Title && out[j].c.label != sel.Session.Title
			})
		}
	}
	res := make([]completion, len(out))
	for i, o := range out {
		res[i] = o.c
	}
	return res
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (c *cmdlineOverlay) complete(m *Model) {
	comps := c.completions(m)
	if len(comps) == 0 {
		return
	}
	pick := comps[min(c.sel, len(comps)-1)]
	if name, _, hasArg := strings.Cut(c.text, " "); hasArg {
		c.text = name + " " + pick.insert
	} else {
		c.text = pick.insert + " "
	}
	c.sel = 0
}

func (c *cmdlineOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if c.jk.escape(m, k, &c.text) {
		m.overlay = nil
		return nil
	}
	comps := c.completions(m)
	switch key {
	case "esc":
		m.overlay = nil
		return nil
	case "enter":
		return c.execute(m, comps)
	case "tab":
		c.complete(m)
		return nil
	case "ctrl+j":
		c.sel = min(c.sel+1, max(0, len(comps)-1))
		return nil
	case "ctrl+k", "shift+tab":
		c.sel = max(0, c.sel-1)
		return nil
	case "up", "ctrl+p":
		c.history(m, 1)
		return nil
	case "down", "ctrl+n":
		c.history(m, -1)
		return nil
	case "backspace":
		if c.text == "" {
			m.overlay = nil
			return nil
		}
		r := []rune(c.text)
		c.text = string(r[:len(r)-1])
	case "ctrl+w":
		t := strings.TrimRight(c.text, " ")
		if i := strings.LastIndexByte(t, ' '); i >= 0 {
			c.text = t[:i+1]
		} else {
			c.text = ""
		}
	case "ctrl+u":
		c.text = ""
	default:
		if k.Text == "" || k.Mod&^tea.ModShift != 0 {
			return nil
		}
		c.text += k.Text
	}
	c.sel = 0
	c.histIdx = -1
	return nil
}

func (c *cmdlineOverlay) history(m *Model, d int) {
	if len(m.cmdHist) == 0 {
		return
	}
	if c.histIdx == -1 {
		c.draft = c.text
	}
	c.histIdx = max(-1, min(len(m.cmdHist)-1, c.histIdx+d))
	if c.histIdx == -1 {
		c.text = c.draft
		return
	}
	c.text = m.cmdHist[len(m.cmdHist)-1-c.histIdx]
}

func (c *cmdlineOverlay) execute(m *Model, comps []completion) tea.Cmd {
	line := strings.TrimSpace(c.text)
	m.overlay = nil
	if line == "" {
		return nil
	}
	name, args, _ := strings.Cut(line, " ")
	lname := strings.ToLower(name)
	var cmd *command
	cmds := m.commands()
	for i := range cmds {
		if cmds[i].name == lname || contains(cmds[i].aliases, lname) {
			cmd = &cmds[i]
		}
	}
	if cmd == nil && !strings.Contains(line, " ") && len(comps) > 0 {
		pick := comps[min(c.sel, len(comps)-1)]
		for i := range cmds {
			if cmds[i].name == pick.insert {
				cmd = &cmds[i]
			}
		}
	}
	if cmd == nil {
		return m.setToast("unknown command :"+name+" — : then tab lists commands", toneWarn)
	}
	full := cmd.name
	if strings.TrimSpace(args) != "" {
		full += " " + strings.TrimSpace(args)
	}
	if n := len(m.cmdHist); n == 0 || m.cmdHist[n-1] != full {
		m.cmdHist = append(m.cmdHist, full)
	}
	return cmd.run(m, strings.TrimSpace(args))
}

// view draws the current screen with the command line anchored at the
// bottom and the completion menu above it, like Neovim's wildmenu.
func (c *cmdlineOverlay) view(m *Model, w, h int) []string {
	base := fit(m.viewBody(w, h), w, h)
	comps := c.completions(m)
	maxRows := min(10, max(3, h/2-2))
	start := 0
	if c.sel >= maxRows {
		start = c.sel - maxRows + 1
	}
	var menu []string
	labelW := 0
	for _, cp := range comps {
		labelW = max(labelW, width(cp.label))
	}
	labelW = min(labelW, max(16, w/2-8))
	for i := start; i < min(len(comps), start+maxRows); i++ {
		cp := comps[i]
		sel := i == c.sel
		lab := m.highlight(pad(cp.label, labelW), cp.pos, styleIf(sel, m.th.selText))
		row := m.cursor(sel) + lab + "  " + m.th.dim.Render(trunc(cp.desc, max(0, w-labelW-8)))
		menu = append(menu, m.rowSel(sel, pad(" "+row, w), w))
	}
	if len(comps) > maxRows {
		menu = append(menu, m.th.faint.Render(pad("   "+itoa(c.sel+1)+"/"+itoa(len(comps)), w)))
	}
	input := m.th.key.Render(":") + c.text + m.th.accent.Render("▏")
	if c.text == "" {
		input += m.th.dim.Render("type a command — tab completes, ↑ history")
	}
	panel := append([]string{m.th.faint.Render(strings.Repeat("─", w))}, menu...)
	panel = append(panel, " "+input)
	if len(panel) > h {
		panel = panel[len(panel)-h:]
	}
	return append(base[:h-len(panel)], panel...)
}

// ---- jk escape ----

// jkState implements the owner's "jk leaves insert mode" override for
// text inputs: a k typed within the timeout of a j removes the j and
// reports an escape.
type jkState struct {
	lastJ time.Time
}

func (j *jkState) escape(m *Model, k tea.KeyPressMsg, text *string) bool {
	if !m.km.JKEscape {
		return false
	}
	switch k.String() {
	case "j":
		j.lastJ = time.Now()
	case "k":
		if !j.lastJ.IsZero() && time.Since(j.lastJ) <= m.km.JKTimeout && strings.HasSuffix(*text, "j") {
			*text = strings.TrimSuffix(*text, "j")
			j.lastJ = time.Time{}
			return true
		}
		j.lastJ = time.Time{}
	default:
		j.lastJ = time.Time{}
	}
	return false
}

func styleIf(ok bool, s lipgloss.Style) lipgloss.Style {
	if ok {
		return s
	}
	return lipgloss.NewStyle()
}
