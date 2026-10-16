package termhost

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost/internal/wire"
)

// DialOptions selects the session to attach to and how.
type DialOptions struct {
	Socket string
	Name   string
	// ReadOnly clients never type and never resize the session.
	ReadOnly bool
	// IgnoreSize keeps a writable client from resizing the session.
	IgnoreSize bool
	// Cols, Rows: the client's terminal size (a writable client sizes the
	// session to it: the newest writer wins, like tmux window-size latest).
	Cols, Rows int
}

// Conn is an attached client stream: Read yields what the session draws
// (a repaint of the current screen first), Write types into it (dropped
// when read-only), Resize reports the client's size. When the session's
// size changes, Read returns (0, nil) once and TakeWindowSize reports the
// new size, so a viewer can resize its own emulator before the bytes drawn
// for that size (the contract of the TUI's WindowSizer and the relay's
// fitted streams). Close detaches; the session keeps running.
type Conn struct {
	c        net.Conn
	r        *bufio.Reader
	readOnly bool

	mu      sync.Mutex
	cols    int
	rows    int
	resized bool
	exit    string

	pending []byte
	rbuf    []byte
	wmu     sync.Mutex
}

// DialAttach attaches to a session.
func DialAttach(ctx context.Context, o DialOptions) (*Conn, error) {
	if !sessionhost.ValidName(o.Name) {
		return nil, fmt.Errorf("termhost: invalid session name %q", o.Name)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", o.Socket)
	if err != nil {
		return nil, fmt.Errorf("%w (%s: %v)", sessionhost.ErrNoServer, o.Socket, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	req := wire.Request{V: wire.ProtocolVersion, Op: wire.OpAttach, Name: o.Name, ReadOnly: o.ReadOnly, IgnoreSize: o.IgnoreSize,
		Cols: o.Cols, Rows: o.Rows, PID: os.Getpid()}
	if err := wire.WriteLine(c, req); err != nil {
		c.Close()
		return nil, fmt.Errorf("termhost attach: %w", err)
	}
	r := bufio.NewReaderSize(c, 64<<10)
	var resp wire.Response
	if err := wire.ReadLine(r, 1<<20, &resp); err != nil {
		c.Close()
		return nil, fmt.Errorf("termhost attach: %w", err)
	}
	if !resp.OK {
		c.Close()
		return nil, respErr(wire.OpAttach, resp)
	}
	_ = c.SetDeadline(time.Time{})
	return &Conn{c: c, r: r, readOnly: o.ReadOnly, cols: resp.Cols, rows: resp.Rows, resized: true}, nil
}

// Read returns session output. See Conn for the (0, nil) size signal.
func (c *Conn) Read(p []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	typ, b, err := wire.ReadFrame(c.r, c.rbuf)
	if err != nil {
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return 0, err
	}
	switch typ {
	case wire.FrameOutput:
		n := copy(p, b)
		if n < len(b) {
			c.pending = append(c.pending[:0], b[n:]...)
		}
		if cap(b) > cap(c.rbuf) {
			c.rbuf = b[:0]
		}
		return n, nil
	case wire.FrameSize:
		if cols, rows, ok := wire.ParseSize(b); ok {
			c.mu.Lock()
			c.cols, c.rows, c.resized = cols, rows, true
			c.mu.Unlock()
		}
		return 0, nil
	case wire.FrameExit:
		c.mu.Lock()
		c.exit = string(b)
		c.mu.Unlock()
		return 0, io.EOF
	}
	return 0, nil
}

// Write types p into the session; a read-only client's input is dropped.
func (c *Conn) Write(p []byte) (int, error) {
	if c.readOnly || len(p) == 0 {
		return len(p), nil
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	for off := 0; off < len(p); {
		end := min(len(p), off+wire.MaxFrameBytes)
		if err := wire.WriteFrame(c.c, wire.FrameInput, p[off:end]); err != nil {
			return off, err
		}
		off = end
	}
	return len(p), nil
}

// Resize reports the client's terminal size. The host applies it only for
// a writable client attached without IgnoreSize.
func (c *Conn) Resize(cols, rows int) error {
	if c.readOnly {
		return nil
	}
	cols, rows = wire.ClampDims(cols, rows)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return wire.WriteFrame(c.c, wire.FrameSize, wire.SizePayload(cols, rows))
}

// WindowSize is the session's current size.
func (c *Conn) WindowSize() (cols, rows int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cols, c.rows
}

// TakeWindowSize reports a size change once.
func (c *Conn) TakeWindowSize() (cols, rows int, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ok, c.resized = c.resized, false
	return c.cols, c.rows, ok
}

// ExitReason is why the host ended the stream ("" if it did not).
func (c *Conn) ExitReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exit
}

// Close detaches.
func (c *Conn) Close() error { return c.c.Close() }
