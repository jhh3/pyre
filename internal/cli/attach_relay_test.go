package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/terminal"
)

// fakeRelay is a hub terminal relay (/v1/terminal) speaking the real wire
// protocol: auth frame, binary terminal bytes both ways, resize frames.
type fakeRelay struct {
	srv *httptest.Server

	mu      sync.Mutex
	auth    terminal.Frame
	input   []byte
	resizes [][2]int
	closed  bool
}

func newFakeRelay(t *testing.T) *fakeRelay {
	f := &fakeRelay{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		_, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var auth terminal.Frame
		_ = json.Unmarshal(b, &auth)
		f.mu.Lock()
		f.auth = auth
		f.mu.Unlock()
		_ = ws.Write(ctx, websocket.MessageBinary, []byte("SYNTH-SCREEN\r\n"))
		for {
			typ, b, err := ws.Read(ctx)
			if err != nil {
				f.mu.Lock()
				f.closed = true
				f.mu.Unlock()
				return
			}
			f.mu.Lock()
			if typ == websocket.MessageBinary {
				f.input = append(f.input, b...)
			} else {
				var fr terminal.Frame
				if json.Unmarshal(b, &fr) == nil && fr.Type == terminal.FrameResize {
					f.resizes = append(f.resizes, [2]int{fr.Cols, fr.Rows})
				}
			}
			f.mu.Unlock()
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) snapshot() (terminal.Frame, string, [][2]int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth, string(f.input), append([][2]int(nil), f.resizes...), f.closed
}

// relayAPI is a hub whose session runs on a node with no ssh alias, with a
// terminal relay (DialTerminal, like *client.Client).
type relayAPI struct {
	c.API
	relay   *fakeRelay
	sshDesc bool // native requests succeed with an ssh descriptor
	mu      sync.Mutex
	calls   []string
}

func (f *relayAPI) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *relayAPI) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

func (f *relayAPI) GetSession(ctx context.Context, req c.GetRequest) (c.SessionView, error) {
	return c.SessionView{Session: c.Session{SessionID: req.ID}, Run: &c.Run{RunID: "run_x", NodeID: "node_synth-win"}}, nil
}

func (f *relayAPI) AttachSession(ctx context.Context, req c.AttachRequest) (c.AttachDescriptor, error) {
	if len(req.Preferred) == 0 || req.Preferred[0] != c.AttachWebTerminal {
		f.record("attach native")
		if f.sshDesc {
			return c.AttachDescriptor{RunID: "run_x", Mechanism: c.AttachSSHTmux, Command: []string{"ssh", "-t", "synth-host", "true"}}, nil
		}
		return c.AttachDescriptor{}, c.Errorf(c.ErrUnavailable, "remote attach needs an ssh alias for node node_synth-win (not configured)")
	}
	f.record("attach web readonly=" + map[bool]string{true: "yes", false: "no"}[req.ReadOnly])
	return c.AttachDescriptor{RunID: "run_x", NodeID: "node_synth-win", Mechanism: c.AttachWebTerminal, URL: c.PathTerminal,
		Ticket: "pyt_synthetic-relay-ticket", ReadOnly: req.ReadOnly}, nil
}

func (f *relayAPI) AcquireLease(ctx context.Context, req c.LeaseRequest) (c.InputLease, error) {
	f.record("acquire " + req.RunID)
	return c.InputLease{LeaseID: "lease_test", RunID: req.RunID, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *relayAPI) ReleaseLease(ctx context.Context, req c.LeaseReleaseRequest) (c.InputLease, error) {
	f.record("release " + req.LeaseID)
	return c.InputLease{LeaseID: req.LeaseID}, nil
}

func (f *relayAPI) DialTerminal(ctx context.Context, d c.AttachDescriptor, cols, rows int, fit bool) (*terminal.ClientConn, error) {
	u := "ws" + strings.TrimPrefix(f.relay.srv.URL, "http") + d.URL
	return terminal.Dial(ctx, u, d.Ticket, terminal.DialOptions{Cols: cols, Rows: rows, ReadOnly: d.ReadOnly, Fit: fit})
}

// fakeConsole is the local terminal: raw mode recorded, size fixed,
// resizes injected by the test.
type fakeConsole struct {
	resize   chan [2]int
	mu       sync.Mutex
	raw      bool
	restored bool
	watching bool
}

func (f *fakeConsole) MakeRaw() (func(), error) {
	f.mu.Lock()
	f.raw = true
	f.mu.Unlock()
	return func() { f.mu.Lock(); f.restored = true; f.mu.Unlock() }, nil
}
func (f *fakeConsole) Size() (int, int) { return 120, 40 }
func (f *fakeConsole) WatchResize(stop <-chan struct{}, fn func(cols, rows int)) {
	f.mu.Lock()
	f.watching = true
	f.mu.Unlock()
	for {
		select {
		case <-stop:
			return
		case r := <-f.resize:
			fn(r[0], r[1])
		}
	}
}
func (f *fakeConsole) isWatching() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.watching }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A node with no ssh alias: `pyre attach` falls back to the hub's relay,
// writable under the input lease, with the terminal raw, its size sent on
// start and on resize, and Ctrl-b d detaching (never sent to the session).
func TestAttachFallsBackToRelayWithoutSSHAlias(t *testing.T) {
	relay := newFakeRelay(t)
	api := &relayAPI{relay: relay}
	con := &fakeConsole{resize: make(chan [2]int, 1)}
	pr, pw := io.Pipe()
	defer pw.Close()
	e := newEnv(t, nil)
	done := make(chan result, 1)
	go func() { done <- run(t, e, &App{API: api, Console: con, Stdin: pr}, "attach", "ses_x") }()

	waitUntil(t, "auth frame", func() bool { a, _, _, _ := relay.snapshot(); return a.Type == terminal.FrameAuth })
	if a, _, _, _ := relay.snapshot(); a.Ticket != "pyt_synthetic-relay-ticket" || a.Cols != 120 || a.Rows != 40 || a.Fit {
		t.Fatalf("auth frame: %+v (want the ticket and the terminal's 120x40, not fit)", a)
	}
	if _, err := pw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "input relayed", func() bool { _, in, _, _ := relay.snapshot(); return in == "hello" })
	waitUntil(t, "resize watcher", con.isWatching)
	con.resize <- [2]int{100, 30}
	waitUntil(t, "resize frame", func() bool {
		_, _, rs, _ := relay.snapshot()
		return len(rs) == 1 && rs[0] == [2]int{100, 30}
	})
	// Ctrl-b Ctrl-b sends one Ctrl-b, Ctrl-b x passes through, Ctrl-b d detaches.
	if _, err := pw.Write([]byte("\x02\x02\x02x\x02dIGNORED")); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-b d did not detach")
	}
	expectCode(t, r, 0)
	waitUntil(t, "stream closed", func() bool { _, _, _, closed := relay.snapshot(); return closed })
	if _, in, _, _ := relay.snapshot(); in != "hello\x02\x02x" {
		t.Fatalf("session input %q (the detach keys must not reach it)", in)
	}
	if got := api.log(); got != "attach native,acquire run_x,attach web readonly=no,release lease_test" {
		t.Fatalf("attach/lease protocol: %s", got)
	}
	if !strings.Contains(r.stdout, "SYNTH-SCREEN") {
		t.Fatalf("terminal bytes not shown: %s", r)
	}
	if !strings.Contains(r.stderr, "Ctrl-b d") || !strings.Contains(r.stderr, "terminal relay") || strings.Contains(r.stdout+r.stderr, "pyt_synthetic") {
		t.Fatalf("attach message (detach key, never the ticket): %s", r)
	}
	if !con.raw || !con.restored {
		t.Fatalf("raw mode %v restored %v", con.raw, con.restored)
	}
}

// --read-only: a read-only relay stream, no lease, input never sent, no
// resize; --relay: the relay even when ssh would work.
func TestAttachRelayReadOnlyAndForced(t *testing.T) {
	relay := newFakeRelay(t)
	api := &relayAPI{relay: relay}
	con := &fakeConsole{resize: make(chan [2]int, 1)}
	pr, pw := io.Pipe()
	defer pw.Close()
	e := newEnv(t, nil)
	done := make(chan result, 1)
	go func() {
		done <- run(t, e, &App{API: api, Console: con, Stdin: pr}, "attach", "ses_x", "--read-only")
	}()
	waitUntil(t, "auth frame", func() bool { a, _, _, _ := relay.snapshot(); return a.Type == terminal.FrameAuth })
	if _, err := pw.Write([]byte("typed\x02d")); err != nil {
		t.Fatal(err)
	}
	r := <-done
	expectCode(t, r, 0)
	if got := api.log(); got != "attach native,attach web readonly=yes" {
		t.Fatalf("read-only attach protocol: %s", got)
	}
	if _, in, rs, _ := relay.snapshot(); in != "" || len(rs) != 0 || con.isWatching() {
		t.Fatalf("read-only stream sent input %q / resizes %v", in, rs)
	}
	if !strings.Contains(r.stderr, "read-only") {
		t.Fatalf("attach message: %s", r)
	}

	relay = newFakeRelay(t)
	api = &relayAPI{relay: relay, sshDesc: true}
	pr2, pw2 := io.Pipe()
	defer pw2.Close()
	done = make(chan result, 1)
	go func() {
		done <- run(t, e, &App{API: api, Console: &fakeConsole{resize: make(chan [2]int, 1)}, Stdin: pr2}, "attach", "ses_x", "--relay")
	}()
	waitUntil(t, "auth frame", func() bool { a, _, _, _ := relay.snapshot(); return a.Type == terminal.FrameAuth })
	if _, err := pw2.Write([]byte("\x02d")); err != nil {
		t.Fatal(err)
	}
	r = <-done
	expectCode(t, r, 0)
	if got := api.log(); got != "acquire run_x,attach web readonly=no,release lease_test" {
		t.Fatalf("--relay must not try ssh: %s", got)
	}

	// Without a relay (no direct hub connection), --relay is refused.
	r = run(t, e, &App{API: &attachAPI{t: t}}, "attach", "ses_x", "--relay")
	if r.code == 0 || !strings.Contains(r.stderr, "--relay needs a direct hub connection") {
		t.Fatalf("--relay without a relay: %s", r)
	}
}
