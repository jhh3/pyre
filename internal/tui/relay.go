package tui

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Watching sessions on other machines through Pyre's own connection.
//
// The Sessions pane's read-only view and the Grid's tiles of a session on
// another machine used to run the attach descriptor's command, `ssh host
// tmux attach -r`, plus an ssh window-size probe every 10s per tile. A
// busy Grid made dozens of ssh handshakes a minute from one address; on a
// loaded host they exceeded LoginGraceTime and sshd's per-source penalties
// then refused the whole address, the owner's own shell included
// (2026-10-08). Now, when the TUI has a relay dialer (Options.DialRelay,
// the hub client's DialTerminal), those views ask for a web_terminal
// descriptor and stream the run's terminal through the hub's relay
// (internal/terminal): the run's node runs the read-only tmux client
// locally and no ssh connection is made. Grid tiles ask for a fitted
// stream (contracts.TerminalFit): the node sizes it to the window and
// follows it, so there is no client-side probe either.
//
// Writable attaches (focusing the pane, A/F full screen) keep the native
// descriptor (ssh, multiplexed by sshMux) — except on a node with no ssh
// route (no ssh alias: a Windows cloud VM has no ssh server). There the
// pane's writable attach streams through the relay too, under the input
// lease like the native one (relayTypingFor), and A/F, which would fail
// with "needs an ssh alias", type in the pane instead. Such a node is
// learnt from its first refusal and remembered (relayState.noSSH). The ssh path also remains the
// fallback: Options.SSHPreviews forces it; a node whose relay refused as
// unsupported uses it for relayOffFor; tiles use it when the hub or the
// node does not advertise terminal.fit. Sessions on this machine attach
// locally as before.

// RelayDialer opens a web_terminal descriptor's stream through the hub
// (client.Client.DialTerminal). fit asks the node to size the stream to
// the tmux window; such a conn implements WindowSizer.
type RelayDialer func(ctx context.Context, d contracts.AttachDescriptor, cols, rows int, fit bool) (TermConn, error)

// WindowSizer is a fitted relay stream: the node sizes it to the window.
// TakeWindowSize reports a change once; the embedded terminal applies it
// between reads, before the bytes drawn for the new size.
type WindowSizer interface {
	WindowSize() (cols, rows int)
	TakeWindowSize() (cols, rows int, ok bool)
}

const (
	relayOpenBurst  = 6               // tile streams opened per node per relayOpenWindow
	relayOpenWindow = 3 * time.Second // ...beyond that, tiles wait (gridRelayRetry)
	relayOffFor     = 10 * time.Minute
	gridRelayRetry  = time.Second
)

// relayState is the relay bookkeeping (Update goroutine only, except
// hello, which commands read).
type relayState struct {
	opens map[string][]time.Time // node -> recent tile stream opens
	off   map[string]time.Time   // node -> ssh fallback until
	noSSH map[string]bool        // nodes with no ssh route: typing goes through the relay
	hello *helloCache
}

// helloCache remembers whether the hub forwards fitted streams.
type helloCache struct {
	mu   sync.Mutex
	done bool
	at   time.Time
	fit  bool
}

func (m *Model) relays() *relayState {
	r := &m.relay
	if r.opens == nil {
		r.opens, r.off, r.noSSH, r.hello = map[string][]time.Time{}, map[string]time.Time{}, map[string]bool{}, &helloCache{}
	}
	return r
}

// relayFor reports whether s's read-only views stream through the relay:
// a dialer is configured, ssh is not forced, and s runs on another
// machine whose relay has not refused.
func (m *Model) relayFor(s contracts.SessionView) bool {
	if m.opts.DialRelay == nil || m.opts.SSHPreviews || s.Run == nil {
		return false
	}
	node := s.Run.NodeID
	if node == "" || node == m.opts.ClientNodeID {
		return false // this machine: a local tmux client, no ssh
	}
	if until, ok := m.relays().off[node]; ok && time.Now().Before(until) {
		return false
	}
	return true
}

// relayReachable reports whether s's terminal can be relayed at all: a
// dialer is configured and s runs on another machine whose relay has not
// refused. (Unlike relayFor, PYRE_TUI_PREVIEW=ssh does not matter: it
// picks how to watch, and a node with no ssh route has no other way.)
func (m *Model) relayReachable(s contracts.SessionView) bool {
	if m.opts.DialRelay == nil || s.Run == nil {
		return false
	}
	node := s.Run.NodeID
	if node == "" || node == m.opts.ClientNodeID {
		return false
	}
	if until, ok := m.relays().off[node]; ok && time.Now().Before(until) {
		return false
	}
	return true
}

// relayTypingFor reports whether typing into s goes straight through the
// relay: its node is known to have no ssh route.
func (m *Model) relayTypingFor(s contracts.SessionView) bool {
	return m.relayReachable(s) && m.relays().noSSH[s.Run.NodeID]
}

// relayTileFor reports whether s's Grid tile streams through the relay:
// relayFor, and the node serves fitted streams (the hub is checked by
// hubFits before the stream is asked for).
func (m *Model) relayTileFor(s contracts.SessionView) bool {
	if !m.relayFor(s) {
		return false
	}
	n := m.nodeOf(s)
	return n != nil && contracts.HasCap(n.Capabilities, contracts.TerminalFit)
}

// relayOpenOK rate-limits tile stream opens per node; true records one.
func (m *Model) relayOpenOK(node string) bool {
	r := m.relays()
	now := time.Now()
	recent := r.opens[node][:0]
	for _, t := range r.opens[node] {
		if now.Sub(t) < relayOpenWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= relayOpenBurst {
		r.opens[node] = recent
		return false
	}
	r.opens[node] = append(recent, now)
	return true
}

// relayRefused turns the relay off for node for a while (it answered
// unsupported: no bridge, or an attach it cannot relay).
func (m *Model) relayRefused(node string) {
	if node != "" {
		m.relays().off[node] = time.Now().Add(relayOffFor)
	}
}

// hubFits asks the hub once (rechecked after a failure) whether it
// forwards fitted streams. Safe off the Update goroutine.
func hubFits(ctx context.Context, api contracts.API, hc *helloCache) bool {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.done && (hc.fit || time.Since(hc.at) < time.Minute) {
		return hc.fit
	}
	h, err := api.Hello(ctx, contracts.HelloRequest{Protocol: contracts.ProtocolVersion, Client: "pyre-tui"})
	hc.done, hc.at, hc.fit = true, time.Now(), err == nil && hasString(h.Features, contracts.TerminalFit)
	return hc.fit
}

func hasString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// relayUnsupported reports an error that means the relay cannot serve the
// run (as opposed to a transient failure or capacity).
func relayUnsupported(err error) bool {
	return contracts.IsCode(err, contracts.ErrUnsupported)
}

type gridRelayRetryMsg struct{}

func gridRelayRetryTick() tea.Cmd {
	return tea.Tick(gridRelayRetry, func(time.Time) tea.Msg { return gridRelayRetryMsg{} })
}

// focusPane types into s's embedded pane: A/F on a node with no ssh route,
// whose full-screen attach would need ssh. The pane's writable attach
// streams through the relay (relayTypingFor).
func (m *Model) focusPane(s contracts.SessionView) tea.Cmd {
	sid := s.Session.SessionID
	if m.view == viewGrid && m.grid.list.sel == sid {
		return m.gridZoom()
	}
	var cmds []tea.Cmd
	if m.view != viewSessions {
		m.view, m.back, m.focus = viewSessions, nil, 0
		m.syncSessions()
		cmds = append(cmds, m.refreshView(false)...)
	}
	m.focusSessionQuiet(sid)
	if m.sessList.sel != sid {
		return m.setToast("that session is not in the Sessions list (filter or ended)", toneWarn)
	}
	if m.tableShown() {
		return tea.Batch(append(cmds, m.tableZoom())...)
	}
	return tea.Batch(append(cmds, m.openTermView(), m.focusTerm())...)
}
