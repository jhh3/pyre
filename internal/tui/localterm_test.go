package tui

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

const testNode = "node_synth-here"

// localModel is a model over synthetic embeddable sessions; "ses_here" runs
// on this machine (testNode), "ses_away" on another. api answers attach
// requests (nil: none expected).
func localModel(t *testing.T, api contracts.API, socket string) *Model {
	t.Helper()
	here, away := sv("ses_here", "synth-here", "/src/here"), sv("ses_away", "synth-away", "/src/away")
	here.Run.NodeID, away.Run.NodeID = testNode, "node_synth-away"
	for _, s := range []contracts.SessionView{here, away} {
		s.Run.Capabilities = []string{contracts.CapAttachNative, contracts.CapAttachReadOnly, contracts.CapScreen}
	}
	m := New(t.Context(), api, Options{NoColor: true, KeysFile: "none", StateFile: "none",
		ClientNodeID: testNode, LocalTmuxSocket: socket})
	m.width, m.height = 200, 55
	m.loads["sessions"] = &loadState{val: []contracts.SessionView{here, away}, have: true, seq: 1}
	m.syncSessions()
	t.Cleanup(m.closeTerms)
	return m
}

// fakeSocket is a unix socket that is not a tmux server.
func fakeSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pyre-sl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, "s")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return p
}

// isolatedTmux starts a private tmux server (its own socket in a temp
// dir, never Pyre's or the user's) with one session per name, each
// printing a synthetic line.
func isolatedTmux(t *testing.T, names ...string) (bin, socket string) {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	dir, err := os.MkdirTemp("", "pyre-sl-")
	if err != nil {
		t.Fatal(err)
	}
	socket = filepath.Join(dir, "tmux.sock")
	t.Cleanup(func() {
		_ = exec.Command(bin, "-S", socket, "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
	for _, n := range names {
		sh := "printf 'synthetic local screen of " + n + "\\n'; exec sleep 600"
		cmd := exec.Command(bin, "-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", n, "-x", "60", "-y", "12", "env", "-i", "PATH=/usr/bin:/bin", "sh", "-c", sh)
		cmd.Env = termEnv(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("tmux new-session: %v: %s", err, out)
		}
	}
	return bin, socket
}

// countingAttach answers sessions.attach with a synthetic descriptor and
// counts the calls.
type countingAttach struct {
	contracts.API
	mu    sync.Mutex
	calls int
	d     contracts.AttachDescriptor
}

func (c *countingAttach) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.d, nil
}

func (c *countingAttach) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestLocalForSelectsOnlyThisMachinesRunsWithASocket(t *testing.T) {
	sock := fakeSocket(t)
	m := localModel(t, nil, sock)
	m.local.bin = "/usr/bin/tmux" // never run here
	here, _ := m.session("ses_here")
	away, _ := m.session("ses_away")

	lt, ok := m.localFor(here)
	if !ok || lt.name != "run_ses_here" || lt.socket != sock {
		t.Fatalf("local session: %+v, %v", lt, ok)
	}
	d := lt.descriptor()
	if d.Mechanism != contracts.AttachLocalTmux || !d.ReadOnly || strings.Join(d.Command, " ") != "/usr/bin/tmux -u -S "+sock+" set-option -g mouse on ; attach-session -t =run_ses_here -r" {
		t.Fatalf("descriptor: %+v", d)
	}
	if _, ok := m.localFor(away); ok {
		t.Fatal("a session on another machine went local")
	}
	m.localBad("run_ses_here")
	if _, ok := m.localFor(here); ok {
		t.Fatal("a run that failed locally went local again at once")
	}
	m.local.bad["run_ses_here"] = time.Now().Add(-time.Second)
	if _, ok := m.localFor(here); !ok {
		t.Fatal("the hub-only period never ends")
	}

	// No socket file, no client node, or no configured socket: the hub.
	gone := localModel(t, nil, sock+".gone")
	gone.local.bin = "/usr/bin/tmux"
	if _, ok := gone.localFor(here); ok {
		t.Fatal("a missing socket went local")
	}
	anon := localModel(t, nil, sock)
	anon.opts.ClientNodeID = ""
	if _, ok := anon.localFor(here); ok {
		t.Fatal("a client without a node went local")
	}
	none := localModel(t, nil, "")
	none.local.bin = "/usr/bin/tmux"
	if _, ok := none.localFor(here); ok {
		t.Fatal("no socket configured or learnt, yet local")
	}
}

// Without a configured socket, the first local_tmux descriptor the hub
// returns for a run here teaches it; other shapes teach nothing.
func TestLearnLocalTmuxFromHubDescriptor(t *testing.T) {
	m := localModel(t, nil, "")
	here, _ := m.session("ses_here")
	away, _ := m.session("ses_away")
	d := contracts.AttachDescriptor{RunID: "run_ses_away", Mechanism: contracts.AttachLocalTmux,
		Command: []string{"/opt/tmux", "-u", "-S", "/synthetic/pyre.sock", "set-option", "-g", "mouse", "on", ";", "attach-session", "-t", "=run_ses_away", "-r"}}
	m.learnLocalTmux(away, d) // another machine's run
	if m.local.socket != "" {
		t.Fatalf("learnt from another machine's run: %q", m.local.socket)
	}
	d.RunID, d.Command[11] = "run_ses_here", "=other"
	m.learnLocalTmux(here, d) // not named by its run
	if m.local.socket != "" {
		t.Fatalf("learnt from a target not named by the run: %q", m.local.socket)
	}
	d.Command[11] = "=run_ses_here"
	m.learnLocalTmux(here, d)
	if m.local.socket != "/synthetic/pyre.sock" || m.local.bin != "/opt/tmux" {
		t.Fatalf("learnt %q %q", m.local.bin, m.local.socket)
	}
}

// A local session is opened from its tmux session without the hub, and
// its first frame is the captured pane; a run with no tmux session here
// falls back to sessions.attach and stays on the hub for a while.
func TestRequestTermLocalCaptureAndFallback(t *testing.T) {
	bin, sock := isolatedTmux(t, "run_ses_here")
	api := &countingAttach{d: contracts.AttachDescriptor{RunID: "run_ses_here", Mechanism: contracts.AttachReadOnly, ReadOnly: true, Notes: "synthetic snapshot"}}
	m := localModel(t, api, sock)
	m.local.bin = bin
	var started []contracts.AttachDescriptor
	m.opts.StartTerm = func(d contracts.AttachDescriptor, cols, rows int) (TermConn, error) {
		started = append(started, d)
		return newNopConn(), nil
	}
	here, _ := m.session("ses_here")
	tv := m.termViewFor(here)
	// The shell prints its line asynchronously.
	deadline := time.Now().Add(5 * time.Second)
	for {
		lt, _ := m.localFor(here)
		b, _ := lt.capture(t.Context(), true)
		if strings.Contains(string(b), "synthetic local screen") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	msg := m.requestTerm(here, tv, termReadOnly)().(termAttachMsg)
	if !msg.local || msg.err != nil || api.n() != 0 {
		t.Fatalf("local request: local=%v err=%v hub attaches=%d", msg.local, msg.err, api.n())
	}
	m.onTermAttach(msg)
	if len(started) != 1 || started[0].Command[0] != bin || !started[0].ReadOnly {
		t.Fatalf("started %+v", started)
	}
	if live, _ := tv.live(); !live || !strings.Contains(tv.text(), "synthetic local screen of run_ses_here") {
		t.Fatalf("live %v, screen:\n%s", live, tv.text())
	}
	if m.LocalViews() != 1 {
		t.Fatalf("LocalViews = %d", m.LocalViews())
	}

	// The tmux session is gone: the hub answers, and is asked next time too.
	_ = exec.Command(bin, "-S", sock, "kill-session", "-t", "=run_ses_here").Run()
	tv.closeConn()
	msg = m.requestTerm(here, tv, termReadOnly)().(termAttachMsg)
	if msg.local || msg.localBad != "run_ses_here" || api.n() != 1 {
		t.Fatalf("fallback: local=%v bad=%q hub attaches=%d", msg.local, msg.localBad, api.n())
	}
	m.onTermAttach(msg)
	if _, ok := m.localFor(here); ok {
		t.Fatal("a run that failed locally is tried locally again at once")
	}
	m.requestTerm(here, tv, termReadOnly)()
	if api.n() != 2 {
		t.Fatalf("hub attaches = %d, want 2", api.n())
	}
}

// Typing never takes the local path: the writable attach and its lease
// still come from the hub.
func TestInteractiveTermAlwaysAsksTheHub(t *testing.T) {
	bin, sock := isolatedTmux(t, "run_ses_here")
	api := &countingAttach{d: contracts.AttachDescriptor{RunID: "run_ses_here", Mechanism: contracts.AttachReadOnly, ReadOnly: true}}
	m := localModel(t, api, sock)
	m.local.bin = bin
	here, _ := m.session("ses_here")
	msg := m.requestTerm(here, m.termViewFor(here), termInteractive)().(termAttachMsg)
	if msg.local || api.n() != 1 {
		t.Fatalf("interactive: local=%v hub attaches=%d", msg.local, api.n())
	}
}

func TestScreenBytesClipsAndTrims(t *testing.T) {
	b := string(screenBytes([]byte("\x1b[31mred line that is long\x1b[0m\nsecond\n\n  \n"), 8))
	if !strings.HasPrefix(b, "\x1b[m\x1b[H\x1b[2J\x1b[2;1H") || strings.Contains(b, "is long") || strings.HasSuffix(b, "\r\n") || strings.Count(b, "\r\n") != 1 {
		t.Fatalf("%q", b)
	}
	tv := newTermView("ses_x", "run_x", 8, 4, nil)
	defer tv.close()
	tv.seed([]byte("one\ntwo\n"))
	if got := tv.text(); !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Fatalf("seeded screen %q", got)
	}
	if _, _, drawn := tv.status(); !drawn {
		t.Fatal("a seeded screen does not count as drawn")
	}
}

// The Sessions pane reads no preview for a session on this machine, nor
// for one whose terminal screen it already draws; it still does for a
// remote session with nothing on screen.
func TestPaneWantsPreview(t *testing.T) {
	sock := fakeSocket(t)
	m := localModel(t, nil, sock)
	m.local.bin = "/usr/bin/tmux"
	here, _ := m.session("ses_here")
	away, _ := m.session("ses_away")
	if m.paneWantsPreview(here) {
		t.Fatal("a local session reads the hub's preview")
	}
	if !m.paneWantsPreview(away) {
		t.Fatal("a remote session without a screen reads no preview")
	}
	tv, _ := warmTerm(m, "ses_away", time.Now())
	tv.runID = away.Run.RunID
	if m.paneWantsPreview(away) {
		t.Fatal("a remote session whose screen is drawn still reads the preview")
	}
}

// The Activity section (finder, digest) of a session here shows its local
// capture while the hub's preview loads, never "loading preview…".
func TestActivityShowsLocalCaptureWhileLoading(t *testing.T) {
	sock := fakeSocket(t)
	m := localModel(t, nil, sock)
	m.local.bin = "/usr/bin/tmux"
	here, _ := m.session("ses_here")
	m.loads["preview:ses_here"] = &loadState{inflight: true, started: time.Now().Add(-time.Second)}
	out := strings.Join(m.activityLines(here, 120, 20), "\n")
	if strings.Contains(out, "loading preview") {
		t.Fatalf("a local session says loading:\n%s", out)
	}
	m.loads["localcap:ses_here"] = &loadState{val: "synthetic captured prompt $", have: true, seq: 1}
	if out := strings.Join(m.activityLines(here, 120, 20), "\n"); !strings.Contains(out, "synthetic captured prompt") {
		t.Fatalf("the local capture is not shown:\n%s", out)
	}
	away, _ := m.session("ses_away")
	m.loads["preview:ses_away"] = &loadState{inflight: true, started: time.Now()}
	if out := strings.Join(m.activityLines(away, 120, 20), "\n"); strings.Contains(out, "loading preview") {
		t.Fatalf("a read that just started says loading:\n%s", out)
	}
	m.loads["preview:ses_away"].started = time.Now().Add(-loadingAfter)
	if out := strings.Join(m.activityLines(away, 120, 20), "\n"); !strings.Contains(out, "loading preview") {
		t.Fatalf("a read running %s does not say loading:\n%s", loadingAfter, out)
	}
}
