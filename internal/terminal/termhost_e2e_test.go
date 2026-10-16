//go:build !windows

package terminal_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost"
	_ "github.com/jhh3/pyre/internal/runtime/termhost/hostproc" // this test binary serves __pyre_termhost
	"github.com/jhh3/pyre/internal/terminal"
)

// Runs on a Pyre terminal host (the Windows backend; a real PTY here) are
// relayed by the node bridge connecting to the host directly: a writable
// stream types and sizes the session, a fitted read-only stream follows
// the session's size and never changes it, and detaching leaves the run.
func TestWebTerminalTermhost(t *testing.T) {
	e := newEnv(t)
	th, err := termhost.New(filepath.Join(e.dir, "th.sock"))
	if err != nil {
		t.Fatal(err)
	}
	th.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	t.Cleanup(func() { _ = th.KillServer(context.Background()) })
	e.node.th.Store(th)
	sid, runID := e.startShell()
	name, _ := e.node.thRuns.Load(runID)
	sess := name.(string)
	ctx := context.Background()

	d, err := e.attach(e.owner, sid, false)
	if err != nil {
		t.Fatal(err)
	}
	tc := e.open(d, d.Ticket)
	waitFor(t, "writable client on the host", func() bool {
		cs, _ := th.ListClients(ctx, sess)
		return sessionhost.WritableClients(cs) == 1
	})
	if p, _ := th.Pane(ctx, sess); p.Width != 100 || p.Height != 30 {
		t.Fatalf("writer did not size the session: %+v", p)
	}
	tc.input("echo hi-$((40+2))\r")
	waitFor(t, "command output relayed", func() bool { return strings.Contains(tc.output(), "hi-42") })
	tc.send(terminal.Frame{Type: terminal.FrameResize, Cols: 120, Rows: 40})
	waitFor(t, "resize applied", func() bool { p, _ := th.Pane(ctx, sess); return p.Width == 120 && p.Height == 40 })
	_ = tc.ws.Close(websocket.StatusNormalClosure, "detach")
	waitFor(t, "stream closed", func() bool {
		cs, _ := th.ListClients(ctx, sess)
		return len(cs) == 0 && e.node.bridge.Sessions() == 0 && e.st.Terminal.Streams() == 0
	})
	if p, err := th.Pane(ctx, sess); err != nil || p.Dead {
		t.Fatalf("run ended on detach: %+v %v", p, err)
	}

	// A fitted read-only stream (TUI preview / Grid tile).
	d, err = e.attach(e.owner, sid, true)
	if err != nil {
		t.Fatal(err)
	}
	cc, err := terminal.Dial(ctx, "wss://"+e.addr+d.URL, d.Ticket, terminal.DialOptions{HTTPClient: e.httpClient(), Cols: 40, Rows: 12, ReadOnly: true, Fit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if cols, rows := cc.WindowSize(); cols != 120 || rows != 40 {
		t.Fatalf("fitted size %dx%d, session 120x40", cols, rows)
	}
	dr := drainConn(cc)
	waitFor(t, "repaint relayed", func() bool { return strings.Contains(dr.output(), "hi-42") })
	cs, _ := th.ListClients(ctx, sess)
	if len(cs) != 1 || !cs[0].ReadOnly {
		t.Fatalf("clients %+v", cs)
	}
	_ = cc.Resize(30, 10) // ignored
	if p, _ := th.Pane(ctx, sess); p.Width != 120 || p.Height != 40 {
		t.Fatalf("watching resized the session: %+v", p)
	}
	if err := th.ResizeWindow(ctx, sess, 100, 30); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stream follows the session", func() bool { return dr.cur() == [2]int{100, 30} })
	cc.Close()
	<-dr.done
	waitFor(t, "fitted stream closed", func() bool { return e.node.bridge.Sessions() == 0 })
	if v, _ := e.st.Hub.GetSession(e.owner, c.GetRequest{ID: sid}); v.Run == nil || v.Run.State.Terminal() {
		t.Fatalf("run ended: %+v", v.Run)
	}
}
