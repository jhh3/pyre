package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
)

// ClientConn is a client's end of a relayed terminal stream (the TUI's
// previews and Grid tiles): Read yields terminal bytes, Write sends input
// (dropped for a read-only stream), Resize asks for a new size (ignored
// for a fitted stream, which the node sizes to the window) and Close
// detaches; the run keeps going.
type ClientConn struct {
	ws       *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	readOnly bool
	fit      bool
	once     sync.Once
	closing  atomic.Bool // Close was called: a read error is the detach

	buf     []byte // Read goroutine only
	lastErr *contracts.Error

	mu         sync.Mutex
	cols, rows int  // the window's size (fitted streams)
	resized    bool // a size change waits for TakeWindowSize
}

// DialOptions configures Dial.
type DialOptions struct {
	// HTTPClient carries the pinned TLS configuration (required for wss).
	HTTPClient *http.Client
	Cols, Rows int
	ReadOnly   bool // the descriptor is read-only: input is never sent
	Fit        bool // read-only only: the node sizes the stream to the window
	// FirstFrame bounds the wait for the node's first frame (default 25s:
	// the relay waits up to 15s for the node to dial back).
	FirstFrame time.Duration
	// ReadLimit is the largest frame accepted (default 1 MiB).
	ReadLimit int64
}

// Dial opens PathTerminal at wsURL with a one-time ticket and waits for
// the stream's first frame, so a refusal (unsupported, capacity, expired
// ticket, node unreachable) is returned here as the *contracts.Error the
// relay sent. A fitted stream's first frame must be the window size;
// anything else means the hub or node did not fit it, and the stream is
// closed at once. Never log ticket.
func Dial(ctx context.Context, wsURL, ticket string, o DialOptions) (*ClientConn, error) {
	if o.FirstFrame <= 0 {
		o.FirstFrame = 25 * time.Second
	}
	if o.ReadLimit <= 0 {
		o.ReadLimit = 1 << 20
	}
	if o.Fit && !o.ReadOnly {
		return nil, contracts.Errorf(contracts.ErrInvalid, "fit is only for read-only streams")
	}
	dctx, dcancel := context.WithTimeout(ctx, o.FirstFrame)
	defer dcancel()
	ws, resp, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{HTTPClient: o.HTTPClient, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
		if resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed) {
			return nil, contracts.Errorf(contracts.ErrUnsupported, "this endpoint has no terminal relay (HTTP %d)", resp.StatusCode)
		}
		return nil, contracts.Errorf(contracts.ErrUnavailable, "terminal relay: %v", err)
	}
	ws.SetReadLimit(o.ReadLimit)
	cols, rows := ClampSize(o.Cols, o.Rows)
	auth, _ := json.Marshal(Frame{Type: FrameAuth, Ticket: ticket, Cols: cols, Rows: rows, Fit: o.Fit})
	if err := ws.Write(dctx, websocket.MessageText, auth); err != nil {
		ws.CloseNow()
		return nil, contracts.Errorf(contracts.ErrUnavailable, "terminal relay: %v", err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	c := &ClientConn{ws: ws, ctx: cctx, cancel: cancel, readOnly: o.ReadOnly, fit: o.Fit}
	for {
		typ, b, err := ws.Read(dctx)
		if err != nil {
			c.closeNow()
			if c.lastErr != nil {
				return nil, c.lastErr
			}
			if ctx.Err() == nil && dctx.Err() != nil {
				return nil, contracts.Errorf(contracts.ErrUnavailable, "terminal relay: no output within %s", o.FirstFrame)
			}
			return nil, contracts.Errorf(contracts.ErrUnavailable, "terminal relay: %v", err)
		}
		if typ == websocket.MessageBinary {
			if o.Fit {
				c.closeNow()
				return nil, contracts.Errorf(contracts.ErrUnsupported, "the relay did not fit the stream to the window (hub or node predates %s)", contracts.TerminalFit)
			}
			c.buf = b
			return c, nil
		}
		f, err := parseFrame(b)
		if err != nil {
			continue
		}
		switch f.Type {
		case FrameError:
			if f.Error != nil {
				c.lastErr = f.Error
			}
		case FrameResize:
			if o.Fit {
				c.cols, c.rows = ClampSize(f.Cols, f.Rows)
				return c, nil
			}
		}
	}
}

// Read returns terminal bytes. A size frame of a fitted stream ends a
// Read with no bytes (0, nil) so the caller can apply TakeWindowSize
// before the bytes drawn for the new size.
func (c *ClientConn) Read(p []byte) (int, error) {
	if len(c.buf) > 0 {
		n := copy(p, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}
	typ, b, err := c.ws.Read(c.ctx)
	if err != nil {
		if c.lastErr != nil {
			return 0, c.lastErr
		}
		// The close handshake can shut the connection under a blocked
		// Read before the context is cancelled ("use of closed network
		// connection"); after Close that is still the detach.
		if isNormalClose(err) || c.ctx.Err() != nil || c.closing.Load() {
			return 0, io.EOF
		}
		return 0, err
	}
	if typ == websocket.MessageBinary {
		n := copy(p, b)
		c.buf = b[n:]
		return n, nil
	}
	if f, err := parseFrame(b); err == nil {
		switch {
		case f.Type == FrameError && f.Error != nil:
			c.lastErr = f.Error
		case f.Type == FrameResize && c.fit:
			c.mu.Lock()
			c.cols, c.rows = ClampSize(f.Cols, f.Rows)
			c.resized = true
			c.mu.Unlock()
		}
	}
	return 0, nil
}

// Write sends input; a read-only stream drops it (the relay would too).
func (c *ClientConn) Write(p []byte) (int, error) {
	if c.readOnly || len(p) == 0 {
		return len(p), nil
	}
	ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize asks for a new size; a fitted stream keeps the window's.
func (c *ClientConn) Resize(cols, rows int) error {
	if c.fit {
		return nil
	}
	cols, rows = ClampSize(cols, rows)
	return writeJSON(c.ctx, c.ws, Frame{Type: FrameResize, Cols: cols, Rows: rows})
}

// WindowSize is a fitted stream's current size (the window's).
func (c *ClientConn) WindowSize() (cols, rows int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cols, c.rows
}

// TakeWindowSize reports a size change of a fitted stream once.
func (c *ClientConn) TakeWindowSize() (cols, rows int, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ok, c.resized = c.resized, false
	return c.cols, c.rows, ok
}

// Close detaches without waiting for the close handshake.
func (c *ClientConn) Close() error {
	c.closing.Store(true)
	c.once.Do(func() {
		go func() {
			_ = c.ws.Close(websocket.StatusNormalClosure, "detached")
			c.cancel()
		}()
	})
	return nil
}

func (c *ClientConn) closeNow() {
	c.once.Do(func() {
		c.cancel()
		_ = c.ws.CloseNow()
	})
}

// ErrNotWebTerminal is returned when a descriptor is not a web terminal.
var ErrNotWebTerminal = errors.New("terminal: descriptor is not a web_terminal")

// NoSSHRoute reports an attach refusal meaning the run's node offers this
// client no ssh route (no ssh alias configured: a Windows cloud VM has no
// ssh server). The run's terminal is still reachable through the relay.
func NoSSHRoute(err error) bool {
	return err != nil && contracts.IsCode(err, contracts.ErrUnavailable) && strings.Contains(err.Error(), "ssh alias")
}
