package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Interrupted launches (W-resilience).
//
// pyred marks a run whose launch a restart interrupted before the harness
// started as failed with Run.AdapterData["pyre.never_started"]: nothing ran,
// so R offers to relaunch it (same session: harness, directory, title,
// parent; the node reuses the original prompt, args and account policy).
// A run interrupted before pyred recorded a handle but not provably
// unstarted stays "unknown" with "pyre.no_handle": Pyre cannot stop it, so
// X archives it directly after a confirm that says it may keep running.

const (
	adapterDataNeverStarted = "pyre.never_started"
	adapterDataNoHandle     = "pyre.no_handle"
)

func runNeverStarted(s contracts.SessionView) bool {
	return s.Run != nil && s.Run.AdapterData[adapterDataNeverStarted] == "true"
}

// runNoHandle: unknown run without a handle (cannot be stopped via Pyre).
func runNoHandle(s contracts.SessionView) bool {
	if s.Run == nil || s.Run.State != contracts.RunUnknown {
		return false
	}
	return s.Run.AdapterData[adapterDataNoHandle] == "true" || strings.Contains(s.Run.StateReason, "before a run handle was recorded")
}

// renameOrRelaunch is R: relaunch on a never-started session, else rename.
func (m *Model) renameOrRelaunch() tea.Cmd {
	if s, ok := m.selectedSession(); ok && runNeverStarted(s) {
		return m.confirmRelaunch(s)
	}
	return m.renameSelected()
}

func (m *Model) confirmRelaunch(s contracts.SessionView) tea.Cmd {
	if c, ok := m.overlay.(*confirmOverlay); ok && c.sid != "" {
		return nil
	}
	sid, title := s.Session.SessionID, s.Session.Title
	api := m.api
	m.overlay = &confirmOverlay{
		sid:   sid,
		title: "Relaunch session",
		body: []string{
			fmt.Sprintf("%q (%s) on %s never started: pyred restarted before the harness started, so nothing ran.", title, shortID(sid), orDash(s.NodeName)),
			"",
			"y relaunches it with the same harness, directory, prompt, account policy and title.",
		},
		yes: func(m *Model) tea.Cmd {
			req := contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: m.opID("relaunch:" + sid + ":" + s.Run.RunID)}, SessionID: sid,
				AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, AllowHandoff: true}
			return m.do(func(ctx context.Context) (any, error) { return api.ResumeSession(ctx, req) },
				func(m *Model, _ any, err error) tea.Cmd {
					if err != nil {
						return m.errToast("relaunch failed", err)
					}
					return tea.Batch(m.setToast("relaunching "+trunc(title, 40), toneMuted), m.reload("sessions", nil))
				})
		},
	}
	return nil
}

// confirmArchiveUnstoppable offers to archive a session whose run Pyre
// cannot stop (no handle, or stop unavailable): archiving hides it, and a
// run that does exist keeps running.
func (m *Model) confirmArchiveUnstoppable(s contracts.SessionView, why string) tea.Cmd {
	sid, title := s.Session.SessionID, s.Session.Title
	st := sessionStatus(s, m.nodeOf(s))
	m.overlay = &confirmOverlay{
		sid:   sid,
		title: "Archive anyway",
		body: []string{
			fmt.Sprintf("%q (%s) on %s is %s and cannot be stopped through Pyre: %s.", title, shortID(sid), orDash(s.NodeName), st.label, why),
			"",
			"y archives it anyway (if its run exists, it may keep running).",
			"Archived sessions leave the list; :archived lists them (u restores).",
		},
		yes: func(m *Model) tea.Cmd {
			return m.setArchived([]string{sid}, true, "archived "+trunc(title, 40)+" (its run may keep running) — :archived lists it", nil)
		},
	}
	return nil
}
