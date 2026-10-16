package hostproc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost"
	"github.com/jhh3/pyre/internal/runtime/termhost/internal/wire"
)

// fakeProc is a scripted program: tests write its output and read what it
// was sent.
type fakeProc struct {
	argv     []string
	env      []string
	outR     *io.PipeReader
	outW     *io.PipeWriter
	mu       sync.Mutex
	in       bytes.Buffer
	cols     int
	rows     int
	exitOnce sync.Once
	exitCh   chan *int
	termed   bool
}

func (p *fakeProc) Read(b []byte) (int, error) { return p.outR.Read(b) }
func (p *fakeProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.in.Write(b)
}
func (p *fakeProc) Resize(c, r int) error {
	p.mu.Lock()
	p.cols, p.rows = c, r
	p.mu.Unlock()
	return nil
}
func (p *fakeProc) Pid() int    { return 4242 }
func (p *fakeProc) Wait() *int  { return <-p.exitCh }
func (p *fakeProc) Exited()     {}
func (p *fakeProc) exit(c *int) { p.exitOnce.Do(func() { p.exitCh <- c; close(p.exitCh) }) }
func (p *fakeProc) Terminate(time.Duration) {
	p.mu.Lock()
	p.termed = true
	p.mu.Unlock()
	p.exit(nil)
}
func (p *fakeProc) Kill()         { p.exit(nil) }
func (p *fakeProc) Close() error  { return p.outW.Close() }
func (p *fakeProc) emit(s string) { _, _ = p.outW.Write([]byte(s)) }
func (p *fakeProc) input() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.in.String()
}

type fakes struct {
	mu    sync.Mutex
	procs []*fakeProc
}

func (f *fakes) start(argv []string, cwd string, env []string, cols, rows int) (proc, error) {
	if argv[0] == "missing" {
		return nil, errors.New("not found")
	}
	r, w := io.Pipe()
	p := &fakeProc{argv: argv, env: env, outR: r, outW: w, cols: cols, rows: rows, exitCh: make(chan *int, 1)}
	f.mu.Lock()
	f.procs = append(f.procs, p)
	f.mu.Unlock()
	return p, nil
}

func (f *fakes) last() *fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[len(f.procs)-1]
}

func shortDir(t *testing.T) string {
	t.Helper()
	base := "/tmp" // short: socket paths are limited to ~100 bytes
	if runtime.GOOS == "windows" {
		base = ""
	}
	d, err := os.MkdirTemp(base, "th")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// startFake runs an in-process host with fake programs.
func startFake(t *testing.T) (*termhost.Host, *fakes, *Server) {
	t.Helper()
	f := &fakes{}
	sock := filepath.Join(shortDir(t), "th.sock")
	srv := &Server{Socket: sock, Env: []string{"PATH=/bin", "KEEP=1", "DROP=1"}, start: f.start}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(srv.Shutdown)
	h, err := termhost.New(sock)
	if err != nil {
		t.Fatal(err)
	}
	h.Exe = "/nonexistent/pyred" // EnsureServer must never need to spawn here
	return h, f, srv
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSessionLifecycle(t *testing.T) {
	h, f, _ := startFake(t)
	ctx := context.Background()
	spec := sessionhost.SessionSpec{Name: "run1", Argv: []string{"prog", "a"}, Env: map[string]string{"SECRET": "s3"}, Unset: []string{"DROP"}, Width: 100, Height: 30}
	if err := h.NewSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := h.NewSession(ctx, spec); err == nil || !strings.Contains(err.Error(), "duplicate session") {
		t.Fatalf("duplicate: %v", err)
	}
	p := f.last()
	env := strings.Join(p.env, "\n")
	if !strings.Contains(env, "SECRET=s3") || !strings.Contains(env, "KEEP=1") || strings.Contains(env, "DROP=") {
		t.Fatalf("child env %q", p.env)
	}
	if ok, err := h.HasSession(ctx, "run1"); err != nil || !ok {
		t.Fatalf("has: %v %v", ok, err)
	}
	if ok, err := h.HasSession(ctx, "nope"); err != nil || ok {
		t.Fatalf("has nope: %v %v", ok, err)
	}
	if names, _ := h.ListSessions(ctx); len(names) != 1 || names[0] != "run1" {
		t.Fatalf("list %v", names)
	}
	p.emit("hello \x1b[31mred\x1b[0m world\r\nsecond")
	waitFor(t, "capture", func() bool {
		s, _ := h.CapturePane(ctx, "run1", sessionhost.CaptureOptions{})
		return strings.HasPrefix(s, "hello red world\nsecond\n")
	})
	esc, _ := h.CapturePane(ctx, "run1", sessionhost.CaptureOptions{Escapes: true})
	if !strings.Contains(esc, "\x1b[") || !strings.Contains(esc, "red") {
		t.Fatalf("escapes capture %q", esc[:60])
	}
	pi, err := h.Pane(ctx, "run1")
	if err != nil || pi.Dead || pi.PID != 4242 || pi.Width != 100 || pi.Height != 30 || pi.CurrentCommand != "prog" {
		t.Fatalf("pane %+v %v", pi, err)
	}
	// Keys: plain, then application cursor mode.
	if err := h.SendKeys(ctx, "run1", sessionhost.KeyUp, sessionhost.KeyEnter); err != nil {
		t.Fatal(err)
	}
	p.emit("\x1b[?1h")
	waitFor(t, "DECCKM", func() bool {
		_ = h.SendKeys(ctx, "run1", sessionhost.KeyDown)
		return strings.Contains(p.input(), "\x1bOB")
	})
	if !strings.HasPrefix(p.input(), "\x1b[A\r") {
		t.Fatalf("keys %q", p.input())
	}
	if err := h.SendKeys(ctx, "run1", sessionhost.Key("x")); err == nil {
		t.Fatal("literal key accepted")
	}
	// Paste: LF -> CR, bracketed once the program asks.
	_ = h.Paste(ctx, "run1", "b1", []byte("a\nb"))
	waitFor(t, "paste", func() bool { return strings.HasSuffix(p.input(), "a\rb") })
	p.emit("\x1b[?2004h")
	waitFor(t, "bracketed paste", func() bool {
		_ = h.Paste(ctx, "run1", "b2", []byte("z"))
		return strings.Contains(p.input(), "\x1b[200~z\x1b[201~")
	})
	// Exit keeps the session (remain-on-exit) with its status.
	code := 3
	p.exit(&code)
	waitFor(t, "dead pane", func() bool {
		pi, _ := h.Pane(ctx, "run1")
		return pi.Dead && pi.ExitStatus != nil && *pi.ExitStatus == 3
	})
	// Respawn starts a new program in the same session.
	if err := h.RespawnPane(ctx, "run1", sessionhost.SessionSpec{Argv: []string{"prog2"}}); err != nil {
		t.Fatal(err)
	}
	if pi, _ := h.Pane(ctx, "run1"); pi.Dead || pi.CurrentCommand != "prog2" {
		t.Fatalf("respawned pane %+v", pi)
	}
	if err := h.TerminateSession(ctx, "run1", time.Second); err != nil {
		t.Fatal(err)
	}
	if !f.last().termed {
		t.Fatal("terminate did not reach the program")
	}
	if ok, _ := h.HasSession(ctx, "run1"); ok {
		t.Fatal("session survived terminate")
	}
	if _, err := h.Pane(ctx, "run1"); !errors.Is(err, sessionhost.ErrNoSession) {
		t.Fatalf("pane of gone session: %v", err)
	}
	if err := h.KillSession(ctx, "run1"); err != nil {
		t.Fatalf("kill of gone session: %v", err)
	}
	if err := h.NewSession(ctx, sessionhost.SessionSpec{Name: "bad", Argv: []string{"missing"}}); err == nil {
		t.Fatal("start failure not reported")
	}
	if ok, _ := h.HasSession(ctx, "bad"); ok {
		t.Fatal("failed session kept")
	}
}

// sink drains an attached stream in the background.
type sink struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	sizes [][2]int
	err   error
	done  chan struct{}
}

func drain(c *termhost.Conn) *sink {
	s := &sink{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		b := make([]byte, 4096)
		for {
			n, err := c.Read(b)
			s.mu.Lock()
			s.buf.Write(b[:n])
			if cols, rows, ok := c.TakeWindowSize(); ok {
				s.sizes = append(s.sizes, [2]int{cols, rows})
			}
			s.err = err
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *sink) has(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.buf.String(), want)
}

func (s *sink) hasSize(c, r int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, z := range s.sizes {
		if z == [2]int{c, r} {
			return true
		}
	}
	return false
}

func TestAttachClients(t *testing.T) {
	h, f, _ := startFake(t)
	ctx := context.Background()
	if err := h.NewSession(ctx, sessionhost.SessionSpec{Name: "s", Argv: []string{"prog"}, Width: 80, Height: 24}); err != nil {
		t.Fatal(err)
	}
	p := f.last()
	p.emit("before attach")
	waitFor(t, "output", func() bool {
		s, _ := h.CapturePane(ctx, "s", sessionhost.CaptureOptions{})
		return strings.Contains(s, "before attach")
	})

	// A read-only viewer: session size first, then a repaint; it never
	// resizes and its input is dropped.
	ro, err := termhost.DialAttach(ctx, termhost.DialOptions{Socket: h.Socket, Name: "s", ReadOnly: true, Cols: 40, Rows: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if c, r := ro.WindowSize(); c != 80 || r != 24 {
		t.Fatalf("viewer size %d %d", c, r)
	}
	view := drain(ro)
	waitFor(t, "repaint", func() bool { return view.has("before attach") && view.hasSize(80, 24) })
	_, _ = ro.Write([]byte("typed by viewer"))
	cs, _ := h.ListClients(ctx, "s")
	if len(cs) != 1 || !cs[0].ReadOnly || sessionhost.WritableClients(cs) != 0 {
		t.Fatalf("clients %+v", cs)
	}

	// Terminal queries are answered by the emulator while no writer is
	// attached.
	p.emit("\x1b[c")
	waitFor(t, "DA1 reply", func() bool { return strings.Contains(p.input(), "\x1b[?") })

	// A writer sizes the session (newest writer wins) and types.
	rw, err := termhost.DialAttach(ctx, termhost.DialOptions{Socket: h.Socket, Name: "s", Cols: 120, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	writer := drain(rw)
	if pi, _ := h.Pane(ctx, "s"); pi.Width != 120 || pi.Height != 40 {
		t.Fatalf("writer did not size the session: %+v", pi)
	}
	p.mu.Lock()
	pc, pr := p.cols, p.rows
	p.mu.Unlock()
	if pc != 120 || pr != 40 {
		t.Fatalf("pty size %dx%d", pc, pr)
	}
	if _, err := rw.Write([]byte("typed by writer")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "writer input", func() bool { return strings.Contains(p.input(), "typed by writer") })
	if strings.Contains(p.input(), "typed by viewer") {
		t.Fatal("read-only input reached the program")
	}
	cs, _ = h.ListClients(ctx, "s")
	if sessionhost.WritableClients(cs) != 1 {
		t.Fatalf("clients %+v", cs)
	}
	waitFor(t, "viewer told the new size", func() bool { return view.hasSize(120, 40) })
	// Writer resize applies; output reaches both.
	if err := rw.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resize", func() bool { pi, _ := h.Pane(ctx, "s"); return pi.Width == 100 && pi.Height == 30 })
	p.emit("broadcast!")
	waitFor(t, "broadcast", func() bool { return writer.has("broadcast!") && view.has("broadcast!") })
	rw.Close()
	waitFor(t, "writer detached", func() bool { cs, _ := h.ListClients(ctx, "s"); return len(cs) == 1 })

	// Killing the session ends attached streams with a reason.
	if err := h.KillSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-view.done:
	case <-time.After(3 * time.Second):
		t.Fatal("viewer stream not ended")
	}
	if ro.ExitReason() != "session killed" {
		t.Fatalf("exit reason %q (%v)", ro.ExitReason(), view.err)
	}
}

func TestDetachFilter(t *testing.T) {
	var f detachFilter
	out, d := f.feed([]byte("ab\x02\x02c\x02x"))
	if d || string(out) != "ab\x02c\x02x" {
		t.Fatalf("%q %v", out, d)
	}
	out, d = f.feed([]byte("q\x02"))
	if d || string(out) != "q" {
		t.Fatalf("%q %v", out, d)
	}
	out, d = f.feed([]byte("d tail"))
	if !d || len(out) != 0 {
		t.Fatalf("%q %v", out, d)
	}
}

func TestChildEnv(t *testing.T) {
	got := childEnv([]string{"A=1", "B=2", "C=3", "TMUX=x"}, map[string]string{"B": "new", "D": "4"}, []string{"C", "D"})
	want := "A=1 TMUX=x B=new D=4"
	if strings.Join(got, " ") != want {
		t.Fatalf("%v", got)
	}
	if b := wire.HostBaseEnv([]string{"TMUX=1", "TMUX_PANE=%1", "X=1"}); len(b) != 1 || b[0] != "X=1" {
		t.Fatalf("%v", b)
	}
}

// ping asks the host on h's socket for its ping response.
func ping(ctx context.Context, h *termhost.Host) (wire.Response, error) {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := d.DialContext(dctx, "unix", h.Socket)
	if err != nil {
		return wire.Response{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if err := wire.WriteLine(c, wire.Request{V: wire.ProtocolVersion, Op: wire.OpPing}); err != nil {
		return wire.Response{}, err
	}
	var resp wire.Response
	if err := wire.ReadLine(bufio.NewReader(c), 1<<20, &resp); err != nil {
		return wire.Response{}, err
	}
	return resp, nil
}
