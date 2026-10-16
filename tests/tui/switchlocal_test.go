package tui_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// hubCounter counts the hub calls a switch can make, per session.
type hubCounter struct {
	contracts.API
	mu      sync.Mutex
	reads   map[string]int
	attachs map[string]int
}

func newHubCounter(api contracts.API) *hubCounter {
	return &hubCounter{API: api, reads: map[string]int{}, attachs: map[string]int{}}
}

func (c *hubCounter) ReadSession(ctx context.Context, r contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	c.mu.Lock()
	c.reads[r.SessionID]++
	c.mu.Unlock()
	return c.API.ReadSession(ctx, r)
}

func (c *hubCounter) AttachSession(ctx context.Context, r contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	c.mu.Lock()
	c.attachs[r.SessionID]++
	c.mu.Unlock()
	return c.API.AttachSession(ctx, r)
}

func (c *hubCounter) counts(sid string) (reads, attaches int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[sid], c.attachs[sid]
}

// localTmuxServer starts a private tmux server (its own socket in a temp
// dir; never Pyre's socket or the user's server) with a session named
// run, showing one synthetic line.
func localTmuxServer(t *testing.T, run string) string {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	dir, err := os.MkdirTemp("", "pyre-swl-")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "tmux.sock")
	t.Cleanup(func() {
		_ = exec.Command(bin, "-S", sock, "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
	cmd := exec.Command(bin, "-S", sock, "-f", "/dev/null", "new-session", "-d", "-s", run, "-x", "80", "-y", "20",
		"env", "-i", "PATH=/usr/bin:/bin", "sh", "-c", "printf 'synthetic local pane of "+run+"\\n'; exec sleep 600")
	cmd.Env = append(os.Environ(), "TMUX=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	deadline := time.Now().Add(5 * sec)
	for time.Now().Before(deadline) {
		out, _ := exec.Command(bin, "-S", sock, "capture-pane", "-p", "-t", "="+run+":").Output()
		if strings.Contains(string(out), "synthetic local pane") {
			return sock
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the synthetic pane never printed")
	return ""
}

// Switching to a session that runs on this machine paints its tmux pane
// (captured locally) without asking the hub for an attach or a preview,
// never says "loading preview", and watches it read-only.
func TestLocalSwitchSkipsTheHub(t *testing.T) {
	const sid, run, node = "ses_synth-a", "run_synth-a-1", "node_synth-mac"
	sock := localTmuxServer(t, run)
	var hub *hubCounter
	var mu sync.Mutex
	var started []contracts.AttachDescriptor
	h, _ := start(t, 160, 40, func(a contracts.API) contracts.API {
		hub = newHubCounter(tuitest.NewEmbeddable(a))
		return hub
	}, func(o *tui.Options) {
		o.ClientNodeID, o.LocalTmuxSocket = node, sock
		o.StartTerm = func(d contracts.AttachDescriptor, cols, rows int) (tui.TermConn, error) {
			mu.Lock()
			started = append(started, d)
			mu.Unlock()
			// A stream that draws nothing: what shows is the capture.
			return tuitest.NewFakeTerm(d, cols, rows, func(int, int, string) string { return "" }), nil
		}
	})
	if h.M.SelectedSession() == sid {
		h.Key("down")
	}
	h.Settle()
	reads0, attaches0 := hub.counts(sid)
	selectSession(t, h, sid)
	t0 := time.Now()
	for !h.Contains("synthetic local pane of " + run) {
		if h.Contains("loading preview") {
			t.Fatalf("a local session said loading preview:\n%s", h.View())
		}
		if time.Since(t0) > 5*sec {
			t.Fatalf("no captured pane after the switch:\n%s", h.View())
		}
		h.SettleFor(2*time.Millisecond, 20*time.Millisecond)
	}
	t.Logf("first paint of the local pane %s after the switch", time.Since(t0))
	h.Settle()
	if reads, attaches := hub.counts(sid); reads != reads0 || attaches != attaches0 {
		t.Fatalf("switching to a local session asked the hub: %d reads, %d attaches", reads-reads0, attaches-attaches0)
	}
	mu.Lock()
	defer mu.Unlock()
	var mine []contracts.AttachDescriptor
	for _, d := range started {
		if d.RunID == run {
			mine = append(mine, d)
		}
	}
	if len(mine) != 1 || !mine[0].ReadOnly || mine[0].Mechanism != contracts.AttachLocalTmux ||
		!strings.Contains(strings.Join(mine[0].Command, " "), "-S "+sock+" ") || mine[0].Command[len(mine[0].Command)-1] != "-r" {
		t.Fatalf("local attach: %+v", mine)
	}
	if h.M.LocalViews() < 1 {
		t.Fatal("no view was opened locally")
	}
}

// A session on this machine with no tmux session here (the socket exists,
// the run's session does not) falls back to the hub's attach.
func TestLocalSwitchFallsBackToTheHub(t *testing.T) {
	const sid, node = "ses_synth-a", "node_synth-mac"
	sock := localTmuxServer(t, "run_synth-other")
	var hub *hubCounter
	h, _ := start(t, 160, 40, func(a contracts.API) contracts.API {
		hub = newHubCounter(tuitest.NewEmbeddable(a))
		return hub
	}, func(o *tui.Options) { o.ClientNodeID, o.LocalTmuxSocket = node, sock })
	selectSession(t, h, sid)
	h.WaitFor("the hub's attach", 5*sec, func() bool { _, a := hub.counts(sid); return a > 0 && h.M.TermLive(sid) })
}

// One session selected and idle for a simulated minute (refresh ticks,
// no events) reads its preview at most a few times: the safety net is
// one read per 30 s, never one per refresh.
func TestIdlePreviewPollRate(t *testing.T) {
	var hub *hubCounter
	var clk *mock.FakeClock
	h, _ := start(t, 160, 40, func(a contracts.API) contracts.API {
		hub = newHubCounter(a) // the mock's snapshot-only attach: the pane shows the preview
		return hub
	}, func(o *tui.Options) {
		o.RefreshInterval = 20 * time.Millisecond
		clk = o.Clock.(*mock.FakeClock)
	})
	sid := h.M.SelectedSession()
	h.WaitFor("first preview", 5*sec, func() bool { return h.M.Loaded("preview:" + sid) })
	r0, _ := hub.counts(sid)
	for i := 0; i < 60; i++ { // a minute, one second per refresh tick
		clk.Advance(time.Second)
		h.SettleFor(25*time.Millisecond, 50*time.Millisecond)
	}
	h.SettleFor(100*time.Millisecond, 300*time.Millisecond) // refresh ticks never let it idle long
	if h.M.SelectedSession() != sid {
		t.Fatalf("selection moved to %s", h.M.SelectedSession())
	}
	reads, _ := hub.counts(sid)
	t.Logf("%d preview reads over a simulated idle minute (+%d initial)", reads-r0, r0)
	if reads-r0 > 2 {
		t.Fatalf("%d preview reads over an idle minute; want at most 2 (one per 30 s)", reads-r0)
	}
}
