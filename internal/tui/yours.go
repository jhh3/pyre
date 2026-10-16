package tui

import (
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Recently yours (owner request 2026-10-13: mark "one I've recently
// interacted with (i.e. actually sent a message to)"). The hub records
// Session.OwnerActiveAt when the owner messages or types into a session.
// A Sessions row (sidenav list, table) draws a bar in its cursor slot when
// it is not selected: accent within the hour, faint within the day,
// nothing older. Grid tiles show "you 5m" in the caption; the facts say
// "You  messaged or typed 12m ago"; g s can sort by it.

// yoursRecency is how recently the owner messaged or typed into a session.
type yoursRecency int

const (
	yoursNone yoursRecency = iota // never, or more than a day ago
	yoursDay                      // within 24 hours
	yoursHour                     // within the hour
)

// ownerActive is when the owner last messaged or typed into s (zero: never).
func ownerActive(s contracts.SessionView) time.Time {
	if s.Session.OwnerActiveAt == nil {
		return time.Time{}
	}
	return *s.Session.OwnerActiveAt
}

// yoursAt classifies s's OwnerActiveAt against now.
func yoursAt(now time.Time, s contracts.SessionView) yoursRecency {
	at := ownerActive(s)
	if at.IsZero() {
		return yoursNone
	}
	switch d := now.Sub(at); {
	case d < time.Hour:
		return yoursHour
	case d < 24*time.Hour:
		return yoursDay
	}
	return yoursNone
}

// sessionCursor is a session row's two-cell cursor slot: the › cursor when
// selected, else the recently-yours bar ("▎", accent within the hour,
// faint within the day) or blank.
func (m *Model) sessionCursor(sel bool, s contracts.SessionView) string {
	if sel {
		return m.cursor(true)
	}
	switch yoursAt(m.now(), s) {
	case yoursHour:
		return m.th.accent.Render("▎") + " "
	case yoursDay:
		return m.th.faint.Render("▎") + " "
	}
	return "  "
}

// yoursCaption is a Grid caption's "you 5m" segment; "" when the owner
// has not been active in the last day.
func (m *Model) yoursCaption(s contracts.SessionView) string {
	r := yoursAt(m.now(), s)
	if r == yoursNone {
		return ""
	}
	t := "you " + age(m.now(), ownerActive(s))
	if r == yoursHour {
		return m.th.accent.Render(t)
	}
	return m.th.faint.Render(t)
}
