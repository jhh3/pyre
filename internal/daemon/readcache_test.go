package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

var synthProject = "prj_01SYNTHETICPROJECT000000000"

func synthDoc(rev string) contracts.DocumentWithRevision {
	return contracts.DocumentWithRevision{
		Document: contracts.Document{DocumentID: "doc_01SYNTHETIC0000000000000000", ProjectID: &synthProject, Title: "Brief", Slug: "brief",
			HeadRevisionID: rev, HeadSeq: 1},
		Revision: contracts.DocumentRevision{RevisionID: rev, DocumentID: "doc_01SYNTHETIC0000000000000000", Seq: 1, Content: "synthetic brief " + rev},
	}
}

func (c *hubClient) GetDocument(ctx context.Context, r contracts.GetDocumentRequest) (contracts.DocumentWithRevision, error) {
	if err := c.unavailable(); err != nil {
		return contracts.DocumentWithRevision{}, err
	}
	if r.DocumentID == "doc_missing" {
		return contracts.DocumentWithRevision{}, contracts.Errorf(contracts.ErrNotFound, "no such document")
	}
	return synthDoc("rev_head"), nil
}

func (c *hubClient) GetProject(ctx context.Context, r contracts.GetRequest) (contracts.Project, error) {
	if err := c.unavailable(); err != nil {
		return contracts.Project{}, err
	}
	return contracts.Project{ProjectID: r.ID, Name: "synthetic-project"}, nil
}

func (c *hubClient) ListDocuments(ctx context.Context, r contracts.ListDocumentsRequest) (contracts.Page[contracts.Document], error) {
	if err := c.unavailable(); err != nil {
		return contracts.Page[contracts.Document]{}, err
	}
	return contracts.Page[contracts.Document]{Items: []contracts.Document{synthDoc("rev_head").Document}, SnapshotAt: time.Now()}, nil
}

// During a hub outage the node answers project/document reads it has seen
// from its read cache, per caller, marked with the copy's age; reads it has
// not seen (or seen only for another principal) still report the outage.
func TestReadCacheServesProjectMaterialDuringHubOutage(t *testing.T) {
	dir := shortDir(t)
	hub := &fakeHub{byOp: map[string]contracts.MessageView{}}
	ma := mock.NewAdapter()
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ma}, Upstream: hub.client, OwnerToken: "pyre_owner_test_token_0000000000", NodeName: "cachenode", OutboxFlush: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- n.d.ServeLocal(ctx) }()
	sock := n.d.SocketPath()
	waitFor(t, "socket", func() bool { _, err := os.Stat(sock); return err == nil })
	sc := startCmd("ses_c", "run_c", contracts.HarnessMock, t.TempDir())
	sc.SessionToken = "pyre_s_session_token_for_cache_0000"
	n.start(sc)

	owner := socketClient(sock, "")
	agent := socketClient(sock, sc.SessionToken)
	var notes []contracts.Freshness
	fctx := client.WithFreshnessSink(ctx, func(op string, f contracts.Freshness) { notes = append(notes, f) })

	// Hub up: live answers, cached as a side effect, no freshness header.
	byID := contracts.GetDocumentRequest{DocumentID: "doc_01SYNTHETIC0000000000000000"}
	live, err := owner.GetDocument(fctx, byID)
	if err != nil || live.Revision.Content != "synthetic brief rev_head" {
		t.Fatalf("live get: %+v %v", live, err)
	}
	if _, err := owner.ListDocuments(fctx, contracts.ListDocumentsRequest{ProjectID: &synthProject}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.GetProject(fctx, contracts.GetRequest{ID: synthProject}); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Fatalf("live answers carried freshness: %+v", notes)
	}
	confirmed := time.Now()

	hub.down.Store(true)
	got, err := owner.GetDocument(fctx, byID)
	if err != nil || got.Revision.Content != live.Revision.Content || got.Document.HeadRevisionID != "rev_head" {
		t.Fatalf("cached get: %+v %v", got, err)
	}
	if len(notes) != 1 || !notes[0].Stale || notes[0].Source != "node-cache:"+n.d.NodeID() || notes[0].ObservedAt.After(confirmed) || !strings.Contains(notes[0].Reason, "hub unavailable") {
		t.Fatalf("cached freshness: %+v", notes)
	}
	// The same head is readable by project+slug (alias written on the live read).
	if got, err := owner.GetDocument(fctx, contracts.GetDocumentRequest{ProjectID: synthProject, Slug: "brief"}); err != nil || got.Revision.RevisionID != "rev_head" {
		t.Fatalf("alias get: %+v %v", got, err)
	}
	page, err := owner.ListDocuments(fctx, contracts.ListDocumentsRequest{ProjectID: &synthProject})
	if err != nil || len(page.Items) != 1 || page.SnapshotAt.After(confirmed) || page.SnapshotAt.IsZero() {
		t.Fatalf("cached list: %+v %v", page, err)
	}
	if p, err := owner.GetProject(fctx, contracts.GetRequest{ID: synthProject}); err != nil || p.Name != "synthetic-project" {
		t.Fatalf("cached project: %+v %v", p, err)
	}
	// Another principal (a session) never read it: no cross-principal answers.
	if _, err := agent.GetDocument(fctx, byID); !contracts.IsCode(err, contracts.ErrUnavailable) || !strings.Contains(err.Error(), "no copy") {
		t.Fatalf("session read of the owner's cache: %v", err)
	}
	// Never seen: still unavailable.
	if _, err := owner.GetDocument(fctx, contracts.GetDocumentRequest{DocumentID: "doc_never_read"}); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("uncached read while down: %v", err)
	}
	// Writes still need the hub.
	if _, err := owner.ProposeRevision(fctx, contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: "op_prop"}, DocumentID: byID.DocumentID, BaseRevisionID: "rev_head", Content: "x"}); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("propose while down: %v", err)
	}

	// Hub back: live again, no header; a not-found is not masked by the cache.
	hub.down.Store(false)
	notes = nil
	if _, err := owner.GetDocument(fctx, byID); err != nil || len(notes) != 0 {
		t.Fatalf("live after outage: %v %+v", err, notes)
	}
	if _, err := owner.GetDocument(fctx, contracts.GetDocumentRequest{DocumentID: "doc_missing"}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("not found: %v", err)
	}
	cancel()
	<-served
}

func TestReadCacheIsBounded(t *testing.T) {
	dir := shortDir(t)
	// A writer queue that holds every value, so the table itself overflows.
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}, ReadCacheQueue: readCacheMaxEntries + 200})
	ctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "local"})
	for i := 0; i < readCacheMaxEntries+150; i++ {
		n.d.cachePut(ctx, contracts.OpGetProject, contracts.GetRequest{ID: fmt.Sprintf("prj_%d", i)}, contracts.Project{ProjectID: fmt.Sprint(i)})
	}
	n.d.cache.wg.Wait() // the writer drains the whole queue (one fsync per value)
	if st := n.d.ReadCacheStats(); st.Dropped != 0 || st.Written < readCacheMaxEntries+150 {
		t.Fatalf("writer stats %+v", st)
	}
	n.d.pruneReadCache(ctx)
	var count int
	if err := n.d.db.QueryRow(`SELECT COUNT(*) FROM read_cache`).Scan(&count); err != nil || count > readCacheMaxEntries {
		t.Fatalf("read cache holds %d entries (max %d): %v", count, readCacheMaxEntries, err)
	}
	// Oversized values are not cached at all.
	big := contracts.Project{ProjectID: "prj_big", Description: strings.Repeat("x", readCacheMaxValue+1)}
	n.d.cachePut(ctx, contracts.OpGetProject, contracts.GetRequest{ID: "prj_big"}, big)
	var out contracts.Project
	if _, ok := n.d.cacheGet(ctx, contracts.OpGetProject, contracts.GetRequest{ID: "prj_big"}, &out); ok {
		t.Fatal("oversized value was cached")
	}
}

func (c *hubClient) ProposeRevision(ctx context.Context, r contracts.ProposeRevisionRequest) (contracts.DocumentWithRevision, error) {
	if err := c.unavailable(); err != nil {
		return contracts.DocumentWithRevision{}, err
	}
	return synthDoc("rev_next"), nil
}

// While the hub link is down pyred's "unavailable" answer is definitive:
// it carries client.NoRetryHeader so the CLI exits at once instead of
// retrying the socket with backoff (measured ~4 s before). With the link
// up the header is absent.
func TestOutageUnavailableIsDefinitiveForSocketClients(t *testing.T) {
	dir := shortDir(t)
	hub := &fakeHub{byOp: map[string]contracts.MessageView{}}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}, Upstream: hub.client, OwnerToken: "pyre_owner_test_token_0000000000", NodeName: "norretry", OutboxFlush: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- n.d.ServeLocal(ctx) }()
	sock := n.d.SocketPath()
	waitFor(t, "socket", func() bool { _, err := os.Stat(sock); return err == nil })
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	cl := client.New("http://pyred.local", "", client.Options{HTTPClient: hc, MaxAttempts: 5, RetryBase: 300 * time.Millisecond, RetryMax: time.Second})
	raw := func() http.Header {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://pyred.local"+contracts.RPCPath(contracts.OpGetDocument), strings.NewReader(`{"document_id":"doc_never_read"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status %d", resp.StatusCode)
		}
		return resp.Header
	}

	hub.down.Store(true)
	if h := raw(); h.Get(client.NoRetryHeader) != "" {
		t.Fatalf("link up (hub flaky): header %q set; the client's retries should apply", h.Get(client.NoRetryHeader))
	}
	n.d.SetHubLink(false)
	if h := raw(); h.Get(client.NoRetryHeader) == "" {
		t.Fatal("link down: unavailable answer lacks the no-retry header")
	}
	start := time.Now()
	_, err := cl.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: "doc_never_read"})
	if !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("uncached read during outage: %v", err)
	}
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Fatalf("uncached read took %s; the client retried a definitive answer", d)
	}
	cancel()
	<-served
}
