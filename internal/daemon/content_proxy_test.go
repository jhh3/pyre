package daemon

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// A `pyre artifact get` on a non-hub node goes CLI -> local pyred -> hub.
// A resume must forward the Range to the hub (only the missing bytes cross
// the slow link), and when the CLI aborts a stalled attempt, the pyred's
// upstream request must stop too.
func TestNodeProxyForwardsContentRangeAndCancels(t *testing.T) {
	owner := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "owner"}
	b := mock.New(mock.Options{DefaultPrincipal: &owner})
	hubSrv := httptest.NewServer(httpapi.NewServer(b, httpapi.NewStaticTokens(map[string]contracts.Principal{"hubtok": owner}), httpapi.Options{}))
	t.Cleanup(hubSrv.Close)
	hubURL, _ := url.Parse(hubSrv.URL)

	var mu sync.Mutex
	var ranges []string
	stall := false
	upstreamGone := make(chan struct{}, 4)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/content/") {
			httputil.NewSingleHostReverseProxy(hubURL).ServeHTTP(w, r)
			return
		}
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		stalled := stall
		mu.Unlock()
		if stalled {
			w.WriteHeader(http.StatusPartialContent)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			upstreamGone <- struct{}{}
			return
		}
		httputil.NewSingleHostReverseProxy(hubURL).ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	data := bytes.Repeat([]byte("synthetic kit bytes\n"), 5000)
	rec, err := client.New(hubSrv.URL, "hubtok", client.Options{}).Publish(ctx, bytes.NewReader(data), client.PublishMeta{Title: "kit", Filename: "kit.tgz", MediaType: "application/gzip"})
	if err != nil {
		t.Fatal(err)
	}

	dir := shortDir(t)
	n := startNode(t, dir, Config{Upstream: func(string) contracts.API { return client.New(front.URL, "hubtok", client.Options{}) }, NodeName: "proxynode"})
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	go func() { _ = n.d.ServeLocal(sctx) }()
	sock := n.d.SocketPath()
	waitFor(t, "socket", func() bool { _, err := os.Stat(sock); return err == nil })
	cl := socketClient(sock, "")

	rc, err := cl.OpenContentRange(ctx, rec.Version.VersionID, 40_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data[40_000:]) {
		t.Fatalf("ranged bytes via node: %d %v", len(got), err)
	}
	mu.Lock()
	if len(ranges) != 1 || ranges[0] != "bytes=40000-99999" {
		t.Fatalf("hub saw %q; the node must forward the range", ranges)
	}
	stall = true
	mu.Unlock()

	// A stalled hub link: the CLI gives up, and so does the node.
	actx, acancel := context.WithTimeout(ctx, 300*time.Millisecond)
	rc, err = cl.OpenContentRange(actx, rec.Version.VersionID, 10, 0)
	if err != nil {
		t.Fatalf("the node must answer headers while the hub body stalls: %v", err)
	}
	_, _ = io.ReadAll(rc)
	rc.Close()
	acancel()
	select {
	case <-upstreamGone:
	case <-time.After(10 * time.Second):
		t.Fatal("the node kept the stalled hub request open after the CLI gave up")
	}
}
