package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/jhh3/pyre/internal/contracts"
)

// Grid tiles: each tile is a read-only tmux client on a pty, emulated with
// the embedded terminal's machinery (termView). A tile must never change
// the agent's real window:
//
//   - the client attaches with `-r -f ignore-size` (tmux ≥ 3.2), so it is
//     ignored for sizing whenever any other (human) client is attached;
//   - tmux still sizes a window to an ignore-size client when it is the
//     only kind attached (verified on tmux 3.4: a 40x12 `-r -f ignore-size`
//     client shrank a 200x50 window to 40x11 and left it there after
//     detaching). So before attaching, the tile asks tmux for the window's
//     current size and makes its pty exactly that size: whatever tmux
//     decides, the window keeps its size. The emulator is window-sized and
//     the tile draws a crop of it (the rows that end at the last line with
//     content, from the left edge).
//
// A descriptor that is not a recognisable tmux attach (local or over ssh)
// is used as is at the tile size (synthetic terminals in tests and the
// demo); a tmux attach whose window size cannot be read is not attached at
// all (no guess that could shrink the window).

const (
	gridMaxTiles   = 12               // at most this many tile ptys
	gridFrameGap   = time.Second / 15 // tiles repaint at most 15 times a second
	gridPulseHalf  = time.Second      // needs-you border: 1 s bright, 1 s soft (0.5 Hz)
	gridActiveFor  = 2 * time.Second  // a working tile's indicator moves while output arrives
	gridRetryWait  = 15 * time.Second // after a failed or unavailable attach
	gridEndedWait  = 5 * time.Second  // after the attach command exited
	gridProbeLimit = 5 * time.Second  // window-size query
	gridProbeEvery = 10 * time.Second // live tiles re-read their window's size
	gridSizeFormat = "#{window_width} #{window_height} #{status} #{version}"
)

// gridTile is one tile's terminal (Update goroutine only, except lastOut).
type gridTile struct {
	sid, runID string
	tv         *termView // nil until the attach starts
	tok        int
	state      termState
	note       string // why there is no live view
	retryAt    time.Time
	argv       []string     // the attach command that runs (tests, info)
	probe      []string     // window-size query (nil: not a tmux attach)
	lastOut    atomic.Int64 // unix nanos of the last output (pty reader goroutine)
}

type gridFrameMsg struct{}
type gridPulseMsg struct{}
type gridSettleMsg struct{}
type gridProbeTickMsg struct{}

// gridSizeMsg is a live tile's window size, re-read periodically: a human
// may resize the window while the tile watches, and the tile client must
// match the window again before that human detaches.
type gridSizeMsg struct {
	sid        string
	tok        int
	cols, rows int
}

type gridAttachMsg struct {
	sid        string
	tok        int
	conn       TermConn
	cols, rows int
	argv       []string
	probe      []string
	note       string
	err        error
	relayOff   string // the node whose relay refused as unsupported (relay.go)
	localBad   string // the run whose local attach failed (localterm.go)
}

// gridRequest asks the hub for a read-only attach descriptor for s, sizes
// it safely (gridPrepare) and starts it, all off the Update goroutine. A
// session on another machine streams through the hub's relay, fitted to
// its window by the node (relay.go): no ssh connection and no probe.
func (m *Model) gridRequest(s contracts.SessionView, t *gridTile, cols, rows int) tea.Cmd {
	relay := m.relayTileFor(s)
	if relay && !m.relayOpenOK(s.Run.NodeID) {
		return m.gridRelayWait() // per-node open budget spent: try again shortly
	}
	t.tok++
	t.state, t.note = termConnecting, ""
	tok, sid := t.tok, s.Session.SessionID
	req := contracts.AttachRequest{
		Mutation:     contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		SessionID:    sid,
		ClientNodeID: m.opts.ClientNodeID,
		Preferred:    []contracts.AttachMechanism{contracts.AttachLocalTmux, contracts.AttachSSHTmux},
		ReadOnly:     true,
	}
	api, parent, timeout, start := m.api, m.ctx, m.opts.LoadTimeout, m.starter()
	dial, node, hello := m.opts.DialRelay, s.Run.NodeID, m.relays().hello
	local, isLocal := localTarget{}, false
	if !relay {
		local, isLocal = m.localFor(s) // this machine: no hub (localterm.go)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		msg := gridAttachMsg{sid: sid, tok: tok}
		if isLocal {
			// The window-size probe proves the session exists; anything
			// failing here asks the hub below, as before.
			if d, probe, c, r, err := gridPrepare(ctx, local.descriptor(), cols, rows); err == nil {
				if conn, err := start(d, c, r); err == nil {
					msg.conn, msg.cols, msg.rows, msg.argv, msg.probe = conn, c, r, d.Command, probe
					return msg
				}
			}
			msg.localBad = local.name
		}
		var d contracts.AttachDescriptor
		var err error
		answered := false
		if relay && hubFits(ctx, api, hello) {
			rreq := req
			rreq.Mutation = contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
			rreq.Preferred = append([]contracts.AttachMechanism{contracts.AttachWebTerminal}, req.Preferred...)
			d, err = api.AttachSession(ctx, rreq)
			answered = err != nil || d.Mechanism != contracts.AttachWebTerminal
			if !answered {
				dctx, dcancel := context.WithTimeout(parent, timeout)
				conn, derr := dial(dctx, d, cols, rows, true)
				dcancel()
				switch {
				case derr == nil:
					ws, ok := conn.(WindowSizer)
					if !ok {
						_ = conn.Close()
						msg.err = errors.New("the relayed tile is not sized to its window")
						return msg
					}
					msg.conn, msg.argv = conn, []string{"pyre-relay", d.URL}
					msg.cols, msg.rows = ws.WindowSize()
					return msg
				case relayUnsupported(derr):
					msg.relayOff = node // watch it the old way below
				default:
					msg.err = derr
					return msg
				}
			}
		}
		if !answered {
			d, err = api.AttachSession(ctx, req)
		}
		switch {
		case err != nil:
			msg.err = err
			return msg
		case !embedMechanism(d):
			msg.note = orDash(d.Notes)
			return msg
		case !d.ReadOnly:
			// A writable client would count as a human for delivery and
			// sizing; a tile never runs one.
			msg.note = "only a writable attach is offered from here"
			return msg
		}
		d, probe, c, r, err := gridPrepare(ctx, d, cols, rows)
		if err != nil {
			msg.err = err
			return msg
		}
		conn, err := start(d, c, r)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.conn, msg.cols, msg.rows, msg.argv, msg.probe = conn, c, r, d.Command, probe
		return msg
	}
}

// gridPrepare turns a read-only tmux attach into a tile attach: the client
// gets `-f ignore-size` and a pty the size of the window. Unrecognised
// commands run as they are at cols x rows.
func gridPrepare(ctx context.Context, d contracts.AttachDescriptor, cols, rows int) (contracts.AttachDescriptor, []string, int, int, error) {
	tc, ok := parseTmuxAttach(d.Command)
	if !ok {
		return d, nil, cols, rows, nil
	}
	probe := tc.probeCommand()
	w, h, ignoreSize, err := runProbe(ctx, probe)
	if err != nil {
		return d, nil, 0, 0, err
	}
	d.Command = tc.attachCommand(ignoreSize)
	return d, probe, w, h, nil
}

// runProbe runs a window-size query.
func runProbe(ctx context.Context, probe []string) (cols, rows int, ignoreSize bool, err error) {
	pctx, cancel := context.WithTimeout(ctx, gridProbeLimit)
	defer cancel()
	probe = sshMux(probe)
	cmd := exec.CommandContext(pctx, probe[0], probe[1:]...)
	cmd.Env = termEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, false, fmt.Errorf("cannot read the window size (%v); not attaching a tile that could resize it", err)
	}
	return parseWindowSize(string(out))
}

// gridReprobe re-reads every live tile's window size (every
// gridProbeEvery while the Grid shows tmux tiles).
func (m *Model) gridReprobe() tea.Cmd {
	var cmds []tea.Cmd
	parent := m.ctx
	for _, t := range m.grid.tiles {
		if t.tv == nil || t.probe == nil {
			continue
		}
		if live, _ := t.tv.live(); !live {
			continue
		}
		sid, tok, probe := t.sid, t.tok, t.probe
		cmds = append(cmds, func() tea.Msg {
			c, r, _, err := runProbe(parent, probe)
			if err != nil {
				return nil
			}
			return gridSizeMsg{sid: sid, tok: tok, cols: c, rows: r}
		})
	}
	return tea.Batch(cmds...)
}

// parseWindowSize reads gridSizeFormat's answer: the pty size that matches
// the window (status lines included) and whether the client flag
// ignore-size exists (tmux ≥ 3.2).
func parseWindowSize(out string) (cols, rows int, ignoreSize bool, err error) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) < 2 {
		return 0, 0, false, fmt.Errorf("unexpected window size %q", strings.TrimSpace(out))
	}
	cols, err1 := strconv.Atoi(f[0])
	rows, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || cols < 2 || rows < 1 {
		return 0, 0, false, fmt.Errorf("unexpected window size %q", strings.TrimSpace(out))
	}
	if len(f) > 2 {
		switch st := f[2]; st {
		case "off":
		case "on":
			rows++
		default:
			if n, err := strconv.Atoi(st); err == nil && n > 0 && n <= 5 {
				rows += n
			}
		}
	}
	if len(f) > 3 {
		ignoreSize = tmuxAtLeast(f[3], 3, 2)
	}
	return cols, rows, ignoreSize, nil
}

// tmuxAtLeast compares a tmux version ("3.4", "3.2a", "next-3.6").
func tmuxAtLeast(v string, major, minor int) bool {
	v = strings.TrimPrefix(v, "next-")
	maj, rest, _ := strings.Cut(v, ".")
	a, err := strconv.Atoi(maj)
	if err != nil {
		return false
	}
	digits := strings.TrimRightFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	b, _ := strconv.Atoi(digits)
	return a > major || a == major && b >= minor
}

// tmuxAttach is a parsed tmux attach command, local or over ssh.
type tmuxAttach struct {
	argv   []string // the tmux command line
	verb   int      // index of the attach verb in argv
	glob   int      // end of the binary and its global flags in argv
	target string
	ssh    []string // ssh argv before the remote command; nil for a local client
	wrap   []string // remote wrapper ("bash", "-lc"); nil when the remote command is tmux itself
}

// parseTmuxAttach recognises `tmux … [cmd … ;] attach-session -t T [-r]`
// and `ssh … "bash -lc '<that>'"` (what Pyre's descriptors run: the tmux
// runtime prefixes `set-option -g mouse on ;`).
func parseTmuxAttach(cmd []string) (tmuxAttach, bool) {
	if len(cmd) == 0 {
		return tmuxAttach{}, false
	}
	if filepath.Base(cmd[0]) != "ssh" {
		return parseLocalAttach(cmd)
	}
	if len(cmd) < 3 {
		return tmuxAttach{}, false
	}
	words, ok := shellWords(cmd[len(cmd)-1])
	if !ok {
		return tmuxAttach{}, false
	}
	var wrap []string
	if len(words) == 3 && (words[0] == "bash" || words[0] == "sh") && (words[1] == "-lc" || words[1] == "-c") {
		wrap = words[:2]
		if words, ok = shellWords(words[2]); !ok {
			return tmuxAttach{}, false
		}
	}
	t, ok := parseLocalAttach(words)
	if !ok {
		return tmuxAttach{}, false
	}
	t.ssh, t.wrap = append([]string(nil), cmd[:len(cmd)-1]...), wrap
	return t, true
}

func parseLocalAttach(argv []string) (tmuxAttach, bool) {
	if len(argv) < 3 || !strings.Contains(filepath.Base(argv[0]), "tmux") {
		return tmuxAttach{}, false
	}
	glob := 0
	for i := 1; i < len(argv); i++ {
		switch argv[i] {
		case "-S", "-L", "-f", "-c", "-T":
			i++ // global flag with a value
		case "attach-session", "attach", "a":
			if glob == 0 {
				glob = i
			}
			t := tmuxAttach{argv: argv, verb: i, glob: glob}
			for j := i + 1; j < len(argv)-1 && argv[j] != ";"; j++ {
				if argv[j] == "-t" {
					t.target = argv[j+1]
				}
			}
			return t, t.target != ""
		default:
			if strings.HasPrefix(argv[i], "-") && glob == 0 {
				continue
			}
			// Another command before the attach: skip to the next ";".
			if glob == 0 {
				glob = i
			}
			for i < len(argv) && argv[i] != ";" {
				i++
			}
			if i >= len(argv) {
				return tmuxAttach{}, false // no attach
			}
		}
	}
	return tmuxAttach{}, false
}

// attachCommand is the tile's client: read-only, and ignore-size when the
// server knows the flag. Other attach flags of the original are dropped
// (never -d: that would detach the human).
func (t tmuxAttach) attachCommand(ignoreSize bool) []string {
	tail := []string{"attach-session", "-t", t.target, "-r"}
	if ignoreSize {
		tail = append(tail, "-f", "ignore-size")
	}
	return t.command(tail, true)
}

// probeCommand asks the same server for the window's size.
func (t tmuxAttach) probeCommand() []string {
	return t.command([]string{"display-message", "-p", "-t", t.target, gridSizeFormat}, false)
}

func (t tmuxAttach) command(tail []string, tty bool) []string {
	glob := t.glob
	if glob == 0 {
		glob = t.verb
	}
	argv := append(append([]string(nil), t.argv[:glob]...), tail...)
	if t.ssh == nil {
		return argv
	}
	remote := shellJoinArgs(argv)
	if t.wrap != nil {
		remote = strings.Join(t.wrap, " ") + " " + shellQuoteArg(remote)
	}
	var out []string
	for _, a := range t.ssh {
		if !tty && (a == "-t" || a == "-tt") {
			continue
		}
		out = append(out, a)
	}
	if !tty {
		out = append(out[:1], append([]string{"-o", "BatchMode=yes"}, out[1:]...)...)
	}
	return append(out, remote)
}

// shellWords splits a POSIX shell word list (single and double quotes,
// backslash escapes); false on unbalanced quotes.
func shellWords(s string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			in = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, false
			}
			in = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			in = true
		case c == ' ' || c == '\t' || c == '\n':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out, true
}

func shellQuoteArg(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func shellJoinArgs(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuoteArg(a)
	}
	return strings.Join(q, " ")
}

// onGridAttach installs a started tile client (or records why not).
func (m *Model) onGridAttach(msg gridAttachMsg) tea.Cmd {
	if msg.relayOff != "" {
		m.relayRefused(msg.relayOff)
	}
	m.localBad(msg.localBad)
	t := m.grid.tiles[msg.sid]
	if t == nil || t.tok != msg.tok || m.view != viewGrid {
		if msg.conn != nil {
			_ = msg.conn.Close() // the tile went away meanwhile
		}
		return nil
	}
	switch {
	case msg.err != nil:
		t.state, t.note, t.retryAt = termFailed, m.humanIDs(errText(msg.err)), time.Now().Add(gridRetryWait)
		return nil
	case msg.conn == nil:
		t.state, t.note, t.retryAt = termUnavailable, msg.note, time.Now().Add(gridRetryWait)
		return nil
	}
	if t.tv != nil {
		t.tv.close()
	}
	notify := m.grid.notify
	t.tv = newTermView(t.sid, t.runID, msg.cols, msg.rows, func() {
		t.lastOut.Store(time.Now().UnixNano())
		select {
		case notify <- struct{}{}:
		default:
		}
	})
	t.tv.setConn(msg.conn, termReadOnly)
	t.state, t.note, t.argv, t.probe = termLive, "", msg.argv, msg.probe
	return m.gridWait()
}

// gridRelayWait schedules one retry tick for tiles held back by the
// per-node relay open budget.
func (m *Model) gridRelayWait() tea.Cmd {
	if m.grid.relayWaiting {
		return nil
	}
	m.grid.relayWaiting = true
	return gridRelayRetryTick()
}

// gridWait waits off the Update goroutine for a tile to draw, no sooner
// than one frame after the last: at most 15 frames a second, nothing when
// no tile changes.
func (m *Model) gridWait() tea.Cmd {
	g := &m.grid
	if g.waiting || len(g.tiles) == 0 {
		return nil
	}
	g.waiting = true
	next, notify, done := g.lastFrame.Add(gridFrameGap), g.notify, m.ctx.Done()
	return func() tea.Msg {
		select {
		case <-notify:
		case <-done:
			return nil
		}
		if d := time.Until(next); d > 0 {
			select {
			case <-time.After(d):
			case <-done:
				return nil
			}
		}
		return gridFrameMsg{}
	}
}

// closeGridTiles ends every tile client (leaving the view, zooming, quit).
func (m *Model) closeGridTiles() {
	for sid, t := range m.grid.tiles {
		if t.tv != nil {
			t.tv.close()
		}
		delete(m.grid.tiles, sid)
	}
}

// GridLiveTiles is how many tile clients are running (tests).
func (m *Model) GridLiveTiles() int {
	n := 0
	for _, t := range m.grid.tiles {
		if t.tv != nil {
			if ok, _ := t.tv.live(); ok {
				n++
			}
		}
	}
	return n
}

// GridTileCommand is the attach command a tile runs ("" when none; tests).
func (m *Model) GridTileCommand(sid string) []string {
	if t := m.grid.tiles[sid]; t != nil {
		return append([]string(nil), t.argv...)
	}
	return nil
}

// GridTileText is the plain text of a tile's emulator (tests).
func (m *Model) GridTileText(sid string) string {
	if t := m.grid.tiles[sid]; t != nil && t.tv != nil {
		return t.tv.text()
	}
	return ""
}

// cropRender draws w x h cells of the emulator without resizing it: from
// the left edge, the h rows that end at the last row with content (tmux's
// status line excluded), so a tall window shows its newest lines.
func (t *termView) cropRender(w, h int, plain bool) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	first, last := 0, t.rows-1
	if t.isStatusRow(last) {
		last--
	} else if t.isStatusRow(0) {
		first = 1
	}
	bottom := first
	for y := last; y >= first; y-- {
		if !t.blankRow(y) {
			bottom = y
			break
		}
	}
	if cy := t.em.CursorPosition().Y; !t.cursorOff && cy > bottom && cy <= last {
		bottom = cy
	}
	top := max(first, bottom-h+1)
	out := make([]string, 0, h)
	line := make(uv.Line, w)
	for y := top; y < top+h; y++ {
		if y > last {
			out = append(out, "")
			continue
		}
		for x := 0; x < w; x++ {
			c := t.em.CellAt(x, y)
			if c == nil || x >= t.cols {
				line[x] = uv.EmptyCell
				continue
			}
			line[x] = *c
			if c.Width > 1 && x+c.Width > w {
				line[x] = uv.EmptyCell // a wide character cut by the tile edge
			}
			if plain {
				line[x].Style, line[x].Link = uv.Style{}, uv.Link{}
			}
		}
		out = append(out, line.Render())
	}
	return out
}

// blankRow reports whether row y has no visible content. Caller holds mu.
func (t *termView) blankRow(y int) bool {
	for x := 0; x < t.cols; x++ {
		c := t.em.CellAt(x, y)
		if c == nil {
			continue
		}
		if strings.TrimSpace(c.Content) != "" || c.Style.Bg != nil {
			return false
		}
	}
	return true
}

// GridTileSize is a tile emulator's (and pty's) size (tests).
func (m *Model) GridTileSize(sid string) (cols, rows int) {
	if t := m.grid.tiles[sid]; t != nil && t.tv != nil {
		return t.tv.size()
	}
	return 0, 0
}
