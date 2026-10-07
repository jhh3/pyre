package tui_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// TestHelperMouseEcho is not a test: run with PYRE_TEST_MOUSE_ECHO=1 in a
// raw-mode pty it is a full-screen application that enables mouse
// reporting (?1000, ?1002, ?1006, like Claude Code and Codex) and prints
// every input chunk it receives.
func TestHelperMouseEcho(t *testing.T) {
	if os.Getenv("PYRE_TEST_MOUSE_ECHO") != "1" {
		t.Skip("helper process for TestEmbeddedMouseRealTmux")
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?1000h\x1b[?1002h\x1b[?1006hMECHO READY\r\n")
	buf := make([]byte, 512)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			fmt.Fprintf(os.Stdout, "GOT %q\r\n", buf[:n])
		}
		if err != nil {
			os.Exit(0)
		}
	}
}

// mouseAPI answers attach requests for ses_synth-a with Pyre's own attach
// command (tmux.AttachArgv: mouse on, then attach) for a private server.
type mouseAPI struct {
	contracts.API
	bin, sock string
}

func (a *mouseAPI) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	d, err := a.API.AttachSession(ctx, contracts.AttachRequest{Mutation: req.Mutation, SessionID: req.SessionID, ReadOnly: true})
	if err != nil || req.SessionID != "ses_synth-a" {
		return d, err
	}
	d.Mechanism, d.ReadOnly = contracts.AttachLocalTmux, req.ReadOnly
	d.Command = tmux.AttachArgv(a.bin, a.sock, "embed", req.ReadOnly)
	return d, nil
}

// TestEmbeddedMouseRealTmux runs a mouse-reporting application in a real
// tmux server started with mouse off (an older Pyre's server) and drives
// the embedded pane with Bubble Tea mouse events: the wheel over the
// watched pane reaches the application (a writable attach held for the
// wheel; the keyboard stays with the list); a click focuses the pane and
// reaches the application at the pane-relative cell; typing still works.
func TestEmbeddedMouseRealTmux(t *testing.T) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("pyre-wembedmouse-test-%d", os.Getpid())
	run := func(args ...string) string {
		out, _ := exec.Command(bin, append([]string{"-L", name}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		path := run("display", "-p", "#{socket_path}")
		run("kill-server")
		if strings.HasPrefix(path, "/") {
			_ = os.Remove(path)
		}
	})
	script := "stty raw -echo; PYRE_TEST_MOUSE_ECHO=1 exec " + tmux.ShellQuote(self) + " -test.run='^TestHelperMouseEcho$'"
	if out, err := exec.Command(bin, "-L", name, "-f", "/dev/null", "new-session", "-d", "-s", "embed", "-x", "50", "-y", "10", "/bin/sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	a := &mouseAPI{bin: bin, sock: run("display", "-p", "#{socket_path}")}
	if run("show-options", "-gv", "mouse") != "off" {
		t.Fatal("test server should start with mouse off")
	}
	var rec *tuitest.Recorder
	h, _ := start(t, 160, 40, func(api contracts.API) contracts.API {
		a.API = api
		rec = tuitest.NewRecorder(a)
		return rec
	}, func(o *tui.Options) { o.StartTerm = tui.StartPTY })
	selectSession(t, h, "ses_synth-a")
	text := func() string { return h.M.TermText("ses_synth-a") }
	h.WaitFor("echo app in the pane", 10*sec, func() bool { return strings.Contains(text(), "MECHO READY") })
	clients := func() string { return run("list-clients", "-t", "embed", "-F", "#{client_readonly}") }
	h.WaitFor("one read-only client", 5*sec, func() bool { return clients() == "1" })
	if run("show-options", "-gv", "mouse") != "on" {
		t.Fatal("the attach did not turn mouse on")
	}
	x0, y0 := at(t, h, "MECHO READY") // emulator cell (0, 0)

	// The wheel over the watched pane scrolls the application.
	h.Send(tea.MouseWheelMsg{X: x0 + 3, Y: y0 + 1, Button: tea.MouseWheelUp})
	h.WaitFor("wheel up reaches the app", 10*sec, func() bool { return strings.Contains(text(), `\x1b[<64;4;2M`) })
	h.Send(tea.MouseWheelMsg{X: x0 + 3, Y: y0 + 1, Button: tea.MouseWheelDown})
	h.WaitFor("wheel down reaches the app", 10*sec, func() bool { return strings.Contains(text(), `\x1b[<65;4;2M`) })
	if h.M.TermFocused() {
		t.Fatal("the wheel took the keyboard")
	}
	if c := clients(); c != "0" {
		t.Fatalf("clients while the wheel scrolls: %q, want one writable", c)
	}
	if n := len(rec.Calls("AcquireLease")); n != 1 {
		t.Fatalf("wheel hold leases: %d, want 1", n)
	}

	// A click focuses the pane and passes through.
	h.Send(tea.MouseClickMsg{X: x0 + 5, Y: y0 + 2, Button: tea.MouseLeft})
	h.Send(tea.MouseReleaseMsg{X: x0 + 5, Y: y0 + 2, Button: tea.MouseLeft})
	h.WaitFor("click reaches the app", 10*sec, func() bool {
		return strings.Contains(text(), `\x1b[<0;6;3M`) && strings.Contains(text(), `\x1b[<0;6;3m`)
	})
	if !h.M.TermFocused() {
		t.Fatal("a click on the terminal should focus it")
	}
	// A drag selects and copies in Pyre (termselect.go); it never reaches
	// the app, like native Claude Code where dragging selects.
	h.Send(tea.MouseClickMsg{X: x0 + 1, Y: y0 + 1, Button: tea.MouseLeft})
	h.Send(tea.MouseMotionMsg{X: 0, Y: y0 + 1, Button: tea.MouseLeft})
	h.Send(tea.MouseReleaseMsg{X: 0, Y: y0 + 1, Button: tea.MouseLeft})
	h.SettleFor(200*time.Millisecond, 2*sec)
	if strings.Contains(text(), `\x1b[<32;`) || strings.Contains(text(), `\x1b[<0;1;2`) {
		t.Fatalf("a drag reached the app:\n%s", text())
	}
	h.Type("z")
	h.WaitFor("typing still works", 10*sec, func() bool { return strings.Contains(text(), `GOT "z"`) })
	if n := len(rec.Calls("AcquireLease")); n != 1 {
		t.Fatalf("focus after the wheel hold should reuse its attach and lease, got %d leases", n)
	}

	h.Key("ctrl+]")
	h.WaitFor("back to one read-only client", 10*sec, func() bool { return !h.M.TermFocused() && clients() == "1" })
	h.WaitFor("lease released", 5*sec, func() bool { return len(rec.Calls("ReleaseLease")) == 1 })
	h.Close()
	h.WaitFor("no clients after quit", 10*sec, func() bool { return clients() == "" })
}
