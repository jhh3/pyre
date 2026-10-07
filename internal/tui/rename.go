package tui

import (
	"context"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Session rename: R on a session (or a click on the title in the pane
// header, or :rename) opens a one-line title editor prefilled with the
// current title. Enter saves through sessions.update; Esc cancels. A
// person's title replaces an automatic one for good (the hub drops the
// reserved title:auto tag), so auto-naming never overwrites it.

// renameMaxTitle bounds a title; the hub stores what it is given.
const renameMaxTitle = 120

// reservedTitleTag reports the hub's reserved title-provenance tags
// ("title:auto", "title:agent"), which are bookkeeping, not user tags.
func reservedTitleTag(t string) bool { return strings.HasPrefix(t, "title:") }

// userTags is s's tags without the reserved title-provenance tags.
func userTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !reservedTitleTag(t) && !reservedPinTag(t) {
			out = append(out, t)
		}
	}
	return out
}

// renameSelected opens the title editor for the selected session.
func (m *Model) renameSelected() tea.Cmd {
	s, ok := m.selectedSession()
	if !ok {
		return m.setToast("select a session to rename", toneMuted)
	}
	return m.openRename(s)
}

type renameOverlay struct {
	sid  string
	old  string
	in   textinput.Model
	note string // a calm inline hint (empty title)
}

func (m *Model) openRename(s contracts.SessionView) tea.Cmd {
	in := textinput.New()
	in.CharLimit = renameMaxTitle
	in.Prompt = "> "
	in.Placeholder = "a short name for this session"
	in.SetValue(s.Session.Title)
	in.CursorEnd()
	r := &renameOverlay{sid: s.Session.SessionID, old: s.Session.Title, in: in}
	m.overlay = r
	return r.in.Focus()
}

func (r *renameOverlay) hints() []hint { return []hint{{"enter", "save"}, {"esc", "cancel"}} }

func (r *renameOverlay) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	r.in, cmd = r.in.Update(msg)
	return cmd
}

func (r *renameOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.overlay = nil
		return nil
	case "enter":
		v := strings.TrimSpace(r.in.Value())
		if v == "" {
			r.note = "A session needs a name: type one, or esc to keep “" + r.old + "”."
			return nil
		}
		m.overlay = nil
		if v == r.old {
			return nil
		}
		return m.renameSession(r.sid, v)
	}
	r.note = ""
	var cmd tea.Cmd
	r.in, cmd = r.in.Update(k)
	return cmd
}

func (r *renameOverlay) view(m *Model, w, h int) []string {
	bw := min(64, w-2)
	r.in.SetWidth(bw - 8)
	lines := []string{r.in.View(), ""}
	if r.note != "" {
		lines = append(lines, m.th.tone(toneWarn, trunc(r.note, bw-4)))
	} else {
		lines = append(lines, m.th.faint.Render(trunc("Your name stays: automatic naming stops for this session.", bw-4)))
	}
	lines = append(lines, "", m.th.key.Render("enter")+m.th.dim.Render(" save   ")+m.th.key.Render("esc")+m.th.dim.Render(" cancel"))
	return m.box("Rename session", lines, bw, w, h)
}

// renameSession sets a person's title through sessions.update. The
// operation id is stable per (session, title), so a retry after an unknown
// outcome is deduplicated by the hub.
func (m *Model) renameSession(sid, title string) tea.Cmd {
	title = strings.TrimSpace(title)
	if r := []rune(title); len(r) > renameMaxTitle {
		title = string(r[:renameMaxTitle])
	}
	key := "rename:" + sid + ":" + title
	req := contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: m.opID(key)}, SessionID: sid, Title: &title}
	api := m.api
	return m.do(func(ctx context.Context) (any, error) {
		return api.UpdateSession(ctx, req)
	}, func(m *Model, v any, err error) tea.Cmd {
		if err != nil {
			if contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) {
				return m.setToast("rename: outcome unknown; repeating it is safe (same operation id)", toneWarn)
			}
			delete(m.opIDs, key)
			return m.errToast("rename failed", err)
		}
		delete(m.opIDs, key)
		return tea.Batch(m.setToast("renamed to "+strconv.Quote(title), toneMuted), m.reload("sessions", nil))
	})
}

// renameCommand is :rename [title]: with a title it renames the selected
// session at once; without one it opens the editor.
func (m *Model) renameCommand() command {
	return command{name: "rename", aliases: []string{"title"}, args: "[title]", desc: "rename the selected session (no title: edit it)",
		run: func(m *Model, args string) tea.Cmd {
			s, ok := m.selectedSession()
			if !ok {
				return m.setToast("select a session to rename", toneWarn)
			}
			if strings.TrimSpace(args) == "" {
				return m.openRename(s)
			}
			return m.renameSession(s.Session.SessionID, args)
		}}
}

// markTitle makes a rendered session title clickable: a click acts like R.
func (m *Model) markTitle(title string) string {
	return m.mark(zoneDef{kind: zoneHint, name: "R"}, title)
}
