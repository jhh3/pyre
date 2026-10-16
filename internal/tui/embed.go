package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/terminal"
)

// Embedded terminal: model integration. The Sessions view's right-hand
// pane is the selected session's live terminal. Viewing runs a read-only
// attach (`tmux attach -r`, never the input lease); focusing the pane
// (enter, click, tab) swaps in a writable attach that holds the run's
// input lease, and keys go straight to the session until the escape chord
// (ctrl+] by default, "term.unfocus" in keys.toml) returns focus to the
// list. While the lease is held, Pyre's paste delivery treats the session
// as attached by a human (a writable tmux client) and rings the doorbell
// instead.

const (
	termFrameGap  = time.Second / 30  // a watched pane: at most ~30 frames per second
	termFocusGap  = time.Second / 120 // the pane you type into: one renderer frame (renderFPS)
	termEchoGap   = 2 * time.Millisecond
	termEchoFor   = 250 * time.Millisecond // output this soon after a key is its echo (local or remote): drawn at once
	leaseTTL      = 120                    // seconds; renewed while focused
	leaseRenew    = 40 * time.Second
	termRetryWait = 15 * time.Second // after a failed or unavailable attach
)

// The warm-terminal cache (switching sessions without a wait). Moving the
// selection back to a session viewed a moment ago used to re-run the whole
// attach (sessions.attach, then the relay's websocket or a tmux client,
// then the first screen) because only two attaches stayed running. Now the
// selected session's stream and a few recently viewed (or prefetched
// neighbour) streams keep running read-only, so switching to one paints a
// live screen on the first frame; terminals whose stream was closed keep
// their last screen, shown dimmed until the new stream draws. Streams out
// of sight never schedule frames (termPoke), streams idle for termWarmIdle
// are closed, and terminals of archived or vanished sessions are dropped
// (sweepTerms).
const (
	maxLiveTerms      = 5                      // embedded attach commands kept running (warm streams)
	maxCachedTerms    = 8                      // terminals kept with their last screen, live or not
	termWarmIdle      = 3 * time.Minute        // a warm stream not viewed this long is closed (its screen stays)
	termPrefetchDwell = 300 * time.Millisecond // the cursor rests this long before its neighbours are warmed
	termPrefetchAge   = time.Minute            // a prefetched terminal ranks as viewed this long ago
)

// MaxWarmTerms is how many embedded attach commands the Sessions view keeps
// running at most (tests).
const MaxWarmTerms = maxLiveTerms

// termMgr holds the embedded terminals (Update goroutine only).
type termMgr struct {
	views     map[string]*termView
	shown     atomic.Value // string: the session whose terminal the pane draws (read by pokes)
	notify    chan struct{}
	waiting   bool
	lastFrame time.Time
	paneW     int // last drawn terminal area
	paneH     int
	// lastKey is when input was last written to a writable attach (unix
	// nanoseconds; read by the frame waiter off the Update goroutine).
	lastKey atomic.Int64
}

type termFrameMsg struct{}

type termAttachMsg struct {
	sid      string
	tok      int
	mode     termMode
	d        contracts.AttachDescriptor
	leaseID  string
	leaseAt  time.Time
	takeover bool
	err      error
	conn     TermConn // a relayed stream, already open (relay.go)
	relayOff string   // the node whose relay refused as unsupported
	noSSH    string   // the node found to have no ssh route
	local    bool     // d was built here: a tmux session on this machine (localterm.go)
	screen   []byte   // its captured screen, drawn before the stream
	localBad string   // the run whose local attach failed (the hub answered instead)
}

type leaseTickMsg struct{ sid, leaseID string }

func (m *Model) terms() *termMgr {
	if m.term == nil {
		m.term = &termMgr{views: map[string]*termView{}, notify: make(chan struct{}, 1)}
	}
	return m.term
}

func (m *Model) starter() TermStarter {
	if m.opts.StartTerm != nil {
		return m.opts.StartTerm
	}
	return StartPTY
}

// termWait waits (off the Update goroutine) for an embedded terminal to
// draw, then paces the frame: a watched pane repaints at most ~30 times a
// second; the pane you type into at the renderer's rate; and output that
// follows a key within termEchoFor (the echo of what you typed) is drawn
// at once, so typing feels like a native terminal. An idle terminal costs
// nothing.
func (m *Model) termWait() tea.Cmd {
	tm := m.terms()
	if tm.waiting || len(tm.views) == 0 {
		return nil
	}
	tm.waiting = true
	gap := termFrameGap
	if m.typingTerm() {
		gap = termFocusGap
	}
	last, notify, done, lastKey := tm.lastFrame, tm.notify, m.ctx.Done(), &tm.lastKey
	return func() tea.Msg {
		select {
		case <-notify:
		case <-done:
			return nil
		}
		if d := time.Until(last.Add(frameGap(gap, lastKey.Load(), time.Now()))); d > 0 {
			select {
			case <-time.After(d):
			case <-done:
				return nil
			}
		}
		return termFrameMsg{}
	}
}

// frameGap is the pause kept after the last frame: gap, or termEchoGap
// when a key was written within termEchoFor (unix nanoseconds; 0: never),
// so the echo of what you type is drawn at the next renderer flush.
func frameGap(gap time.Duration, lastKey int64, now time.Time) time.Duration {
	if lastKey != 0 && now.Sub(time.Unix(0, lastKey)) < termEchoFor {
		return min(gap, termEchoGap)
	}
	return gap
}

// typingTerm reports whether a pane holds (or is taking) a writable
// attach: the focused pane, or one the wheel is scrolling.
func (m *Model) typingTerm() bool {
	if m.focusedTerm() != nil {
		return true
	}
	for _, tv := range m.terms().views {
		if m.wheelHeld(tv) {
			return true
		}
	}
	return false
}

func (m *Model) onTermFrame() tea.Cmd {
	tm := m.terms()
	tm.waiting = false
	tm.lastFrame = time.Now()
	return m.termWait()
}

// embeddable reports whether s has a terminal the pane can attach to, or
// why not (in words for the pane).
func (m *Model) embeddable(s contracts.SessionView) (bool, string) {
	if s.Run == nil {
		return false, "No run yet."
	}
	if s.Run.State.Terminal() {
		return false, "The run has ended."
	}
	if n := m.nodeOf(s); n != nil && n.Connection.State != contracts.ConnConnected {
		return false, orDash(s.NodeName) + " is offline."
	}
	if !contracts.HasCap(s.Run.Capabilities, contracts.CapAttachNative) {
		return false, "No embeddable terminal: " + harnessName(s) + " offers no native attach."
	}
	return true, ""
}

// termView returns the embedded terminal of s, creating it (and closing
// the least recently used beyond maxLiveTerms) when needed.
func (m *Model) termViewFor(s contracts.SessionView) *termView {
	tm := m.terms()
	sid, runID := s.Session.SessionID, ""
	if s.Run != nil {
		runID = s.Run.RunID
	}
	tv := tm.views[sid]
	if tv != nil && tv.runID != runID {
		m.dropTerm(tv) // a new run: a new terminal
		tv = nil
	}
	if tv == nil {
		w, h := tm.paneW, tm.paneH
		if w <= 0 || h <= 0 {
			w, h = max(40, m.width*60/100), max(10, m.height-6)
		}
		tv = newTermView(sid, runID, w, h+statusRows, m.termPoke(sid))
		tm.views[sid] = tv
	}
	tv.lastUse = time.Now()
	return tv
}

// termPoke tells the frame waiter that sid's terminal drew; a warm
// terminal out of sight updates its screen without scheduling a frame.
func (m *Model) termPoke(sid string) func() {
	tm := m.terms()
	ch := tm.notify
	return func() {
		if shown, _ := tm.shown.Load().(string); shown != sid {
			return
		}
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// pruneTerms keeps at most maxLiveTerms attach commands running (closing
// the least recently viewed) and at most maxCachedTerms terminals. The
// selected session's, keep's, a lazily focused one and one holding the
// input lease are never evicted.
func (m *Model) pruneTerms(keep string) {
	tm := m.terms()
	evictable := func(tv *termView) bool {
		return tv.sid != keep && tv.sid != m.sessList.sel && tv.sid != m.termLazy && tv.leaseID == ""
	}
	for {
		var lru *termView
		live := 0
		for _, tv := range tm.views {
			if ok, _ := tv.live(); ok {
				live++
				if evictable(tv) && (lru == nil || tv.lastUse.Before(lru.lastUse)) {
					lru = tv
				}
			}
		}
		if live <= maxLiveTerms || lru == nil {
			break
		}
		lru.closeConn()
	}
	for len(tm.views) > maxCachedTerms {
		var lru *termView
		for _, tv := range tm.views {
			if evictable(tv) && (lru == nil || tv.lastUse.Before(lru.lastUse)) {
				lru = tv
			}
		}
		if lru == nil {
			break
		}
		m.dropTerm(lru)
	}
}

// sweepTerms closes what the cache should no longer hold (refresh tick,
// session list changes): terminals of sessions that are archived or gone
// are dropped; streams of ended or replaced runs, and warm streams not
// viewed for termWarmIdle, are closed (their last screen stays).
func (m *Model) sweepTerms() {
	if m.term == nil {
		return
	}
	now := time.Now()
	for _, tv := range m.term.views {
		if tv.leaseID != "" || tv.sid == m.termLazy || tv.wantInteractive() {
			continue
		}
		s, ok := m.session(tv.sid)
		if !ok || s.Session.Archived {
			m.dropTerm(tv)
			continue
		}
		if live, _ := tv.live(); !live || (m.view == viewSessions && tv.sid == m.sessList.sel) {
			continue
		}
		if s.Run == nil || s.Run.RunID != tv.runID || s.Run.State.Terminal() || now.Sub(tv.lastUse) > termWarmIdle {
			tv.closeConn()
		}
	}
}

// termPrefetchMsg: the cursor rested; warm its neighbours (token: previewTok).
type termPrefetchMsg struct{ token int }

func (m *Model) schedulePrefetch(tok int) tea.Cmd {
	return tea.Tick(termPrefetchDwell, func(time.Time) tea.Msg { return termPrefetchMsg{token: tok} })
}

func (m *Model) onTermPrefetch(msg termPrefetchMsg) tea.Cmd {
	if msg.token != m.previewTok {
		return nil
	}
	return m.prefetchTerms()
}

// prefetchTerms warms the read-only terminals of the rows above and below
// the cursor once it rests, so the next move paints a live screen at once.
// Only streams that cost no ssh handshake are prefetched (the hub's relay,
// or a tmux client on this machine), within the warm cap (pruneTerms); a
// prefetched terminal ranks as viewed termPrefetchAge ago, so the sessions
// actually looked at are evicted last.
func (m *Model) prefetchTerms() tea.Cmd {
	if m.view != viewSessions || m.tableShown() || m.overlay != nil {
		return nil
	}
	l := &m.sessList
	if l.sel == "" || l.idx < 0 || l.idx >= len(l.ids) || l.ids[l.idx] != l.sel {
		return nil
	}
	var cmds []tea.Cmd
	for _, i := range []int{l.idx + 1, l.idx - 1} {
		if i < 0 || i >= len(l.ids) {
			continue
		}
		s, ok := m.session(l.ids[i])
		if !ok {
			continue // a group or machine row
		}
		if emb, _ := m.embeddable(s); !emb {
			continue
		}
		relay := m.relayFor(s)
		if !relay && (m.opts.ClientNodeID == "" || s.Run.NodeID != m.opts.ClientNodeID) {
			continue // an ssh attach: never a handshake for a session not asked for
		}
		sid := s.Session.SessionID
		if tv := m.terms().views[sid]; tv != nil && tv.runID == s.Run.RunID {
			live, _ := tv.live()
			st, _, _ := tv.status()
			if live || tv.wantInteractive() || st == termConnecting || st == termStarting || time.Now().Before(tv.unavailableUntil) {
				continue
			}
		}
		if relay && !m.relayOpenOK(s.Run.NodeID) {
			continue
		}
		var prev time.Time
		if tv := m.terms().views[sid]; tv != nil {
			prev = tv.lastUse
		}
		tv := m.termViewFor(s)
		tv.lastUse = time.Now().Add(-termPrefetchAge)
		if prev.After(tv.lastUse) {
			tv.lastUse = prev
		}
		cmds = append(cmds, m.requestTerm(s, tv, termReadOnly))
	}
	return tea.Batch(cmds...)
}

func (m *Model) dropTerm(tv *termView) {
	if tv.leaseID != "" {
		m.releaseLease(tv.leaseID)
		tv.leaseID = ""
	}
	tv.close()
	delete(m.terms().views, tv.sid)
}

// LiveTerms reports how many embedded attach commands are running (tests).
func (m *Model) LiveTerms() int {
	n := 0
	for _, tv := range m.terms().views {
		if ok, _ := tv.live(); ok {
			n++
		}
	}
	return n
}

// TermLive reports whether a session's embedded terminal has a running
// attach (tests).
func (m *Model) TermLive(sessionID string) bool {
	if tv := m.terms().views[sessionID]; tv != nil {
		live, _ := tv.live()
		return live
	}
	return false
}

// TermText is the plain text of a session's embedded terminal (tests).
func (m *Model) TermText(sessionID string) string {
	if tv := m.terms().views[sessionID]; tv != nil {
		return tv.text()
	}
	return ""
}

// TermFocused reports whether keys go to the embedded terminal (tests).
func (m *Model) TermFocused() bool { return m.focusedTerm() != nil }

// openTermView starts the read-only view of the selected session's
// terminal (no-op while one runs).
func (m *Model) openTermView() tea.Cmd {
	if m.view != viewSessions || m.tableShown() { // the table has no pane
		return nil
	}
	s, ok := m.selectedSession()
	if !ok {
		return nil
	}
	if ok, _ := m.embeddable(s); !ok {
		return nil
	}
	tv := m.termViewFor(s)
	if live, _ := tv.live(); live || tv.wantInteractive() {
		return nil
	}
	if st, _, _ := tv.status(); st == termConnecting || time.Now().Before(tv.unavailableUntil) {
		return nil
	}
	return m.requestTerm(s, tv, termReadOnly)
}

func (tv *termView) wantInteractive() bool { return tv.reqMode == termInteractive }

// requestTerm asks the hub for an attach descriptor (and, for an
// interactive attach, the run's input lease), then starts it.
func (m *Model) requestTerm(s contracts.SessionView, tv *termView, mode termMode) tea.Cmd {
	tv.reqTok++
	tv.reqMode = mode
	tok, sid, runID := tv.reqTok, s.Session.SessionID, s.Run.RunID
	takeover := mode == termInteractive && tv.takeover
	tv.takeover = false
	if live, _ := tv.live(); !live {
		tv.setState(termConnecting, "")
	}
	req := contracts.AttachRequest{
		Mutation:     contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		SessionID:    sid,
		ClientNodeID: m.opts.ClientNodeID,
		Preferred:    []contracts.AttachMechanism{contracts.AttachLocalTmux, contracts.AttachSSHTmux},
		ReadOnly:     mode == termReadOnly,
	}
	// Watching a session on another machine streams through the hub's
	// relay, never ssh (relay.go); typing keeps the native attach unless
	// the node has no ssh route (then typing is relayed too).
	relay := mode == termReadOnly && m.relayFor(s)
	if relay {
		req.Preferred = append([]contracts.AttachMechanism{contracts.AttachWebTerminal}, req.Preferred...)
	}
	typeRelay := mode == termInteractive && m.relayTypingFor(s)
	canTypeRelay := mode == termInteractive && m.relayReachable(s)
	// Watching a session on this machine: its tmux session directly, no
	// hub (localterm.go); the hub answers when that fails.
	local, isLocal := localTarget{}, false
	if mode == termReadOnly && !relay {
		local, isLocal = m.localFor(s)
	}
	cols, rows := tv.size()
	api, parent, timeout, dial, node := m.api, m.ctx, m.opts.LoadTimeout, m.opts.DialRelay, s.Run.NodeID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		if typeRelay {
			return relayTyping(ctx, api, dial, req, termAttachMsg{sid: sid, tok: tok, mode: mode, takeover: takeover}, runID, cols, rows)
		}
		bad := ""
		if isLocal {
			screen, err := local.capture(ctx, true)
			if err == nil {
				return termAttachMsg{sid: sid, tok: tok, mode: mode, d: local.descriptor(), local: true, screen: screen}
			}
			bad = runID
		}
		d, err := api.AttachSession(ctx, req)
		msg := termAttachMsg{sid: sid, tok: tok, mode: mode, d: d, err: err, takeover: takeover, localBad: bad}
		if canTypeRelay && terminal.NoSSHRoute(err) {
			// No ssh to that node: type through the relay, and remember.
			msg = relayTyping(ctx, api, dial, req, termAttachMsg{sid: sid, tok: tok, mode: mode, takeover: takeover}, runID, cols, rows)
			msg.noSSH = node
			return msg
		}
		if relay && err == nil && d.Mechanism == contracts.AttachWebTerminal {
			dctx, dcancel := context.WithTimeout(parent, timeout)
			defer dcancel()
			conn, err := dial(dctx, d, cols, rows, false)
			if err != nil {
				msg.err = err
				if relayUnsupported(err) {
					msg.relayOff = node
				}
				return msg
			}
			msg.conn = conn
			return msg
		}
		if err != nil || mode != termInteractive || d.ReadOnly || !embedMechanism(d) {
			return msg
		}
		l, err := api.AcquireLease(ctx, contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, RunID: runID, TTLSeconds: leaseTTL, Takeover: takeover})
		if err != nil {
			msg.err = err
			return msg
		}
		msg.leaseID, msg.leaseAt = l.LeaseID, l.AcquiredAt
		return msg
	}
}

// relayTyping opens a writable relayed stream of the run under its input
// lease: the lease first (the hub refuses an interactive web terminal
// while someone else holds it, so a takeover happens before), then a
// web_terminal descriptor and the stream. Any failure gives the lease back.
func relayTyping(ctx context.Context, api contracts.API, dial RelayDialer, req contracts.AttachRequest, msg termAttachMsg, runID string, cols, rows int) termAttachMsg {
	l, err := api.AcquireLease(ctx, contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, RunID: runID, TTLSeconds: leaseTTL, Takeover: msg.takeover})
	if err != nil {
		msg.err = err
		return msg
	}
	release := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = api.ReleaseLease(rctx, contracts.LeaseReleaseRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, LeaseID: l.LeaseID})
	}
	req.Mutation = contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
	req.ReadOnly = false
	req.Preferred = []contracts.AttachMechanism{contracts.AttachWebTerminal, contracts.AttachLocalTmux, contracts.AttachSSHTmux}
	d, err := api.AttachSession(ctx, req)
	msg.d = d
	switch {
	case err != nil:
		msg.err = err
	case d.Mechanism != contracts.AttachWebTerminal:
		msg.err = contracts.Errorf(contracts.ErrUnsupported, "the hub has no terminal relay for this session (it answered %s)", d.Mechanism)
	case d.ReadOnly:
		msg.err = contracts.Errorf(contracts.ErrUnsupported, "the relay offers this session read-only")
	}
	if msg.err != nil {
		release()
		return msg
	}
	conn, err := dial(ctx, d, cols, rows, false)
	if err != nil {
		release()
		msg.err = err
		return msg
	}
	msg.conn, msg.leaseID, msg.leaseAt = conn, l.LeaseID, l.AcquiredAt
	return msg
}

func embedMechanism(d contracts.AttachDescriptor) bool {
	return (d.Mechanism == contracts.AttachLocalTmux || d.Mechanism == contracts.AttachSSHTmux) && len(d.Command) > 0
}

func (m *Model) onTermAttach(msg termAttachMsg) tea.Cmd {
	tm := m.terms()
	tv := tm.views[msg.sid]
	if tv == nil || msg.tok != tv.reqTok {
		if msg.leaseID != "" {
			m.releaseLease(msg.leaseID) // a stale answer: never keep its lease
		}
		if msg.conn != nil {
			_ = msg.conn.Close()
		}
		return nil
	}
	if msg.noSSH != "" {
		m.relays().noSSH[msg.noSSH] = true
	}
	m.localBad(msg.localBad)
	if msg.err == nil && !msg.local {
		if s, ok := m.session(msg.sid); ok {
			m.learnLocalTmux(s, msg.d)
		}
	}
	if msg.relayOff != "" {
		// The node cannot relay this run: watch it the old way (ssh).
		m.relayRefused(msg.relayOff)
		tv.reqMode = termReadOnly
		if live, _ := tv.live(); !live {
			tv.setState(termIdle, "")
		}
		return m.openTermView()
	}
	if msg.conn != nil && msg.mode == termReadOnly { // the newest request: a relayed read-only view
		tv.setConn(msg.conn, termReadOnly)
		tv.note, tv.startWait = "", 0
		m.pruneTerms(msg.sid)
		return m.termWait()
	}
	interactive := msg.mode == termInteractive
	relayed := msg.conn != nil // a writable relayed stream (relayTyping)
	if interactive && !m.wantsInteractive(msg.sid) && m.wheelHeld(tv) &&
		(msg.err != nil || (!relayed && (!embedMechanism(msg.d) || msg.d.ReadOnly))) {
		return m.wheelHoldFailed(tv, msg)
	}
	fail := func(state termState, text string) tea.Cmd {
		tv.reqMode = termReadOnly
		tv.unavailableUntil = time.Now().Add(termRetryWait)
		if live, _ := tv.live(); !live {
			tv.setState(state, text)
		}
		if interactive {
			m.focus = 0
			tv.pending = nil
		}
		return nil
	}
	switch {
	case msg.err != nil && m.startingConflict(msg.sid, msg.err):
		return m.termStarting(tv, interactive) // calm "starting…", retried until live
	case msg.err != nil:
		text := m.humanIDs(errText(msg.err))
		if interactive {
			if _, lease := leaseConflict(msg.err); lease {
				fail(termFailed, "")
				tv.unavailableUntil = time.Time{}
				if live, _ := tv.live(); !live {
					tv.setState(termIdle, "")
				}
				return tea.Batch(m.focusRefused(tv, msg.err), m.openTermView())
			}
			fail(termFailed, text)
			tv.unavailableUntil = time.Time{}
			return m.setToast("cannot type into the session: "+text, toneWarn)
		}
		return fail(termFailed, text)
	case relayed:
	case !embedMechanism(msg.d):
		if interactive {
			fail(termUnavailable, msg.d.Notes)
			tv.unavailableUntil = time.Time{}
			if s, ok := m.session(msg.sid); ok {
				return m.attach(s) // web terminal / read-only: the full-screen path explains it
			}
			return nil
		}
		tv.note = msg.d.Notes
		return fail(termUnavailable, msg.d.Notes)
	case interactive && msg.d.ReadOnly:
		tv.reqMode, tv.pending, m.focus = termReadOnly, nil, 0
		return m.setToast("this session's terminal is read-only from here; m sends a message", toneWarn)
	}
	if interactive && !m.keepsInteractive(tv) {
		// Focus moved away while the attach was being prepared.
		if msg.leaseID != "" {
			m.releaseLease(msg.leaseID)
		}
		if relayed {
			_ = msg.conn.Close()
		}
		tv.reqMode = termReadOnly
		return nil
	}
	if !interactive && !relayed && msg.d.Mechanism == contracts.AttachSSHTmux && msg.sid != m.sessList.sel {
		// A prefetch (or a row already left) answered with ssh (a hub
		// without the relay): no handshake for a session not on screen.
		if live, _ := tv.live(); !live {
			tv.setState(termIdle, "")
		}
		return nil
	}
	if len(msg.screen) > 0 {
		tv.seed(msg.screen) // the first frame: the pane as it is now
	}
	cols, rows := tv.size()
	conn, err := msg.conn, error(nil)
	if !relayed {
		conn, err = m.starter()(msg.d, cols, rows)
	}
	if err != nil && msg.local {
		m.localBad(msg.d.RunID) // next time through the hub
	}
	if err != nil {
		if msg.leaseID != "" {
			m.releaseLease(msg.leaseID)
		}
		if interactive {
			fail(termFailed, err.Error())
			tv.unavailableUntil = time.Time{}
			return m.setToast("cannot embed the terminal: "+err.Error()+" (A attaches full-screen)", toneBad)
		}
		return fail(termFailed, err.Error())
	}
	tv.setConn(conn, msg.mode)
	tv.note, tv.startWait = "", 0
	if msg.local {
		m.local.opened++
	}
	var cmds []tea.Cmd
	if interactive {
		tv.leaseID, tv.leaseAt, tv.lastKey, tv.leaseIdle = msg.leaseID, msg.leaseAt, time.Now(), false
		tv.askTakeover, tv.focusNote = "", ""
		for _, b := range tv.pending {
			tv.send(b)
		}
		tv.pending = nil
		if tv.leaseID != "" {
			cmds = append(cmds, m.leaseTick(tv.sid, tv.leaseID))
		}
		if msg.takeover {
			cmds = append(cmds, m.setToast("took over typing here; the other client is watching", toneGood))
		}
	}
	m.pruneTerms(msg.sid)
	cmds = append(cmds, m.termWait())
	return tea.Batch(cmds...)
}

// releaseLease releases an input lease in the background.
func (m *Model) releaseLease(leaseID string) {
	api, parent := m.api, m.ctx
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer cancel()
		_, _ = api.ReleaseLease(ctx, contracts.LeaseReleaseRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, LeaseID: leaseID})
	}()
}

// focusTerm gives the embedded terminal of the selected session the
// keyboard (enter, click, tab): it swaps the read-only view for a writable
// attach that holds the input lease. A session without an embeddable
// terminal falls back to the full-screen attach.
func (m *Model) focusTerm() tea.Cmd {
	s, ok := m.selectedSession()
	if !ok {
		return nil
	}
	if r := m.actionReason(s, "attach"); r != "" {
		return m.setToast("attach unavailable: "+r, toneWarn)
	}
	if ok, _ := m.embeddable(s); !ok {
		return m.attach(s)
	}
	m.focus, m.detailOff = 1, 0
	tv := m.termViewFor(s)
	// The owner's own other client holding the lease is taken over (the
	// header said "enter to take over"); anyone else is never overridden.
	tv.takeover = tv.takeoverOffered() || (s.Run.Lease != nil && s.Run.Lease.Active(m.now()) &&
		s.Run.Lease.LeaseID != tv.leaseID && s.Run.Lease.Holder.Kind == contracts.PrincipalOwner)
	tv.askTakeover, tv.focusNote = "", ""
	if live, mode := tv.live(); live && mode == termInteractive {
		return nil
	}
	if tv.wantInteractive() {
		return nil // already connecting
	}
	if st, _, _ := tv.status(); st == termStarting {
		tv.reqMode = termInteractive // the starting retry attaches writable; keys wait in pending
		return m.termWait()
	}
	return tea.Batch(m.requestTerm(s, tv, termInteractive), m.termWait())
}

// wantsInteractive reports whether sid's terminal should hold the keyboard.
func (m *Model) wantsInteractive(sid string) bool {
	return m.view == viewSessions && m.focus == 1 && m.overlay == nil && !m.filtering && m.sessList.sel == sid
}

// focusedTerm is the embedded terminal that receives keys, if any.
func (m *Model) focusedTerm() *termView {
	if m.term == nil || !m.wantsInteractive(m.sessList.sel) {
		return nil
	}
	tv := m.term.views[m.sessList.sel]
	if tv == nil || (!tv.wantInteractive() && !m.lazyFocus(tv)) {
		return nil
	}
	return tv
}

// reconcileTerms runs after every update: a terminal that holds (or is
// acquiring) the keyboard without focus goes back to read-only and gives
// up the input lease, whatever moved the focus (escape chord, a click on
// the list, a tab, another view).
func (m *Model) reconcileTerms() tea.Cmd {
	if m.term == nil {
		return nil
	}
	cmds := []tea.Cmd{m.leaseWatch()}
	for _, tv := range m.term.views {
		if !tv.wantInteractive() || m.keepsInteractive(tv) {
			continue
		}
		tv.holdUntil = time.Time{}
		tv.reqMode = termReadOnly
		tv.reqTok++ // drop an attach still being prepared
		tv.pending, tv.leaseIdle, tv.retaking = nil, false, false
		if tv.leaseID != "" {
			m.releaseLease(tv.leaseID)
			tv.leaseID = ""
		}
		if live, mode := tv.live(); live && mode == termInteractive {
			tv.closeConn() // the screen stays until the read-only view redraws it
		} else if st, _, _ := tv.status(); st == termConnecting {
			tv.setState(termIdle, "")
		}
		if m.view == viewSessions && m.sessList.sel == tv.sid {
			cmds = append(cmds, m.openTermView())
		}
	}
	return tea.Batch(cmds...)
}

// termKey sends a key to the focused terminal; the escape chord returns
// focus to the list instead.
func (m *Model) termKey(tv *termView, k tea.KeyPressMsg) tea.Cmd {
	if m.isUnfocusKey(k.String()) {
		m.focus = 0
		return nil
	}
	tv.clearSel()
	appCursor, _ := tv.keyModes()
	b := encodeKey(k, appCursor)
	if len(b) == 0 {
		return nil
	}
	if (k.Text != "" || k.Code == tea.KeyEnter) && k.Mod&(tea.ModCtrl|tea.ModAlt) == 0 && tv.inCopyMode() {
		b = append([]byte("q"), b...) // typing leaves tmux copy-mode, like a terminal snapping to the bottom
	}
	return m.termSend(tv, b)
}

func (m *Model) termSend(tv *termView, b []byte) tea.Cmd {
	tv.lastKey = time.Now()
	activate := m.activateLazy(tv) // focus followed a switch: this key takes the lease (switch.go)
	if live, mode := tv.live(); !live || mode != termInteractive || tv.leaseIdle || tv.retaking {
		if len(tv.pending) < 256 {
			tv.pending = append(tv.pending, b)
		}
		if live && mode == termInteractive {
			return tea.Batch(activate, m.retakeLease(tv)) // idle pane: take the lease back, then the keys follow
		}
		return activate
	}
	m.terms().lastKey.Store(time.Now().UnixNano())
	if !tv.send(b) {
		return m.setToast("the session is not reading input; keys dropped", toneWarn)
	}
	return nil
}

// termPaste sends pasted text, bracketed when the application asked.
func (m *Model) termPaste(tv *termView, text string) tea.Cmd {
	_, bracketed := tv.keyModes()
	return m.termSend(tv, bracketPaste(text, bracketed))
}

// termCell maps a screen cell to a cell of the embedded terminal drawn in
// the last frame (pane-relative, 0-based). clamp maps cells outside the
// terminal to its nearest edge (a drag that leaves the pane).
func (m *Model) termCell(x, y int, clamp bool) (col, row int, ok bool) {
	ox, oy, w, h := -1, -1, 0, 0
	for _, hit := range m.zhits {
		if hit.def < 0 || hit.def >= len(m.frameDefs) || m.frameDefs[hit.def].kind != zoneTerm {
			continue
		}
		r := m.frameDefs[hit.def].idx
		if hit.y == y && x >= hit.x0 && x < hit.x1 {
			return x - hit.x0, r, true
		}
		if ox < 0 || hit.y-r < oy {
			ox, oy = hit.x0, hit.y-r
		}
		w, h = max(w, hit.x1-hit.x0), max(h, r+1)
	}
	if !clamp || ox < 0 {
		return 0, 0, false
	}
	return min(max(x-ox, 0), w-1), min(max(y-oy, 0), h-1), true
}

// wheelHoldIdle is how long a writable attach taken for the wheel alone
// (the pane not focused) is kept after the last wheel event.
const wheelHoldIdle = 8 * time.Second

type wheelHoldMsg struct{ sid string }

// termMouse sends a mouse event over the embedded terminal to the session,
// encoded for the mouse modes its tmux client enabled; ok is false when
// the event is not the terminal's (elsewhere on screen, another view). A
// press focuses the pane and passes through (Claude's clickable elements);
// a drag stays with the terminal until the release. The wheel over a
// watched pane scrolls it without taking the keyboard: a read-only tmux
// client sends no mouse input, so a writable attach (with the input lease)
// is held while the wheel moves and released wheelHoldIdle later.
func (m *Model) termMouse(ev mouseEvent, ms tea.Mouse) (tea.Cmd, bool) {
	if m.view != viewSessions || m.overlay != nil || m.filtering || m.term == nil {
		m.termDrag, m.termPress = "", nil
		return nil, false
	}
	dragging := m.termDrag != "" && m.termDrag == m.sessList.sel
	if (ev == mouseRelease || ev == mouseMotion) && !dragging {
		return nil, false
	}
	col, row, ok := m.termCell(ms.X, ms.Y, dragging)
	if !ok {
		return nil, false
	}
	s, ok := m.selectedSession()
	if !ok {
		return nil, false
	}
	if emb, _ := m.embeddable(s); !emb {
		return nil, false
	}
	tv := m.terms().views[s.Session.SessionID]
	if tv == nil {
		return nil, false
	}
	mm, top := tv.mouseState()
	// The left button selects (drag) or clicks (termselect.go): the press
	// is held back until the release says which.
	if (ev == mousePress && ms.Button == tea.MouseLeft) || ((ev == mouseMotion || ev == mouseRelease) && m.termPress != nil && m.termPress.sid == tv.sid) {
		switch ev {
		case mousePress:
			m.termDrag = tv.sid
		case mouseRelease:
			m.termDrag = ""
		}
		return m.termSelectMouse(ev, ms, tv, col, top+row, func(press tea.Mouse, x, y int) tea.Cmd {
			cmds := []tea.Cmd{m.activateLazy(tv)}
			if m.focusedTerm() == nil {
				cmds = append(cmds, m.focusTerm()) // a click on the terminal types into it, and still clicks
			}
			if live, mode := tv.live(); !(live && mode == termInteractive) && !tv.wantInteractive() {
				return tea.Batch(cmds...)
			}
			for _, e := range []mouseEvent{mousePress, mouseRelease} {
				if b := encodeMouse(mm, e, press, x, y); len(b) > 0 {
					cmds = append(cmds, m.termSend(tv, b))
				}
			}
			return tea.Batch(cmds...)
		}), true
	}
	if ev == mousePress {
		tv.clearSel()
	}
	var cmds []tea.Cmd
	if ev == mousePress || ev == mouseWheel {
		cmds = append(cmds, m.activateLazy(tv)) // focus followed a switch: take the lease now
	}
	switch ev {
	case mousePress:
		m.termDrag = tv.sid
		if m.focusedTerm() == nil {
			cmds = append(cmds, m.focusTerm()) // a click on the terminal types into it, and still clicks
		}
	case mouseRelease:
		m.termDrag = ""
	case mouseWheel:
		if m.focusedTerm() == nil {
			if time.Now().Before(tv.holdBlockedUntil) {
				return nil, true
			}
			cmds = append(cmds, m.holdForWheel(s, tv))
		}
		if !mm.tracking() {
			if m.focusedTerm() != nil {
				return m.termWheel(tv, ms.Button == tea.MouseWheelUp), true // a tmux server with mouse off
			}
			mm = mouseModes{normal: true, sgr: true} // tmux reads SGR mouse input whatever it enabled
		}
	}
	live, mode := tv.live()
	if !(live && mode == termInteractive) && !tv.wantInteractive() {
		return tea.Batch(cmds...), true // no writable attach coming: never queue input for later
	}
	if b := encodeMouse(mm, ev, ms, col, top+row); len(b) > 0 {
		cmds = append(cmds, m.termSend(tv, b))
	}
	return tea.Batch(cmds...), true
}

// holdForWheel keeps (or starts) a writable attach for the wheel while the
// pane is not focused.
func (m *Model) holdForWheel(s contracts.SessionView, tv *termView) tea.Cmd {
	arm := tv.holdUntil.IsZero()
	tv.holdUntil = time.Now().Add(wheelHoldIdle)
	var cmds []tea.Cmd
	if arm {
		cmds = append(cmds, wheelHoldTick(tv.sid, wheelHoldIdle))
	}
	if live, mode := tv.live(); !(live && mode == termInteractive) && !tv.wantInteractive() {
		cmds = append(cmds, m.requestTerm(s, tv, termInteractive), m.termWait())
	}
	return tea.Batch(cmds...)
}

func wheelHoldTick(sid string, d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return wheelHoldMsg{sid: sid} })
}

// onWheelHold re-arms the hold timer while the wheel keeps moving; once
// idle, reconcileTerms (after this update) returns the pane to watching.
func (m *Model) onWheelHold(msg wheelHoldMsg) tea.Cmd {
	tv := m.terms().views[msg.sid]
	if tv == nil || tv.holdUntil.IsZero() {
		return nil
	}
	if d := time.Until(tv.holdUntil); d > 0 {
		return wheelHoldTick(msg.sid, d)
	}
	tv.holdUntil = time.Time{}
	return nil
}

// wheelHeld reports whether tv keeps a writable attach for the wheel.
func (m *Model) wheelHeld(tv *termView) bool {
	return !tv.holdUntil.IsZero() && time.Now().Before(tv.holdUntil) &&
		m.view == viewSessions && m.overlay == nil && !m.filtering && m.sessList.sel == tv.sid
}

// keepsInteractive reports whether tv should keep (or get) its writable
// attach: the pane has the keyboard, or the wheel is scrolling it.
func (m *Model) keepsInteractive(tv *termView) bool {
	return m.wantsInteractive(tv.sid) || m.wheelHeld(tv)
}

// wheelHoldFailed ends a wheel hold whose writable attach was refused: one
// toast, no retries for a while, never the full-screen fallback.
func (m *Model) wheelHoldFailed(tv *termView, msg termAttachMsg) tea.Cmd {
	if msg.leaseID != "" {
		m.releaseLease(msg.leaseID)
	}
	tv.reqMode, tv.pending, tv.holdUntil = termReadOnly, nil, time.Time{}
	tv.holdBlockedUntil = time.Now().Add(termRetryWait)
	if live, _ := tv.live(); !live {
		tv.setState(termIdle, "")
	}
	why := msg.d.Notes
	switch {
	case msg.err != nil && contracts.IsCode(msg.err, contracts.ErrConflict):
		why = "another client holds this run's input lease"
	case msg.err != nil:
		why = m.humanIDs(errText(msg.err))
	case msg.d.ReadOnly:
		why = "its terminal is read-only from here"
	}
	return tea.Batch(m.setToast("the wheel cannot scroll this session: "+orDash(why), toneWarn), m.openTermView())
}

// termWheel scrolls the focused terminal's history through tmux
// copy-mode when its tmux client reports no mouse (a server with mouse
// off): the prefix key (Pyre's tmux servers keep the default ctrl+b) then
// [ enters it, ctrl+up/down scroll, q leaves it at the bottom.
func (m *Model) termWheel(tv *termView, up bool) tea.Cmd {
	in := tv.inCopyMode()
	switch {
	case up && !in:
		return m.termSend(tv, []byte("\x02[\x1b[1;5A\x1b[1;5A\x1b[1;5A"))
	case up:
		return m.termSend(tv, []byte("\x1b[1;5A\x1b[1;5A\x1b[1;5A"))
	case in && tv.copyAtBottom():
		return m.termSend(tv, []byte("q"))
	case in:
		return m.termSend(tv, []byte("\x1b[1;5B\x1b[1;5B\x1b[1;5B"))
	}
	return nil
}

// isUnfocusKey reports whether key is the escape chord (term.unfocus).
func (m *Model) isUnfocusKey(key string) bool {
	for _, seq := range m.km.bind["term.unfocus"] {
		if len(seq) == 1 && seq[0] == key {
			return true
		}
	}
	return false
}

// unfocusLabel is the escape chord for hints ("ctrl+]").
func (m *Model) unfocusLabel() string {
	if k := m.km.keysFor("term.unfocus"); k != "" {
		return strings.SplitN(k, " / ", 2)[0]
	}
	return "ctrl+]"
}

// closeTerms ends every embedded attach and releases held leases (quit).
func (m *Model) closeTerms() {
	if m.term == nil {
		return
	}
	for _, tv := range m.term.views {
		if tv.leaseID != "" {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), 3*time.Second)
			_, _ = m.api.ReleaseLease(ctx, contracts.LeaseReleaseRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, LeaseID: tv.leaseID})
			cancel()
			tv.leaseID = ""
		}
		tv.close()
	}
	m.term.views = map[string]*termView{}
}
