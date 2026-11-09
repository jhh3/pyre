package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// TestCodexTUIRelaunchedAfterAppServerRestart: a live TUI pane left on a
// dead app-server ("Reconnect failed" in real Codex) is relaunched on the
// new one, by Observe and by Attach (iss_…).
func TestCodexTUIRelaunchedAfterAppServerRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *Options) { o.DisableTUI = false })
	ctx := context.Background()
	h := e.launch(e.a, "run_tr", e.home, nil, map[string]string{"PYRE_TEST_ROLE": "faketui"})
	rr, err := e.a.run(h)
	if err != nil {
		t.Fatal(err)
	}
	panePID := func() int {
		t.Helper()
		p, err := e.a.srv.Pane(ctx, h.RunID)
		if err != nil || p.Dead {
			s, _ := e.a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{Join: true})
			_, serr := os.Stat(rr.hs.sock)
			hist, _ := exec.Command("tmux", "-S", e.o.TmuxSocket, "capture-pane", "-p", "-S", "-200", "-t", h.RunID).CombinedOutput()
			t.Fatalf("TUI pane: %+v %v sock=%s %v now=%q\n%q\n%q", p, err, rr.hs.sock, serr, rr.hs.instanceNow(), s, hist)
		}
		return p.PID
	}
	killServer := func() {
		t.Helper()
		b, _ := os.ReadFile(filepath.Join(filepath.Dir(rr.hs.sock), "fake-starts.log"))
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		pid, _ := strconv.Atoi(strings.Fields(lines[len(lines)-1] + " ")[0])
		p, err := os.FindProcess(pid)
		if pid <= 0 || err != nil {
			t.Fatalf("app-server pid from %q: %v", b, err)
		}
		_ = p.Kill()
		for i := 0; i < 200; i++ {
			if ok, _ := rr.hs.running(ctx); !ok && rr.hs.connected() == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("app-server still running")
	}
	var recorded func() string
	recorded = func() string {
		b, _ := os.ReadFile(filepath.Join(h.StateDir, tuiServerFile))
		return string(b)
	}
	first := recorded()
	if !strings.HasPrefix(first, "as") {
		t.Fatalf("tui-server record %q", first)
	}
	waitScreen := func(want string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if s, _ := e.a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{}); strings.Contains(s, want) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		s, _ := e.a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{})
		t.Fatalf("screen lacks %q:\n%s", want, s)
	}
	waitScreen("FAKE CODEX TUI attached")
	before := panePID()

	// Observe path: the server comes back (here: the next client call) and
	// the next observation relaunches the TUI.
	killServer()
	if _, err := e.a.client(ctx, rr, true); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(10 * time.Second)
	for panePID() == before {
		if time.Now().After(end) {
			t.Fatal("TUI not relaunched by Observe")
		}
		_, _ = e.a.Observe(ctx, h, "")
		time.Sleep(50 * time.Millisecond)
	}
	second := recorded()
	if second == first || second != rr.hs.instanceNow() {
		t.Fatalf("record %q after relaunch (was %q, server %q)", second, first, rr.hs.instanceNow())
	}

	// Attach path: the server is down while the run is idle; attaching
	// starts it and relaunches the TUI in the same call.
	for rr.tuiRelaunch.Load() {
		time.Sleep(20 * time.Millisecond)
	}
	before = panePID()
	killServer()
	if _, err := e.a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID}); err != nil {
		t.Fatal(err)
	}
	if panePID() == before {
		t.Fatal("TUI not relaunched by Attach")
	}
	if got := recorded(); got == second || got != rr.hs.instanceNow() {
		t.Fatalf("record %q after attach (was %q)", got, second)
	}
	// Nothing changed: Observe leaves the TUI alone.
	before = panePID()
	for i := 0; i < 5; i++ {
		_, _ = e.a.Observe(ctx, h, "")
	}
	time.Sleep(200 * time.Millisecond)
	if panePID() != before {
		t.Fatal("TUI relaunched without a server change")
	}
}
