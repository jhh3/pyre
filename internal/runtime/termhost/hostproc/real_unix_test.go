//go:build !windows

package hostproc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost"
)

// TestRealHostProcess runs the host as its own detached process (this test
// binary serves __pyre_termhost) with real PTY programs: start, capture,
// private environment, exit status, re-finding sessions from a fresh Host
// (a pyred restart), attach, terminate and shutdown.
func TestRealHostProcess(t *testing.T) {
	ctx := context.Background()
	sock := filepath.Join(shortDir(t), "th.sock")
	h, err := termhost.New(sock)
	if err != nil {
		t.Fatal(err)
	}
	h.Env = []string{"PATH=/usr/bin:/bin", "HOME=/tmp", "LANG=C"}
	t.Cleanup(func() { _ = h.KillServer(context.Background()) })
	if desc, err := h.Check(ctx); err != nil || !strings.Contains(desc, "pty") {
		t.Fatalf("check %q %v", desc, err)
	}
	err = h.NewSession(ctx, sessionhost.SessionSpec{Name: "sh1", Cwd: "/tmp", Argv: []string{"sh", "-c", `printf 'tok=%s\n' "$PYRE_TEST_TOKEN"; read line; echo "got:$line"; exit 7`},
		Env: map[string]string{"PYRE_TEST_TOKEN": "synthetic-token"}, Width: 90, Height: 20})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "env output", func() bool {
		s, _ := h.CapturePane(ctx, "sh1", sessionhost.CaptureOptions{})
		return strings.Contains(s, "tok=synthetic-token")
	})
	p1, err := h.Pane(ctx, "sh1")
	if err != nil || p1.Dead || p1.PID <= 0 || p1.Width != 90 {
		t.Fatalf("pane %+v %v", p1, err)
	}

	// A new Host on the same socket (pyred restarted) finds the session.
	h2, _ := termhost.New(sock)
	if ok, err := h2.HasSession(ctx, "sh1"); err != nil || !ok {
		t.Fatalf("after restart: %v %v", ok, err)
	}
	// Attach a writer and answer the program through it.
	c, err := termhost.DialAttach(ctx, termhost.DialOptions{Socket: sock, Name: "sh1", Cols: 90, Rows: 20})
	if err != nil {
		t.Fatal(err)
	}
	view := drain(c)
	waitFor(t, "repaint", func() bool { return view.has("tok=synthetic-token") })
	if _, err := c.Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit 7", func() bool {
		p, _ := h2.Pane(ctx, "sh1")
		return p.Dead && p.ExitStatus != nil && *p.ExitStatus == 7
	})
	if !view.has("got:hello") {
		t.Fatal("attached client did not see the program's answer")
	}
	c.Close()

	// Terminate ends a whole process group.
	if err := h2.NewSession(ctx, sessionhost.SessionSpec{Name: "sleeper", Argv: []string{"sh", "-c", "sleep 300 & sleep 300"}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := h2.TerminateSession(ctx, "sleeper", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("terminate took too long")
	}
	if ok, _ := h2.HasSession(ctx, "sleeper"); ok {
		t.Fatal("terminated session still listed")
	}
	if err := h2.KillServer(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "host gone", func() bool {
		_, err := ping(ctx, h2)
		return err != nil
	})
}
