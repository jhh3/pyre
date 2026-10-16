package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/terminal"
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

// schedulePreview loads the preview and terminal of the current selection.
// A single move loads at once: nothing waits on the switch path. Moves that
// follow within PreviewDelay (a held arrow key, typing in the filter) are
// debounced, so a burst issues one load, for the row it rests on.
func (m *Model) schedulePreview() tea.Cmd {
	m.previewTok++
	tok := m.previewTok
	now := time.Now()
	burst := now.Sub(m.previewAt) < m.opts.PreviewDelay
	m.previewAt = now
	if !burst {
		return func() tea.Msg { return previewDueMsg{token: tok} }
	}
	return tea.Tick(m.opts.PreviewDelay, func(time.Time) tea.Msg { return previewDueMsg{token: tok} })
}

// Preview reads (sessions.read: hub → node → transcript parse) are paced:
// a preview is re-read when an event says its session changed (at most
// once per previewMinGap) and otherwise at most once per previewMaxAge,
// never on every refresh; at most previewReadsMax run at once. The
// Sessions pane reads none while it shows the session's own terminal
// screen, or when the session is on this machine (localterm.go).
const (
	previewMaxAge   = 30 * time.Second
	previewMinGap   = 5 * time.Second
	previewReadsMax = 2
	loadingAfter    = 300 * time.Millisecond // "loading…" only once nothing has shown this long
	localCapMaxAge  = 5 * time.Second        // a local capture for the Activity section (finder, digest)
)

type previewRetryMsg struct{}

type loadingDueMsg struct{}

func (m *Model) loadPreview(force bool) tea.Cmd {
	s, ok := m.selectedSession()
	if !ok {
		return nil
	}
	var read tea.Cmd
	if m.paneWantsPreview(s) {
		read = m.loadPreviewFor(s, force)
	}
	return tea.Batch(read, m.loadSessionMessage(s))
}

// paneWantsPreview reports whether the Sessions pane would draw s's
// preview: only its last captured screen is used there, and only while
// the pane has no screen of the session's terminal and no local capture
// is coming.
func (m *Model) paneWantsPreview(s contracts.SessionView) bool {
	if m.view != viewSessions || m.tableShown() || s.Run == nil {
		return false
	}
	if emb, _ := m.embeddable(s); emb {
		if _, ok := m.localFor(s); ok {
			return false
		}
	}
	if tv := m.terms().views[s.Session.SessionID]; tv != nil && tv.runID == s.Run.RunID {
		if st, _, drawn := tv.status(); drawn && st != termUnavailable {
			return false
		}
	}
	return true
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
	var cmds []tea.Cmd
	if emb, _ := m.embeddable(s); emb {
		// A session here: its screen at once in the Activity section
		// (finder, digest); the Sessions pane never asks (paneWantsPreview).
		cmds = append(cmds, m.loadLocalCapture(s))
	}
	st := m.loads[key]
	if st != nil && st.inflight {
		if force {
			st.dirty = true
		}
		return tea.Batch(cmds...)
	}
	if !force && st != nil && st.have && !st.stale && m.now().Sub(st.at) < previewMaxAge {
		return tea.Batch(cmds...)
	}
	if !force && st != nil && st.have {
		// Changed (an event) or aged out: not more often than previewMinGap.
		if wait := previewMinGap - m.now().Sub(m.previewReadAt[key]); wait > 0 {
			return tea.Batch(append(cmds, m.previewLater(wait))...)
		}
	}
	if m.previewReads() >= previewReadsMax {
		m.previewQueued = true // asked again when one lands (onLoaded)
		return tea.Batch(cmds...)
	}
	if m.previewReadAt == nil {
		m.previewReadAt = map[string]time.Time{}
	}
	var load tea.Cmd
	if force {
		load = m.reload(key, fn)
	} else {
		load = m.ensure(key, previewMaxAge, fn)
	}
	if load != nil {
		m.previewReadAt[key] = m.now()
		if st == nil || !st.have {
			cmds = append(cmds, tea.Tick(loadingAfter, func(time.Time) tea.Msg { return loadingDueMsg{} }))
		}
	}
	return tea.Batch(append(cmds, load)...)
}

// previewReads counts preview reads in flight.
func (m *Model) previewReads() int {
	n := 0
	for k, st := range m.loads {
		if st.inflight && strings.HasPrefix(k, "preview:") {
			n++
		}
	}
	return n
}

// previewLater asks for the shown preview again after d (one tick at a
// time).
func (m *Model) previewLater(d time.Duration) tea.Cmd {
	if m.previewTick {
		return nil
	}
	m.previewTick = true
	return tea.Tick(d, func(time.Time) tea.Msg { return previewRetryMsg{} })
}

// previewRetry re-asks for the preview the current screen shows (a paced
// or queued read).
func (m *Model) previewRetry() tea.Cmd {
	switch m.view {
	case viewSessions:
		return m.loadPreview(false)
	case viewDigest:
		return m.digestPreview()
	}
	return nil
}

// loadLocalCapture captures the screen of s's tmux session on this machine
// (plain text) for the Activity section while the hub's preview loads.
func (m *Model) loadLocalCapture(s contracts.SessionView) tea.Cmd {
	lt, ok := m.localFor(s)
	if !ok {
		return nil
	}
	return m.ensure("localcap:"+s.Session.SessionID, localCapMaxAge, func(ctx context.Context) (any, error) {
		b, err := lt.capture(ctx, false)
		return string(b), err
	})
}

// localCapture is the last local capture of sid's screen ("" when none).
func (m *Model) localCapture(sid string) string {
	v, st := get[string](m, "localcap:"+sid)
	if st == nil || !st.have {
		return ""
	}
	return v
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
	if emb, _ := m.embeddable(s); emb && m.relayTypingFor(s) {
		return m.focusPane(s) // no ssh to its machine (learnt): type in the relayed pane
	}
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
				if terminal.NoSSHRoute(err) && m.relayReachable(s) {
					if emb, _ := m.embeddable(s); emb {
						// No ssh to that node (a Windows cloud VM): type in
						// the pane through the hub's relay instead.
						m.relays().noSSH[s.Run.NodeID] = true
						return tea.Batch(m.setToast("no ssh to "+orDash(s.NodeName)+": typing in the pane through the hub ("+m.unfocusLabel()+" returns to the list)", toneMuted), m.focusPane(s))
					}
				}
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
