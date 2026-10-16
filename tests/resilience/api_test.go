//go:build !windows

package resilience

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/artifacts"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/projects"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// base lifts the mock one embedding level down so the real SQLite stores
// (depth 1) win method selection over it.
type base struct{ contracts.API }

// composite serves projects/documents/notes and artifacts from the real
// SQLite stores and everything else (sessions, messages, events) from the
// deterministic mock, until the WP05 hub composes them all.
type composite struct {
	base
	*projectsStore
	*artifactsStore
}

// Aliases give the two embedded *Store fields distinct names.
type (
	projectsStore  = projects.Store
	artifactsStore = artifacts.Store
)

var _ contracts.API = (*composite)(nil)

const (
	ownerToken   = "test-owner-token-synthetic"
	sessionToken = "test-session-token-synthetic"
)

var ownerP = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_resilience", Name: "owner (synthetic)"}

type apiStack struct {
	mock   *mock.Backend
	proj   *projects.Store
	arts   *artifacts.Store
	artDir string
	srv    *httptest.Server
	seen   *opRecorder
	lossy  *lossyTransport
	auth   *httpapi.StaticTokens
}

type stackOpts struct {
	mockOpts mock.Options
	artOpts  artifacts.Options
	httpOpts httpapi.Options
}

func newAPIStack(t *testing.T, o stackOpts) *apiStack {
	t.Helper()
	db := svctest.OpenDB(t)
	sink := svctest.NewSink(t, db)
	ctx := context.Background()
	proj, err := projects.New(ctx, db, sink, projects.Options{})
	if err != nil {
		t.Fatal(err)
	}
	o.artOpts.DataDir = filepath.Join(t.TempDir(), "artifacts")
	o.artOpts.Projects = proj
	arts, err := artifacts.New(ctx, db, sink, o.artOpts)
	if err != nil {
		t.Fatal(err)
	}
	proj.SetArtifactIndex(arts)
	if o.mockOpts.Clock == nil {
		o.mockOpts.Clock = mock.NewFakeClock(mock.FixtureEpoch)
	}
	mb := mock.New(o.mockOpts)
	auth := httpapi.NewStaticTokens(map[string]contracts.Principal{ownerToken: ownerP})
	if o.httpOpts.StreamWait == 0 {
		o.httpOpts.StreamWait = time.Second
	}
	rec := &opRecorder{counts: map[string]int{}}
	srv := httptest.NewServer(rec.wrap(httpapi.NewServer(&composite{base{mb}, proj, arts}, auth, o.httpOpts)))
	t.Cleanup(srv.Close)
	return &apiStack{mock: mb, proj: proj, arts: arts, artDir: o.artOpts.DataDir, srv: srv, seen: rec,
		lossy: &lossyTransport{inner: http.DefaultTransport, rules: map[string]int{}}, auth: auth}
}

// client returns an HTTP client whose transport can lose responses.
func (s *apiStack) client(token string) *client.Client {
	return client.New(s.srv.URL, token, client.Options{
		HTTPClient: &http.Client{Transport: s.lossy}, RetryBase: time.Millisecond, RetryMax: 20 * time.Millisecond, MaxAttempts: 6,
	})
}

// opRecorder counts requests per (path, operation_id) on the server.
type opRecorder struct {
	mu     sync.Mutex
	counts map[string]int
}

func (r *opRecorder) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.Body != nil {
			b, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(b))
			var m struct {
				OperationID string `json:"operation_id"`
			}
			_ = json.Unmarshal(b, &m)
			if m.OperationID != "" {
				r.mu.Lock()
				r.counts[req.URL.Path+"|"+m.OperationID]++
				r.mu.Unlock()
			}
		}
		h.ServeHTTP(w, req)
	})
}

// maxRepeats returns the highest number of requests seen for one
// operation_id on path.
func (r *opRecorder) maxRepeats(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := 0
	for k, n := range r.counts {
		if strings.HasPrefix(k, path+"|") && n > m {
			m = n
		}
	}
	return m
}

// lossyTransport performs the real round trip (so the hub commits) and
// then discards the response for the next N requests matching a path
// prefix: the "lost acknowledgement" failure.
type lossyTransport struct {
	inner   http.RoundTripper
	mu      sync.Mutex
	rules   map[string]int
	dropped atomic.Int64
}

func (l *lossyTransport) loseNext(pathPrefix string, n int) {
	l.mu.Lock()
	l.rules[pathPrefix] += n
	l.mu.Unlock()
}

var errLost = errors.New("faultnet: connection reset after the hub committed (response lost)")

func (l *lossyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.inner.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	l.mu.Lock()
	drop := false
	for p, n := range l.rules {
		if n > 0 && strings.HasPrefix(req.URL.Path, p) && resp.StatusCode < 300 {
			l.rules[p] = n - 1
			drop = true
			break
		}
	}
	l.mu.Unlock()
	if drop {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		l.dropped.Add(1)
		return nil, errLost
	}
	return resp, nil
}

func op() contracts.Mutation {
	return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
}

func ownerCtx() context.Context { return contracts.WithPrincipal(context.Background(), ownerP) }

// startMockSession launches a mock session usable as a message recipient.
func startMockSession(t *testing.T, api contracts.API) string {
	t.Helper()
	res, err := api.StartSession(ownerCtx(), contracts.StartSessionRequest{
		Mutation: op(), NodeID: "node_synth-mac", Harness: contracts.HarnessMock, Cwd: "/tmp/pyre-resilience", Title: "recipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Session.Session.SessionID
}

// TestLostResponseAfterCommitReturnsStoredResult: the hub commits, the
// response is lost, the client retries with the same operation_id and gets
// the stored result; exactly one record exists.
func TestLostResponseAfterCommitReturnsStoredResult(t *testing.T) {
	ctx := ctxT(t)
	s := newAPIStack(t, stackOpts{})
	cl := s.client(ownerToken)

	t.Run("projects.create", func(t *testing.T) {
		s.lossy.loseNext(contracts.RPCPath(contracts.OpCreateProject), 2)
		p, err := cl.CreateProject(ctx, contracts.CreateProjectRequest{Name: "lost-ack-project", Brief: "synthetic brief"})
		if err != nil {
			t.Fatal(err)
		}
		all, err := cl.ListProjects(ctx, contracts.ListProjectsRequest{Query: "lost-ack-project"})
		if err != nil {
			t.Fatal(err)
		}
		if len(all.Items) != 1 || all.Items[0].ProjectID != p.ProjectID {
			t.Fatalf("projects after lost responses = %+v", all.Items)
		}
		if r := s.seen.maxRepeats(contracts.RPCPath(contracts.OpCreateProject)); r != 3 {
			t.Fatalf("server saw %d attempts of the operation, want 3", r)
		}
	})

	t.Run("documents.propose", func(t *testing.T) {
		d, err := cl.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: op(), Title: "plan", Content: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		s.lossy.loseNext(contracts.RPCPath(contracts.OpProposeRevision), 1)
		r, err := cl.ProposeRevision(ctx, contracts.ProposeRevisionRequest{DocumentID: d.Document.DocumentID, BaseRevisionID: d.Revision.RevisionID, Content: "v2"})
		if err != nil {
			t.Fatalf("retry after lost response must replay the stored result, got %v", err)
		}
		revs, err := cl.ListRevisions(ctx, contracts.ListRevisionsRequest{DocumentID: d.Document.DocumentID})
		if err != nil {
			t.Fatal(err)
		}
		if len(revs.Items) != 2 || r.Document.HeadSeq != 2 {
			t.Fatalf("revisions = %d head=%d; a retried proposal must not conflict with itself or double-apply", len(revs.Items), r.Document.HeadSeq)
		}
	})

	t.Run("artifacts.chunk+finalize", func(t *testing.T) {
		s.lossy.loseNext("/v1/uploads/", 1)
		s.lossy.loseNext(contracts.RPCPath(contracts.OpFinalizeUpload), 1)
		data := bytes.Repeat([]byte("synthetic artifact bytes\n"), 4000) // ~100 KiB, several chunks
		rec, err := cl.Publish(ctx, bytes.NewReader(data), client.PublishMeta{Title: "lost-ack-artifact", Filename: "a.txt", ChunkSize: 32 << 10})
		if err != nil {
			t.Fatal(err)
		}
		vs, err := cl.ListVersions(ctx, contracts.ListVersionsRequest{ArtifactID: rec.Artifact.ArtifactID})
		if err != nil {
			t.Fatal(err)
		}
		if len(vs.Items) != 1 || vs.Items[0].Size != int64(len(data)) {
			t.Fatalf("versions after lost finalize = %+v", vs.Items)
		}
		rc, _, err := cl.OpenContent(ctx, rec.Version.VersionID)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, data) {
			t.Fatal("published bytes differ after chunk/finalize retries")
		}
	})

	t.Run("messages.send", func(t *testing.T) {
		sid := startMockSession(t, s.mock)
		s.lossy.loseNext(contracts.RPCPath(contracts.OpSendMessage), 2)
		m, err := cl.SendMessage(ctx, contracts.SendMessageRequest{Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "synthetic hello"}})
		if err != nil {
			t.Fatal(err)
		}
		inbox, err := cl.ListMessages(ctx, contracts.ListMessagesRequest{Box: "inbox", Address: &contracts.Address{Kind: contracts.AddrSession, ID: sid}})
		if err != nil {
			t.Fatal(err)
		}
		if len(inbox.Items) != 1 || inbox.Items[0].Envelope.MessageID != m.Envelope.MessageID {
			t.Fatalf("recipient inbox after lost responses has %d messages", len(inbox.Items))
		}
	})
	evidence(t, "%d responses dropped after commit; every retry replayed the stored result, one record each", s.lossy.dropped.Load())
}

// TestDoubledRequestsSameOperation: the same request delivered several
// times concurrently (doubled by a proxy or an impatient client) creates
// one record; the same operation_id with a different body is a conflict.
func TestDoubledRequestsSameOperation(t *testing.T) {
	ctx := ctxT(t)
	s := newAPIStack(t, stackOpts{})
	cl := s.client(ownerToken)
	sid := startMockSession(t, s.mock)

	projReq := contracts.CreateProjectRequest{Mutation: op(), Name: "doubled"}
	msgReq := contracts.SendMessageRequest{Mutation: op(), Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "once"}}
	var wg sync.WaitGroup
	ids := make([]string, 8)
	mids := make([]string, 8)
	errs := make([]error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			p, err := cl.CreateProject(ctx, projReq)
			ids[i], errs[i] = p.ProjectID, err
		}(i)
		go func(i int) {
			defer wg.Done()
			m, err := cl.SendMessage(ctx, msgReq)
			mids[i], errs[8+i] = m.Envelope.MessageID, err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("doubled request %d: %v", i, err)
		}
	}
	for i := 1; i < 8; i++ {
		if ids[i] != ids[0] || mids[i] != mids[0] {
			t.Fatalf("doubled requests returned different records: %v %v", ids, mids)
		}
	}
	all, _ := cl.ListProjects(ctx, contracts.ListProjectsRequest{Query: "doubled"})
	if len(all.Items) != 1 {
		t.Fatalf("%d projects from one doubled operation", len(all.Items))
	}
	inbox, _ := cl.ListMessages(ctx, contracts.ListMessagesRequest{Box: "inbox", Address: &contracts.Address{Kind: contracts.AddrSession, ID: sid}})
	if len(inbox.Items) != 1 {
		t.Fatalf("%d messages from one doubled operation", len(inbox.Items))
	}
	conflict := projReq
	conflict.Name = "doubled-but-different"
	if _, err := cl.CreateProject(ctx, conflict); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("same operation_id, different body = %v, want conflict", err)
	}
	conflictMsg := msgReq
	conflictMsg.Body.Text = "twice?"
	if _, err := cl.SendMessage(ctx, conflictMsg); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("same message operation, different body = %v, want conflict", err)
	}
	evidence(t, "8 concurrent copies each of projects.create and messages.send -> 1 record each; changed body -> conflict")
}

// TestDelayedDuplicateDoesNotRegressState: a stale copy of an earlier
// update arriving after a later update replays the earlier stored result
// and leaves the current state untouched.
func TestDelayedDuplicateDoesNotRegressState(t *testing.T) {
	ctx := ctxT(t)
	s := newAPIStack(t, stackOpts{})
	cl := s.client(ownerToken)
	p, err := cl.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: op(), Name: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	one, two := "one", "two"
	a := contracts.UpdateProjectRequest{Mutation: op(), ProjectID: p.ProjectID, Version: p.Version, Name: &one}
	pa, err := cl.UpdateProject(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := cl.UpdateProject(ctx, contracts.UpdateProjectRequest{Mutation: op(), ProjectID: p.ProjectID, Version: pa.Version, Name: &two})
	if err != nil {
		t.Fatal(err)
	}
	late, err := cl.UpdateProject(ctx, a) // the delayed duplicate
	if err != nil {
		t.Fatalf("delayed duplicate = %v, want the stored result", err)
	}
	if late.Name != "one" || late.Version != pa.Version {
		t.Fatalf("delayed duplicate result = %+v, want the original stored result", late)
	}
	cur, err := cl.GetProject(ctx, contracts.GetRequest{ID: p.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Name != "two" || cur.Version != pb.Version {
		t.Fatalf("state regressed after a delayed duplicate: %+v", cur)
	}
	evidence(t, "delayed duplicate replayed v%d result; current state stayed %q v%d", pa.Version, cur.Name, cur.Version)
}

// TestStaleCursorResetRequired: a watcher resuming from a cursor older than
// retention gets reset_required with a snapshot cursor (never a silent
// skip), and resuming from the snapshot cursor delivers new events.
func TestStaleCursorResetRequired(t *testing.T) {
	ctx := ctxT(t)
	s := newAPIStack(t, stackOpts{mockOpts: mock.Options{EventRetention: 10}})
	cl := s.client(ownerToken)
	api, octx := s.mock, ownerCtx()
	for i := 0; i < 30; i++ {
		if _, err := api.CreateNote(octx, contracts.CreateNoteRequest{Mutation: op(), Kind: contracts.NoteNote, Title: "n", Body: "synthetic"}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := cl.ListEvents(ctx, contracts.ListEventsRequest{AfterCursor: 1})
	if !contracts.IsCode(err, contracts.ErrResetRequired) {
		t.Fatalf("events.list from a trimmed cursor = %v, want reset_required", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var delivered int
	err = cl.WatchEvents(wctx, 1, func(contracts.Event) error { delivered++; return nil })
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrResetRequired {
		t.Fatalf("WatchEvents from stale cursor = %v, want reset_required", err)
	}
	if delivered != 0 {
		t.Fatalf("watcher was handed %d events past a gap before reset_required", delivered)
	}
	snap, ok := ce.Details["snapshot_cursor"].(float64)
	if !ok {
		t.Fatalf("reset_required lacks snapshot_cursor: %+v", ce.Details)
	}
	got := make(chan contracts.Event, 1)
	go func() {
		_ = cl.WatchEvents(wctx, int64(snap), func(e contracts.Event) error {
			if e.Kind == contracts.EvNoteCreated {
				select {
				case got <- e:
				default:
				}
			}
			return nil
		})
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := api.CreateNote(octx, contracts.CreateNoteRequest{Mutation: op(), Kind: contracts.NoteNote, Title: "after-reset", Body: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.Cursor <= int64(snap) {
			t.Fatalf("event cursor %d not after snapshot %d", e.Cursor, int64(snap))
		}
	case <-time.After(8 * time.Second):
		t.Fatal("no event after resuming from snapshot cursor")
	}
	evidence(t, "stale cursor -> reset_required (details %v); resumed from snapshot_cursor and received new events", ce.Details)
}

// TestStoragePressureArtifacts: tiny per-version and total limits. Over-
// size and over-quota publishes are rejected cleanly before any bytes are
// accepted, nothing partial is published, staged bytes are cleaned up, and
// an oversized chunk is rejected without corrupting a resumable upload.
func TestStoragePressureArtifacts(t *testing.T) {
	ctx := ctxT(t)
	s := newAPIStack(t, stackOpts{
		artOpts:  artifacts.Options{MaxBytes: 64 << 10, MaxTotalBytes: 100 << 10},
		httpOpts: httpapi.Options{MaxChunkBytes: 16 << 10},
	})
	cl := s.client(ownerToken)
	blob := func(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

	_, err := cl.Publish(ctx, bytes.NewReader(blob(80<<10, 'a')), client.PublishMeta{Title: "too-big", Filename: "big.bin", ChunkSize: 16 << 10})
	if !contracts.IsCode(err, contracts.ErrPayloadTooLarge) {
		t.Fatalf("over-size publish = %v, want payload_too_large", err)
	}
	if _, err := cl.Publish(ctx, bytes.NewReader(blob(40<<10, 'b')), client.PublishMeta{Title: "fits-1", Filename: "1.bin", ChunkSize: 16 << 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Publish(ctx, bytes.NewReader(blob(40<<10, 'c')), client.PublishMeta{Title: "fits-2", Filename: "2.bin", ChunkSize: 16 << 10}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = cl.Publish(ctx, bytes.NewReader(blob(40<<10, 'd')), client.PublishMeta{Title: "over-quota", Filename: "3.bin", ChunkSize: 16 << 10})
	if !contracts.IsCode(err, contracts.ErrCapacityExhausted) {
		t.Fatalf("over-quota publish = %v, want capacity_exhausted", err)
	}
	quotaRetry := time.Since(start)

	// Oversized chunk: rejected; the same publish resumes with a valid size.
	meta := client.PublishMeta{Title: "resume", Filename: "r.bin", ChunkSize: 32 << 10, OperationID: contracts.NewID(contracts.PrefixOperation)}
	data := blob(20<<10, 'e')
	if _, err := cl.Publish(ctx, bytes.NewReader(data), meta); !contracts.IsCode(err, contracts.ErrPayloadTooLarge) {
		// 20 KiB fits the remaining quota but one 32 KiB chunk exceeds the
		// server's 16 KiB chunk bound.
		t.Fatalf("oversized chunk = %v, want payload_too_large", err)
	}
	meta.ChunkSize = 8 << 10
	rec, err := cl.Publish(ctx, bytes.NewReader(data), meta)
	if err != nil {
		t.Fatalf("resumed publish with valid chunks: %v", err)
	}
	if rec.Version.Size != int64(len(data)) {
		t.Fatalf("resumed version size %d", rec.Version.Size)
	}

	arts, err := cl.ListArtifacts(ctx, contracts.ListArtifactsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, a := range arts.Items {
		titles[a.Title] = true
	}
	if titles["too-big"] || titles["over-quota"] || !titles["fits-1"] || !titles["fits-2"] || !titles["resume"] {
		t.Fatalf("published artifacts under pressure = %v", titles)
	}
	staged, _ := os.ReadDir(filepath.Join(s.artDir, "uploads"))
	if len(staged) != 0 {
		t.Fatalf("%d staged upload files left behind", len(staged))
	}
	blobs, used, err := s.arts.StorageUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if blobs+used > 100<<10 {
		t.Fatalf("storage %d+%d exceeds the 100 KiB bound", blobs, used)
	}
	evidence(t, "over-size -> payload_too_large; over-quota -> capacity_exhausted after client retries (%v, retryable code); oversized chunk rejected then resumed; usage %d B blobs + %d B open uploads <= 100 KiB",
		quotaRetry.Round(time.Millisecond), blobs, used)
}
