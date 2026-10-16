package terminal_test

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/internal/terminal"
)

func windowSize(t *testing.T, tm supervisor.Tmux) string {
	t.Helper()
	out, err := exec.Command("tmux", "-S", tm.Socket, "display-message", "-p", "-t", "pyre", "#{window_width}x#{window_height}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// drain reads a ClientConn until it ends, collecting output and applying
// size changes the way the TUI does.
type drain struct {
	mu   sync.Mutex
	out  strings.Builder
	size [2]int
	err  error
	done chan struct{}
}

func drainConn(cc *terminal.ClientConn) *drain {
	d := &drain{done: make(chan struct{})}
	d.size[0], d.size[1] = cc.WindowSize()
	go func() {
		defer close(d.done)
		buf := make([]byte, 4096)
		for {
			n, err := cc.Read(buf)
			d.mu.Lock()
			if cols, rows, ok := cc.TakeWindowSize(); ok {
				d.size = [2]int{cols, rows}
			}
			d.out.Write(buf[:n])
			if err != nil {
				d.err = err
			}
			d.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return d
}

func (d *drain) output() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.out.String()
}

func (d *drain) cur() [2]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.size
}

// A fitted read-only stream (TUI Grid tiles) is sized by the node to the
// tmux window whatever size the client asks for, never resizes the window,
// follows a window resize, and ignores client resize requests.
func TestWebTerminalFittedStream(t *testing.T) {
	e := newEnv(t)
	sid, runID := e.startShell()
	tm, _ := e.node.tmuxFor(runID)
	before := windowSize(t, tm)

	d, err := e.attach(e.owner, sid, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cc, err := terminal.Dial(ctx, "wss://"+e.addr+d.URL, d.Ticket, terminal.DialOptions{HTTPClient: e.httpClient(), Cols: 40, Rows: 12, ReadOnly: true, Fit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	cols, rows := cc.WindowSize()
	var ww, wh int
	if _, err := fmt.Sscanf(before, "%dx%d", &ww, &wh); err != nil {
		t.Fatal(err)
	}
	if cols != ww || rows != wh+1 { // tmux's status line is on
		t.Fatalf("fitted size %dx%d, window %s (+1 status row)", cols, rows, before)
	}
	dr := drainConn(cc)
	waitFor(t, "fitted read-only client", func() bool {
		cl := tmuxClients(t, tm)
		return len(cl) == 1 && strings.HasSuffix(cl[0], "ro=1")
	})
	if cl := tmuxClients(t, tm); !strings.HasPrefix(cl[0], before[:strings.IndexByte(before, 'x')]+"x") {
		t.Fatalf("client %q is not the window's width (%s)", cl, before)
	}
	if got := windowSize(t, tm); got != before {
		t.Fatalf("watching resized the window: %s -> %s", before, got)
	}
	// Client resize requests are ignored.
	_ = cc.Resize(30, 10)
	tm.SendKeys(ctx, "pyre", true, "echo fit-$((40+2))")
	tm.SendKeys(ctx, "pyre", false, "Enter")
	waitFor(t, "output relayed", func() bool { return strings.Contains(dr.output(), "fit-42") })
	if got := windowSize(t, tm); got != before {
		t.Fatalf("a client resize request resized the window: %s -> %s", before, got)
	}
	// A human resizes the window: the stream follows.
	if err := exec.Command("tmux", "-S", tm.Socket, "resize-window", "-t", "pyre", "-x", "100", "-y", "30").Run(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stream follows the window", func() bool { return dr.cur() == [2]int{100, 31} })
	cc.Close()
	<-dr.done
	if dr.err != io.EOF {
		t.Fatalf("detach ends reads with EOF, got %v", dr.err)
	}
	waitFor(t, "stream closed", func() bool { return e.node.bridge.Sessions() == 0 && len(tmuxClients(t, tm)) == 0 })
}

// A plain read-only stream (the TUI's preview pane) through Dial: output
// flows, the first frame is already buffered, and refusals come back as
// the relay's error.
func TestWebTerminalDialReadOnly(t *testing.T) {
	e := newEnv(t)
	sid, runID := e.startShell()
	tm, _ := e.node.tmuxFor(runID)
	d, err := e.attach(e.owner, sid, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cc, err := terminal.Dial(ctx, "wss://"+e.addr+d.URL, d.Ticket, terminal.DialOptions{HTTPClient: e.httpClient(), Cols: 90, Rows: 20, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	dr := drainConn(cc)
	waitFor(t, "read-only client at the asked size", func() bool {
		cl := tmuxClients(t, tm)
		return len(cl) == 1 && cl[0] == "90x20 ro=1"
	})
	tm.SendKeys(ctx, "pyre", true, "echo pane-$((40+2))")
	tm.SendKeys(ctx, "pyre", false, "Enter")
	waitFor(t, "output relayed", func() bool { return strings.Contains(dr.output(), "pane-42") })
	cc.Close()
	<-dr.done

	// The used ticket is refused with the relay's error.
	if _, err := terminal.Dial(ctx, "wss://"+e.addr+d.URL, d.Ticket, terminal.DialOptions{HTTPClient: e.httpClient(), ReadOnly: true}); !c.IsCode(err, c.ErrConflict) {
		t.Fatalf("reused ticket: %v", err)
	}
}
