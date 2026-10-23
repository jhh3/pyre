package cli

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// slowFront sits between the CLI and the test hub like a congested relay:
// for request i to /v1/content/ it forwards the request (Range included),
// sends plan(i) bytes of the body and then stops sending without closing
// the connection (plan < 0 sends everything). Everything else passes
// through.
type slowFront struct {
	hub  *url.URL
	plan func(i int) int

	mu     sync.Mutex
	ranges []string
}

func newSlowFront(t *testing.T, h *hub, plan func(int) int) (*slowFront, *httptest.Server) {
	t.Helper()
	u, _ := url.Parse(h.srv.URL)
	f := &slowFront{hub: u, plan: plan}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *slowFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/content/") {
		httputil.NewSingleHostReverseProxy(f.hub).ServeHTTP(w, r)
		return
	}
	f.mu.Lock()
	i := len(f.ranges)
	f.ranges = append(f.ranges, r.Header.Get("Range"))
	f.mu.Unlock()
	up, _ := http.NewRequestWithContext(r.Context(), r.Method, f.hub.String()+r.URL.Path, nil)
	up.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(up)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	n := f.plan(i)
	if n < 0 {
		_, _ = io.Copy(w, resp.Body)
		return
	}
	_, _ = io.CopyN(w, resp.Body, int64(n))
	w.(http.Flusher).Flush()
	<-r.Context().Done() // stalled: nothing more until the client gives up
}

func (f *slowFront) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges...)
}

func publishSynthetic(t *testing.T, e env, size int) (c.ArtifactVersion, []byte) {
	t.Helper()
	data := make([]byte, size)
	rand.New(rand.NewSource(7)).Read(data)
	file := filepath.Join(t.TempDir(), "synthetic-kit.tgz")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, e, nil, "--json", "publish", file, "--title", "Synthetic kit")
	expectCode(t, r, 0)
	return mustJSON[c.PublishReceipt](t, r).Version, data
}

func fastRetries(t *testing.T) {
	old := downloadBackoff
	downloadBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { downloadBackoff = old })
}

func TestArtifactGetStallKeepsPartThenResumes(t *testing.T) {
	fastRetries(t)
	h := newHub(t)
	e := newEnv(t, h)
	ver, data := publishSynthetic(t, e, 300_000)
	const first = 100_000

	// Every attempt stalls: the first after 100 kB, the retries at once.
	front, srv := newSlowFront(t, h, func(i int) int {
		if i == 0 {
			return first
		}
		return 0
	})
	got := filepath.Join(t.TempDir(), "got.tgz")
	start := time.Now()
	r := run(t, e.with(EnvHub, srv.URL), nil, "artifact", "get", ver.VersionID, "-o", got, "--stall-timeout", "200ms", "--no-cache")
	if r.code == 0 {
		t.Fatalf("a stalled download must fail: %s", r)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("stall detection took %s", time.Since(start))
	}
	for _, want := range []string{"stalled (no bytes for 200ms)", "0.1 MB of 0.3 MB received", "link to the hub looks slow", got + ".part", "rerun the same command to resume", "retrying"} {
		if !strings.Contains(r.stderr, want) {
			t.Fatalf("stderr lacks %q: %s", want, r)
		}
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("unverified bytes must not be at the destination: %v", err)
	}
	if b, err := os.ReadFile(got + ".part"); err != nil || !bytes.Equal(b, data[:first]) {
		t.Fatalf("partial file: %d bytes, %v", len(b), err)
	}
	want := []string{"bytes=0-", "bytes=100000-", "bytes=100000-", "bytes=100000-"}
	if s := front.seen(); strings.Join(s, ",") != strings.Join(want, ",") {
		t.Fatalf("attempts/ranges: %q, want %q", s, want)
	}

	// The link recovers: a rerun resumes from the .part with Range and
	// verifies the whole.
	front.plan = func(int) int { return -1 }
	r = run(t, e.with(EnvHub, srv.URL), nil, "artifact", "get", ver.VersionID, "-o", got, "--stall-timeout", "200ms", "--no-cache")
	expectCode(t, r, 0)
	if s := front.seen(); s[len(s)-1] != "bytes=100000-" {
		t.Fatalf("rerun did not resume: %q", s)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, data) {
		t.Fatalf("resumed bytes differ (%d bytes)", len(b))
	}
	if !strings.Contains(r.stderr, "resuming") || !strings.Contains(r.stderr, "verified") {
		t.Fatalf("stderr: %s", r)
	}
	if _, err := os.Stat(got + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part must be gone after success: %v", err)
	}
}

func TestArtifactGetAutoResumesAfterStall(t *testing.T) {
	fastRetries(t)
	h := newHub(t)
	e := newEnv(t, h)
	ver, data := publishSynthetic(t, e, 250_000)
	front, srv := newSlowFront(t, h, func(i int) int {
		if i == 0 {
			return 70_000
		}
		return -1
	})
	got := filepath.Join(t.TempDir(), "got.tgz")
	r := run(t, e.with(EnvHub, srv.URL), nil, "--json", "artifact", "get", ver.VersionID, "-o", got, "--stall-timeout", "200ms")
	expectCode(t, r, 0)
	if b, _ := os.ReadFile(got); !bytes.Equal(b, data) {
		t.Fatalf("bytes differ after auto-resume (%d bytes)", len(b))
	}
	if s := front.seen(); strings.Join(s, ",") != "bytes=0-,bytes=70000-" {
		t.Fatalf("ranges: %q", s)
	}
	if !strings.Contains(r.stderr, "retrying") {
		t.Fatalf("stderr: %s", r)
	}
	// --json: stdout is only the contract type.
	if v := mustJSON[c.ArtifactVersion](t, r); v.VersionID != ver.VersionID {
		t.Fatalf("stdout: %s", r.stdout)
	}
}

func TestArtifactGetBadPartialIsDiscarded(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	ver, data := publishSynthetic(t, e, 50_000)
	got := filepath.Join(t.TempDir(), "got.tgz")
	if err := os.WriteFile(got+".part", bytes.Repeat([]byte{'x'}, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, e, nil, "artifact", "get", ver.VersionID, "-o", got, "--no-cache")
	if r.code == 0 || !strings.Contains(r.stderr, "do not match") {
		t.Fatalf("corrupt partial must fail verification: %s", r)
	}
	if _, err := os.Stat(got + ".part"); !os.IsNotExist(err) {
		t.Fatalf("a .part that does not verify must be removed: %v", err)
	}
	r = run(t, e, nil, "artifact", "get", ver.VersionID, "-o", got, "--no-cache")
	expectCode(t, r, 0)
	if b, _ := os.ReadFile(got); !bytes.Equal(b, data) {
		t.Fatal("retry after discarding the partial differs")
	}
}

func TestDownloadProgressRateLimited(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	var buf bytes.Buffer
	p := newProgress(&buf, "voy850-r3-review-kit.tgz", 54_100_000, 0, false, clock)
	var n int64
	for i := 0; i < 200; i++ { // 20 s of updates every 100 ms at 210 kB/s
		now = now.Add(100 * time.Millisecond)
		n += 21_000
		p.update(n)
	}
	p.finish()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("non-TTY progress must be at most one line per 5s; got %d:\n%s", len(lines), buf.String())
	}
	if lines[0] != "downloading voy850-r3-review-kit.tgz: 1.1/54.1 MB, 210 KB/s" {
		t.Fatalf("line: %q", lines[0])
	}

	// A download that finishes within the first interval prints nothing.
	buf.Reset()
	p = newProgress(&buf, "x", 10, 0, false, clock)
	p.update(10)
	p.finish()
	if buf.Len() != 0 {
		t.Fatalf("fast download printed progress: %q", buf.String())
	}

	// On a terminal: once a second, one overwritten line, closed at the end.
	buf.Reset()
	p = newProgress(&buf, "x", 54_100_000, 0, true, clock)
	for i := 0; i < 30; i++ {
		now = now.Add(100 * time.Millisecond)
		p.update(int64(i) * 1000)
	}
	p.finish()
	if got := strings.Count(buf.String(), "\r"); got != 3 || !strings.HasSuffix(buf.String(), "\n") || strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("TTY progress: %q", buf.String())
	}
}
