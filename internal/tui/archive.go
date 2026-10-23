package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Ended sessions and archiving.
//
// The Sessions list shows live sessions by default: starting, working,
// waiting, needing a human, and offline/stale (the run may still be
// alive). Ended sessions (the run exited or failed, or there is no run)
// are hidden until "." shows them; the choice is remembered in the state
// file. X archives: an ended session at once, a live one after a confirmed
// stop, and on the ended view a machine header archives all of that
// machine's ended sessions. Archived sessions never show in the list;
// :archived lists them and u restores one.

// machineRowPrefix marks a machine-header row in the Sessions list ids
// (only present while ended sessions are shown).
const machineRowPrefix = "@machine:"

func machineRowID(name string) string { return machineRowPrefix + name }

func machineOfRow(id string) (string, bool) {
	if !strings.HasPrefix(id, machineRowPrefix) {
		return "", false
	}
	return strings.TrimPrefix(id, machineRowPrefix), true
}

// sessionEnded reports that nothing of the session is running: its run
// exited or failed, or it has no run. Offline and stale runs are not
// ended: they may still be alive.
func sessionEnded(s contracts.SessionView) bool {
	return s.Run == nil || s.Run.State.Terminal()
}

// endedSessions are the non-archived ended sessions, optionally of one
// machine, in list order.
func (m *Model) endedSessions(machine string, everyMachine bool) []contracts.SessionView {
	var out []contracts.SessionView
	for _, s := range m.sessions() {
		if s.Session.Archived || !sessionEnded(s) {
			continue
		}
		if !everyMachine && s.NodeName != machine {
			continue
		}
		out = append(out, s)
	}
	sortSessionsByMachine(out)
	return out
}

// hiddenEnded counts ended sessions the list hides (with a filter, only
// those matching it).
func (m *Model) hiddenEnded() int {
	if m.showEnded {
		return 0
	}
	n := 0
	for _, s := range m.endedSessions("", true) {
		if m.filter == "" || m.sessionMatches(s, m.filter) {
			n++
		}
	}
	return n
}

// toggleEnded shows or hides ended sessions and remembers the choice.
func (m *Model) toggleEnded() tea.Cmd {
	m.showEnded = !m.showEnded
	m.syncSessions()
	n := len(m.endedSessions("", true))
	msg := "ended sessions hidden"
	if m.showEnded {
		msg = "showing " + plural(n, "ended session", "ended sessions") + " · X archives · . hides"
	}
	return tea.Batch(m.setToast(msg, toneMuted), m.saveState(), m.schedulePreview())
}

// archiveSelected is X in the Sessions list.
func (m *Model) archiveSelected() tea.Cmd {
	if c, ok := m.overlay.(*confirmOverlay); ok && c.sid != "" {
		return nil // a confirm is open: X never replaces it with another session's
	}
	if name, ok := machineOfRow(m.sessList.sel); ok {
		return m.confirmArchiveMachine(name)
	}
	if pid := m.projectArchiveKey(m.sessList.sel); pid != "" { // W-archive2
		return m.confirmArchiveProject(pid)
	}
	s, ok := m.selectedSession()
	if !ok {
		return nil
	}
	if m.archiveAfterStop[s.Session.SessionID] {
		return m.setToast("stop sent; "+trunc(s.Session.Title, 32)+" is archived once its run has exited", toneMuted)
	}
	return m.archiveSession(s)
}

// archiveSession archives an ended session at once; a live one is offered
// stop + archive (confirmed), archived once the stop is observed.
func (m *Model) archiveSession(s contracts.SessionView) tea.Cmd {
	sid, title := s.Session.SessionID, s.Session.Title
	if sessionEnded(s) {
		done := "archived " + trunc(title, 40) + " — :archived lists it (u restores)"
		if runNeverStarted(s) {
			done = "never started — archived " + trunc(title, 40) + " — :archived lists it (u restores)"
		}
		// The worktree offer follows the archive's outcome, so its toast
		// never races the archive's (W-quicklaunch).
		return m.setArchived([]string{sid}, true, done, func(m *Model) tea.Cmd { return m.offerWorktreeCleanup(s) })
	}
	st := sessionStatus(s, m.nodeOf(s))
	if runNoHandle(s) { // W-resilience: no handle, nothing to stop
		return m.confirmArchiveUnstoppable(s, "its launch was interrupted before pyred recorded a run handle")
	}
	if r := m.actionReason(s, "stop"); r != "" {
		// Never a dead end: offer to archive anyway (W-resilience).
		return m.confirmArchiveUnstoppable(s, "stop unavailable: "+r)
	}
	runID := s.Run.RunID
	api := m.api
	// The dialog is bound to this session and run: it names them, and y
	// acts on them whatever the list selects by then (refreshes, other
	// sessions arriving). Its continuation archives without asking again.
	m.overlay = &confirmOverlay{
		sid:   sid,
		title: "Stop and archive session",
		body: []string{
			fmt.Sprintf("%q (%s) on %s is still %s.", title, shortID(sid), orDash(s.NodeName), st.label),
			"",
			"y stops the run, then archives the session once the run has exited.",
			"Archived sessions leave the list; :archived lists them (u restores).",
		},
		yes: func(m *Model) tea.Cmd {
			if m.archiveAfterStop[sid] {
				return nil // already confirmed: never a second stop
			}
			m.archiveAfterStop[sid] = true
			req := contracts.SessionCommandRequest{Mutation: contracts.Mutation{OperationID: m.opID("stop:" + sid + ":" + runID)}, SessionID: sid, RunID: runID, Reason: "stop and archive from pyre tui"}
			return m.do(func(ctx context.Context) (any, error) { return api.StopSession(ctx, req) },
				func(m *Model, v any, err error) tea.Cmd {
					if err != nil {
						delete(m.archiveAfterStop, sid)
						if contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) {
							return m.setToast("stop: outcome unknown; not archived (repeating reuses the same operation id)", toneWarn)
						}
						return m.errToast("stop failed; not archived", err)
					}
					r := v.(contracts.CommandReceipt)
					if !r.Accepted {
						delete(m.archiveAfterStop, sid)
						return m.setToast("stop not accepted ("+string(r.Outcome)+"); not archived", toneWarn)
					}
					return tea.Batch(m.setToast("stop sent; "+trunc(title, 32)+" is archived once its run has exited", toneMuted), m.reload("sessions", nil))
				})
		},
	}
	return nil
}

// archivePending archives sessions whose stop+archive stop has now been
// observed (called after every sessions load).
func (m *Model) archivePending() tea.Cmd {
	var ids []string
	for sid := range m.archiveAfterStop {
		s, ok := m.session(sid)
		if !ok {
			delete(m.archiveAfterStop, sid)
			continue
		}
		if sessionEnded(s) {
			ids = append(ids, sid)
			delete(m.archiveAfterStop, sid)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	var then func(*Model) tea.Cmd
	if len(ids) == 1 { // W-quicklaunch
		if s, ok := m.session(ids[0]); ok {
			then = func(m *Model) tea.Cmd { return m.offerWorktreeCleanup(s) }
		}
	}
	return m.setArchived(ids, true, plural(len(ids), "stopped session", "stopped sessions")+" archived — :archived lists them", then)
}

// confirmArchiveMachine archives every ended session of one machine.
func (m *Model) confirmArchiveMachine(name string) tea.Cmd {
	ss := m.endedSessions(name, false)
	if len(ss) == 0 {
		return m.setToast("no ended sessions on "+orDash(name), toneMuted)
	}
	ids := make([]string, len(ss))
	body := []string{fmt.Sprintf("Archive %s on %s?", plural(len(ss), "ended session", "ended sessions"), orDash(name)), ""}
	for i, s := range ss {
		ids[i] = s.Session.SessionID
		if i < 6 {
			st := sessionStatus(s, m.nodeOf(s))
			body = append(body, "  "+st.sym+" "+trunc(s.Session.Title, 50)+"  "+st.label)
		}
	}
	if len(ss) > 6 {
		body = append(body, fmt.Sprintf("  … and %d more", len(ss)-6))
	}
	body = append(body, "", "Live sessions are untouched. :archived lists archived sessions (u restores).")
	m.overlay = &confirmOverlay{
		title: "Archive ended sessions",
		body:  body,
		yes: func(m *Model) tea.Cmd {
			return m.setArchived(ids, true, "archived "+plural(len(ids), "ended session", "ended sessions")+" on "+orDash(name)+" — :archived lists them", nil)
		},
	}
	return nil
}

// setArchived archives or restores sessions through sessions.update. Each
// session has a stable operation id per direction, so a retry after an
// unknown outcome never acts twice.
// then, when set, runs after a successful change (its toast follows done).
func (m *Model) setArchived(ids []string, archived bool, done string, then func(*Model) tea.Cmd) tea.Cmd {
	api := m.api
	reqs := make([]contracts.UpdateSessionRequest, len(ids))
	for i, id := range ids {
		a := archived
		reqs[i] = contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: m.opID(fmt.Sprintf("archive:%s:%v", id, archived))}, SessionID: id, Archived: &a}
	}
	verb := "archive"
	if !archived {
		verb = "unarchive"
	}
	return m.do(func(ctx context.Context) (any, error) {
		n := 0
		for _, r := range reqs {
			if _, err := api.UpdateSession(ctx, r); err != nil {
				return n, err
			}
			n++
		}
		return n, nil
	}, func(m *Model, v any, err error) tea.Cmd {
		// The next use of these ids is a new logical operation.
		for _, id := range ids {
			delete(m.opIDs, fmt.Sprintf("archive:%s:%v", id, archived))
		}
		reload := tea.Batch(m.reload("sessions", nil), m.reloadArchived())
		if err != nil {
			n, _ := v.(int)
			prefix := verb + " failed"
			if len(ids) > 1 {
				prefix = fmt.Sprintf("%s failed after %d of %d", verb, n, len(ids))
			}
			return tea.Batch(m.errToast(prefix, err), reload)
		}
		cmd := m.setToast(done, toneMuted)
		if then != nil {
			cmd = tea.Batch(cmd, then(m))
		}
		return tea.Batch(cmd, reload)
	})
}

// ---- :archived ----

func (m *Model) archivedLoader() loaderFn {
	api := m.api
	return func(ctx context.Context) (any, error) {
		list, err := all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.SessionView], error) {
			return api.ListSessions(ctx, contracts.ListSessionsRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}, IncludeArchived: true})
		})
		var out []contracts.SessionView
		for _, s := range list {
			if s.Session.Archived && !s.Session.IsMonitor() {
				out = append(out, s)
			}
		}
		sortSessionsByMachine(out)
		return out, err
	}
}

// reloadArchived refreshes the archived list only while it is open.
func (m *Model) reloadArchived() tea.Cmd {
	if _, ok := m.overlay.(*archivedOverlay); !ok {
		return nil
	}
	return m.reload("sessions:archived", m.archivedLoader())
}

func (m *Model) openArchived() tea.Cmd {
	m.overlay = &archivedOverlay{}
	return m.reload("sessions:archived", m.archivedLoader())
}

// archivedOverlay lists archived sessions; u restores the selected one.
type archivedOverlay struct {
	l   listState
	top int
}

func (a *archivedOverlay) hints() []hint {
	return []hint{{"u", "unarchive"}, {"↑↓", "move"}, {"esc", "close"}}
}

func (a *archivedOverlay) items(m *Model) []contracts.SessionView {
	ss, _ := get[[]contracts.SessionView](m, "sessions:archived")
	ids := make([]string, len(ss))
	for i, s := range ss {
		ids[i] = s.Session.SessionID
	}
	a.l.setIDs(ids)
	return ss
}

func (a *archivedOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	ss := a.items(m)
	switch k.String() {
	case "esc", "q":
		m.overlay = nil
	case "down", "j", "ctrl+n":
		a.l.move(1)
	case "up", "k", "ctrl+p":
		a.l.move(-1)
	case "home", "g":
		a.l.home()
	case "end", "G":
		a.l.end()
	case "u":
		for _, s := range ss {
			if s.Session.SessionID == a.l.sel {
				return m.setArchived([]string{s.Session.SessionID}, false, "restored "+trunc(s.Session.Title, 40)+" to the Sessions list", nil)
			}
		}
	}
	return nil
}

func (a *archivedOverlay) view(m *Model, w, h int) []string {
	bw := min(96, max(60, w*70/100))
	inner := min(bw, w-2) - 4
	ss := a.items(m)
	_, st := get[[]contracts.SessionView](m, "sessions:archived")
	var lines []string
	switch {
	case len(ss) == 0 && (st == nil || st.inflight):
		lines = append(lines, m.th.dim.Render("loading archived sessions…"))
	case len(ss) == 0 && st.err != nil:
		lines = append(lines, m.th.tone(toneBad, "✗ could not load: "+errText(st.err)))
	case len(ss) == 0:
		lines = append(lines, m.th.dim.Render("No archived sessions. X archives one from the Sessions list."))
	}
	if len(ss) > 0 {
		rows := max(3, min(h-8, len(ss)))
		groups := make([]string, len(ss))
		for i, s := range ss {
			groups[i] = s.NodeName
		}
		now := m.now()
		for _, d := range groupedWindow(groups, a.l.idx, rows, &a.top) {
			if d.item < 0 {
				lines = append(lines, m.th.dim.Render(orDash(d.group)))
				continue
			}
			s := ss[d.item]
			sel := d.item == a.l.idx
			stt := sessionStatus(s, m.nodeOf(s))
			ag := age(now, observedAt(s))
			tw := max(10, inner-2-12-1-8-1-width(ag))
			row := m.cursor(sel) + m.statusCell(stt, 10) + " " + m.titleCell(sel, iconTitle(s), tw) + " " + m.th.dim.Render(pad(string(s.Session.Harness), 7)) + " " + m.th.dim.Render(ag)
			lines = append(lines, m.rowSel(sel, row, inner))
		}
		lines = append(lines, "", m.th.faint.Render(plural(len(ss), "archived session", "archived sessions"))+m.th.faint.Render(" · ")+m.hintSeg(hint{"u", "unarchive"}))
	}
	return m.box("Archived sessions", lines, bw, w, h)
}

// ---- archive a project (W-archive2) ----

// projectArchiveKey is the project a folded project group row names ("" if
// the row is not one).
func (m *Model) projectArchiveKey(id string) string {
	k, ok := groupOfRow(id)
	if !ok || m.groupMode() != groupProject || k == noGroup || k == pinnedGroup {
		return ""
	}
	return k
}

// confirmArchiveProject is X on a project (a folded project group in
// Sessions, or the Projects tab): the confirm counts what happens to its
// sessions; y archives the ended ones now, schedules the live ones to
// archive when their current turn ends, then archives the project.
func (m *Model) confirmArchiveProject(pid string) tea.Cmd {
	p := m.project(pid)
	if p == nil {
		return m.setToast("project not loaded yet", toneMuted)
	}
	var live, ended []string
	for _, s := range m.sessions() {
		if s.Session.Archived || s.Session.ProjectID == nil || *s.Session.ProjectID != pid {
			continue
		}
		if sessionEnded(s) || runNoHandle(s) {
			ended = append(ended, s.Session.SessionID)
		} else {
			live = append(live, s.Session.SessionID)
		}
	}
	name, version := p.Name, p.Version
	body := []string{fmt.Sprintf("Archive project %q?", name), ""}
	if len(live) > 0 {
		body = append(body, "  "+plural(len(live), "live session", "live sessions")+" will archive when done (stopped gracefully after the current turn)")
	}
	if len(ended) > 0 {
		body = append(body, "  "+plural(len(ended), "ended session", "ended sessions")+" archive now")
	}
	if len(live)+len(ended) == 0 {
		body = append(body, "  no sessions")
	}
	body = append(body, "", "The project leaves the Projects list; pyre project archive --unarchive restores it.")
	api := m.api
	m.overlay = &confirmOverlay{
		title: "Archive project",
		body:  body,
		yes: func(m *Model) tea.Cmd {
			yes := true
			op := func(kind, id string) contracts.Mutation {
				return contracts.Mutation{OperationID: m.opID("archive-project:" + pid + ":" + kind + ":" + id)}
			}
			var reqs []contracts.UpdateSessionRequest
			for _, id := range append(append([]string{}, ended...), live...) {
				reqs = append(reqs, contracts.UpdateSessionRequest{Mutation: op("session", id), SessionID: id, Archived: &yes, WhenDone: true})
			}
			preq := contracts.UpdateProjectRequest{Mutation: op("project", pid), ProjectID: pid, Version: version, Archived: &yes}
			return m.do(func(ctx context.Context) (any, error) {
				for _, r := range reqs {
					if _, err := api.UpdateSession(ctx, r); err != nil {
						return nil, err
					}
				}
				return api.UpdateProject(ctx, preq)
			}, func(m *Model, v any, err error) tea.Cmd {
				reload := tea.Batch(m.reload("sessions", nil), m.reload("projects", nil))
				if err != nil {
					return tea.Batch(m.errToast("archive project failed", err), reload)
				}
				for _, r := range reqs {
					delete(m.opIDs, "archive-project:"+pid+":session:"+r.SessionID)
				}
				delete(m.opIDs, "archive-project:"+pid+":project:"+pid)
				msg := "archived project " + trunc(name, 32)
				if len(live) > 0 {
					msg += " · " + plural(len(live), "live session", "live sessions") + " archive when done"
				}
				return tea.Batch(m.setToast(msg, toneMuted), reload)
			})
		},
	}
	return nil
}
