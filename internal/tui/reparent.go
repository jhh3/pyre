package tui

import (
	"context"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Reparenting (W-reparent): :parent <session> / :parent none moves the
// selected session under another session (or to the top level); > (or
// :parent with no argument) opens a switcher-style picker of the possible
// parents. The new parent receives the session's automatic reports; the
// hub tells both sides with a Pyre message.

// reparentNone is the picker row that moves a session to the top level.
const reparentNone = "none"

func (m *Model) reparentCommand() command {
	return command{name: "parent", aliases: []string{"reparent"}, arg: "session", args: "[session|none]",
		desc: "move the selected session under another session (its reports go there); none: to the top level; no argument: pick",
		run: func(m *Model, args string) tea.Cmd {
			s, ok := m.selectedSession()
			if !ok {
				return m.setToast("select a session first", toneWarn)
			}
			args = strings.TrimSpace(args)
			switch {
			case args == "":
				return m.openReparentPicker(s)
			case strings.EqualFold(args, reparentNone):
				return m.reparentSession(s.Session.SessionID, "")
			}
			p, ok := m.sessionArg(args)
			if !ok {
				return m.setToast("no session matches "+strconv.Quote(args), toneWarn)
			}
			return m.reparentSession(s.Session.SessionID, p.Session.SessionID)
		}}
}

// reparentSelected is > on the Sessions list: pick the selected session's
// new parent.
func (m *Model) reparentSelected() tea.Cmd {
	s, ok := m.selectedSession()
	if !ok {
		return m.setToast("select a session to move", toneWarn)
	}
	return m.openReparentPicker(s)
}

// openReparentPicker lists the sessions s may move under: every listed
// session except s and its descendants, plus "top level" when s has a
// parent.
func (m *Model) openReparentPicker(s contracts.SessionView) tea.Cmd {
	sid := s.Session.SessionID
	kids := map[string][]string{}
	for _, x := range m.sessions() {
		if p := x.Session.Parent(); p != "" {
			kids[p] = append(kids[p], x.Session.SessionID)
		}
	}
	ex := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if ex[id] {
			return
		}
		ex[id] = true
		for _, k := range kids[id] {
			walk(k)
		}
	}
	walk(sid)
	cur := s.Session.Parent()
	title := iconTitle(s)
	if title == "" {
		title = shortID(sid)
	}
	o := &switcherOverlay{pick: func(m *Model, id string) tea.Cmd {
		if id == reparentNone {
			id = ""
		}
		if id == cur {
			return m.setToast("already there", toneMuted)
		}
		return m.reparentSession(sid, id)
	}, exclude: ex, none: cur != "", curParent: cur, boxTitle: trunc("Move "+title+" under…", 60)}
	o.build(m)
	o.rank(m)
	if len(o.res) > 1 && o.res[0].it.s.Session.SessionID == reparentNone {
		o.idx = 1 // the cursor starts on the first session, not "top level"
	}
	if len(o.res) > 0 {
		o.sel = o.res[o.idx].it.s.Session.SessionID
	}
	m.overlay = o
	return nil
}

// reparentSession moves sid under parent ("" = top level).
func (m *Model) reparentSession(sid, parent string) tea.Cmd {
	key := "parent:" + sid + ":" + parent
	req := contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: m.opID(key)}, SessionID: sid, Parent: &parent}
	api := m.api
	return m.do(func(ctx context.Context) (any, error) {
		return api.UpdateSession(ctx, req)
	}, func(m *Model, v any, err error) tea.Cmd {
		delete(m.opIDs, key)
		if err != nil {
			return m.errToast("move", err)
		}
		msg := "moved to the top level"
		if parent != "" {
			msg = "now reports to " + m.sessionTitle(parent)
		}
		if sv, ok := v.(contracts.SessionView); ok {
			m.patchSession(sv)
		}
		return tea.Batch(m.setToast(msg, toneMuted), m.reload("sessions", nil))
	})
}

// patchSession replaces a cached session's record with the hub's answer and
// re-lays the lists (the tree moves before the next reload), keeping the
// selection.
func (m *Model) patchSession(sv contracts.SessionView) {
	st := m.loads["sessions"]
	if st == nil || !st.have {
		return
	}
	ss, ok := st.val.([]contracts.SessionView)
	if !ok {
		return
	}
	out := append([]contracts.SessionView(nil), ss...)
	for i := range out {
		if out[i].Session.SessionID == sv.Session.SessionID {
			out[i].Session = sv.Session
		}
	}
	st.val = out
	st.rev++
	sel := m.sessList.sel
	m.syncSessions()
	m.sessList.selectID(sel)
}

// SwitcherRows lists the session IDs the open switcher or picker shows
// (tests).
func (m *Model) SwitcherRows() []string {
	o, ok := m.overlay.(*switcherOverlay)
	if !ok {
		return nil
	}
	o.refresh(m)
	ids := make([]string, 0, len(o.res))
	for _, r := range o.res {
		ids = append(ids, r.it.s.Session.SessionID)
	}
	return ids
}
