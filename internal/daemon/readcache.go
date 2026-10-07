package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Node-local read cache for hub-owned project material (architecture §3:
// project metadata, document revisions and review comments are owned by the
// hub; other machines keep "cached reads"). Every successful proxied read of
// projects, documents, revisions, comments, notes and launch context is kept
// per caller principal. While the hub is unreachable the same read is
// answered from that copy: the body is exactly what the hub returned, list
// pages carry snapshot_at = when the copy was last confirmed, and the local
// socket response carries client.FreshnessHeader (source "node-cache:<node>",
// stale, observed_at). Nothing is written through the cache: edits still
// need the hub (offline edits remain proposals, never local commits).
//
// The cache lives in its own migration namespace so an older pyred (rollback)
// still opens the database. It is bounded by entry count and entry size.
//
// Writing the cache is background work: the read that produced a value
// queues it (bounded by Config.ReadCacheQueue, coalesced per key) and
// returns; one writer goroutine stores the queue, so an interactive read
// never waits for a cache write and its fsync. A queued value already
// answers offline reads. When the queue is full the value is not cached
// (the read itself is unaffected; the next read caches it).

// ReadCacheMigrations is the "readcache" namespace.
var ReadCacheMigrations = []sqlitex.Migration{{Namespace: "readcache", Version: 1, Name: "read_cache", SQL: `
CREATE TABLE read_cache (
	key        TEXT PRIMARY KEY,  -- sha256(principal, op, request)
	op         TEXT NOT NULL,
	principal  TEXT NOT NULL,
	digest     TEXT NOT NULL,
	value      BLOB NOT NULL,
	cached_at  TEXT NOT NULL      -- last time the hub returned this value
);
CREATE INDEX read_cache_age ON read_cache(cached_at);
`}}

const (
	readCacheMaxEntries = 2000
	readCacheMaxValue   = 2 << 20 // larger responses are not cached
	// readCacheTouchEvery bounds writes for unchanged values: an identical
	// response refreshes cached_at at most this often.
	readCacheTouchEvery = time.Minute
)

func principalKey(ctx context.Context) string {
	p, _ := contracts.PrincipalFrom(ctx)
	return string(p.Kind) + ":" + p.ID
}

func readCacheKey(principal, op string, req any) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(principal))
	h.Write([]byte{0})
	h.Write([]byte(op))
	h.Write([]byte{0})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DefaultReadCacheQueue is Config.ReadCacheQueue's default.
const DefaultReadCacheQueue = 256

// ReadCacheStats describes the read cache's background writer.
type ReadCacheStats struct {
	Pending    int `json:"pending"`     // values queued or being written
	MaxPending int `json:"max_pending"` // high-water mark (never above Queue)
	Queue      int `json:"queue"`       // the cap
	Written    int `json:"written"`
	Coalesced  int `json:"coalesced"` // replaced a queued value for the same key
	Dropped    int `json:"dropped"`   // not cached: queue full
}

// cacheWriter queues read-cache writes for one background writer.
type cacheWriter struct {
	mu      sync.Mutex
	pending map[string]*cacheEntry
	order   []string // keys waiting to be written, oldest first
	running bool
	closed  bool // Close: no new writer starts
	wg      sync.WaitGroup
	stats   ReadCacheStats
	// hook runs before each write (tests slow the writer down with it).
	hook func()
}

type cacheEntry struct {
	op, principal string
	v             any
	at            time.Time
	writing       bool
}

// cachePut queues v as the answer to (caller, op, req) and returns at once.
// Failures only skip caching; they never fail or delay the read.
func (d *Daemon) cachePut(ctx context.Context, op string, req, v any) {
	pk := principalKey(ctx)
	key, err := readCacheKey(pk, op, req)
	if err != nil {
		return
	}
	e := &cacheEntry{op: op, principal: pk, v: v, at: d.now()}
	w := &d.cache
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == nil {
		w.pending = map[string]*cacheEntry{}
	}
	if old, ok := w.pending[key]; ok {
		w.pending[key] = e
		w.stats.Coalesced++
		if old.writing { // the writer took the old value: queue the key again
			w.order = append(w.order, key)
		}
		return
	}
	if w.closed || len(w.pending) >= d.cfg.ReadCacheQueue {
		w.stats.Dropped++
		return
	}
	w.pending[key] = e
	w.order = append(w.order, key)
	w.stats.MaxPending = max(w.stats.MaxPending, len(w.pending))
	if !w.running {
		w.running = true
		w.wg.Add(1)
		go d.writeCache()
	}
}

// writeCache stores queued values one at a time until the queue is empty.
func (d *Daemon) writeCache() {
	w := &d.cache
	defer w.wg.Done()
	for {
		w.mu.Lock()
		if len(w.order) == 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		key := w.order[0]
		w.order = w.order[1:]
		e := w.pending[key]
		if e != nil {
			e.writing = true
		}
		hook := w.hook
		w.mu.Unlock()
		if e == nil {
			continue
		}
		if hook != nil {
			hook()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		d.storeCache(ctx, key, e)
		cancel()
		w.mu.Lock()
		if w.pending[key] == e {
			delete(w.pending, key)
		}
		w.stats.Written++
		w.mu.Unlock()
	}
}

// ReadCacheStats returns the read cache writer's counters.
func (d *Daemon) ReadCacheStats() ReadCacheStats {
	d.cache.mu.Lock()
	defer d.cache.mu.Unlock()
	s := d.cache.stats
	s.Pending = len(d.cache.pending)
	s.Queue = d.cfg.ReadCacheQueue
	return s
}

// storeCache writes one queued value.
func (d *Daemon) storeCache(ctx context.Context, key string, e *cacheEntry) {
	val, err := json.Marshal(e.v)
	if err != nil || len(val) > readCacheMaxValue {
		return
	}
	sum := sha256.Sum256(val)
	digest := hex.EncodeToString(sum[:])
	now := e.at
	var oldDigest, oldAt string
	if err := d.db.QueryRowContext(ctx, `SELECT digest, cached_at FROM read_cache WHERE key=?`, key).Scan(&oldDigest, &oldAt); err == nil && oldDigest == digest {
		if t, perr := sqlitex.ParseTime(oldAt); perr == nil && now.Sub(t) < readCacheTouchEvery && !now.Before(t) {
			return
		}
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO read_cache(key, op, principal, digest, value, cached_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET digest=excluded.digest, value=excluded.value, cached_at=excluded.cached_at`,
		key, e.op, e.principal, digest, val, ftime(now)); err != nil {
		return
	}
	if d.cacheWrites.Add(1)%64 == 1 {
		d.pruneReadCache(ctx)
	}
}

func (d *Daemon) pruneReadCache(ctx context.Context) {
	_, _ = d.db.ExecContext(ctx, `DELETE FROM read_cache WHERE key IN (
		SELECT key FROM read_cache ORDER BY cached_at DESC LIMIT -1 OFFSET ?)`, readCacheMaxEntries)
}

// cacheGet decodes the cached answer to (caller, op, req) into out.
func (d *Daemon) cacheGet(ctx context.Context, op string, req, out any) (time.Time, bool) {
	key, err := readCacheKey(principalKey(ctx), op, req)
	if err != nil {
		return time.Time{}, false
	}
	d.cache.mu.Lock()
	e := d.cache.pending[key]
	d.cache.mu.Unlock()
	if e != nil { // queued, not yet written
		val, err := json.Marshal(e.v)
		if err != nil || len(val) > readCacheMaxValue || json.Unmarshal(val, out) != nil {
			return time.Time{}, false
		}
		return e.at, true
	}
	var val []byte
	var at string
	err = d.db.QueryRowContext(ctx, `SELECT value, cached_at FROM read_cache WHERE key=?`, key).Scan(&val, &at)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return time.Time{}, false
	}
	if json.Unmarshal(val, out) != nil {
		return time.Time{}, false
	}
	t, _ := sqlitex.ParseTime(at)
	return t, true
}

// cacheFreshness describes an answer served from the read cache.
func (d *Daemon) cacheFreshness(at time.Time) contracts.Freshness {
	return contracts.Freshness{ObservedAt: at, Source: "node-cache:" + d.nodeID, Stale: true,
		Reason: "hub unavailable; copy cached on this node, last confirmed with the hub at " + at.UTC().Format(time.RFC3339)}
}

// cachedRead proxies a read to the hub and caches the answer; when the hub
// is unreachable it returns the cached answer (fromCache non-zero) or the
// original unavailable error when there is none.
func cachedRead[Req, Resp any](l *LocalAPI, ctx context.Context, op string, req Req, call func(context.Context, Req) (Resp, error)) (Resp, time.Time, error) {
	resp, err := call(ctx, req)
	if err == nil {
		l.d.cachePut(ctx, op, req, resp)
		return resp, time.Time{}, nil
	}
	if !hubDown(err) {
		return resp, time.Time{}, err
	}
	var out Resp
	at, ok := l.d.cacheGet(ctx, op, req, &out)
	if !ok {
		return resp, time.Time{}, contracts.Errorf(contracts.ErrUnavailable, "%s; no copy of this %s is cached on node %s", errMessage(err), op, l.d.nodeID)
	}
	noteFreshness(ctx, l.d.cacheFreshness(at))
	return out, at, nil
}

func errMessage(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce.Message
	}
	return err.Error()
}

// ---- freshness annotation on the local socket response ----

type freshnessNoteKey struct{}

type freshnessNote struct {
	mu sync.Mutex
	f  *contracts.Freshness
}

func noteFreshness(ctx context.Context, f contracts.Freshness) {
	if n, ok := ctx.Value(freshnessNoteKey{}).(*freshnessNote); ok {
		n.mu.Lock()
		n.f = &f
		n.mu.Unlock()
	}
}

// freshnessWriter adds client.FreshnessHeader before the response is
// written when the call noted a cached answer, and client.NoRetryHeader on
// an "unavailable" answer given while the hub is known unreachable.
type freshnessWriter struct {
	http.ResponseWriter
	note        *freshnessNote
	wroteHeader bool
	// definitive reports whether an unavailable answer is final for this
	// call (the hub link is down, or no hub is configured): pyred already
	// made its single attempt or answered from local state, so a client
	// retrying the socket with backoff only delays the same answer.
	definitive func() bool
}

func (w *freshnessWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.note.mu.Lock()
		f := w.note.f
		w.note.mu.Unlock()
		if f != nil && code >= 200 && code < 300 {
			if b, err := json.Marshal(f); err == nil {
				w.Header().Set(client.FreshnessHeader, string(b))
			}
		}
		if code == http.StatusServiceUnavailable && w.definitive != nil && w.definitive() {
			w.Header().Set(client.NoRetryHeader, "hub-unreachable")
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *freshnessWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *freshnessWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---- cached LocalAPI reads ----

func (l *LocalAPI) GetProject(ctx context.Context, r contracts.GetRequest) (contracts.Project, error) {
	v, _, err := cachedRead(l, ctx, contracts.OpGetProject, r, l.proxy.GetProject)
	return v, err
}

func (l *LocalAPI) ListProjects(ctx context.Context, r contracts.ListProjectsRequest) (contracts.Page[contracts.Project], error) {
	v, at, err := cachedRead(l, ctx, contracts.OpListProjects, r, l.proxy.ListProjects)
	if !at.IsZero() {
		v.SnapshotAt = at
	}
	return v, err
}

// docAliases are the GetDocument requests a head read also answers, so a
// document read by ID can be read offline by project+slug and vice versa.
func docAliases(dr contracts.DocumentWithRevision) []contracts.GetDocumentRequest {
	out := []contracts.GetDocumentRequest{{DocumentID: dr.Document.DocumentID}}
	if dr.Document.ProjectID != nil && dr.Document.Slug != "" {
		out = append(out, contracts.GetDocumentRequest{ProjectID: *dr.Document.ProjectID, Slug: dr.Document.Slug})
	}
	return out
}

func (l *LocalAPI) cacheDocHead(ctx context.Context, dr contracts.DocumentWithRevision) {
	if dr.Document.DocumentID == "" || dr.Revision.RevisionID != dr.Document.HeadRevisionID {
		return
	}
	for _, a := range docAliases(dr) {
		l.d.cachePut(ctx, contracts.OpGetDocument, a, dr)
	}
	l.d.cachePut(ctx, contracts.OpGetDocument, contracts.GetDocumentRequest{DocumentID: dr.Document.DocumentID, RevisionID: dr.Revision.RevisionID}, dr)
}

func (l *LocalAPI) GetDocument(ctx context.Context, r contracts.GetDocumentRequest) (contracts.DocumentWithRevision, error) {
	v, at, err := cachedRead(l, ctx, contracts.OpGetDocument, r, l.proxy.GetDocument)
	if err == nil && at.IsZero() && r.RevisionID == "" {
		l.cacheDocHead(ctx, v)
	}
	return v, err
}

func (l *LocalAPI) CreateDocument(ctx context.Context, r contracts.CreateDocumentRequest) (contracts.DocumentWithRevision, error) {
	v, err := l.proxy.CreateDocument(ctx, r)
	if err == nil {
		l.cacheDocHead(ctx, v)
	}
	return v, err
}

func (l *LocalAPI) ProposeRevision(ctx context.Context, r contracts.ProposeRevisionRequest) (contracts.DocumentWithRevision, error) {
	v, err := l.proxy.ProposeRevision(ctx, r)
	if err == nil {
		l.cacheDocHead(ctx, v)
	}
	return v, err
}

func (l *LocalAPI) ListDocuments(ctx context.Context, r contracts.ListDocumentsRequest) (contracts.Page[contracts.Document], error) {
	v, at, err := cachedRead(l, ctx, contracts.OpListDocuments, r, l.proxy.ListDocuments)
	if !at.IsZero() {
		v.SnapshotAt = at
	}
	return v, err
}

func (l *LocalAPI) ListRevisions(ctx context.Context, r contracts.ListRevisionsRequest) (contracts.Page[contracts.DocumentRevision], error) {
	v, at, err := cachedRead(l, ctx, contracts.OpListRevisions, r, l.proxy.ListRevisions)
	if !at.IsZero() {
		v.SnapshotAt = at
	}
	return v, err
}

func (l *LocalAPI) GetComment(ctx context.Context, r contracts.GetRequest) (contracts.ReviewComment, error) {
	v, _, err := cachedRead(l, ctx, contracts.OpGetComment, r, l.proxy.GetComment)
	return v, err
}

func (l *LocalAPI) ListComments(ctx context.Context, r contracts.ListCommentsRequest) (contracts.Page[contracts.ReviewComment], error) {
	v, at, err := cachedRead(l, ctx, contracts.OpListComments, r, l.proxy.ListComments)
	if !at.IsZero() {
		v.SnapshotAt = at
	}
	return v, err
}

func (l *LocalAPI) GetNote(ctx context.Context, r contracts.GetRequest) (contracts.Note, error) {
	v, _, err := cachedRead(l, ctx, contracts.OpGetNote, r, l.proxy.GetNote)
	return v, err
}

func (l *LocalAPI) ListNotes(ctx context.Context, r contracts.ListNotesRequest) (contracts.Page[contracts.Note], error) {
	v, at, err := cachedRead(l, ctx, contracts.OpListNotes, r, l.proxy.ListNotes)
	if !at.IsZero() {
		v.SnapshotAt = at
	}
	return v, err
}

func (l *LocalAPI) GetLaunchContext(ctx context.Context, r contracts.ContextRequest) (contracts.LaunchContext, error) {
	v, _, err := cachedRead(l, ctx, contracts.OpGetLaunchContext, r, l.proxy.GetLaunchContext)
	return v, err
}
