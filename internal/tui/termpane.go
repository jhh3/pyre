package tui

import (
	"errors"
	"image/color"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// The embedded terminal: a session's tmux client runs on a pty and its
// output is emulated (charmbracelet/x/vt) into a cell grid that the
// Sessions view draws as the right-hand pane. One termView per session;
// its emulator survives connection swaps (read-only view ⇄ interactive
// attach), so the screen never blanks while one client replaces another.

type termMode int

const (
	termReadOnly    termMode = iota // `tmux attach -r`: watching never takes the input lease
	termInteractive                 // writable attach while the pane has focus (input lease held)
)

type termState int

const (
	termIdle        termState = iota // no connection
	termConnecting                   // attach requested
	termLive                         // the attach command is running
	termEnded                        // the attach command exited
	termFailed                       // the attach could not start
	termUnavailable                  // the run offers no embeddable terminal here
	termStarting                     // the run is still starting: retried until it is live
)

// termView is one session's embedded terminal. Fields under mu are shared
// with the pty reader goroutine; the rest belong to the Update goroutine.
type termView struct {
	sid, runID string

	mu        sync.Mutex
	em        *vt.Emulator
	cols      int // emulator (and pty) size
	rows      int
	conn      TermConn
	gen       int // bumped on every connection swap; stale readers drop output
	mode      termMode
	state     termState
	err       string
	drawn     bool       // the current emulator has received output
	appCursor bool       // DECCKM, as set by the client in the pane
	bracketed bool       // bracketed paste enabled
	cursorOff bool       // DECTCEM reset
	mouse     mouseModes // mouse tracking the client in the pane enabled
	top       int        // emulator row drawn as the pane's first row
	sel       cellSel    // mouse selection (emulator cells; termselect.go)

	in     chan []byte   // bytes for the pty (keys, paste, emulator replies)
	done   chan struct{} // closed by close()
	emDone chan struct{} // closed when the emulator reply reader exits
	poke   func()        // tells the model a new frame is ready

	// Update goroutine only.
	lastUse          time.Time
	reqTok           int
	reqMode          termMode // the attach wanted: interactive while the pane has focus
	leaseID          string
	unavailableUntil time.Time
	pending          [][]byte  // typed while the interactive attach was starting
	holdUntil        time.Time // writable attach kept for the wheel until then (pane not focused)
	holdBlockedUntil time.Time // a wheel hold was refused: no new try before then
	note             string    // why the terminal is unavailable

	// Input lease bookkeeping (embed_lease.go).
	leaseAt     time.Time     // hub time our lease was acquired (a newer one is someone else's)
	lastKey     time.Time     // last key, paste or mouse input sent while focused
	leaseIdle   bool          // lease released after leaseIdle without keys; re-taken on the next key
	retaking    bool          // a re-take after idleness is in flight
	askTakeover string        // the owner's other client holding the lease: enter takes over
	askUntil    time.Time     // the takeover offer stands until then
	takeover    bool          // the next interactive request takes the lease over
	focusNote   string        // why focus could not be taken (shown inline), until focus is retried
	startWait   time.Duration // backoff while the run is starting
	startState  string        // the run state seen when the attach was refused as starting
}

func newTermView(sid, runID string, cols, rows int, poke func()) *termView {
	cols, rows = max(cols, 2), max(rows, 2)
	t := &termView{sid: sid, runID: runID, cols: cols, rows: rows, in: make(chan []byte, 512), done: make(chan struct{}), emDone: make(chan struct{}), poke: poke}
	if t.poke == nil {
		t.poke = func() {}
	}
	t.em = t.newEmulator(cols, rows)
	go t.writeLoop()
	return t
}

// newEmulator builds an emulator whose replies (device attributes, cursor
// reports, colour queries) flow to the pty, and whose mode changes are
// mirrored for key encoding. Callbacks run inside em.Write, under t.mu.
func (t *termView) newEmulator(cols, rows int) *vt.Emulator {
	em := vt.NewEmulator(cols, rows)
	em.SetScrollbackSize(0) // tmux keeps history; copy-mode scrolls it
	em.SetCallbacks(vt.Callbacks{
		EnableMode:       func(m ansi.Mode) { t.setMode(m, true) },
		DisableMode:      func(m ansi.Mode) { t.setMode(m, false) },
		CursorVisibility: func(v bool) { t.cursorOff = !v },
	})
	go func() {
		defer close(t.emDone)
		buf := make([]byte, 4096)
		for {
			n, err := em.Read(buf)
			if n > 0 {
				b := append([]byte(nil), buf[:n]...)
				select {
				case t.in <- b:
				default: // never block the emulator (it holds t.mu)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return em
}

func (t *termView) setMode(m ansi.Mode, on bool) {
	switch m {
	case ansi.ModeCursorKeys:
		t.appCursor = on
	case ansi.ModeBracketedPaste:
		t.bracketed = on
	case ansi.ModeTextCursorEnable:
		t.cursorOff = !on
	default:
		t.mouse.set(m, on)
	}
}

func (t *termView) writeLoop() {
	for {
		select {
		case <-t.done:
			return
		case b := <-t.in:
			t.mu.Lock()
			c := t.conn
			t.mu.Unlock()
			if c != nil {
				_, _ = c.Write(b)
			}
		}
	}
}

// feed writes terminal output into the emulator (tests and the reader).
func (t *termView) feed(b []byte) {
	t.mu.Lock()
	_, _ = t.em.Write(b)
	t.drawn = true
	t.mu.Unlock()
}

// setConn makes c the live connection (closing the previous one) and
// starts copying its output into the emulator.
func (t *termView) setConn(c TermConn, mode termMode) {
	t.mu.Lock()
	t.gen++
	gen := t.gen
	old := t.conn
	t.conn, t.mode, t.state, t.err = c, mode, termLive, ""
	t.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	go t.readLoop(c, gen)
}

func (t *termView) readLoop(c TermConn, gen int) {
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			t.mu.Lock()
			if t.gen == gen {
				_, _ = t.em.Write(buf[:n])
				t.drawn = true
			}
			t.mu.Unlock()
			t.poke()
		}
		if err != nil {
			t.mu.Lock()
			if t.gen == gen {
				t.conn = nil
				t.state = termEnded
				if !errors.Is(err, io.EOF) && !isPtyEOF(err) {
					t.err = err.Error()
				}
			}
			t.mu.Unlock()
			t.poke()
			return
		}
	}
}

// isPtyEOF: reading a pty master whose slave side closed fails with EIO.
func isPtyEOF(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "input/output error") || strings.Contains(err.Error(), "file already closed"))
}

// closeConn ends the live connection, keeping the emulator's screen.
func (t *termView) closeConn() {
	t.mu.Lock()
	t.gen++
	old := t.conn
	t.conn = nil
	if t.state == termLive || t.state == termConnecting {
		t.state = termIdle
	}
	t.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (t *termView) close() {
	t.closeConn()
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	// vt.Emulator.Close sets an unsynchronised flag that Emulator.Read
	// checks, so closing while the reply reader is inside Read is a data
	// race. Close the reply pipe first (under mu, so no em.Write is
	// mid-reply), wait for the reader to see EOF and exit, then close.
	t.mu.Lock()
	pipe, ok := t.em.InputPipe().(io.Closer)
	if ok {
		_ = pipe.Close()
	}
	t.mu.Unlock()
	if ok {
		<-t.emDone
	}
	t.mu.Lock()
	_ = t.em.Close()
	t.mu.Unlock()
}

// live reports whether a connection is running, and in which mode.
func (t *termView) live() (bool, termMode) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn != nil, t.mode
}

func (t *termView) status() (termState, string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state, t.err, t.drawn
}

func (t *termView) setState(s termState, err string) {
	t.mu.Lock()
	t.state, t.err = s, err
	t.mu.Unlock()
}

// send queues bytes for the pty without blocking; false when the queue is
// full (the command is not reading).
func (t *termView) send(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	select {
	case t.in <- b:
		return true
	default:
		return false
	}
}

// mouseState returns the mouse modes and the emulator row the pane draws
// first (pane row r is emulator row top+r).
func (t *termView) mouseState() (mouseModes, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mouse, t.top
}

func (t *termView) keyModes() (appCursor, bracketed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.appCursor, t.bracketed
}

// resize sets the emulator and pty to cols x rows (no-op when unchanged).
func (t *termView) resize(cols, rows int) {
	cols, rows = max(cols, 2), max(rows, 2)
	t.mu.Lock()
	if cols == t.cols && rows == t.rows {
		t.mu.Unlock()
		return
	}
	t.cols, t.rows = cols, rows
	t.em.Resize(cols, rows)
	c := t.conn
	t.mu.Unlock()
	if c != nil {
		_ = c.Resize(cols, rows)
	}
}

func (t *termView) size() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows
}

// statusRows is how many rows the pane hides: the pty is one row taller
// than the pane so tmux's status line can sit below it, out of sight.
const statusRows = 1

// render draws the pane: w x h cells of the emulator. The emulator is
// sized w x h+statusRows; when its last row is tmux's status line (one
// uniform background) the rows above it are shown, otherwise the bottom h
// rows. cursor draws the cursor (reverse video) when the pane has focus;
// plain drops colours and attributes (NO_COLOR, tests).
func (t *termView) render(w, h int, cursor, plain bool) []string {
	t.resize(w, h+statusRows)
	t.mu.Lock()
	defer t.mu.Unlock()
	top := 0
	if !t.isStatusRow(t.rows - 1) {
		top = t.rows - h
	}
	t.top = top
	cx, cy := -1, -1
	if cursor && !t.cursorOff {
		p := t.em.CursorPosition()
		cx, cy = p.X, p.Y
	}
	out := make([]string, 0, h)
	line := make(uv.Line, w)
	for y := top; y < top+h && y < t.rows; y++ {
		for x := 0; x < w; x++ {
			c := t.em.CellAt(x, y)
			if c == nil {
				line[x] = uv.EmptyCell
				continue
			}
			line[x] = *c
			if plain {
				line[x].Style = uv.Style{}
				line[x].Link = uv.Link{}
			}
			if t.sel.contains(x, y) {
				if plain {
					line[x].Style.Attrs |= uv.AttrReverse
				} else {
					line[x].Style.Attrs ^= uv.AttrReverse
				}
			}
			if x == cx && y == cy {
				if plain {
					line[x].Style.Attrs |= uv.AttrReverse
				} else {
					line[x].Style.Attrs ^= uv.AttrReverse
				}
				if line[x].Content == "" || line[x].Width == 0 {
					line[x].Content, line[x].Width = " ", 1
				}
			}
		}
		out = append(out, line.Render())
	}
	return out
}

// isStatusRow reports whether row y looks like tmux's status line: every
// cell has the same explicit background colour. Caller holds mu.
func (t *termView) isStatusRow(y int) bool {
	if y < 0 || y >= t.rows {
		return false
	}
	first := t.em.CellAt(0, y)
	if first == nil || first.Style.Bg == nil {
		return false
	}
	for x := 1; x < t.cols; x++ {
		c := t.em.CellAt(x, y)
		if c == nil {
			continue
		}
		if c.Width == 0 && c.Content == "" {
			continue // wide-char placeholder
		}
		if !sameColor(c.Style.Bg, first.Style.Bg) {
			return false
		}
	}
	return true
}

func sameColor(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == b
	}
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

var copyModeRe = regexp.MustCompile(`\[\d+/\d+\]`)

// inCopyMode reports whether tmux shows its copy-mode position indicator
// ("[12/340]") at the top right of the pane.
func (t *termView) inCopyMode() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	for x := max(0, t.cols-40); x < t.cols; x++ {
		if c := t.em.CellAt(x, 0); c != nil {
			b.WriteString(c.Content)
		}
	}
	return copyModeRe.MatchString(b.String())
}

var copyTopRe = regexp.MustCompile(`\[0/\d+\]`)

// copyAtBottom reports whether copy-mode is scrolled to the newest line.
func (t *termView) copyAtBottom() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	for x := max(0, t.cols-40); x < t.cols; x++ {
		if c := t.em.CellAt(x, 0); c != nil {
			b.WriteString(c.Content)
		}
	}
	return copyTopRe.MatchString(b.String())
}

// text returns the visible screen as plain text (tests, diagnostics).
func (t *termView) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.em.String()
}
