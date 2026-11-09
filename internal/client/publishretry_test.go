package client

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
)

// fastPublishRetries shrinks Publish's outage backoff for a test.
func fastPublishRetries(t *testing.T) {
	t.Helper()
	old := publishBackoff
	publishBackoff = func(int) time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() { publishBackoff = old })
}

// noticeLog collects WithNotices output.
type noticeLog struct {
	mu   sync.Mutex
	msgs []string
}

func (n *noticeLog) ctx() context.Context {
	return WithNotices(context.Background(), func(m string) { n.mu.Lock(); n.msgs = append(n.msgs, m); n.mu.Unlock() })
}

func (n *noticeLog) joined() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.Join(n.msgs, "\n")
}

// restartingHub serves api on a fixed loopback address and can go away
// (closing every connection, as a hub process exiting does) and come back
// on the same address.
type restartingHub struct {
	t    *testing.T
	addr string
	h    http.Handler
	mu   sync.Mutex
	srv  *http.Server
}

func newRestartingHub(t *testing.T, h http.Handler) *restartingHub {
	r := &restartingHub{t: t, h: h}
	r.start("127.0.0.1:0")
	t.Cleanup(func() { r.stop() })
	return r
}

func (r *restartingHub) start(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		r.t.Error(err)
		return
	}
	srv := &http.Server{Handler: r.h, ReadHeaderTimeout: 5 * time.Second}
	r.mu.Lock()
	r.addr, r.srv = ln.Addr().String(), srv
	r.mu.Unlock()
	go srv.Serve(ln)
}

func (r *restartingHub) stop() {
	r.mu.Lock()
	srv := r.srv
	r.srv = nil
	r.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

// restart takes the hub down for down, then serves again on its address.
func (r *restartingHub) restart(down time.Duration) {
	r.stop()
	time.AfterFunc(down, func() { r.start(r.addr) })
}

func (r *restartingHub) url() string { return "http://" + r.addr }

// versions counts the versions fake has created (FinalizeUpload commits).
func (f *fake) versions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, u := range f.uploads {
		if u.done != nil {
			n++
		}
	}
	return n
}

// The hub goes away mid-upload (the connection drops, then nothing
// listens for a while) and comes back: the publish resumes and finishes
// with exactly one version. A second outage swallows FinalizeUpload's
// answer after the hub committed it: the replay under the same operation
// id returns that version instead of creating another.
func TestPublishRidesOutHubRestart(t *testing.T) {
	fastPublishRetries(t)
	f := &fake{}
	var chunks, finals atomic.Int32
	var hub *restartingHub
	inner := httpapi.NewServer(f, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner}), httpapi.Options{StreamWait: time.Second})
	hub = newRestartingHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && chunks.Add(1) == 3:
			// Mid-upload: the process exits with this chunk in flight.
			hub.restart(300 * time.Millisecond)
			panic(http.ErrAbortHandler)
		case r.URL.Path == c.RPCPath(c.OpFinalizeUpload) && finals.Add(1) == 1:
			inner.ServeHTTP(httptestDiscard{}, r) // committed ...
			hub.restart(200 * time.Millisecond)
			panic(http.ErrAbortHandler) // ... but the answer is lost
		}
		inner.ServeHTTP(w, r)
	}))
	cl := New(hub.url(), "tok", fastOpts())
	data := bytes.Repeat([]byte("synthetic-"), 2000) // 20 KB, 7 chunks
	var notes noticeLog
	rec, err := cl.Publish(notes.ctx(), bytes.NewReader(data), PublishMeta{Title: "restart", Filename: "r.bin", ChunkSize: 3000, RetryWindow: 10 * time.Second})
	if err != nil {
		t.Fatalf("publish through a hub restart: %v\nnotices:\n%s", err, notes.joined())
	}
	if rec.Version.Digest != digestOf(data) || rec.Version.Size != int64(len(data)) {
		t.Fatalf("receipt %+v", rec.Version)
	}
	if n := f.versions(); n != 1 {
		t.Fatalf("%d versions, want exactly 1", n)
	}
	if n := len(f.uploads); n != 1 {
		t.Fatalf("%d uploads begun, want 1", n)
	}
	got := notes.joined()
	for _, w := range []string{"publish: upload at", "hub restarting? retrying in", "finalize failed", "succeeded after"} {
		if !strings.Contains(got, w) {
			t.Fatalf("notices lack %q:\n%s", w, got)
		}
	}
}

// httptestDiscard is a ResponseWriter that drops the answer.
type httptestDiscard struct{}

func (httptestDiscard) Header() http.Header         { return http.Header{} }
func (httptestDiscard) Write(b []byte) (int, error) { return len(b), nil }
func (httptestDiscard) WriteHeader(int)             {}

// 503 answers to upload chunks (a proxy in front of a restarting hub, or
// the hub's own interrupted-request answer) outlast the client's attempt
// budget and are still ridden out.
func TestPublishRetries503(t *testing.T) {
	fastPublishRetries(t)
	f := &fake{}
	var refused atomic.Int32
	inner := httpapi.NewServer(f, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner}), httpapi.Options{StreamWait: time.Second})
	hub := newRestartingHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && refused.Add(1) <= 12 {
			httpapi.WriteError(w, httpapi.ErrInterrupted())
			return
		}
		inner.ServeHTTP(w, r)
	}))
	cl := New(hub.url(), "tok", fastOpts())
	var notes noticeLog
	data := []byte("synthetic artifact bytes")
	if _, err := cl.Publish(notes.ctx(), bytes.NewReader(data), PublishMeta{Title: "503", Filename: "a.txt", RetryWindow: 10 * time.Second}); err != nil {
		t.Fatalf("publish through 503s: %v", err)
	}
	if f.versions() != 1 || !strings.Contains(notes.joined(), "request interrupted") {
		t.Fatalf("versions %d, notices:\n%s", f.versions(), notes.joined())
	}
}

// A hub that predates the interrupted-request answer says "internal
// error" for an upload its shutdown cut off: a few are retried; a
// persistent one fails after publishMaxInternal with a message naming it.
func TestPublishRetriesFewInternalErrors(t *testing.T) {
	fastPublishRetries(t)
	f := &fake{}
	var fails atomic.Int32
	limit := int32(2)
	inner := httpapi.NewServer(f, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner}), httpapi.Options{StreamWait: time.Second})
	hub := newRestartingHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && fails.Add(1) <= atomic.LoadInt32(&limit) {
			httpapi.WriteError(w, context.Canceled) // the old hub's "internal error"
			return
		}
		inner.ServeHTTP(w, r)
	}))
	cl := New(hub.url(), "tok", fastOpts())
	if _, err := cl.Publish(context.Background(), strings.NewReader("synthetic"), PublishMeta{Title: "i", Filename: "i.txt", RetryWindow: 10 * time.Second}); err != nil {
		t.Fatalf("two internal errors then success: %v", err)
	}
	fails.Store(0)
	atomic.StoreInt32(&limit, 1000)
	_, err := cl.Publish(context.Background(), strings.NewReader("synthetic 2"), PublishMeta{Title: "i2", Filename: "i2.txt", RetryWindow: 10 * time.Second})
	if !c.IsCode(err, c.ErrInternal) || !strings.Contains(err.Error(), "times in a row with an internal error") || !strings.Contains(err.Error(), "--operation-id") {
		t.Fatalf("persistent internal error: %v", err)
	}
	if n := fails.Load(); n != publishMaxInternal+1 {
		t.Fatalf("%d attempts, want %d", n, publishMaxInternal+1)
	}
}

// A permanent 4xx (the file is larger than the hub accepts) fails at once
// with the hub's message; nothing is retried.
func TestPublishTooLargeFailsFast(t *testing.T) {
	fastPublishRetries(t)
	var begins atomic.Int32
	hub := newRestartingHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		begins.Add(1)
		httpapi.WriteError(w, c.Errorf(c.ErrPayloadTooLarge, "artifact size 367001600 exceeds the hub limit of 268435456 bytes"))
	}))
	cl := New(hub.url(), "tok", fastOpts())
	var notes noticeLog
	start := time.Now()
	_, err := cl.Publish(notes.ctx(), strings.NewReader("synthetic"), PublishMeta{Title: "big", Filename: "big.bin"})
	if !c.IsCode(err, c.ErrPayloadTooLarge) || !strings.Contains(err.Error(), "exceeds the hub limit") {
		t.Fatalf("too large: %v", err)
	}
	if begins.Load() != 1 || notes.joined() != "" || time.Since(start) > 2*time.Second {
		t.Fatalf("retried a permanent error: %d requests, notices %q, %v", begins.Load(), notes.joined(), time.Since(start))
	}
}

// When the hub stays away past the window the error says so, instead of
// a bare "internal error", and names the operation id that resumes it.
func TestPublishGivesUpWithClearError(t *testing.T) {
	fastPublishRetries(t)
	f := &fake{}
	inner := httpapi.NewServer(f, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner}), httpapi.Options{StreamWait: time.Second})
	var hub *restartingHub
	hub = newRestartingHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			hub.stop() // gone for good
			panic(http.ErrAbortHandler)
		}
		inner.ServeHTTP(w, r)
	}))
	cl := New(hub.url(), "tok", fastOpts())
	start := time.Now()
	_, err := cl.Publish(context.Background(), strings.NewReader("synthetic"), PublishMeta{Title: "gone", Filename: "g.txt", OperationID: "op_synth-gone", RetryWindow: 300 * time.Millisecond})
	if !c.IsCode(err, c.ErrUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
	for _, w := range []string{"hub unavailable (restarting?) after", "of retries", "upl_synth-gone", "--operation-id op_synth-gone"} {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error lacks %q: %v", w, err)
		}
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 10*time.Second {
		t.Fatalf("gave up after %v", d)
	}
}
