package tui

import (
	"context"
	"fmt"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// visibleSessions applies the ended-sessions toggle, the filter and
// stable ordering. Archived sessions are never listed.
func (m *Model) visibleSessions() []contracts.SessionView {
	var out []contracts.SessionView
	for _, s := range m.sessions() {
		if s.Session.Archived || (!m.showEnded && sessionEnded(s)) {
			continue
		}
		if m.filter != "" && !m.sessionMatches(s, m.filter) {
			continue
		}
		out = append(out, s)
	}
	m.sortGrouped(out) // grouping.go: pinned, then the grouping mode
	return out
}

func (m *Model) sessionMatches(s contracts.SessionView, q string) bool {
	st := sessionStatus(s, m.nodeOf(s))
	fields := []string{s.Session.Title, string(s.Session.Harness), s.NodeName, st.label, m.projectName(s.Session.ProjectID), s.Session.SessionID}
	fields = append(fields, userTags(s.Session.Tags)...)
	if s.Run != nil {
		fields = append(fields, s.Run.Cwd, m.nodeName(s.Run.NodeID))
		if s.Run.Checkout != nil {
			fields = append(fields, s.Run.Checkout.Branch)
		}
	}
	_, ok := filterMatch(q, fields...)
	return ok
}

func (m *Model) nodeOf(s contracts.SessionView) *contracts.Node {
	if s.Run == nil {
		return nil
	}
	return m.node(s.Run.NodeID)
}

// syncSessions sets the list rows. While ended sessions are shown, each
// machine with ended sessions gets a selectable header row (X there
// archives all of them).
func (m *Model) syncSessions() {
	m.sessList.setIDs(m.groupedIDs(m.visibleSessions())) // grouping.go
}

func (m *Model) selectedSession() (contracts.SessionView, bool) {
	if m.sessList.sel == "" {
		return contracts.SessionView{}, false
	}
	return m.session(m.sessList.sel)
}

// schedulePreview debounces the preview load for the current selection, so
// holding an arrow key does not issue a read per row.
func (m *Model) schedulePreview() tea.Cmd {
	m.previewTok++
	tok := m.previewTok
	return tea.Tick(m.opts.PreviewDelay, func(time.Time) tea.Msg { return previewDueMsg{token: tok} })
}

func (m *Model) loadPreview(force bool) tea.Cmd {
	s, ok := m.selectedSession()
	if !ok {
		return nil
	}
	return tea.Batch(m.loadPreviewFor(s, force), m.loadSessionMessage(s))
}

// loadPreviewFor loads the transcript tail and screen snapshot of s (used
// by the Sessions preview and the digest's related-session column).
func (m *Model) loadPreviewFor(s contracts.SessionView, force bool) tea.Cmd {
	if s.Run == nil {
		return nil
	}
	if !contracts.HasCap(s.Run.Capabilities, contracts.CapTranscript) && !contracts.HasCap(s.Run.Capabilities, contracts.CapScreen) {
		return nil
	}
	sid, api := s.Session.SessionID, m.api
	fn := func(ctx context.Context) (any, error) {
		return api.ReadSession(ctx, contracts.ReadSessionRequest{SessionID: sid, Tail: true, Limit: 30})
	}
	key := "preview:" + sid
	if force {
		return m.reload(key, fn)
	}
	return m.ensure(key, 3*time.Second, fn)
}

// ---- keys ----

func (m *Model) keySessions(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if k.IsRepeat && (key == "X" || key == "x" || key == "!") {
		return nil // a held key never opens a second confirm for the next row
	}
	if navKey(&m.sessList, key, m.halfPage()) {
		return m.schedulePreview()
	}
	if cmd, ok := m.groupRowKey(key); ok { // a folded group row (grouping.go)
		return cmd
	}
	s, ok := m.selectedSession()
	switch key {
	case "esc":
		if m.filter != "" {
			m.applyFilter("")
		}
		return nil
	case "enter":
		if !ok {
			return nil
		}
		return m.focusTerm()
	case "A":
		if !ok {
			return nil
		}
		return m.attach(s)
	case "i":
		if !ok {
			return nil
		}
		m.overlay = &infoOverlay{sid: s.Session.SessionID}
		return tea.Batch(m.loadSwitches(s.Session.SessionID), m.loadWorktrees(s.Session.SessionID)) // W-quicklaunch
	case "m":
		if !ok {
			return nil
		}
		return m.replyToSession(s) // threaded to its message for you, if any
	case "d":
		if !ok {
			return nil
		}
		return m.dismissMessage(s)
	case "M":
		if !ok {
			return nil
		}
		m.curMsgSess = s.Session.SessionID
		return m.push(viewMessages)
	case "!":
		if !ok {
			return nil
		}
		return m.confirmSessionCommand(s, "interrupt")
	case "x":
		if !ok {
			return nil
		}
		return m.confirmSessionCommand(s, "stop")
	case "X":
		return m.archiveSelected()
	case ".":
		return m.toggleEnded()
	case "p":
		if !ok {
			return nil
		}
		if s.Session.ProjectID != nil && *s.Session.ProjectID != "" {
			return m.openProject(*s.Session.ProjectID)
		}
		return m.openAssociate(s)
	}
	return nil
}

func (m *Model) actionReason(s contracts.SessionView, label string) string {
	for _, a := range m.sessionActions(s) {
		if a.label == label {
			return a.reason
		}
	}
	return ""
}

// ---- attach ----

type attachDoneMsg struct {
	sessionID string
	err       error
}

type attachExec struct {
	fn func(contracts.AttachDescriptor) error
	d  contracts.AttachDescriptor
}

func (a *attachExec) Run() error          { return minDuration(func() error { return a.fn(a.d) }) }
func (a *attachExec) SetStdin(io.Reader)  {}
func (a *attachExec) SetStdout(io.Writer) {}
func (a *attachExec) SetStderr(io.Writer) {}

func (m *Model) attach(s contracts.SessionView) tea.Cmd {
	if r := m.actionReason(s, "attach"); r != "" {
		return m.setToast("attach unavailable: "+r, toneWarn)
	}
	sid := s.Session.SessionID
	if m.busy["attach"] {
		return m.setToast("attach already in progress…", toneMuted)
	}
	m.busy["attach"] = true
	req := contracts.AttachRequest{
		Mutation:     contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		SessionID:    sid,
		ClientNodeID: m.opts.ClientNodeID,
		Preferred:    []contracts.AttachMechanism{contracts.AttachLocalTmux, contracts.AttachSSHTmux, contracts.AttachWebTerminal},
	}
	api := m.api
	return tea.Batch(m.setToast("attaching to "+trunc(s.Session.Title, 48)+"… (detach with ctrl+b d)", toneMuted), m.do(
		func(ctx context.Context) (any, error) { return api.AttachSession(ctx, req) },
		func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				m.busy["attach"] = false
				return m.errToast("attach failed", err)
			}
			d := v.(contracts.AttachDescriptor)
			switch d.Mechanism {
			case contracts.AttachReadOnly:
				m.busy["attach"] = false
				note := "read-only attach: showing transcript/screen in the preview"
				if d.Notes != "" {
					note += " (" + d.Notes + ")"
				}
				return tea.Batch(m.setToast(note, toneWarn), m.loadPreview(true))
			case contracts.AttachWebTerminal:
				if m.defaultAttach {
					m.busy["attach"] = false
					return m.setToast("web terminal: open "+d.URL, toneAccent)
				}
			}
			return m.opts.ExecFunc(&attachExec{fn: m.opts.Attach, d: d}, func(err error) tea.Msg {
				return attachDoneMsg{sessionID: sid, err: err}
			})
		}))
}

func (m *Model) onAttachDone(msg attachDoneMsg) tea.Cmd {
	m.busy["attach"] = false
	var t tea.Cmd
	if msg.err != nil {
		t = m.errToast("attach ended with an error", msg.err)
	} else {
		t = m.setToast("detached from "+trunc(m.sessionTitle(msg.sessionID), 48)+" — the session keeps running", toneMuted)
	}
	// The view, filter and selection are untouched; just refresh.
	return tea.Batch(t, m.refreshVisible(true))
}

// ---- interrupt / stop ----

func (m *Model) confirmSessionCommand(s contracts.SessionView, verb string) tea.Cmd {
	if r := m.actionReason(s, verb); r != "" {
		return m.setToast(verb+" unavailable: "+r, toneWarn)
	}
	runID := ""
	if s.Run != nil {
		runID = s.Run.RunID
	}
	sid, title := s.Session.SessionID, s.Session.Title
	opKey := verb + ":" + sid + ":" + runID
	body := []string{
		fmt.Sprintf("%s the current agent work in %q", titleCase(verb), title),
		fmt.Sprintf("on %s (run %s)?", s.NodeName, shortID(runID)),
		"",
	}
	if verb == "interrupt" {
		body = append(body, "The session stays alive; the current turn is interrupted.")
	} else {
		body = append(body, "The run will exit. The session can be resumed later if supported.")
	}
	body = append(body, "Acceptance of the command is reported separately from its observed effect.")
	api := m.api
	m.overlay = &confirmOverlay{
		sid:   sid,
		title: titleCase(verb) + " session",
		body:  body,
		yes: func(m *Model) tea.Cmd {
			req := contracts.SessionCommandRequest{Mutation: contracts.Mutation{OperationID: m.opID(opKey)}, SessionID: sid, RunID: runID, Reason: "requested from pyre tui"}
			return m.do(func(ctx context.Context) (any, error) {
				if verb == "interrupt" {
					return api.InterruptSession(ctx, req)
				}
				return api.StopSession(ctx, req)
			}, func(m *Model, v any, err error) tea.Cmd {
				if err != nil {
					if contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) {
						return m.setToast(verb+": outcome unknown; repeating it reuses the same operation id (safe)", toneWarn)
					}
					return m.errToast(verb+" failed", err)
				}
				r := v.(contracts.CommandReceipt)
				msg := fmt.Sprintf("%s command %s: accepted=%v, outcome %s", verb, shortID(r.CommandID), r.Accepted, r.Outcome)
				if r.Detail != "" {
					msg += " — " + r.Detail
				}
				return tea.Batch(m.setToast(msg+" (watch the run state for the effect)", toneMuted), m.reload("sessions", nil))
			})
		},
	}
	return nil
}

// ---- confirm overlay ----

type confirmOverlay struct {
	sid   string // the session it acts on, when it is about one
	title string
	body  []string
	yes   func(m *Model) tea.Cmd
}

func (c *confirmOverlay) hints() []hint { return []hint{{"y", "confirm"}, {"n esc", "cancel"}} }

func (c *confirmOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	if k.IsRepeat {
		return nil // a held key neither confirms nor cancels
	}
	switch k.String() {
	case "y", "Y":
		m.overlay = nil
		return c.yes(m)
	case "n", "N", "esc", "q":
		m.overlay = nil
		return m.setToast("cancelled", toneMuted)
	}
	return nil
}

func (c *confirmOverlay) view(m *Model, w, h int) []string {
	lines := append([]string{}, c.body...)
	lines = append(lines, "", m.th.key.Render("y")+" confirm   "+m.th.key.Render("n")+" cancel")
	return m.box(c.title, lines, 72, w, h)
}
