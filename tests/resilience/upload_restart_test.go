//go:build !windows

package resilience

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
)

// slowBody delays its first read, so a chunk is still arriving when the
// hub starts shutting down.
type slowBody struct {
	io.ReadCloser
	once  sync.Once
	delay time.Duration
}

func (b *slowBody) Read(p []byte) (int, error) {
	b.once.Do(func() { time.Sleep(b.delay) })
	return b.ReadCloser.Read(p)
}

// answer records the status and error code a handler wrote.
type answer struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (a *answer) WriteHeader(s int) { a.status = s; a.ResponseWriter.WriteHeader(s) }
func (a *answer) Write(b []byte) (int, error) {
	a.body.Write(b)
	return a.ResponseWriter.Write(b)
}

// TestUploadSurvivesHubRestart is iss_…: the hub
// restarts (graceful shutdown as cmd/pyre-hub does it: in-flight requests
// keep running for a grace, then their contexts are cancelled) while a
// publish is uploading. The interrupted chunk, against the real artifact
// store, is answered with a retryable 503 rather than "internal error";
// the client waits for the hub to come back on the same address and
// resumes, and exactly one version is published.
func TestUploadSurvivesHubRestart(t *testing.T) {
	ctx := ctxT(t)
	s := newAPIStack(t, stackOpts{})
	api := &composite{base{s.mock}, s.proj, s.arts}
	inner := httpapi.NewServer(api, s.auth, httpapi.Options{StreamWait: time.Second})

	var (
		puts        atomic.Int32
		mu          sync.Mutex
		interrupted *answer
		srv         *http.Server
		addr        string
		restarted   = make(chan struct{})
	)
	var serve func()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || puts.Add(1) != 3 {
			inner.ServeHTTP(w, r)
			return
		}
		// The third chunk is in flight when the hub is told to stop.
		r.Body = &slowBody{ReadCloser: r.Body, delay: 300 * time.Millisecond}
		mu.Lock()
		cur := srv
		mu.Unlock()
		go func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = cur.Shutdown(sctx) // waits for this handler
			time.Sleep(200 * time.Millisecond)
			serve() // the upgraded hub, same address and data
			close(restarted)
		}()
		a := &answer{ResponseWriter: w}
		inner.ServeHTTP(a, r)
		mu.Lock()
		interrupted = a
		mu.Unlock()
	})
	serve = func() {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Error(err)
			return
		}
		baseCtx, cancelBase := context.WithCancel(context.Background())
		hs := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second,
			BaseContext: func(net.Listener) context.Context { return baseCtx }}
		hs.RegisterOnShutdown(func() { time.AfterFunc(50*time.Millisecond, cancelBase) }) // the hub's shutdownGrace, shortened
		mu.Lock()
		srv, addr = hs, ln.Addr().String()
		mu.Unlock()
		go hs.Serve(ln)
		t.Cleanup(func() { hs.Close(); cancelBase() })
	}
	addr = "127.0.0.1:0"
	serve()

	cl := client.New("http://"+addr, ownerToken, client.Options{RetryBase: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond})
	var notes []string
	var nmu sync.Mutex
	nctx := client.WithNotices(ctx, func(m string) { nmu.Lock(); notes = append(notes, m); nmu.Unlock() })
	data := bytes.Repeat([]byte("synthetic artifact "), 4000) // 76 KB
	rec, err := cl.Publish(nctx, bytes.NewReader(data), client.PublishMeta{Title: "restart-proof", Filename: "proof.bin", ChunkSize: 16 << 10, RetryWindow: 30 * time.Second})
	if err != nil {
		t.Fatalf("publish through a hub restart: %v\nnotices: %q", err, notes)
	}
	<-restarted

	mu.Lock()
	a := interrupted
	mu.Unlock()
	if a == nil {
		t.Fatal("no chunk was in flight at shutdown")
	}
	var eb contracts.ErrorBody
	_ = json.Unmarshal(a.body.Bytes(), &eb)
	if a.status != http.StatusServiceUnavailable || eb.Error == nil || eb.Error.Code != contracts.ErrUnavailable || !eb.Error.Retryable {
		t.Fatalf("in-flight chunk at shutdown answered %d %s, want a retryable 503", a.status, a.body.String())
	}
	if rec.Version.Size != int64(len(data)) || rec.Version.Seq != 1 {
		t.Fatalf("receipt %+v", rec.Version)
	}
	vers, err := cl.ListVersions(ctx, contracts.ListVersionsRequest{ArtifactID: rec.Artifact.ArtifactID})
	if err != nil || len(vers.Items) != 1 {
		t.Fatalf("versions = %d (%v), want exactly 1", len(vers.Items), err)
	}
	nmu.Lock()
	joined := strings.Join(notes, "\n")
	nmu.Unlock()
	if !strings.Contains(joined, "retrying in") {
		t.Fatalf("no retry notice: %q", joined)
	}
	evidence(t, "in-flight chunk at shutdown -> %d %s; publish resumed after the restart; 1 version (%s); notices: %q", a.status, eb.Error.Code, rec.Version.VersionID, notes)
}
