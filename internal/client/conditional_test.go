package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
)

// listFake answers nodes.list with n nodes (each with a long name, so the
// answer is big enough to compress) and a fresh snapshot_at on every read.
type listFake struct {
	c.API
	mu sync.Mutex
	n  int
}

func (f *listFake) ListNodes(ctx context.Context, req c.ListNodesRequest) (c.Page[c.Node], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := c.Page[c.Node]{SnapshotAt: time.Now().UTC()}
	for i := 0; i < f.n; i++ {
		p.Items = append(p.Items, c.Node{NodeID: "node_" + strconv.Itoa(i), Name: strings.Repeat("synthetic-node-", 20) + strconv.Itoa(i)})
	}
	return p, nil
}

func (f *listFake) set(n int) {
	f.mu.Lock()
	f.n = n
	f.mu.Unlock()
}

// wireLog records what crossed the wire for each request.
type wireLog struct {
	mu   sync.Mutex
	reqs []wireReq
}

type wireReq struct {
	ifNoneMatch, acceptEnc string
	status                 int
	contentEnc             string
	bytes                  int
}

type countingWriter struct {
	http.ResponseWriter
	status int
	n      int
}

func (w *countingWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *countingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += n
	return n, err
}

func serveLogged(t *testing.T, api c.API, opts httpapi.Options) (*httptest.Server, *wireLog) {
	t.Helper()
	inner := httpapi.NewServer(api, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner, "tok2": {Kind: c.PrincipalOwner, ID: "cli_other"}}), opts)
	wl := &wireLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &countingWriter{ResponseWriter: w}
		inner.ServeHTTP(cw, r)
		wl.mu.Lock()
		wl.reqs = append(wl.reqs, wireReq{ifNoneMatch: r.Header.Get("If-None-Match"), acceptEnc: r.Header.Get("Accept-Encoding"),
			status: cw.status, contentEnc: w.Header().Get("Content-Encoding"), bytes: cw.n})
		wl.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, wl
}

func (wl *wireLog) last() wireReq {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	return wl.reqs[len(wl.reqs)-1]
}

func TestConditionalListAndGzip(t *testing.T) {
	f := &listFake{n: 40}
	srv, wl := serveLogged(t, f, httpapi.Options{Compress: true, ETags: true})
	cl := New(srv.URL, "tok", fastOpts())
	ctx := context.Background()

	p1, err := cl.ListNodes(ctx, c.ListNodesRequest{})
	if err != nil || len(p1.Items) != 40 {
		t.Fatalf("first list: %d items, %v", len(p1.Items), err)
	}
	first := wl.last()
	if first.status != http.StatusOK || first.ifNoneMatch != "" || first.contentEnc != "gzip" || !strings.Contains(first.acceptEnc, "gzip") {
		t.Fatalf("first read on the wire: %+v (want 200, gzip, no If-None-Match)", first)
	}

	time.Sleep(2 * time.Millisecond) // a later snapshot_at
	p2, err := cl.ListNodes(ctx, c.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	second := wl.last()
	if second.status != http.StatusNotModified || second.ifNoneMatch == "" || second.bytes != 0 {
		t.Fatalf("unchanged list on the wire: %+v (want a bodiless 304)", second)
	}
	if len(p2.Items) != 40 || p2.Items[39].Name != p1.Items[39].Name {
		t.Fatalf("304 answer not decoded from the kept body: %d items", len(p2.Items))
	}
	if !p2.SnapshotAt.After(p1.SnapshotAt) {
		t.Fatalf("snapshot_at not moved to the hub's read time: %v then %v", p1.SnapshotAt, p2.SnapshotAt)
	}

	f.set(41)
	p3, err := cl.ListNodes(ctx, c.ListNodesRequest{})
	if err != nil || len(p3.Items) != 41 || wl.last().status != http.StatusOK {
		t.Fatalf("changed list: %d items, %v, wire %+v", len(p3.Items), err, wl.last())
	}

	// Another principal's identical request never gets this one's tag.
	if _, err := New(srv.URL, "tok2", fastOpts()).ListNodes(ctx, c.ListNodesRequest{}); err != nil || wl.last().ifNoneMatch != "" {
		t.Fatalf("other token sent a kept tag: %+v %v", wl.last(), err)
	}
	// A different request body is a different entry.
	if _, err := cl.ListNodes(ctx, c.ListNodesRequest{IncludeRevoked: true}); err != nil || wl.last().ifNoneMatch != "" {
		t.Fatalf("different request sent a kept tag: %+v %v", wl.last(), err)
	}
}

// A new client against a hub without ETags/compression (an older hub)
// gets plain 200s and never sends If-None-Match.
func TestConditionalAgainstOldHub(t *testing.T) {
	f := &listFake{n: 40}
	srv, wl := serveLogged(t, f, httpapi.Options{})
	cl := New(srv.URL, "tok", fastOpts())
	for i := 0; i < 3; i++ {
		p, err := cl.ListNodes(context.Background(), c.ListNodesRequest{})
		if err != nil || len(p.Items) != 40 {
			t.Fatalf("read %d: %d items, %v", i, len(p.Items), err)
		}
		if w := wl.last(); w.status != http.StatusOK || w.ifNoneMatch != "" || w.contentEnc != "" {
			t.Fatalf("read %d on the wire: %+v", i, w)
		}
	}
}

// An old client (no If-None-Match; with or without Accept-Encoding) reads a
// new hub exactly as before.
func TestOldClientAgainstNewHub(t *testing.T) {
	f := &listFake{n: 40}
	srv, _ := serveLogged(t, f, httpapi.Options{Compress: true, ETags: true})
	post := func(acceptGzip bool) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+c.RPCPath(c.OpListNodes), strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set(c.ProtocolHeader, strconv.Itoa(c.ProtocolVersion))
		if acceptGzip {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, b := post(false)
	var p c.Page[c.Node]
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || json.Unmarshal(b, &p) != nil || len(p.Items) != 40 {
		t.Fatalf("plain read: status %d enc %q items %d", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(p.Items))
	}
	resp, b = post(true)
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip not used when accepted")
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	p = c.Page[c.Node]{}
	if json.Unmarshal(plain, &p) != nil || len(p.Items) != 40 {
		t.Fatalf("gzip body did not decode")
	}
	// Small answers stay plain whatever the client accepts.
	f.set(1)
	if resp, _ = post(true); resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("small answer was compressed")
	}
}

func TestCondCacheBounded(t *testing.T) {
	cc := newCondCache(100)
	cc.put("a", `"1"`, make([]byte, 60))
	cc.put("b", `"2"`, make([]byte, 60))
	if _, ok := cc.get("a"); ok {
		t.Fatal("oldest entry kept past the byte bound")
	}
	if _, ok := cc.get("b"); !ok {
		t.Fatal("newest entry evicted")
	}
	cc.put("b", "", nil) // an untagged answer forgets the entry
	if _, ok := cc.get("b"); ok {
		t.Fatal("untagged answer left a stale entry")
	}
}
