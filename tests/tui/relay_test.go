package tui_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// startRelay starts the TUI against a hub with a terminal relay (synthetic
// web_terminal descriptors, fake relayed streams). The client is on no
// node, so every session is on another machine.
func startRelay(t *testing.T, w, ht int, cfg func(*tuitest.Relayed), mod func(*tui.Options)) (*tuitest.Harness, *tuitest.Recorder) {
	t.Helper()
	var rec *tuitest.Recorder
	h, _ := start(t, w, ht, func(a contracts.API) contracts.API {
		r := tuitest.NewRelayed(tuitest.NewEmbeddable(a))
		if cfg != nil {
			cfg(r)
		}
		rec = tuitest.NewRecorder(r)
		return rec
	}, func(o *tui.Options) {
		o.DialRelay = tuitest.FakeRelayDialer
		if mod != nil {
			mod(o)
		}
	})
	return h, rec
}

// nativeTerms are the terminals started from a native (ssh/local)
// descriptor, i.e. what the ssh path would have run.
func nativeTerms(h *tuitest.Harness, readOnly bool) int {
	n := 0
	for _, f := range h.Terms() {
		if f.D.ReadOnly == readOnly {
			n++
		}
	}
	return n
}

// The pane watching a session on another machine streams through the
// relay (no ssh attach runs); typing into it keeps the native attach.
func TestRelayPreviewWatchesWithoutSSH(t *testing.T) {
	h, rec := startRelay(t, 160, 40, nil, nil)
	selectSession(t, h, "ses_synth-a")
	h.WaitFor("relayed read-only view", 3*sec, func() bool {
		return len(h.Relays()) > 0 && strings.Contains(h.M.TermText("ses_synth-a"), "Welcome to Claude Code (synthetic)")
	})
	for _, r := range h.Relays() {
		if r.Fit || !r.D.ReadOnly || r.D.Mechanism != contracts.AttachWebTerminal {
			t.Fatalf("preview stream: fit=%v %+v", r.Fit, r.D)
		}
	}
	if n := nativeTerms(h, true); n != 0 {
		t.Fatalf("a read-only native (ssh) attach ran %d times", n)
	}
	if n := len(rec.Calls("AcquireLease")); n != 0 {
		t.Fatalf("watching took the input lease (%d)", n)
	}
	h.Key("enter") // focus: a writable native attach with the lease
	h.WaitFor("writable native attach", 3*sec, func() bool {
		f := termOf(h, "ses_synth-a")
		return f != nil && !f.D.ReadOnly && len(rec.Calls("AcquireLease")) == 1
	})
}

// Grid tiles of other machines' sessions are fitted relay streams: sized
// to the window by the node, following it, with no ssh attach and no
// window-size probe.
func TestRelayGridTilesFitted(t *testing.T) {
	h, rec := startRelay(t, 240, 65, nil, nil)
	openGrid(t, h)
	tiles := h.M.GridTiles()
	h.WaitFor("every tile live", 8*sec, func() bool { return h.M.GridLiveTiles() == len(tiles) })
	if n := len(h.Terms()); n != 0 {
		t.Fatalf("tiles ran %d native (ssh) attaches", n)
	}
	fitted := 0
	for _, r := range h.Relays() {
		if r.Fit && r.D.ReadOnly {
			fitted++
		}
	}
	if fitted != len(tiles) { // the others are the Sessions pane's preview
		t.Fatalf("%d fitted streams for %d tiles", fitted, len(tiles))
	}
	sid := tiles[0]
	if cmd := h.M.GridTileCommand(sid); len(cmd) == 0 || cmd[0] != "pyre-relay" {
		t.Fatalf("tile command: %q", cmd)
	}
	if c, r := h.M.GridTileSize(sid); c != tuitest.FitCols || r != tuitest.FitRows {
		t.Fatalf("tile emulator %dx%d, want the window's %dx%d", c, r, tuitest.FitCols, tuitest.FitRows)
	}
	// The node follows a resized window: the tile's emulator follows it.
	var last *tuitest.RelayTerm
	for _, r := range h.Relays() {
		if r.Fit && !r.Closed() {
			last = r
		}
	}
	last.SetWindow(100, 30)
	h.WaitFor("tile follows the window", 3*sec, func() bool {
		for _, s := range h.M.GridTiles() {
			if c, r := h.M.GridTileSize(s); c == 100 && r == 30 {
				return true
			}
		}
		return false
	})
	if n := len(rec.Calls("AcquireLease")); n != 0 {
		t.Fatalf("tiles took the input lease (%d)", n)
	}
	h.Key("G")
	h.WaitFor("tile streams closed", 3*sec, func() bool { return h.M.GridLiveTiles() == 0 })
}

// Without terminal.fit on the node (an older pyred), tiles keep the old
// path rather than risk resizing the window; previews still relay.
func TestRelayGridFallsBackWithoutFit(t *testing.T) {
	h, _ := startRelay(t, 240, 65, func(r *tuitest.Relayed) { r.NoNodeFit = true }, nil)
	openGrid(t, h)
	for _, r := range h.Relays() {
		if r.Fit {
			t.Fatalf("a fitted stream was asked of a node without %s", contracts.TerminalFit)
		}
	}
	if nativeTerms(h, true) == 0 {
		t.Fatal("tiles should use the native read-only attach")
	}
}

// The same when the hub does not forward fit (an older hub).
func TestRelayGridFallsBackWithoutHubFit(t *testing.T) {
	h, _ := startRelay(t, 240, 65, func(r *tuitest.Relayed) { r.NoHubFit = true }, nil)
	openGrid(t, h)
	for _, r := range h.Relays() {
		if r.Fit {
			t.Fatalf("a fitted stream was asked of a hub without %s", contracts.TerminalFit)
		}
	}
	if nativeTerms(h, true) == 0 {
		t.Fatal("tiles should use the native read-only attach")
	}
}

// SSHPreviews forces the old ssh path for previews and tiles.
func TestRelayForcedSSH(t *testing.T) {
	h, _ := startRelay(t, 160, 40, nil, func(o *tui.Options) { o.SSHPreviews = true })
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)
	if len(h.Relays()) != 0 {
		t.Fatalf("SSHPreviews still opened %d relay streams", len(h.Relays()))
	}
}

// A node whose relay refuses as unsupported is watched the old way.
func TestRelayUnsupportedFallsBack(t *testing.T) {
	h, _ := startRelay(t, 160, 40, nil, nil)
	h.SetRelayErr(contracts.Errorf(contracts.ErrUnsupported, "synthetic: no terminal bridge on this node"))
	n0 := len(h.Relays())
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)
	if n := len(h.Relays()); n != n0 {
		t.Fatalf("relay dials failed, yet %d new streams", n-n0)
	}
}

// noSSHNodes is a hub whose nodes have no ssh alias (Windows cloud VMs):
// every attach that does not ask for the relay is refused as the node's
// runtime refuses it.
type noSSHNodes struct{ contracts.API }

func (n noSSHNodes) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	if len(req.Preferred) == 0 || req.Preferred[0] != contracts.AttachWebTerminal {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnavailable, "remote attach needs an ssh alias for node node_synth-win (not configured)")
	}
	return n.API.AttachSession(ctx, req)
}

// Typing into a session on a node with no ssh route streams through the
// relay, writable, under the input lease (instead of "needs an ssh
// alias"); F (full screen, which would need ssh) types in the pane too.
func TestRelayTypingOnNodeWithoutSSH(t *testing.T) {
	var rec *tuitest.Recorder
	h, _ := start(t, 160, 40, func(a contracts.API) contracts.API {
		rec = tuitest.NewRecorder(noSSHNodes{tuitest.NewRelayed(tuitest.NewEmbeddable(a))})
		return rec
	}, func(o *tui.Options) { o.DialRelay = tuitest.FakeRelayDialer })
	selectSession(t, h, "ses_synth-a")
	h.WaitFor("relayed read-only view", 3*sec, func() bool { return len(h.Relays()) > 0 })

	writable := func() *tuitest.RelayTerm {
		rs := h.Relays()
		for i := len(rs) - 1; i >= 0; i-- {
			if !rs[i].D.ReadOnly && !rs[i].Closed() {
				return rs[i]
			}
		}
		return nil
	}
	h.Key("enter")
	h.WaitFor("writable relayed attach under the lease", 3*sec, func() bool {
		return writable() != nil && len(rec.Calls("AcquireLease")) == 1 && h.M.TermFocused()
	})
	if n := len(h.Terms()); n != 0 {
		t.Fatalf("a native (ssh) terminal ran %d times", n)
	}
	r := writable()
	if r.Fit || r.D.Mechanism != contracts.AttachWebTerminal {
		t.Fatalf("typing stream: fit=%v %+v", r.Fit, r.D)
	}
	h.Type("hi")
	h.WaitFor("keys reach the relayed stream", 3*sec, func() bool { return strings.Contains(r.Input(), "hi") })
	if strings.Contains(h.View(), "ssh alias") {
		t.Fatalf("the ssh refusal leaked to the screen:\n%s", h.View())
	}

	// Leaving the pane gives the lease back and closes the writable stream.
	h.Key("ctrl+]")
	h.WaitFor("lease released", 3*sec, func() bool { return len(rec.Calls("ReleaseLease")) == 1 && r.Closed() })

	// F: learnt that the node has no ssh, it types in the pane directly.
	before := len(rec.Calls("AttachSession"))
	h.Key("F")
	h.WaitFor("F types in the relayed pane", 3*sec, func() bool {
		return writable() != nil && h.M.TermFocused() && len(rec.Calls("AcquireLease")) == 2
	})
	for _, c := range rec.Calls("AttachSession")[before:] {
		req := c.Request.(contracts.AttachRequest)
		if len(req.Preferred) == 0 || req.Preferred[0] != contracts.AttachWebTerminal {
			t.Fatalf("F asked for a native attach again: %+v", req.Preferred)
		}
	}
	if n := len(h.Attaches()); n != 0 {
		t.Fatalf("F ran a full-screen attach: %+v", h.Attaches())
	}
}
