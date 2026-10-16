//go:build !windows

package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigHasMouseOn(t *testing.T) {
	for _, v := range []Version{{Major: 3, Minor: 4}, {Major: 3, Minor: 6}, {}} {
		if !strings.Contains(ConfigFor(v, nil), "\nset-option -g mouse on\n") {
			t.Fatalf("tmux %d.%d: config lacks mouse on:\n%s", v.Major, v.Minor, ConfigFor(v, nil))
		}
	}
	a := AttachArgv("tmux", "/s", "n", true)
	if strings.Join(a[:9], " ") != "tmux -u -S /s set-option -g mouse on ;" {
		t.Fatalf("attach argv does not set mouse on first: %q", a)
	}
}

// TestMouseThroughAttach runs a mouse-reporting application on a server
// started without mouse on (an older Pyre's config), attaches a tmux
// client with AttachArgv inside a second private server, and types SGR
// mouse input into that client: the attach turns mouse on; a read-only
// client delivers neither mouse nor keys; a writable one delivers both,
// with the pane-relative cell.
func TestMouseThroughAttach(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	old := filepath.Join(t.TempDir(), "old.conf")
	if err := os.WriteFile(old, []byte("set-option -g exit-empty off\nset-option -wg remain-on-exit on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Config = old
	app := []string{"/bin/sh", "-c", `printf '\033[?1000h\033[?1006hREADY\n'; exec cat -v`}
	if err := s.NewSession(ctx, SessionSpec{Name: "m", Argv: app}); err != nil {
		t.Fatal(err)
	}
	if out, _ := s.Run(ctx, nil, "show-options", "-gv", "mouse"); strings.TrimSpace(out) != "off" {
		t.Fatalf("old server mouse = %q", out)
	}
	outer := fmt.Sprintf("pyre-mouse-test-%d", os.Getpid())
	otmux := func(args ...string) string {
		out, _ := exec.Command(s.Bin, append([]string{"-L", outer, "-f", "/dev/null"}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	var outerSock string // the outer server exits with its last session
	t.Cleanup(func() {
		otmux("kill-server")
		if strings.HasPrefix(outerSock, "/") {
			_ = os.Remove(outerSock)
		}
	})
	pane := func() string { out, _ := s.Run(ctx, nil, "capture-pane", "-p", "-t", PaneTarget("m")); return out }
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s; pane:\n%s\nouter:\n%s", what, pane(), otmux("capture-pane", "-p", "-t", "o"))
	}
	waitFor("app ready", func() bool { return strings.Contains(pane(), "READY") })
	for _, ro := range []bool{true, false} {
		argv := append([]string{"env", "-u", "TMUX"}, AttachArgv(s.Bin, s.Socket, "m", ro)...)
		// Through /bin/sh, not the user's shell (in zsh an unquoted =name is
		// command expansion), and not as tmux argv (the outer tmux would
		// split at ";").
		if out := otmux("new-session", "-d", "-s", "o", "-x", "80", "-y", "24", "/bin/sh", "-c", ShellJoin(argv)); out != "" {
			t.Fatalf("outer tmux: %s", out)
		}
		outerSock = otmux("display", "-p", "#{socket_path}")
		want := "0"
		if ro {
			want = "1"
		}
		waitFor("client attached", func() bool {
			out, _ := s.Run(ctx, nil, "list-clients", "-F", "#{client_readonly}")
			return strings.TrimSpace(out) == want
		})
		if out, _ := s.Run(ctx, nil, "show-options", "-gv", "mouse"); strings.TrimSpace(out) != "on" {
			t.Fatalf("attach did not turn mouse on: %q", out)
		}
		tag := "rw"
		if ro {
			tag = "ro"
		}
		otmux("send-keys", "-t", "o", "-l", "\x1b[<0;5;3M\x1b[<0;5;3m\x1b[<64;7;4M"+tag+"\r")
		if ro {
			time.Sleep(700 * time.Millisecond)
			if p := pane(); strings.Contains(p, "^[[<") || strings.Contains(p, "ro") {
				t.Fatalf("read-only client delivered input:\n%s", p)
			}
			// Pyre's own keys still reach the pane while a read-only
			// viewer is the only client (tmux 3.7 refuses send-keys whose
			// implied target client is read-only).
			if err := s.Paste(ctx, "m", "pyre-test", []byte("viewer-ok")); err != nil {
				t.Fatal(err)
			}
			if err := s.SendKeys(ctx, "m", KeyEnter); err != nil {
				t.Fatalf("SendKeys with a read-only client attached: %v", err)
			}
			waitFor("paste + Enter with a read-only client attached", func() bool {
				return strings.Contains(pane(), "viewer-ok\n")
			})
		} else {
			waitFor("mouse and keys from the writable client", func() bool {
				return strings.Contains(pane(), "^[[<0;5;3M^[[<0;5;3m^[[<64;7;4Mrw")
			})
		}
		otmux("kill-session", "-t", "o")
		waitFor("client gone", func() bool {
			out, _ := s.Run(ctx, nil, "list-clients", "-F", "#{client_readonly}")
			return strings.TrimSpace(out) == ""
		})
	}
	// Named keys and pastes leave copy-mode first, so they reach the app.
	if _, err := s.Run(ctx, nil, "copy-mode", "-t", PaneTarget("m")); err != nil {
		t.Fatal(err)
	}
	if err := s.Paste(ctx, "m", "pyre-test", []byte("pasted")); err != nil {
		t.Fatal(err)
	}
	if err := s.SendKeys(ctx, "m", KeyEnter); err != nil {
		t.Fatal(err)
	}
	waitFor("paste + Enter reach the app out of copy-mode", func() bool {
		in, _ := s.Run(ctx, nil, "display", "-p", "-t", PaneTarget("m"), "#{pane_in_mode}")
		return strings.TrimSpace(in) == "0" && strings.Contains(pane(), "pasted")
	})
}
