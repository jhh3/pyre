//go:build windows

package hostproc

// Real ConPTY tests. They need a real Windows 10 1809+ machine (or VM) and
// are not run on the macOS/Linux CI path. Run on Windows with:
//
//	go test -count=1 -v ./internal/runtime/termhost/hostproc/
//
// They start the host as its own detached process (this test binary
// serves __pyre_termhost), so they also exercise DETACHED_PROCESS /
// breakaway spawning, AF_UNIX on Windows, the job object and
// CTRL_CLOSE_EVENT termination.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost"
)

func realHost(t *testing.T) *termhost.Host {
	t.Helper()
	sock := filepath.Join(shortDir(t), "th.sock")
	h, err := termhost.New(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.KillServer(context.Background()) })
	if desc, err := h.Check(context.Background()); err != nil || !strings.Contains(desc, "conpty") {
		t.Fatalf("check %q %v", desc, err)
	}
	return h
}

func TestConPTYSession(t *testing.T) {
	ctx := context.Background()
	h := realHost(t)
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	err := h.NewSession(ctx, sessionhost.SessionSpec{Name: "cmd1", Cwd: os.TempDir(),
		Argv: []string{comspec, "/d", "/c", "echo tok=%PYRE_TEST_TOKEN%& set /p line=& call echo got:%%line%%& exit /b 7"},
		Env:  map[string]string{"PYRE_TEST_TOKEN": "synthetic-token"}, Width: 100, Height: 30})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "env output", func() bool {
		s, _ := h.CapturePane(ctx, "cmd1", sessionhost.CaptureOptions{})
		return strings.Contains(s, "tok=synthetic-token")
	})
	p, err := h.Pane(ctx, "cmd1")
	if err != nil || p.Dead || p.PID <= 0 || p.Width != 100 || p.Height != 30 {
		t.Fatalf("pane %+v %v", p, err)
	}
	// Attach a writer; it answers the prompt through ConPTY input.
	c, err := termhost.DialAttach(ctx, termhost.DialOptions{Socket: h.Socket, Name: "cmd1", Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	view := drain(c)
	waitFor(t, "repaint", func() bool { return view.has("tok=synthetic-token") })
	if _, err := c.Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit 7", func() bool {
		p, _ := h.Pane(ctx, "cmd1")
		return p.Dead && p.ExitStatus != nil && *p.ExitStatus == 7
	})
	waitFor(t, "answer drawn", func() bool { return view.has("got:hello") })
	c.Close()
	if ok, _ := h.HasSession(ctx, "cmd1"); !ok {
		t.Fatal("dead session not kept (remain-on-exit)")
	}
}

func TestConPTYResizeAndKeys(t *testing.T) {
	ctx := context.Background()
	h := realHost(t)
	ps := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	if err := h.NewSession(ctx, sessionhost.SessionSpec{Name: "ps", Argv: []string{ps, "-NoLogo", "-NoProfile"}, Width: 120, Height: 40}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "prompt", func() bool {
		s, _ := h.CapturePane(ctx, "ps", sessionhost.CaptureOptions{})
		return strings.Contains(s, "PS ")
	})
	if err := h.Paste(ctx, "ps", "b1", []byte(`Write-Output "size=$($Host.UI.RawUI.WindowSize.Width)x$($Host.UI.RawUI.WindowSize.Height)"`)); err != nil {
		t.Fatal(err)
	}
	if err := h.SendKeys(ctx, "ps", sessionhost.KeyEnter); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "size 120x40", func() bool {
		s, _ := h.CapturePane(ctx, "ps", sessionhost.CaptureOptions{})
		return strings.Contains(s, "size=120x40")
	})
	if err := h.ResizeWindow(ctx, "ps", 90, 25); err != nil {
		t.Fatal(err)
	}
	_ = h.Paste(ctx, "ps", "b2", []byte(`Write-Output "now=$($Host.UI.RawUI.WindowSize.Width)x$($Host.UI.RawUI.WindowSize.Height)"`))
	_ = h.SendKeys(ctx, "ps", sessionhost.KeyEnter)
	waitFor(t, "size 90x25", func() bool {
		s, _ := h.CapturePane(ctx, "ps", sessionhost.CaptureOptions{History: 100})
		return strings.Contains(s, "now=90x25")
	})
	// Ctrl-C reaches the program as an interrupt.
	_ = h.Paste(ctx, "ps", "b3", []byte(`Start-Sleep -Seconds 300`))
	_ = h.SendKeys(ctx, "ps", sessionhost.KeyEnter)
	time.Sleep(500 * time.Millisecond)
	_ = h.SendKeys(ctx, "ps", sessionhost.KeyCtrlC)
	_ = h.Paste(ctx, "ps", "b4", []byte(`Write-Output "after-ctrl-c"`))
	_ = h.SendKeys(ctx, "ps", sessionhost.KeyEnter)
	waitFor(t, "interrupted", func() bool {
		s, _ := h.CapturePane(ctx, "ps", sessionhost.CaptureOptions{History: 100})
		return strings.Contains(s, "after-ctrl-c")
	})
}

func TestConPTYTerminateTree(t *testing.T) {
	ctx := context.Background()
	h := realHost(t)
	comspec := os.Getenv("ComSpec")
	// A child that ignores the console closing would survive without the
	// job object: start a detached grandchild and the foreground sleeper.
	if err := h.NewSession(ctx, sessionhost.SessionSpec{Name: "tree", Argv: []string{comspec, "/d", "/c", "start /b ping -n 300 127.0.0.1 >NUL & ping -n 300 127.0.0.1 >NUL"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	start := time.Now()
	if err := h.TerminateSession(ctx, "tree", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("terminate took too long")
	}
	if ok, _ := h.HasSession(ctx, "tree"); ok {
		t.Fatal("terminated session still listed")
	}
	// Manual check for the lead: no ping.exe from this test remains
	// (tasklist /fi "imagename eq ping.exe").
}

// TestHostSurvivesClientRestart: a new Host (pyred restarted) re-finds the
// sessions of the detached host process.
func TestHostSurvivesClientRestart(t *testing.T) {
	ctx := context.Background()
	h := realHost(t)
	comspec := os.Getenv("ComSpec")
	if err := h.NewSession(ctx, sessionhost.SessionSpec{Name: "keep", Argv: []string{comspec, "/d", "/k", "echo alive"}}); err != nil {
		t.Fatal(err)
	}
	h2, _ := termhost.New(h.Socket)
	if ok, err := h2.HasSession(ctx, "keep"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	resp, err := ping(ctx, h2)
	if err != nil || resp.PID == os.Getpid() {
		t.Fatalf("host is not a separate process: %+v %v", resp, err)
	}
}
