package tui

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
	rtmux "github.com/jhh3/pyre/internal/runtime/tmux"
)

// Same-machine fast path (owner report 2026-10-14: "tmux isn't slow when
// I switch between sessions"). A session whose run is on this machine
// lives in Pyre's own tmux server here, named by its run ID. Watching it
// used to ask the hub for an attach descriptor (sessions.attach) and a
// transcript tail with the last screen (sessions.read, hub → node →
// transcript parse) before anything was drawn: ~0.4–1.7 s per switch on a
// busy hub. Now the read-only view of such a session runs the same
// `tmux attach -r` the node would have described, without asking, and its
// first frame is `tmux capture-pane` of the pane (a few ms). The hub is
// used again for that run when anything local fails (no socket, no such
// tmux session, tmux missing): the old path, unchanged. Typing (the input
// lease and a writable attach) always goes through the hub, as before.
//
// The socket is Options.LocalTmuxSocket, or learnt from the first
// local_tmux descriptor the hub returns for a run on this machine.

const (
	localCaptureLimit = 2 * time.Second // capture-pane (proves the session exists)
	localBadFor       = 2 * time.Minute // a run whose local attach failed uses the hub this long
	localCaptureMax   = 256 << 10       // bytes of captured screen kept
)

// localTmux is this machine's Pyre tmux server as the TUI knows it
// (Update goroutine only).
type localTmux struct {
	bin, socket string
	looked      bool      // bin looked up on PATH
	opened      int       // read-only views opened locally (tests)
	statAt      time.Time // when the socket was last looked at (views ask every frame)
	statOK      bool
	bad         map[string]time.Time // run -> use the hub until
}

// localTarget is one run's tmux session on this machine.
type localTarget struct {
	bin, socket, name, node string
}

// localFor reports whether s's terminal can be watched straight from this
// machine's tmux server: the run is on this machine, the server's socket
// is known and present, and the run has not failed locally lately.
func (m *Model) localFor(s contracts.SessionView) (localTarget, bool) {
	if s.Run == nil || m.opts.ClientNodeID == "" || s.Run.NodeID != m.opts.ClientNodeID || runtime.GOOS == "windows" {
		return localTarget{}, false
	}
	lt := &m.local
	if lt.socket == "" {
		lt.socket = m.opts.LocalTmuxSocket
	}
	if lt.socket == "" || !rtmux.ValidName(s.Run.RunID) {
		return localTarget{}, false
	}
	if until, ok := lt.bad[s.Run.RunID]; ok {
		if time.Now().Before(until) {
			return localTarget{}, false
		}
		delete(lt.bad, s.Run.RunID)
	}
	if lt.bin == "" && !lt.looked {
		lt.looked = true
		if p, err := exec.LookPath("tmux"); err == nil {
			lt.bin = p
		}
	}
	if lt.bin == "" {
		return localTarget{}, false
	}
	if now := time.Now(); now.Sub(lt.statAt) > time.Second {
		fi, err := os.Stat(lt.socket)
		lt.statAt, lt.statOK = now, err == nil && fi.Mode()&os.ModeSocket != 0
	}
	if !lt.statOK {
		return localTarget{}, false
	}
	return localTarget{bin: lt.bin, socket: lt.socket, name: s.Run.RunID, node: s.Run.NodeID}, true
}

// localBad sends runID's views through the hub for a while.
func (m *Model) localBad(runID string) {
	if runID == "" {
		return
	}
	if m.local.bad == nil {
		m.local.bad = map[string]time.Time{}
	}
	m.local.bad[runID] = time.Now().Add(localBadFor)
}

// learnLocalTmux remembers this machine's tmux server from a local_tmux
// descriptor the hub returned for a run here (`tmux -u -S <socket> …
// attach-session -t =<run>`), when Options gave none.
func (m *Model) learnLocalTmux(s contracts.SessionView, d contracts.AttachDescriptor) {
	if m.local.socket != "" || m.opts.LocalTmuxSocket != "" || d.Mechanism != contracts.AttachLocalTmux ||
		s.Run == nil || m.opts.ClientNodeID == "" || s.Run.NodeID != m.opts.ClientNodeID || d.RunID != s.Run.RunID {
		return
	}
	t, ok := parseLocalAttach(d.Command)
	if !ok || t.target != rtmux.SessionTarget(s.Run.RunID) {
		return // not Pyre's naming: never guess
	}
	for i := 1; i+1 < t.glob; i++ {
		if t.argv[i] == "-S" && strings.HasPrefix(t.argv[i+1], "/") {
			m.local.socket, m.local.bin, m.local.statAt = t.argv[i+1], t.argv[0], time.Time{}
			return
		}
	}
}

// descriptor is the read-only attach the node would describe for a
// client on its own machine (runtime/tmux.BuildAttach).
func (t localTarget) descriptor() contracts.AttachDescriptor {
	return contracts.AttachDescriptor{
		RunID:     t.name,
		NodeID:    t.node,
		Mechanism: contracts.AttachLocalTmux,
		ReadOnly:  true,
		Command:   rtmux.AttachArgv(t.bin, t.socket, t.name, true),
		Notes:     rtmux.DetachNotes,
	}
}

// capture is the pane's visible screen (`capture-pane -p`, with -e its
// colours): bounded by the pane's height, and an error when the session
// does not exist on this server.
func (t localTarget) capture(ctx context.Context, colors bool) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, localCaptureLimit)
	defer cancel()
	args := []string{"-S", t.socket, "capture-pane", "-p", "-t", rtmux.PaneTarget(t.name)}
	if colors {
		args = append(args, "-e")
	}
	cmd := exec.CommandContext(cctx, t.bin, args...)
	cmd.Env = termEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	if len(out) > localCaptureMax {
		out = out[len(out)-localCaptureMax:]
	}
	return out, nil
}

// screenBytes turns a capture into what the emulator draws: the screen
// cleared, then the lines (clipped to cols, trailing blank lines dropped,
// the newest last) from the second row: the pane shows the emulator's
// bottom rows when its last row is not a tmux status line (render).
func screenBytes(capture []byte, cols int) []byte {
	lines := strings.Split(strings.TrimRight(string(bytes.ToValidUTF8(capture, nil)), "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	b.WriteString("\x1b[m\x1b[H\x1b[2J\x1b[2;1H")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(ansi.Truncate(l, cols, ""))
		b.WriteString("\x1b[m")
	}
	return []byte(b.String())
}

// seed draws a captured screen before the stream does (the stream's first
// output redraws over it).
func (t *termView) seed(capture []byte) {
	cols, _ := t.size()
	t.feed(screenBytes(capture, cols))
}

// LocalViews counts read-only views opened straight from this machine's
// tmux server, without the hub (tests).
func (m *Model) LocalViews() int { return m.local.opened }
