package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
)

// fake implements the subset of contracts.API exercised here.
type fake struct {
	c.API

	mu        sync.Mutex
	failSend  int // fail the next N SendMessage calls with unavailable
	sendOps   []string
	stored    map[string]c.MessageView // by operation id
	content   map[string][]byte
	badDigest bool
	events    []c.Event
	oldest    int64
	dropAfter int // close the event stream (via ctx) after N events per poll
	uploads   map[string]*fakeUpload
	chunkFail int // fail next N chunk uploads after writing (simulates lost response)
	nodes     int
}

type fakeUpload struct {
	req  c.BeginUploadRequest
	data []byte
	done *c.PublishReceipt
}

func (f *fake) WhoAmI(ctx context.Context) (c.Principal, error) {
	p, _ := c.PrincipalFrom(ctx)
	return p, nil
}

func (f *fake) SendMessage(ctx context.Context, req c.SendMessageRequest) (c.MessageView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendOps = append(f.sendOps, req.OperationID)
	if req.Body.Text == "conflict" {
		return c.MessageView{}, c.Errorf(c.ErrConflict, "nope")
	}
	if f.stored == nil {
		f.stored = map[string]c.MessageView{}
	}
	mv, ok := f.stored[req.OperationID]
	if !ok {
		mv = c.MessageView{Envelope: c.MessageEnvelope{MessageID: c.NewID(c.PrefixMessage), OperationID: req.OperationID, Body: req.Body}}
		f.stored[req.OperationID] = mv // committed before the (simulated) failure
	}
	if f.failSend > 0 {
		f.failSend--
		return c.MessageView{}, c.Errorf(c.ErrUnavailable, "simulated outage after commit")
	}
	return mv, nil
}

func (f *fake) ListNodes(ctx context.Context, req c.ListNodesRequest) (c.Page[c.Node], error) {
	start := 0
	if req.Cursor != "" {
		start, _ = strconv.Atoi(req.Cursor)
	}
	var p c.Page[c.Node]
	for i := start; i < f.nodes && i < start+3; i++ {
		p.Items = append(p.Items, c.Node{NodeID: "node_" + strconv.Itoa(i)})
	}
	if start+3 < f.nodes {
		p.NextCursor = strconv.Itoa(start + 3)
	}
	return p, nil
}

func digestOf(b []byte) c.Digest {
	s := sha256.Sum256(b)
	return c.Digest("sha256:" + hex.EncodeToString(s[:]))
}

func (f *fake) GetVersion(ctx context.Context, req c.GetRequest) (c.ArtifactVersion, error) {
	b, ok := f.content[req.ID]
	if !ok {
		return c.ArtifactVersion{}, c.Errorf(c.ErrNotFound, "no such version")
	}
	d := digestOf(b)
	if f.badDigest {
		d = digestOf([]byte("other"))
	}
	return c.ArtifactVersion{VersionID: req.ID, Size: int64(len(b)), Digest: d, MediaType: "text/plain", Filename: "x.txt"}, nil
}

type rsc struct{ *bytes.Reader }

func (rsc) Close() error { return nil }

func (f *fake) OpenContent(ctx context.Context, id string) (io.ReadCloser, c.ArtifactVersion, error) {
	v, err := f.GetVersion(ctx, c.GetRequest{ID: id})
	if err != nil {
		return nil, v, err
	}
	return rsc{bytes.NewReader(f.content[id])}, v, nil
}

func (f *fake) ListEvents(ctx context.Context, req c.ListEventsRequest) (c.ListEventsResponse, error) {
	f.mu.Lock()
	if req.AfterCursor < f.oldest {
		f.mu.Unlock()
		return c.ListEventsResponse{}, c.Errorf(c.ErrResetRequired, "stale").WithDetail("snapshot_cursor", 99)
	}
	var out []c.Event
	for _, e := range f.events {
		if e.Cursor > req.AfterCursor {
			out = append(out, e)
		}
	}
	drop := f.dropAfter
	f.mu.Unlock()
	if drop > 0 && len(out) > drop {
		out = out[:drop]
		// The server sends these then the connection dies on the next poll.
		return c.ListEventsResponse{Events: out, NextCursor: out[len(out)-1].Cursor}, nil
	}
	if len(out) == 0 {
		select {
		case <-ctx.Done():
			return c.ListEventsResponse{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		return c.ListEventsResponse{NextCursor: req.AfterCursor}, nil
	}
	return c.ListEventsResponse{Events: out, NextCursor: out[len(out)-1].Cursor}, nil
}

func (f *fake) BeginUpload(ctx context.Context, req c.BeginUploadRequest) (c.Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploads == nil {
		f.uploads = map[string]*fakeUpload{}
	}
	id := "upl_" + strings.TrimPrefix(req.OperationID, "op_")
	if _, ok := f.uploads[id]; !ok {
		f.uploads[id] = &fakeUpload{req: req}
	}
	return c.Upload{UploadID: id, Size: req.Size, Digest: req.Digest, Received: 0, State: "open"}, nil
}

func (f *fake) UploadChunk(ctx context.Context, id string, off int64, r io.Reader) (c.Upload, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return c.Upload{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.uploads[id]
	if off > int64(len(u.data)) {
		return c.Upload{}, c.Errorf(c.ErrConflict, "gap").WithDetail("received", len(u.data))
	}
	if end := off + int64(len(b)); end > int64(len(u.data)) {
		u.data = append(u.data[:off], b...)
	}
	if f.chunkFail > 0 {
		f.chunkFail--
		return c.Upload{}, c.Errorf(c.ErrUnavailable, "lost response")
	}
	return c.Upload{UploadID: id, Received: int64(len(u.data)), Size: u.req.Size, State: "open"}, nil
}

func (f *fake) GetUpload(ctx context.Context, req c.GetRequest) (c.Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.uploads[req.ID]
	if !ok {
		return c.Upload{}, c.Errorf(c.ErrNotFound, "no upload")
	}
	return c.Upload{UploadID: req.ID, Received: int64(len(u.data)), Size: u.req.Size, State: "open"}, nil
}

func (f *fake) FinalizeUpload(ctx context.Context, req c.FinalizeUploadRequest) (c.PublishReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.uploads[req.UploadID]
	if u.done != nil {
		return *u.done, nil
	}
	if digestOf(u.data) != u.req.Digest || int64(len(u.data)) != u.req.Size {
		return c.PublishReceipt{}, c.Errorf(c.ErrConflict, "digest mismatch")
	}
	r := c.PublishReceipt{Version: c.ArtifactVersion{VersionID: "ver_1", Size: u.req.Size, Digest: u.req.Digest}}
	u.done = &r
	return r, nil
}

var owner = c.Principal{Kind: c.PrincipalOwner, ID: "cli_owner"}

func serve(t *testing.T, api c.API, tlsServer bool) *httptest.Server {
	t.Helper()
	h := httpapi.NewServer(api, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner}), httpapi.Options{StreamWait: time.Second})
	var srv *httptest.Server
	if tlsServer {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv
}

func fastOpts() Options {
	return Options{RetryBase: time.Millisecond, RetryMax: 5 * time.Millisecond, Timeout: 5 * time.Second}
}

func TestRoundTripAndTypedErrors(t *testing.T) {
	srv := serve(t, &fake{}, false)
	cl := New(srv.URL, "tok", fastOpts())
	ctx := context.Background()
	p, err := cl.WhoAmI(ctx)
	if err != nil || p != owner {
		t.Fatalf("whoami %+v %v", p, err)
	}
	_, err = New(srv.URL, "wrong", fastOpts()).WhoAmI(ctx)
	var ce *c.Error
	if !errors.As(err, &ce) || ce.Code != c.ErrUnauthorized {
		t.Fatalf("want typed unauthorized, got %v", err)
	}
	if New("ftp://x", "t", Options{}).Err() == nil {
		t.Fatal("bad scheme accepted")
	}
	if New("http://x", "t", Options{CertSHA256: "ab"}).Err() == nil {
		t.Fatal("pin on http accepted")
	}
}

func TestRetrySameOperationID(t *testing.T) {
	f := &fake{failSend: 2}
	srv := serve(t, f, false)
	cl := New(srv.URL, "tok", fastOpts())
	mv, err := cl.SendMessage(context.Background(), c.SendMessageRequest{Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, Body: c.MessageBody{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sendOps) != 3 {
		t.Fatalf("attempts %d", len(f.sendOps))
	}
	if f.sendOps[0] == "" || f.sendOps[0] != f.sendOps[1] || f.sendOps[1] != f.sendOps[2] {
		t.Fatalf("operation ids differ across retries: %v", f.sendOps)
	}
	if mv.Envelope.OperationID != f.sendOps[0] || len(f.stored) != 1 {
		t.Fatalf("expected one stored message, got %d", len(f.stored))
	}

	// Non-retryable errors are returned after one attempt.
	f.sendOps = nil
	_, err = cl.SendMessage(context.Background(), c.SendMessageRequest{Body: c.MessageBody{Text: "conflict"}})
	if !c.IsCode(err, c.ErrConflict) || len(f.sendOps) != 1 {
		t.Fatalf("conflict: %v attempts=%d", err, len(f.sendOps))
	}

	// Bounded attempts.
	f.sendOps = nil
	f.failSend = 100
	_, err = cl.SendMessage(context.Background(), c.SendMessageRequest{Body: c.MessageBody{Text: "x"}})
	if !c.IsCode(err, c.ErrUnavailable) || len(f.sendOps) != DefaultMaxAttempts {
		t.Fatalf("bounded: %v attempts=%d", err, len(f.sendOps))
	}

	// Caller-supplied operation id is preserved.
	f.sendOps = nil
	f.failSend = 0
	_, _ = cl.SendMessage(context.Background(), c.SendMessageRequest{Mutation: c.Mutation{OperationID: "op_mine"}, Body: c.MessageBody{Text: "y"}})
	if f.sendOps[0] != "op_mine" {
		t.Fatalf("op id overwritten: %v", f.sendOps)
	}
}

func TestRetryAfterTransportFailure(t *testing.T) {
	f := &fake{}
	inner := httpapi.NewServer(f, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner}), httpapi.Options{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Let the handler commit, then kill the connection before responding.
			inner.ServeHTTP(httptest.NewRecorder(), r)
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cl := New(srv.URL, "tok", fastOpts())
	if _, err := cl.SendMessage(context.Background(), c.SendMessageRequest{Body: c.MessageBody{Text: "z"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.sendOps) != 2 || f.sendOps[0] != f.sendOps[1] || len(f.stored) != 1 {
		t.Fatalf("transport retry: ops=%v stored=%d", f.sendOps, len(f.stored))
	}
}

func TestCancellationStopsRetries(t *testing.T) {
	f := &fake{failSend: 1000}
	srv := serve(t, f, false)
	cl := New(srv.URL, "tok", Options{RetryBase: 50 * time.Millisecond, RetryMax: time.Second, MaxAttempts: 100})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_, err := cl.SendMessage(ctx, c.SendMessageRequest{Body: c.MessageBody{Text: "x"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestTLSPin(t *testing.T) {
	srv := serve(t, &fake{}, true)
	fp := CertFingerprint(srv.Certificate().Raw)
	for _, pin := range []string{fp, "sha256:" + strings.ToUpper(fp), colonize(fp)} {
		cl := New(srv.URL, "tok", Options{CertSHA256: pin})
		if _, err := cl.WhoAmI(context.Background()); err != nil {
			t.Fatalf("pin %q: %v", pin, err)
		}
	}
	wrong := strings.Repeat("ab", 32)
	cl := New(srv.URL, "tok", Options{CertSHA256: wrong, RetryBase: time.Millisecond})
	_, err := cl.WhoAmI(context.Background())
	var ce *c.Error
	if !errors.As(err, &ce) || ce.Code != c.ErrUnauthorized || ce.Retryable || !strings.Contains(ce.Message, fp) {
		t.Fatalf("wrong pin: %v", err)
	}
	// Without a pin, a self-signed cert is rejected by normal verification.
	_, err = New(srv.URL, "tok", Options{}).WhoAmI(context.Background())
	if !c.IsCode(err, c.ErrUnauthorized) {
		t.Fatalf("unpinned self-signed: %v", err)
	}
	if _, err := ParsePin("zz"); err == nil {
		t.Fatal("bad pin parsed")
	}
}

func colonize(h string) string {
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func TestContentDownloadVerifyAndRange(t *testing.T) {
	f := &fake{content: map[string][]byte{"ver_a": []byte("hello, synthetic world")}}
	srv := serve(t, f, false)
	cl := New(srv.URL, "tok", fastOpts())
	ctx := context.Background()
	rc, ver, err := cl.OpenContent(ctx, "ver_a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(b) != "hello, synthetic world" || ver.Size != 22 {
		t.Fatalf("download %q %v", b, err)
	}
	r2, err := cl.OpenContentRange(ctx, "ver_a", 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r2)
	r2.Close()
	if string(b) != "synthetic" {
		t.Fatalf("range %q", b)
	}
	r3, _ := cl.OpenContentRange(ctx, "ver_a", 17, 0)
	b, _ = io.ReadAll(r3)
	r3.Close()
	if string(b) != "world" {
		t.Fatalf("open range %q", b)
	}
	f.badDigest = true
	rc, _, err = cl.OpenContent(ctx, "ver_a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	if !c.IsCode(err, c.ErrConflict) {
		t.Fatalf("digest mismatch not detected: %v", err)
	}
	if _, _, err := cl.OpenContent(ctx, "ver_missing"); !c.IsCode(err, c.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestAllPagination(t *testing.T) {
	f := &fake{nodes: 8}
	srv := serve(t, f, false)
	cl := New(srv.URL, "tok", fastOpts())
	nodes, err := All(context.Background(), func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return cl.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil || len(nodes) != 8 || nodes[7].NodeID != "node_7" {
		t.Fatalf("all: %d %v", len(nodes), err)
	}
	_, err = All(context.Background(), func(ctx context.Context, cur string) (c.Page[int], error) {
		return c.Page[int]{Items: []int{1}, NextCursor: "same"}, nil
	})
	if err == nil {
		t.Fatal("repeating cursor not detected")
	}
}

func TestWatchEventsReconnectAndReset(t *testing.T) {
	f := &fake{}
	for i := int64(1); i <= 6; i++ {
		f.events = append(f.events, c.Event{EventID: "evt_" + strconv.FormatInt(i, 10), Cursor: i})
	}
	var conns atomic.Int32
	inner := httpapi.NewServer(f, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner}), httpapi.Options{StreamWait: time.Second})
	// Proxy that kills each stream connection after a short time, forcing reconnects.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == c.PathEventStream {
			conns.Add(1)
			ctx, cancel := context.WithTimeout(r.Context(), 150*time.Millisecond)
			defer cancel()
			r = r.WithContext(ctx)
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	f.dropAfter = 2
	cl := New(srv.URL, "tok", fastOpts())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []int64
	stop := errors.New("stop")
	err := cl.WatchEvents(ctx, 0, func(e c.Event) error {
		got = append(got, e.Cursor)
		if len(got) == 6 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("watch: %v", err)
	}
	for i, cur := range got {
		if cur != int64(i+1) {
			t.Fatalf("events out of order or skipped: %v", got)
		}
	}

	// Callback error: event not consumed; resume after the last accepted one.
	f.mu.Lock()
	f.oldest = 3
	f.mu.Unlock()
	err = cl.WatchEvents(ctx, 1, func(c.Event) error { return nil })
	var ce *c.Error
	if !errors.As(err, &ce) || ce.Code != c.ErrResetRequired {
		t.Fatalf("want reset_required, got %v", err)
	}

	// Unauthorized is returned, not retried forever.
	err = New(srv.URL, "bad", fastOpts()).WatchEvents(ctx, 0, func(c.Event) error { return nil })
	if !c.IsCode(err, c.ErrUnauthorized) {
		t.Fatalf("want unauthorized, got %v", err)
	}
}

func TestPublishResumesAndVerifies(t *testing.T) {
	f := &fake{chunkFail: 1}
	srv := serve(t, f, false)
	cl := New(srv.URL, "tok", fastOpts())
	data := bytes.Repeat([]byte("0123456789"), 1000) // 10 KB
	rec, err := cl.Publish(context.Background(), bytes.NewReader(data), PublishMeta{Title: "t", Filename: "f.txt", ChunkSize: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Version.Digest != digestOf(data) || rec.Version.Size != int64(len(data)) {
		t.Fatalf("receipt %+v", rec.Version)
	}
	// Non-seekable source is spooled.
	rec, err = Publish(context.Background(), cl, io.MultiReader(bytes.NewReader(data)), PublishMeta{Title: "t2", Filename: "g.txt", ChunkSize: 4096})
	if err != nil || rec.Version.Size != int64(len(data)) {
		t.Fatalf("spooled publish: %v", err)
	}
}

func TestDialFailureIsRetryableTransport(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cl := New("http://"+addr, "tok", Options{MaxAttempts: 2, RetryBase: time.Millisecond, Timeout: time.Second})
	_, err := cl.WhoAmI(context.Background())
	var ce *c.Error
	if !errors.As(err, &ce) || ce.Code != c.ErrUnavailable || !ce.Retryable {
		t.Fatalf("want retryable unavailable, got %v", err)
	}
	if strings.Contains(ce.Message, "tok") {
		t.Fatal("token leaked in error")
	}
}

func TestPublishPassesReviewRequested(t *testing.T) {
	f := &fake{}
	srv := serve(t, f, false)
	cl := New(srv.URL, "tok", fastOpts())
	if _, err := cl.Publish(context.Background(), strings.NewReader("synthetic"), PublishMeta{Title: "r", Filename: "r.txt", OperationID: "op_synth-review", ReviewRequested: true}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.uploads["upl_synth-review"]; u == nil || !u.req.ReviewRequested {
		t.Fatalf("review_requested not passed to BeginUpload: %+v", u)
	}
}

// WithMaxAttempts lowers the retry budget for one call; the freshness sink
// sees the Pyre-Freshness header of a successful response.
func TestMaxAttemptsOverrideAndFreshnessHeader(t *testing.T) {
	var calls atomic.Int32
	fresh := `{"observed_at":"2026-10-05T04:00:00Z","source":"node-cache:node_x","stale":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 100 && r.Header.Get("X-Test-Ok") == "" && r.URL.Path != c.RPCPath(c.OpWhoAmI) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"down","retryable":true}}`))
			return
		}
		w.Header().Set(FreshnessHeader, fresh)
		_, _ = w.Write([]byte(`{"kind":"owner","id":"o"}`))
	}))
	defer srv.Close()
	cl := New(srv.URL, "tok", Options{RetryBase: time.Millisecond, RetryMax: 2 * time.Millisecond, MaxAttempts: 5})
	if _, err := cl.GetNode(WithMaxAttempts(context.Background(), 1), c.GetRequest{ID: "n"}); !c.IsCode(err, c.ErrUnavailable) || calls.Load() != 1 {
		t.Fatalf("single attempt: err=%v calls=%d", err, calls.Load())
	}
	calls.Store(0)
	if _, err := cl.GetNode(context.Background(), c.GetRequest{ID: "n"}); err == nil || calls.Load() != 5 {
		t.Fatalf("default attempts: err=%v calls=%d", err, calls.Load())
	}
	var got []c.Freshness
	ctx := WithFreshnessSink(context.Background(), func(op string, f c.Freshness) { got = append(got, f) })
	if _, err := cl.WhoAmI(ctx); err != nil || len(got) != 1 || got[0].Source != "node-cache:node_x" || !got[0].Stale {
		t.Fatalf("freshness sink: %v %+v", err, got)
	}
}

// A definitive "unavailable" answer (a node daemon that already knows its
// hub is unreachable sets NoRetryHeader) is returned at once; the same
// error without the header, and transport failures, are still retried.
func TestNoRetryHeaderStopsRetries(t *testing.T) {
	var calls atomic.Int32
	var final atomic.Bool
	final.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if final.Load() {
			w.Header().Set(NoRetryHeader, "hub-unreachable")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"hub unreachable","retryable":true}}`))
	}))
	defer srv.Close()
	cl := New(srv.URL, "tok", Options{RetryBase: time.Millisecond, RetryMax: 2 * time.Millisecond, MaxAttempts: 5})
	_, err := cl.GetNode(context.Background(), c.GetRequest{ID: "n"})
	if !c.IsCode(err, c.ErrUnavailable) || calls.Load() != 1 {
		t.Fatalf("definitive unavailable: err=%v calls=%d (want 1 attempt)", err, calls.Load())
	}
	var ce *c.Error
	if !errors.As(err, &ce) || ce.Message != "hub unreachable" {
		t.Fatalf("error not passed through: %#v", err)
	}
	final.Store(false)
	calls.Store(0)
	if _, err := cl.GetNode(context.Background(), c.GetRequest{ID: "n"}); err == nil || calls.Load() != 5 {
		t.Fatalf("plain unavailable: err=%v calls=%d (want 5 attempts)", err, calls.Load())
	}
}
