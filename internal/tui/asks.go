package tui

import (
	"context"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Open asks. The inbox (asks to decide, messages for the owner) lives in
// the web app (`pyre web serve`; owner request 2026-10-15: "remove the
// inbox view in app and the inbox right sidebar / there should still be an
// indication that there are questions"). The TUI keeps the signal: the
// header's "● N asks", a lead's "● asks N" chip (leads.go) and a project
// header's "● N asks" (grouping.go), each a link to the web inbox filtered
// to match (weblink.go).

const asksKey = "asks"

// askHotWindow: an open ask whose deadline is nearer than this turns the
// header's count red, like an urgent one.
const askHotWindow = 15 * time.Minute

// asksAPI is the backend's asks interface, when it has one.
func (m *Model) asksAPI() (contracts.AsksAPI, bool) {
	if m.asksUnsupported {
		return nil, false
	}
	a, ok := m.api.(contracts.AsksAPI)
	return a, ok
}

// loadAsks loads the open asks. A hub without the asks ops answers
// unsupported once; the ask signals then stay hidden.
func (m *Model) loadAsks(force bool) tea.Cmd {
	api, ok := m.asksAPI()
	if !ok {
		return nil
	}
	fn := func(ctx context.Context) (any, error) {
		return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.Ask], error) {
			return api.ListAsks(ctx, contracts.ListAsksRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200},
				States: []contracts.AskState{contracts.AskOpen}})
		})
	}
	if force {
		return m.reload(asksKey, fn)
	}
	maxAge := time.Duration(0)
	now := m.now()
	for _, a := range m.asks() {
		if a.State == contracts.AskOpen && a.DeadlineAt != nil && !now.Before(*a.DeadlineAt) {
			maxAge = 5 * time.Second // a deadline passed: the hub has applied the default
			break
		}
	}
	return m.ensure(asksKey, maxAge, fn)
}

// asksRefresh keeps the ask signals current on every screen (the header
// counts them), and the owner's messages where a session's "message for
// you" line shows (attention_flag.go).
func (m *Model) asksRefresh(force bool) tea.Cmd {
	cmds := []tea.Cmd{m.loadAsks(force)}
	if m.view == viewSessions || m.view == viewProject {
		cmds = append(cmds, m.loadOwnerInbox())
	}
	return tea.Batch(cmds...)
}

// asks are the loaded asks (open ones).
func (m *Model) asks() []contracts.Ask {
	v, st := get[[]contracts.Ask](m, asksKey)
	if st != nil && !st.have && st.err != nil && contracts.IsCode(st.err, contracts.ErrUnsupported) {
		m.asksUnsupported = true
	}
	return v
}

// openAsks counts the open asks; hot when any is urgent or its deadline
// is within askHotWindow.
func (m *Model) openAsks() (n int, hot bool) {
	now := m.now()
	for _, a := range m.asks() {
		if a.State != contracts.AskOpen {
			continue
		}
		n++
		if a.Urgent || (a.DeadlineAt != nil && a.DeadlineAt.Sub(now) < askHotWindow) {
			hot = true
		}
	}
	return n, hot
}

// projectAsks counts the open asks of project id.
func (m *Model) projectAsks(id string) int {
	n := 0
	for _, a := range m.asks() {
		if a.State == contracts.AskOpen && a.ProjectID != nil && *a.ProjectID == id {
			n++
		}
	}
	return n
}

// ---- time words ----

var (
	askLocOnce sync.Once
	askLocVal  *time.Location
)

// askLoc is the zone deadlines are shown in: the owner's (Pacific, the
// asks quiet-hours default), else local time.
func askLoc() *time.Location {
	askLocOnce.Do(func() {
		askLocVal = time.Local
		if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
			askLocVal = l
		}
	})
	return askLocVal
}

// clockLabel is "8:00 AM PT".
func clockLabel(t time.Time) string {
	lt := t.In(askLoc())
	zone := lt.Format("MST")
	if askLoc().String() == "America/Los_Angeles" {
		zone = "PT"
	}
	return lt.Format("3:04 PM") + " " + zone
}

// countdown is the time left until t: "42m", "1h 5m", or the clock time
// when it is more than three hours away.
func countdown(now, t time.Time) string {
	d := t.Sub(now)
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return itoa(int(d/time.Minute)) + "m"
	case d < 3*time.Hour:
		h := int(d / time.Hour)
		if mm := int((d % time.Hour) / time.Minute); mm > 0 {
			return itoa(h) + "h " + itoa(mm) + "m"
		}
		return itoa(h) + "h"
	}
	return clockLabel(t)
}

// deadlineWords says what happens at the ask's deadline: "defaults to
// Canary in 42m", "expires 8:00 AM PT"; short drops "defaults to".
func (m *Model) deadlineWords(a contracts.Ask, short bool) string {
	if a.DeadlineAt == nil {
		return "has no deadline"
	}
	cd := countdown(m.now(), *a.DeadlineAt)
	when := "in " + cd
	if strings.HasSuffix(cd, "PT") || strings.Contains(cd, "M ") {
		when = cd // a clock time
	}
	if cd == "now" {
		when = "now"
	}
	if o, ok := a.Option(a.DefaultOption); ok && a.DefaultOption != "" {
		if short {
			return "→ " + o.Label + " " + when
		}
		return "defaults to " + o.Label + " " + when
	}
	return "expires " + when
}

// deadlineTone colours a countdown: red for urgent asks and passed
// deadlines, amber under askHotWindow.
func (m *Model) deadlineTone(a contracts.Ask) tone {
	if a.DeadlineAt == nil {
		return toneMuted
	}
	d := a.DeadlineAt.Sub(m.now())
	switch {
	case a.Urgent || d <= 0:
		return toneBad
	case d < askHotWindow:
		return toneWarn
	}
	return toneMuted
}

// OpenAsks is the header's ask count and whether it reads red (for tests).
func (m *Model) OpenAsks() (n int, hot bool) { return m.openAsks() }
