package tui

import (
	"context"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Pinning. P on a session (or :pin) pins or unpins it. Pins are owner
// state stored on the session through sessions.update as two reserved
// tags, so they sync to every TUI and the phone:
//
//	pinned               the marker
//	pinned_at:<unix s>   when it was pinned (orders the Pinned group)
//
// sessions.update replaces the whole tag list, so a pin edits the current
// tags and keeps every other tag. Pinned sessions sit in a "Pinned" group
// on top of the Sessions list in every grouping mode and lead the Grid.

const (
	pinTag     = "pinned"
	pinAtTagPf = "pinned_at:"
)

// reservedPinTag reports the pin bookkeeping tags (not user tags).
func reservedPinTag(t string) bool { return t == pinTag || strings.HasPrefix(t, pinAtTagPf) }

func tagsPinned(tags []string) bool {
	for _, t := range tags {
		if t == pinTag {
			return true
		}
	}
	return false
}

func sessionPinned(s contracts.SessionView) bool { return tagsPinned(s.Session.Tags) }

// pinnedAt is the pin time (unix seconds) from the tags; 0 when unknown.
func pinnedAt(tags []string) int64 {
	if !tagsPinned(tags) {
		return 0
	}
	for _, t := range tags {
		if strings.HasPrefix(t, pinAtTagPf) {
			if n, err := strconv.ParseInt(strings.TrimPrefix(t, pinAtTagPf), 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// withPin returns tags pinned (at unix seconds) or unpinned, keeping every
// other tag in order.
func withPin(tags []string, pin bool, at int64) []string {
	out := make([]string, 0, len(tags)+2)
	for _, t := range tags {
		if !reservedPinTag(t) {
			out = append(out, t)
		}
	}
	if pin {
		out = append(out, pinTag, pinAtTagPf+strconv.FormatInt(at, 10))
	}
	return out
}

// pinSelected toggles the selected session's pin (P).
func (m *Model) pinSelected() tea.Cmd {
	s, ok := m.selectedSession()
	if !ok {
		return m.setToast("select a session to pin", toneMuted)
	}
	return m.togglePin(s)
}

// togglePin pins or unpins s: the list moves at once (the cached session
// gets the new tags), then sessions.update stores it for every client.
func (m *Model) togglePin(s contracts.SessionView) tea.Cmd {
	sid := s.Session.SessionID
	pin := !sessionPinned(s)
	tags := withPin(s.Session.Tags, pin, m.now().Unix())
	key := "pin:" + sid + ":" + strconv.FormatBool(pin)
	req := contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: m.opID(key)}, SessionID: sid, Tags: &tags}
	m.patchSessionTags(sid, tags)
	verb := "pinned"
	if !pin {
		verb = "unpinned"
	}
	api := m.api
	return tea.Batch(m.setToast(verb+" "+strconv.Quote(trunc(s.Session.Title, 48)), toneMuted), m.do(func(ctx context.Context) (any, error) {
		return api.UpdateSession(ctx, req)
	}, func(m *Model, v any, err error) tea.Cmd {
		if err != nil {
			if contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) {
				return m.setToast("pin: outcome unknown; repeating it is safe (same operation id)", toneWarn)
			}
			delete(m.opIDs, key)
			return tea.Batch(m.errToast("pin failed", err), m.reload("sessions", nil))
		}
		delete(m.opIDs, key)
		return m.reload("sessions", nil)
	}))
}

// patchSessionTags sets a cached session's tags and re-lays the lists, so
// a pin shows before the hub answers.
func (m *Model) patchSessionTags(sid string, tags []string) {
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
		if out[i].Session.SessionID == sid {
			out[i].Session.Tags = append([]string(nil), tags...)
		}
	}
	st.val = out
	st.rev++
	m.syncSessions() // the Grid re-sorts on its next frame
}

// pinCommand is :pin [session]: toggles the pin of a session (default:
// the selected one).
func (m *Model) pinCommand() command {
	return command{name: "pin", aliases: []string{"unpin"}, arg: "session", args: "[session]", desc: "pin / unpin a session (pinned stay on top, everywhere)",
		run: func(m *Model, args string) tea.Cmd {
			s, ok := m.sessionArg(args)
			if !ok {
				return m.setToast("no session matches "+strconv.Quote(args), toneWarn)
			}
			return m.togglePin(s)
		}}
}
