package tui_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// tmuxAPI answers attach requests with a real local_tmux descriptor for
// a shell on a private tmux server (tmux -L <name>, never the user's).
type tmuxAPI struct {
	contracts.API
	bin, sock string
}

func (a *tmuxAPI) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	d, err := a.API.AttachSession(ctx, contracts.AttachRequest{Mutation: req.Mutation, SessionID: req.SessionID, ReadOnly: true})
	if err != nil || req.SessionID != "ses_synth-a" {
		return d, err // other sessions: the mock's snapshot-only descriptor
	}
	d.Mechanism, d.ReadOnly = contracts.AttachLocalTmux, req.ReadOnly
	d.Command = []string{a.bin, "-u", "-L", a.sock, "attach-session", "-t", "embed"}
	if req.ReadOnly {
		d.Command = append(d.Command, "-r")
	}
	return d, nil
}

func (a *tmuxAPI) tmux(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(a.bin, append([]string{"-L", a.sock}, args...)...).CombinedOutput()
	if err != nil {
		t.Logf("tmux %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestEmbeddedTerminalRealTmux runs the embedded pane against a real tmux
// client on a pty: watching attaches read-only and sizes the window to the
// pane; enter swaps in a writable client; typed keys reach the shell;
// the wheel enters tmux copy-mode and typing leaves it; ctrl+] goes back to
// a read-only client.
func TestEmbeddedTerminalRealTmux(t *testing.T) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	a := &tmuxAPI{bin: bin, sock: fmt.Sprintf("pyre-wembed-test-%d", os.Getpid())}
	t.Cleanup(func() {
		path := a.tmux(t, "display", "-p", "#{socket_path}")
		_ = exec.Command(bin, "-L", a.sock, "kill-server").Run()
		if strings.HasPrefix(path, "/") {
			_ = os.Remove(path)
		}
	})
	shell := "env -i HOME=" + home + " PATH=/usr/bin:/bin PS1='$ ' /bin/sh"
	if out, err := exec.Command(bin, "-L", a.sock, "-f", "/dev/null", "new-session", "-d", "-s", "embed", "-x", "50", "-y", "10", shell).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	var rec *tuitest.Recorder
	h, _ := start(t, 160, 40, func(api contracts.API) contracts.API {
		a.API = api
		rec = tuitest.NewRecorder(a)
		return rec
	}, func(o *tui.Options) { o.StartTerm = tui.StartPTY })
	selectSession(t, h, "ses_synth-a")
	h.WaitFor("shell prompt in the pane", 10*sec, func() bool { return strings.Contains(h.M.TermText("ses_synth-a"), "$") })

	clients := func() string { return a.tmux(t, "list-clients", "-t", "embed", "-F", "#{client_readonly}") }
	h.WaitFor("one read-only client", 5*sec, func() bool { return clients() == "1" })
	if n := len(rec.Calls("AcquireLease")); n != 0 {
		t.Fatalf("watching took the input lease (%d)", n)
	}
	// window-size latest: the window follows the pane (the only client).
	h.WaitFor("window sized to the pane", 5*sec, func() bool {
		w, _ := strconv.Atoi(a.tmux(t, "display", "-p", "-t", "embed", "#{window_width}"))
		return w >= 75 // the pane, not tmux's 50-column start
	})
	if strings.Contains(h.View(), "[embed] 0:") {
		t.Fatalf("tmux status line drawn in the pane:\n%s", h.View())
	}

	h.Key("enter")
	h.WaitFor("one writable client", 10*sec, func() bool { return h.M.TermFocused() && clients() == "0" })
	if n := len(rec.Calls("AcquireLease")); n != 1 {
		t.Fatalf("focus should take the input lease once, got %d", n)
	}
	h.Type("echo hi")
	h.Key("enter")
	h.WaitFor("echo output", 10*sec, func() bool { return hasLine(h.M.TermText("ses_synth-a"), "hi") })

	// Scrollback through copy-mode.
	h.Type("seq 1 300")
	h.Key("enter")
	h.WaitFor("seq output", 10*sec, func() bool { return hasLine(h.M.TermText("ses_synth-a"), "300") })
	x, y := at(t, h, "300")
	inMode := func() bool { return a.tmux(t, "display", "-p", "-t", "embed", "#{pane_in_mode}") == "1" }
	for i := 0; i < 4; i++ {
		h.Send(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
	}
	h.WaitFor("copy-mode", 10*sec, inMode)
	h.WaitFor("copy-mode indicator in the pane", 5*sec, func() bool {
		return strings.Contains(h.M.TermText("ses_synth-a"), "/") && strings.Contains(h.View(), "[")
	})
	h.Type("e")
	h.WaitFor("typing leaves copy-mode", 10*sec, func() bool { return !inMode() })
	h.Type("cho bye")
	h.Key("enter")
	h.WaitFor("bye", 10*sec, func() bool { return hasLine(h.M.TermText("ses_synth-a"), "bye") })

	h.Key("ctrl+]")
	h.WaitFor("back to one read-only client", 10*sec, func() bool { return !h.M.TermFocused() && clients() == "1" })
	h.WaitFor("lease released", 5*sec, func() bool { return len(rec.Calls("ReleaseLease")) == 1 })
	h.Close()
	h.WaitFor("no clients after quit", 10*sec, func() bool { return clients() == "" })
}

func hasLine(screen, want string) bool {
	for _, l := range strings.Split(screen, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}
