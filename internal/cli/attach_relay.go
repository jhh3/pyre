package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Interactive attach through the hub's terminal relay (internal/terminal):
// for a node with no ssh route (a Windows cloud VM has no ssh server), a
// descriptor whose command is not on this machine, or --relay. The run's
// node runs its own attach client and the bytes travel over the hub's
// /v1/terminal WebSocket; this terminal is put in raw mode and its size is
// sent on start (auth frame) and on every change (resize frames). The input
// lease is taken and renewed exactly like a native attach. Ctrl-b d
// detaches here, whatever the node runs (tmux or the Pyre terminal host),
// so the key is the same everywhere; Ctrl-b Ctrl-b sends one Ctrl-b and any
// other Ctrl-b sequence reaches the session (tmux prefix bindings work).

// RelayDetachNote is printed when a relayed attach starts.
const RelayDetachNote = "detach with Ctrl-b d (Ctrl-b Ctrl-b sends Ctrl-b); the session keeps running"

// relayPrefixKey is the detach prefix (Ctrl-b, as tmux and the terminal host).
const relayPrefixKey = 0x02

// relayResetSequence leaves the modes a session may have put this terminal
// into (the remote client's own reset never arrives once we detach).
const relayResetSequence = "\x1b[0m\x1b[?1l\x1b[?2004l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?25h\x1b[?1049l"

// AttachConsole is the local terminal a relayed attach drives; tests
// inject a fake (App.Console).
type AttachConsole interface {
	// MakeRaw puts the terminal in raw mode; restore undoes it. A stdin
	// that is not a terminal is left as it is.
	MakeRaw() (restore func(), err error)
	// Size is the terminal size (0, 0 when unknown).
	Size() (cols, rows int)
	// WatchResize calls fn with the new size whenever the terminal is
	// resized, until stop closes.
	WatchResize(stop <-chan struct{}, fn func(cols, rows int))
}

// relayConn is a relayed terminal stream (*terminal.ClientConn).
type relayConn interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

// relayDialer returns the hub's terminal relay when this client talks to
// the hub directly (the local daemon socket has none), else nil.
func (a *App) relayDialer(ops *Ops) terminalDialer {
	td, ok := ops.API.(terminalDialer)
	if !ok || (a.conn != nil && a.conn.Mode != "hub") {
		return nil
	}
	return td
}

func (a *App) console() AttachConsole {
	if a.Console != nil {
		return a.Console
	}
	return osConsole{in: a.Stdin, out: a.Stdout}
}

// attachRelay asks the hub for a web_terminal descriptor and runs it in
// this terminal under the run's input lease (writable) or read-only.
func (a *App) attachRelay(ctx context.Context, ops *Ops, td terminalDialer, sv c.SessionView, req c.AttachRequest, o attachOpts) error {
	req.Preferred = []c.AttachMechanism{c.AttachWebTerminal, c.AttachLocalTmux, c.AttachSSHTmux}
	// A new request body needs its own operation ID: the hub refuses a
	// reused one ("operation_id reused with a different request body").
	if a.opID != "" {
		req.Mutation = mut(a.opID + "-relay")
	} else {
		req.Mutation = mut("")
	}
	if o.printOnly || a.jsonOut {
		d, err := ops.API.AttachSession(ctx, req)
		if err != nil {
			return err
		}
		return a.emit(d, func(w io.Writer) { printDescriptor(w, d) })
	}
	// The lease first: the hub refuses an interactive web terminal while
	// another principal holds it, so a takeover must happen before.
	var lease *c.InputLease
	runID := ""
	if sv.Run != nil {
		runID = sv.Run.RunID
	}
	if !req.ReadOnly {
		if runID == "" {
			return c.Errorf(c.ErrConflict, "session %s has no current run to attach to", sv.Session.SessionID)
		}
		l, err := ops.API.AcquireLease(ctx, c.LeaseRequest{Mutation: mut(""), RunID: runID, TTLSeconds: 120, Takeover: o.takeover})
		if err != nil {
			if c.IsCode(err, c.ErrConflict) && !o.takeover {
				a.warnf("another client holds this run's input lease; attach --read-only to watch, or --takeover to take over (recorded)")
			}
			return err
		}
		lease = &l
		if l.TakeoverOf != "" {
			a.warnf("took over input lease %s", l.TakeoverOf)
		}
	}
	stopRenew := a.renewLease(ctx, ops, runID, lease)
	defer func() {
		stopRenew()
		if lease != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if _, err := ops.API.ReleaseLease(rctx, c.LeaseReleaseRequest{Mutation: mut(""), LeaseID: lease.LeaseID}); err != nil {
				a.warnf("pyre: could not release input lease %s (it expires on its own): %v", lease.LeaseID, err)
			}
			cancel()
		}
	}()
	d, err := ops.API.AttachSession(ctx, req)
	if err != nil {
		return err
	}
	if d.Mechanism != c.AttachWebTerminal {
		return c.Errorf(c.ErrUnsupported, "the hub answered %s instead of a web terminal: it has no terminal relay", d.Mechanism).WithRef(c.KindRun, d.RunID)
	}
	con := a.console()
	cols, rows := con.Size()
	conn, err := td.DialTerminal(ctx, d, cols, rows, false)
	if err != nil {
		return err
	}
	return a.pumpRelay(ctx, conn, con, d)
}

// renewLease renews lease every 40s until the returned stop is called.
func (a *App) renewLease(ctx context.Context, ops *Ops, runID string, lease *c.InputLease) (stop func()) {
	if lease == nil {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(40 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				if _, err := ops.API.AcquireLease(rctx, c.LeaseRequest{Mutation: mut(""), RunID: runID, TTLSeconds: 120}); err != nil {
					a.warnf("pyre: could not renew input lease: %v", err)
				}
				cancel()
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// pumpRelay connects this terminal to conn until Ctrl-b d, the stream's
// end, or ctx.
func (a *App) pumpRelay(ctx context.Context, conn relayConn, con AttachConsole, d c.AttachDescriptor) error {
	defer conn.Close()
	mode := "writable"
	if d.ReadOnly {
		mode = "read-only"
	}
	fmt.Fprintf(a.Stderr, "pyre: attached to run %s through the hub's terminal relay (%s); %s\r\n", d.RunID, mode, RelayDetachNote)
	restore, err := con.MakeRaw()
	if err != nil {
		return c.Errorf(c.ErrUnsupported, "attach needs a terminal: %v", err)
	}
	// Output stops at finish: bytes still arriving after a detach are
	// never drawn over the restored terminal.
	var outMu sync.Mutex
	outDone := false
	finish := func(msg string) {
		outMu.Lock()
		outDone = true
		_, _ = io.WriteString(a.Stdout, relayResetSequence)
		outMu.Unlock()
		restore()
		fmt.Fprintf(a.Stderr, "\r\n[%s]\r\n", msg)
	}

	ended := make(chan error, 1)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				outMu.Lock()
				if outDone {
					outMu.Unlock()
					return
				}
				_, werr := a.Stdout.Write(buf[:n])
				outMu.Unlock()
				if werr != nil {
					ended <- werr
					return
				}
			}
			if err != nil {
				ended <- err
				return
			}
		}
	}()
	detached := make(chan struct{})
	go func() {
		var f relayDetachFilter
		buf := make([]byte, 4096)
		for {
			n, err := a.Stdin.Read(buf)
			if n > 0 {
				b, detach := f.feed(buf[:n])
				if len(b) > 0 && !d.ReadOnly {
					if _, err := conn.Write(b); err != nil {
						return
					}
				}
				if detach {
					close(detached)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	stopResize := make(chan struct{})
	defer close(stopResize)
	if !d.ReadOnly {
		go con.WatchResize(stopResize, func(cols, rows int) { _ = conn.Resize(cols, rows) })
	}
	select {
	case <-detached:
		finish("detached from run " + d.RunID + "; the session keeps running")
		return nil
	case err := <-ended:
		if err == io.EOF {
			finish("detached (the stream ended)")
			return nil
		}
		finish("terminal relay closed: " + err.Error())
		return err
	case <-ctx.Done():
		finish("interrupted")
		return ctx.Err()
	}
}

// relayDetachFilter turns typed bytes into session input, consuming the
// prefix sequences (Ctrl-b d detaches, Ctrl-b Ctrl-b sends one Ctrl-b).
// It keeps state across reads.
type relayDetachFilter struct{ prefixed bool }

func (f *relayDetachFilter) feed(in []byte) (out []byte, detach bool) {
	for _, b := range in {
		if f.prefixed {
			f.prefixed = false
			switch b {
			case 'd', 'D':
				return out, true
			case relayPrefixKey:
				out = append(out, relayPrefixKey)
			default:
				out = append(out, relayPrefixKey, b)
			}
			continue
		}
		if b == relayPrefixKey {
			f.prefixed = true
			continue
		}
		out = append(out, b)
	}
	return out, false
}

// osConsole is the process terminal.
type osConsole struct{ in, out any }

func (o osConsole) MakeRaw() (func(), error) {
	f, ok := o.in.(*os.File)
	if !ok || !term.IsTerminal(f.Fd()) {
		return func() {}, nil
	}
	var out *os.File
	if of, ok := o.out.(*os.File); ok {
		out = of
	}
	return makeRawConsole(f, out)
}

func (o osConsole) Size() (int, int) {
	f, ok := o.out.(*os.File)
	if !ok {
		return 0, 0
	}
	cols, rows, err := term.GetSize(f.Fd())
	if err != nil {
		return 0, 0
	}
	return cols, rows
}

func (o osConsole) WatchResize(stop <-chan struct{}, fn func(cols, rows int)) {
	watchConsoleResize(stop, o.Size, fn)
}
