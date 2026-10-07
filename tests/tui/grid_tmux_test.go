package tui_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
)

// gridTmuxAPI answers attach requests for three sessions with real
// local_tmux descriptors for shells on a private tmux server (tmux -L
// <name>, never the user's), the way Pyre's node builds them.
type gridTmuxAPI struct {
	contracts.API
	bin, sock string
	targets   map[string]string // session ID -> tmux session
}

func (a *gridTmuxAPI) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	d, err := a.API.AttachSession(ctx, contracts.AttachRequest{Mutation: req.Mutation, SessionID: req.SessionID, ReadOnly: true})
	target, ok := a.targets[req.SessionID]
	if err != nil || !ok {
		return d, err
	}
	d.Mechanism, d.ReadOnly = contracts.AttachLocalTmux, req.ReadOnly
	d.Command = []string{a.bin, "-u", "-L", a.sock, "attach-session", "-t", target}
	if req.ReadOnly {
		d.Command = append(d.Command, "-r")
	}
	return d, nil
}

func (a *gridTmuxAPI) tmux(t *testing.T, args ...string) string {
	t.Helper()
	out, _ := exec.Command(a.bin, append([]string{"-L", a.sock}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// sendKeys types into a session whose only clients are the read-only
// tiles. tmux 3.7 refuses send-keys when its implied target client is
// read-only, so name no client (-c "", as Pyre's own SendKeys does); tmux
// before 3.4 has no -c and no such check.
func (a *gridTmuxAPI) sendKeys(t *testing.T, target string, keys ...string) {
	t.Helper()
	out, err := exec.Command(a.bin, append([]string{"-L", a.sock, "send-keys", "-c", "", "-t", target}, keys...)...).CombinedOutput()
	if err != nil && (strings.Contains(string(out), "unknown flag -c") || strings.Contains(string(out), "usage: send-keys")) {
		out, err = exec.Command(a.bin, append([]string{"-L", a.sock, "send-keys", "-t", target}, keys...)...).CombinedOutput()
	}
	if err != nil {
		t.Fatalf("send-keys %s: %v: %s", target, err, out)
	}
}

// TestGridRealTmux tiles three real shell sessions: each tile is a
// read-only `-f ignore-size` client sized to the window, so the agents'
// windows keep their size although the tiles are much smaller; output
// shows up live; leaving the grid detaches every tile client.
func TestGridRealTmux(t *testing.T) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	a := &gridTmuxAPI{bin: bin, sock: fmt.Sprintf("pyre-wgrid-test-%d", os.Getpid()), targets: map[string]string{
		"ses_synth-a": "grid-a", "ses_synth-b": "grid-b", "ses_synth-apr": "grid-c",
	}}
	t.Cleanup(func() {
		path := a.tmux(t, "display", "-p", "#{socket_path}")
		_ = exec.Command(bin, "-L", a.sock, "kill-server").Run()
		if strings.HasPrefix(path, "/") {
			_ = os.Remove(path)
		}
	})
	shell := "env -i HOME=" + home + " HISTFILE=/dev/null PATH=/usr/bin:/bin PS1='$ ' /bin/sh"
	for i, name := range []string{"grid-a", "grid-b", "grid-c"} {
		args := []string{"-L", a.sock}
		if i == 0 {
			args = append(args, "-f", "/dev/null")
		}
		args = append(args, "new-session", "-d", "-s", name, "-x", "150", "-y", "45", shell)
		if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
			t.Fatalf("tmux new-session: %v: %s", err, out)
		}
	}
	size := func(name string) string {
		return a.tmux(t, "display", "-p", "-t", name, "#{window_width}x#{window_height}")
	}
	for _, n := range []string{"grid-a", "grid-b", "grid-c"} {
		if s := size(n); s != "150x45" {
			t.Fatalf("%s starts at %s", n, s)
		}
	}
	h, _ := start(t, 200, 50, func(api contracts.API) contracts.API {
		a.API = api
		return a
	}, func(o *tui.Options) { o.StartTerm = tui.StartPTY })
	h.Key("G")
	clients := func(name string) string {
		return a.tmux(t, "list-clients", "-t", name, "-F", "#{client_readonly} #{client_flags}")
	}
	for sid, name := range a.targets {
		h.WaitFor("tile client for "+name, 10*sec, func() bool {
			return strings.HasPrefix(clients(name), "1 ") && strings.Contains(h.M.GridTileText(sid), "$")
		})
		argv := strings.Join(h.M.GridTileCommand(sid), " ")
		if !strings.Contains(argv, "attach-session -t "+name+" -r -f ignore-size") {
			t.Fatalf("tile attach argv lacks -r -f ignore-size: %s", argv)
		}
		if c := clients(name); !strings.Contains(c, "ignore-size") || !strings.Contains(c, "read-only") {
			t.Fatalf("%s client flags: %q", name, c)
		}
	}
	// The tiles are about 66x14 here; the windows must not follow them.
	for _, n := range []string{"grid-a", "grid-b", "grid-c"} {
		if s := size(n); s != "150x45" {
			t.Fatalf("a tile resized %s to %s", n, s)
		}
	}
	// Live output reaches the tiles.
	for sid, name := range a.targets {
		a.sendKeys(t, name, "echo tile-"+name, "Enter")
		h.WaitFor("output in "+name, 10*sec, func() bool { return hasLine(h.M.GridTileText(sid), "tile-"+name) })
	}
	h.Settle()
	if v := h.View(); !strings.Contains(v, "tile-grid-a") || !strings.Contains(v, "tile-grid-c") {
		t.Fatalf("tile crops should show the newest lines:\n%s", v)
	}
	// A human attaches elsewhere with a smaller terminal: the window
	// follows the human, never the tile; the tile re-reads the size and
	// follows too, so when the human leaves the window keeps the human's
	// size instead of snapping back to the tile's old one.
	human, err := tui.StartPTY(contracts.AttachDescriptor{Mechanism: contracts.AttachLocalTmux,
		Command: []string{bin, "-u", "-L", a.sock, "attach-session", "-t", "grid-b"}}, 130, 36)
	if err != nil {
		t.Fatalf("human client: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, human) }()
	h.WaitFor("window follows the human", 10*sec, func() bool { return size("grid-b") == "130x35" })
	h.WaitFor("tile follows the window", 15*sec, func() bool {
		c, r := h.M.GridTileSize("ses_synth-b")
		return c == 130 && r == 36
	})
	_ = human.Close()
	h.WaitFor("human detached", 10*sec, func() bool {
		return !strings.Contains(clients("grid-b"), "\n") && strings.HasPrefix(clients("grid-b"), "1 ")
	})
	if s := size("grid-b"); s != "130x35" {
		t.Fatalf("the window snapped to %s when the human left", s)
	}
	h.Key("G") // back to the list view
	for name, want := range map[string]string{"grid-a": "150x45", "grid-b": "130x35", "grid-c": "150x45"} {
		h.WaitFor("tile client of "+name+" gone", 10*sec, func() bool { return clients(name) == "" })
		if s := size(name); s != want {
			t.Fatalf("%s is %s after the tiles left, want %s", name, s, want)
		}
	}
}
