package hostproc

import (
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost/internal/wire"
)

// session is one named session: a program on a pseudo-terminal, its
// emulated screen, and the clients attached to it. Like a tmux pane with
// remain-on-exit, it stays (dead, with the exit status) after the program
// ends, until it is killed or respawned.
type session struct {
	srv    *Server
	name   string
	paneID string

	mu      sync.Mutex
	p       proc // nil once killed
	gen     int  // bumped per spawn; goroutines of an older program ignore themselves
	argv0   string
	scr     *screen
	cols    int
	rows    int
	dead    bool
	exit    *int
	clients map[*attachClient]bool
	closed  bool

	inMu    sync.Mutex   // serialises writes to the program's input
	writers atomic.Int32 // attached writable clients
	replies chan []byte  // emulator replies, sent to the program while no writable client is attached
	done    chan struct{}
	nextCID int
}

func newSession(srv *Server, name, paneID string, cols, rows int) *session {
	s := &session{srv: srv, name: name, paneID: paneID, cols: cols, rows: rows,
		clients: map[*attachClient]bool{}, replies: make(chan []byte, 64), done: make(chan struct{})}
	s.scr = newScreen(cols, rows, s.queueReply)
	go s.replyLoop()
	return s
}

// queueReply never blocks: it runs on the emulator's reply reader while
// the emulator (and s.mu) may be busy.
func (s *session) queueReply(b []byte) {
	select {
	case s.replies <- b:
	case <-s.done:
	default:
	}
}

// replyLoop answers the program's terminal queries from the emulator. A
// writable attached client's own terminal answers instead (its input is
// passed through), so exactly one answer reaches the program.
func (s *session) replyLoop() {
	for {
		select {
		case <-s.done:
			return
		case b := <-s.replies:
			if s.writers.Load() == 0 {
				_ = s.writeInput(b)
			}
		}
	}
}

// spawn starts the session's program; the caller holds s.mu.
func (s *session) spawnLocked(spec *wire.Spec) error {
	env := childEnv(s.srv.baseEnv(), spec.Env, spec.Unset)
	p, err := s.srv.start(spec.Argv, spec.Cwd, env, s.cols, s.rows)
	if err != nil {
		return err
	}
	s.gen++
	s.p, s.argv0, s.dead, s.exit = p, spec.Argv[0], false, nil
	go s.readLoop(p)
	go s.waitLoop(p, s.gen)
	return nil
}

func (s *session) readLoop(p proc) {
	buf := make([]byte, 32<<10)
	for {
		n, err := p.Read(buf)
		if n > 0 {
			b := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			if !s.closed {
				s.scr.write(b)
				s.broadcastLocked(wire.FrameOutput, b)
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *session) waitLoop(p proc, gen int) {
	exit := p.Wait()
	p.Exited()
	s.mu.Lock()
	if s.gen == gen && !s.closed {
		s.dead, s.exit = true, exit
	}
	s.mu.Unlock()
	code := "signal"
	if exit != nil {
		code = strconv.Itoa(*exit)
	}
	s.srv.logf("session %s: program exited (%s)", s.name, code)
}

func (s *session) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p != nil && !s.dead
}

// writeInput types b into the program.
func (s *session) writeInput(b []byte) error {
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	if p == nil {
		return sessionhost.ErrNoSession
	}
	s.inMu.Lock()
	defer s.inMu.Unlock()
	for len(b) > 0 {
		n, err := p.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// pasteBytes renders text like `tmux paste-buffer -p`: LF becomes CR, and
// the text is bracketed when the program enabled bracketed paste.
func (s *session) pasteBytes(text []byte) []byte {
	t := strings.ReplaceAll(string(text), "\n", "\r")
	s.mu.Lock()
	bracketed := s.scr.mode(ansi.ModeBracketedPaste)
	s.mu.Unlock()
	if bracketed {
		t = ansi.BracketedPasteStart + t + ansi.BracketedPasteEnd
	}
	return []byte(t)
}

// keyBytes encodes named keys, honouring application cursor-key mode.
func (s *session) keyBytes(keys []string) ([]byte, error) {
	s.mu.Lock()
	app := s.scr.mode(ansi.ModeCursorKeys)
	s.mu.Unlock()
	var b []byte
	for _, k := range keys {
		seq, ok := encodeKey(sessionhost.Key(k), app)
		if !ok {
			return nil, errors.New("termhost: key " + strconv.Quote(k) + " is not an allowed named key")
		}
		b = append(b, seq...)
	}
	return b, nil
}

func encodeKey(k sessionhost.Key, app bool) (string, bool) {
	csi := "\x1b["
	if app {
		csi = "\x1bO"
	}
	switch k {
	case sessionhost.KeyEnter:
		return "\r", true
	case sessionhost.KeyEscape:
		return "\x1b", true
	case sessionhost.KeyCtrlU:
		return "\x15", true
	case sessionhost.KeyCtrlC:
		return "\x03", true
	case sessionhost.KeyCtrlD:
		return "\x04", true
	case sessionhost.KeyBackspace:
		return "\x7f", true
	case sessionhost.KeyUp:
		return csi + "A", true
	case sessionhost.KeyDown:
		return csi + "B", true
	case sessionhost.KeyTab:
		return "\t", true
	case sessionhost.KeyEnd:
		return csi + "F", true
	}
	return "", false
}

// resizeLocked sets the session size and tells every client.
func (s *session) resizeLocked(cols, rows int) {
	cols, rows = wire.ClampDims(cols, rows)
	if cols == s.cols && rows == s.rows {
		return
	}
	s.cols, s.rows = cols, rows
	s.scr.resize(cols, rows)
	if s.p != nil && !s.dead {
		_ = s.p.Resize(cols, rows)
	}
	s.broadcastLocked(wire.FrameSize, wire.SizePayload(cols, rows))
}

func (s *session) resize(cols, rows int) {
	s.mu.Lock()
	s.resizeLocked(cols, rows)
	s.mu.Unlock()
}

func (s *session) broadcastLocked(typ byte, payload []byte) {
	for c := range s.clients {
		c.send(typ, payload)
	}
}

func (s *session) pane() *wire.Pane {
	s.mu.Lock()
	defer s.mu.Unlock()
	pw := &wire.Pane{ID: s.paneID, Dead: s.dead || s.p == nil, Exit: s.exit, Width: s.cols, Height: s.rows}
	if s.p != nil {
		pw.PID = s.p.Pid()
	}
	if s.argv0 != "" {
		base := filepath.Base(strings.ReplaceAll(s.argv0, `\`, "/"))
		pw.Command = strings.TrimSuffix(base, filepath.Ext(base))
	}
	return pw
}

func (s *session) listClients() []wire.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]wire.Client, 0, len(s.clients))
	for c := range s.clients {
		cw := wire.Client{ID: c.id, PID: c.pid, ReadOnly: c.readOnly}
		var flags []string
		if c.readOnly {
			flags = append(flags, "read-only")
		}
		if c.ignoreSize {
			flags = append(flags, "ignore-size")
		}
		cw.Flags = strings.Join(flags, ",")
		out = append(out, cw)
	}
	return out
}

// kill ends the program at once and closes the session.
func (s *session) kill(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	p := s.p
	s.p = nil
	clients := s.clients
	s.clients = map[*attachClient]bool{}
	s.mu.Unlock()
	if p != nil {
		p.Kill()
		_ = p.Close()
	}
	for c := range clients {
		c.send(wire.FrameExit, []byte(reason))
		c.closeAfterFlush()
	}
	close(s.done)
	s.mu.Lock()
	s.scr.close()
	s.mu.Unlock()
}

// terminate ends the program's tree gracefully (forcefully after grace),
// then closes the session.
func (s *session) terminate(grace time.Duration) {
	s.mu.Lock()
	p, live := s.p, s.p != nil && !s.dead
	s.mu.Unlock()
	if live {
		p.Terminate(grace)
	}
	s.kill("session terminated")
}

// ---- attached clients ----

// attachClient is one attached terminal client. Frames go out through a
// bounded queue; a client that cannot keep up is disconnected (it can
// re-attach and gets a fresh repaint) rather than stalling the session.
type attachClient struct {
	id         string
	pid        int
	readOnly   bool
	ignoreSize bool
	conn       net.Conn

	mu     sync.Mutex
	out    chan outFrame
	closed bool
	done   chan struct{}
}

type outFrame struct {
	typ     byte
	payload []byte
}

const clientQueue = 1024

func newAttachClient(id string, conn net.Conn, req wire.Request) *attachClient {
	return &attachClient{id: id, pid: req.PID, readOnly: req.ReadOnly, ignoreSize: req.IgnoreSize || req.ReadOnly,
		conn: conn, out: make(chan outFrame, clientQueue), done: make(chan struct{})}
}

func (c *attachClient) send(typ byte, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.out <- outFrame{typ, payload}:
	default:
		c.closed = true
		close(c.out) // the writer drains what is queued, then drops the client
		_ = c.conn.SetReadDeadline(time.Now())
	}
}

// closeAfterFlush ends the client once queued frames are written.
func (c *attachClient) closeAfterFlush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.out)
	}
}

func (c *attachClient) writeLoop() {
	defer close(c.done)
	defer c.conn.Close()
	for f := range c.out {
		_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := wire.WriteFrame(c.conn, f.typ, f.payload); err != nil {
			go func() {
				for range c.out {
				}
			}()
			return
		}
	}
}
