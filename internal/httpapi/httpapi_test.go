package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakeAPI implements only what the tests call; anything else panics via
// the nil embedded interface (which also exercises panic recovery).
type fakeAPI struct {
	contracts.API

	mu      sync.Mutex
	events  []contracts.Event
	oldest  int64
	content map[string][]byte
	chunks  map[string][]byte
	seekers bool
}

func (f *fakeAPI) WhoAmI(ctx context.Context) (contracts.Principal, error) {
	p, ok := contracts.PrincipalFrom(ctx)
	if !ok {
		return p, contracts.Errorf(contracts.ErrUnauthorized, "no principal")
	}
	return p, nil
}

func (f *fakeAPI) GetNode(ctx context.Context, req contracts.GetRequest) (contracts.Node, error) {
	switch req.ID {
	case "boom":
		return contracts.Node{}, errors.New("sql: secret table exploded at /var/lib/pyre/hub.db")
	case "panic":
		panic("secret panic detail")
	case "internal":
		return contracts.Node{}, contracts.Errorf(contracts.ErrInternal, "leaky internal detail")
	case "busy":
		return contracts.Node{}, contracts.Errorf(contracts.ErrUnavailable, "try later")
	}
	return contracts.Node{NodeID: req.ID, Name: "synthetic"}, nil
}

func (f *fakeAPI) UploadChunk(ctx context.Context, id string, off int64, r io.Reader) (contracts.Upload, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return contracts.Upload{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chunks == nil {
		f.chunks = map[string][]byte{}
	}
	cur := f.chunks[id]
	if off > int64(len(cur)) {
		return contracts.Upload{}, contracts.Errorf(contracts.ErrConflict, "gap").WithDetail("received", len(cur))
	}
	cur = append(cur[:off], b...)
	f.chunks[id] = cur
	return contracts.Upload{UploadID: id, Received: int64(len(cur)), State: "open"}, nil
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

func (f *fakeAPI) OpenContent(ctx context.Context, id string) (io.ReadCloser, contracts.ArtifactVersion, error) {
	b, ok := f.content[id]
	if !ok {
		return nil, contracts.ArtifactVersion{}, contracts.Errorf(contracts.ErrNotFound, "no version").WithRef(contracts.KindArtifactVersion, id)
	}
	ver := contracts.ArtifactVersion{VersionID: id, Size: int64(len(b)), MediaType: "text/html", Filename: "../../etc/\"evil\"\r\nX-Injected: 1.html", Digest: "sha256:abc", CreatedAt: time.Unix(0, 0)}
	if f.seekers {
		return nopSeekCloser{bytes.NewReader(b)}, ver, nil
	}
	return io.NopCloser(bytes.NewReader(b)), ver, nil
}

func (f *fakeAPI) ListEvents(ctx context.Context, req contracts.ListEventsRequest) (contracts.ListEventsResponse, error) {
	f.mu.Lock()
	if req.AfterCursor < f.oldest {
		f.mu.Unlock()
		return contracts.ListEventsResponse{}, contracts.Errorf(contracts.ErrResetRequired, "cursor too old").WithDetail("oldest_cursor", f.oldest)
	}
	var out []contracts.Event
	head := int64(0)
	for _, e := range f.events {
		head = e.Cursor
		if e.Cursor > req.AfterCursor && (req.Limit == 0 || len(out) < req.Limit) {
			out = append(out, e)
		}
	}
	f.mu.Unlock()
	if len(out) == 0 {
		select {
		case <-ctx.Done():
			return contracts.ListEventsResponse{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		return contracts.ListEventsResponse{NextCursor: req.AfterCursor, HeadCursor: head}, nil
	}
	return contracts.ListEventsResponse{Events: out, NextCursor: out[len(out)-1].Cursor, HeadCursor: head}, nil
}

var owner = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_test-owner"}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newTestServer(t *testing.T, api contracts.API, opts Options) (*httptest.Server, *logBuf) {
	t.Helper()
	lb := &logBuf{}
	opts.Logger = slog.New(slog.NewTextHandler(lb, nil))
	auth := NewStaticTokens(map[string]contracts.Principal{"secret-token-123": owner})
	srv := httptest.NewServer(NewServer(api, auth, opts))
	t.Cleanup(srv.Close)
	return srv, lb
}

type result struct {
	status int
	body   []byte
	header http.Header
}

func do(t *testing.T, method, url, token string, hdr map[string]string, body string) result {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, b, resp.Header}
}

func errCode(t *testing.T, b []byte) contracts.ErrorCode {
	t.Helper()
	var eb contracts.ErrorBody
	if err := json.Unmarshal(b, &eb); err != nil || eb.Error == nil {
		t.Fatalf("not an ErrorBody: %s", b)
	}
	return eb.Error.Code
}

func TestEveryRouteOpIsServed(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "contracts", "routes.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if strings.HasPrefix(n.Name, "Op") && i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
						v, _ := strconv.Unquote(lit.Value)
						ops = append(ops, v)
					}
				}
			}
		}
	}
	table := rpcTable(&fakeAPI{})
	if len(ops) < 60 || len(table) != len(ops) {
		t.Fatalf("routes.go has %d ops, server routes %d", len(ops), len(table))
	}
	for _, op := range ops {
		if _, ok := table[op]; !ok {
			t.Errorf("op %s not routed", op)
		}
	}
}

func TestAuthProtocolAndDecoding(t *testing.T) {
	srv, lb := newTestServer(t, &fakeAPI{}, Options{MaxBodyBytes: 64})
	whoami := srv.URL + contracts.RPCPath(contracts.OpWhoAmI)
	getNode := srv.URL + contracts.RPCPath(contracts.OpGetNode)
	cases := []struct {
		name   string
		method string
		url    string
		token  string
		hdr    map[string]string
		body   string
		status int
		code   contracts.ErrorCode
	}{
		{"ok no protocol header", "POST", whoami, "secret-token-123", nil, "", 200, ""},
		{"ok current protocol", "POST", whoami, "secret-token-123", map[string]string{"X-Pyre-Protocol": "1"}, "{}", 200, ""},
		{"missing token", "POST", whoami, "", nil, "{}", 401, contracts.ErrUnauthorized},
		{"bad token", "POST", whoami, "nope", nil, "{}", 401, contracts.ErrUnauthorized},
		{"future protocol", "POST", whoami, "secret-token-123", map[string]string{"X-Pyre-Protocol": "99"}, "{}", 426, contracts.ErrProtocolMismatch},
		{"garbage protocol", "POST", whoami, "secret-token-123", map[string]string{"X-Pyre-Protocol": "x"}, "{}", 426, contracts.ErrProtocolMismatch},
		{"unknown field", "POST", getNode, "secret-token-123", nil, `{"id":"node_a","bogus":1}`, 400, contracts.ErrInvalid},
		{"trailing data", "POST", getNode, "secret-token-123", nil, `{"id":"node_a"}{}`, 400, contracts.ErrInvalid},
		{"malformed", "POST", getNode, "secret-token-123", nil, `{"id":`, 400, contracts.ErrInvalid},
		{"wrong type", "POST", getNode, "secret-token-123", nil, `{"id":5}`, 400, contracts.ErrInvalid},
		{"too large", "POST", getNode, "secret-token-123", nil, `{"id":"` + strings.Repeat("a", 100) + `"}`, 413, contracts.ErrPayloadTooLarge},
		{"GET on rpc", "GET", whoami, "secret-token-123", nil, "", 405, contracts.ErrInvalid},
		{"unknown op", "POST", srv.URL + "/v1/nope.nope", "secret-token-123", nil, "{}", 404, contracts.ErrNotFound},
		{"api error passthrough", "POST", getNode, "secret-token-123", nil, `{"id":"busy"}`, 503, contracts.ErrUnavailable},
		{"plain error sanitized", "POST", getNode, "secret-token-123", nil, `{"id":"boom"}`, 500, contracts.ErrInternal},
		{"internal sanitized", "POST", getNode, "secret-token-123", nil, `{"id":"internal"}`, 500, contracts.ErrInternal},
		{"panic recovered", "POST", getNode, "secret-token-123", nil, `{"id":"panic"}`, 500, contracts.ErrInternal},
		{"unimplemented method panics safely", "POST", srv.URL + contracts.RPCPath(contracts.OpListNodes), "secret-token-123", nil, `{}`, 500, contracts.ErrInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := do(t, tc.method, tc.url, tc.token, tc.hdr, tc.body)
			if r.status != tc.status {
				t.Fatalf("status %d want %d: %s", r.status, tc.status, r.body)
			}
			if r.header.Get(contracts.ProtocolHeader) != "1" {
				t.Errorf("missing protocol response header")
			}
			if tc.code != "" {
				if got := errCode(t, r.body); got != tc.code {
					t.Fatalf("code %s want %s", got, tc.code)
				}
				for _, leak := range []string{"secret", "sql", "/var/lib", "leaky"} {
					if bytes.Contains(r.body, []byte(leak)) {
						t.Fatalf("response leaks %q: %s", leak, r.body)
					}
				}
			} else {
				var p contracts.Principal
				if err := json.Unmarshal(r.body, &p); err != nil || p != owner {
					t.Fatalf("principal %s", r.body)
				}
			}
		})
	}
	if strings.Contains(lb.String(), "secret-token-123") || strings.Contains(lb.String(), "Bearer") {
		t.Fatalf("access log leaked credentials:\n%s", lb.String())
	}
	if !strings.Contains(lb.String(), "secret panic detail") {
		t.Fatalf("internal details should reach the server log:\n%s", lb.String())
	}
	if r := do(t, "POST", whoami, "", nil, "{}"); r.header.Get("WWW-Authenticate") == "" {
		t.Fatal("401 without WWW-Authenticate")
	}
	if r := do(t, "POST", getNode, "secret-token-123", nil, `{"id":"busy"}`); r.header.Get("Retry-After") == "" {
		t.Fatal("unavailable without Retry-After")
	}
}

func TestExtraHandlersAndPublicPaths(t *testing.T) {
	var gotPrincipal contracts.Principal
	extra := map[string]http.Handler{
		contracts.PathNodeConnect: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPrincipal, _ = contracts.PrincipalFrom(r.Context())
			w.WriteHeader(204)
		}),
		contracts.PathPreview: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := contracts.PrincipalFrom(r.Context()); ok {
				t.Error("preview must not receive a principal")
			}
			w.Write([]byte(r.PathValue("version_id")))
		}),
		"POST " + contracts.PathPairExchange: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(contracts.ProtocolHeader) == "99" {
				t.Error("pair exchange handler reached with an incompatible protocol")
			}
			w.WriteHeader(201)
		}),
		"POST " + contracts.PathEnrollExchange: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(contracts.ProtocolHeader) == "99" {
				t.Error("enroll exchange handler reached with an incompatible protocol")
			}
			w.WriteHeader(201)
		}),
		contracts.PathTerminal: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) }),
	}
	srv, lb := newTestServer(t, &fakeAPI{}, Options{Extra: extra})
	if r := do(t, "GET", srv.URL+contracts.PathNodeConnect, "", nil, ""); r.status != 401 {
		t.Fatalf("node connect without token: %d", r.status)
	}
	if r := do(t, "GET", srv.URL+contracts.PathNodeConnect, "secret-token-123", nil, ""); r.status != 204 || gotPrincipal != owner {
		t.Fatalf("node connect: %d %+v", r.status, gotPrincipal)
	}
	if r := do(t, "GET", srv.URL+"/preview/ver_x/tok-SECRET-preview", "", nil, ""); r.status != 200 || string(r.body) != "ver_x" {
		t.Fatalf("preview: %d %s", r.status, r.body)
	}
	if r := do(t, "POST", srv.URL+contracts.PathPairExchange, "", nil, "{}"); r.status != 201 {
		t.Fatalf("pair exchange: %d", r.status)
	}
	if r := do(t, "POST", srv.URL+contracts.PathPairExchange, "", map[string]string{"X-Pyre-Protocol": "1"}, "{}"); r.status != 201 {
		t.Fatalf("pair exchange, current protocol: %d", r.status)
	}
	// An incompatible client is refused before the one-time secret is
	// redeemed, so the code stays usable once the versions overlap again.
	for _, p := range []string{contracts.PathPairExchange, contracts.PathEnrollExchange} {
		if r := do(t, "POST", srv.URL+p, "", map[string]string{"X-Pyre-Protocol": "99"}, "{}"); r.status != 426 || errCode(t, r.body) != contracts.ErrProtocolMismatch {
			t.Fatalf("%s, future protocol: %d %s", p, r.status, r.body)
		}
	}
	if r := do(t, "POST", srv.URL+contracts.PathEnrollExchange, "", nil, "{}"); r.status != 201 {
		t.Fatalf("enroll exchange: %d", r.status)
	}
	if r := do(t, "GET", srv.URL+contracts.PathTerminal, "", nil, ""); r.status != 202 {
		t.Fatalf("terminal: %d", r.status)
	}
	if r := do(t, "GET", srv.URL+contracts.PathHealth, "", nil, ""); r.status != 200 {
		t.Fatalf("health: %d", r.status)
	}
	if strings.Contains(lb.String(), "tok-SECRET-preview") {
		t.Fatalf("preview token logged:\n%s", lb.String())
	}
}

func TestUploadChunk(t *testing.T) {
	api := &fakeAPI{}
	srv, _ := newTestServer(t, api, Options{MaxChunkBytes: 8})
	u := srv.URL + "/v1/uploads/upl_x"
	if r := do(t, "PUT", u+"?offset=0", "secret-token-123", nil, "abcd"); r.status != 200 {
		t.Fatalf("chunk: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", u+"?offset=4", "secret-token-123", nil, "efgh"); r.status != 200 {
		t.Fatalf("chunk2: %d %s", r.status, r.body)
	}
	var up contracts.Upload
	r := do(t, "PUT", u+"?offset=2", "secret-token-123", nil, "cdef")
	json.Unmarshal(r.body, &up)
	if up.Received != 6 {
		t.Fatalf("overlap rewrite received=%d", up.Received)
	}
	if r := do(t, "PUT", u+"?offset=0", "secret-token-123", nil, "123456789"); r.status != 413 {
		t.Fatalf("oversize chunk: %d", r.status)
	}
	if r := do(t, "PUT", u, "secret-token-123", nil, "a"); r.status != 400 {
		t.Fatalf("missing offset: %d", r.status)
	}
	if r := do(t, "PUT", u+"?offset=-1", "secret-token-123", nil, "a"); r.status != 400 {
		t.Fatalf("negative offset: %d", r.status)
	}
	if r := do(t, "PUT", srv.URL+"/v1/uploads/bad..id?offset=0", "secret-token-123", nil, "a"); r.status != 400 {
		t.Fatalf("bad id: %d", r.status)
	}
	if r := do(t, "PUT", u+"?offset=0", "", nil, "a"); r.status != 401 {
		t.Fatalf("unauthenticated upload: %d", r.status)
	}
}

func TestContentRangeAndHeaders(t *testing.T) {
	for _, seek := range []bool{false, true} {
		t.Run("seeker="+strconv.FormatBool(seek), func(t *testing.T) {
			api := &fakeAPI{seekers: seek, content: map[string][]byte{"ver_a": []byte("0123456789")}}
			srv, _ := newTestServer(t, api, Options{})
			u := srv.URL + "/v1/content/ver_a"
			cases := []struct {
				rng    string
				status int
				body   string
				cr     string
			}{
				{"", 200, "0123456789", ""},
				{"bytes=2-4", 206, "234", "bytes 2-4/10"},
				{"bytes=7-", 206, "789", "bytes 7-9/10"},
				{"bytes=-2", 206, "89", "bytes 8-9/10"},
				{"bytes=5-100", 206, "56789", "bytes 5-9/10"},
				{"bytes=20-30", 416, "", "bytes */10"},
			}
			for _, tc := range cases {
				hdr := map[string]string{}
				if tc.rng != "" {
					hdr["Range"] = tc.rng
				}
				r := do(t, "GET", u, "secret-token-123", hdr, "")
				if r.status != tc.status {
					t.Fatalf("%q: status %d want %d", tc.rng, r.status, tc.status)
				}
				if tc.status < 300 && string(r.body) != tc.body {
					t.Fatalf("%q: body %q", tc.rng, r.body)
				}
				if tc.cr != "" && r.header.Get("Content-Range") != tc.cr {
					t.Fatalf("%q: content-range %q", tc.rng, r.header.Get("Content-Range"))
				}
				if tc.status == 200 {
					cd := r.header.Get("Content-Disposition")
					if !strings.HasPrefix(cd, "attachment;") || strings.ContainsAny(cd, "\r\n/\\") || strings.Contains(cd, "..") && strings.Contains(cd, "/") {
						t.Fatalf("unsafe disposition %q", cd)
					}
					if r.header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.header.Get("Content-Security-Policy"), "sandbox") {
						t.Fatal("active content headers missing")
					}
					if r.header.Get("X-Injected") != "" {
						t.Fatal("header injection")
					}
				}
			}
			if r := do(t, "GET", srv.URL+"/v1/content/ver_missing", "secret-token-123", nil, ""); r.status != 404 {
				t.Fatalf("missing: %d", r.status)
			}
			if r := do(t, "GET", u, "", nil, ""); r.status != 401 {
				t.Fatalf("unauthenticated download: %d", r.status)
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"report.md":              "report.md",
		"../../etc/passwd":       "passwd",
		`C:\Users\x\evil.bat`:    "evil.bat",
		"a\"b\r\nc.txt":          "a_b__c.txt",
		"...":                    "download",
		"":                       "download",
		"résumé.pdf":             "résumé.pdf",
		"x\u202egnp.exe":         "xgnp.exe",
		strings.Repeat("a", 300): strings.Repeat("a", 180),
		"semi;colon%percent.txt": "semi_colon_percent.txt",
		"  .hidden  ":            "hidden",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q)=%q want %q", in, got, want)
		}
	}
	cd := ContentDisposition("attachment", "résumé.pdf")
	if !strings.Contains(cd, `filename="r_sum_.pdf"`) || !strings.Contains(cd, "filename*=UTF-8''r%C3%A9sum%C3%A9.pdf") {
		t.Fatalf("disposition %q", cd)
	}
}

func TestEventStream(t *testing.T) {
	api := &fakeAPI{}
	for i := int64(1); i <= 5; i++ {
		api.events = append(api.events, contracts.Event{EventID: "evt_" + strconv.FormatInt(i, 10), Cursor: i, Kind: contracts.EvMessageStored})
	}
	srv, _ := newTestServer(t, api, Options{StreamWait: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + contracts.PathEventStream

	if _, resp, err := websocket.Dial(ctx, wsURL, nil); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("unauthenticated stream should be refused, got %v", err)
	}

	hdr := http.Header{"Authorization": {"Bearer secret-token-123"}}
	c, _, err := websocket.Dial(ctx, wsURL+"?after_cursor=2", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for len(got) < 3 {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ev contracts.Event
		json.Unmarshal(data, &ev)
		got = append(got, ev.Cursor)
	}
	if got[0] != 3 || got[2] != 5 {
		t.Fatalf("cursors %v", got)
	}
	// New event arrives while connected.
	api.mu.Lock()
	api.events = append(api.events, contracts.Event{EventID: "evt_6", Cursor: 6})
	api.mu.Unlock()
	_, data, err := c.Read(ctx)
	if err != nil || !bytes.Contains(data, []byte(`"cursor":6`)) {
		t.Fatalf("live event: %s %v", data, err)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// Stale cursor: single error frame then close with CloseResetRequired.
	api.mu.Lock()
	api.oldest = 4
	api.mu.Unlock()
	c, _, err = websocket.Dial(ctx, wsURL+"?after_cursor=1", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	_, data, err = c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if errCode(t, data) != contracts.ErrResetRequired {
		t.Fatalf("frame %s", data)
	}
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != CloseResetRequired {
		t.Fatalf("close status %v", err)
	}
}

// A client that disconnects mid-upload is not a server fault: no ERROR
// "internal error" line, only a quiet client-cancelled note.
func TestClientCancelledUploadIsNotAnInternalError(t *testing.T) {
	api := &fakeAPI{}
	srv, lb := newTestServer(t, api, Options{MaxChunkBytes: 1 << 20})
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "PUT", srv.URL+"/v1/uploads/upl_synthcancel?offset=0", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-token-123")
	req.ContentLength = 1 << 19
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	if _, err := pw.Write(bytes.Repeat([]byte("s"), 4096)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	pw.CloseWithError(context.Canceled)
	<-done
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(lb.String(), "upl_synthcancel") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	logs := lb.String()
	if !strings.Contains(logs, "upl_synthcancel") {
		t.Fatalf("no access log for the cancelled upload:\n%s", logs)
	}
	if strings.Contains(logs, "level=ERROR") || strings.Contains(logs, "internal error") {
		t.Fatalf("client disconnect logged as a server error:\n%s", logs)
	}
	if !strings.Contains(logs, "client cancelled") {
		t.Fatalf("want a client-cancelled note:\n%s", logs)
	}
}
