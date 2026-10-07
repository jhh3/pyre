// Package mock is Pyre's deterministic in-memory backend. It implements the
// whole contracts.API with real contract semantics (operation idempotency,
// message dedup, independent delivery facts, approvals, leases, document
// CAS, verified immutable artifacts, attention, events with retention) so
// the CLI, TUI, Android app and agent tools can be built and tested before
// the real hub and runtimes exist.
//
// Everything the mock reports is labelled as mock: Hello.Mock is true, run
// and session freshness sources are "mock", and runtime acceptance carries
// EvidenceMock. Mock results are never runtime validation.
//
// Time and delivery progress are deterministic: an injectable Clock supplies
// timestamps, and simulated runtime/node behavior advances only when Tick
// (or Settle, or the optional Run loop) is called. Long-poll waits use real
// time, bounded by WaitSeconds.
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Options configures a Backend.
type Options struct {
	// Clock supplies timestamps (default: system clock). Use NewFakeClock
	// for tests or NewOffsetClock(FixtureEpoch) so fixture ages look right.
	Clock c.Clock
	// Empty starts with no seeded data (no nodes, sessions, ...).
	Empty bool
	// FixtureDirs are loaded (sorted *.json per dir, dirs in order) and
	// merged; later records with the same ID replace earlier ones. When
	// nil and Empty is false, DefaultFixtureDirs() is used.
	FixtureDirs []string
	// Fixtures are merged after FixtureDirs.
	Fixtures []*c.FixtureBundle
	// DefaultPrincipal is used when a call's context carries no principal
	// (in-process use without httpapi). Nil means such calls fail with
	// unauthorized.
	DefaultPrincipal *c.Principal
	// HubID / HubURL / HubCertSHA256 appear in Hello and tickets.
	HubID         string
	HubURL        string
	HubCertSHA256 string
	// EventRetention bounds the event log; older cursors get
	// reset_required (default 10000).
	EventRetention int
	// BusyTicks is how many ticks a run stays "running" after accepting
	// input or an approval answer (default 1).
	BusyTicks int
	// MaxUploadBytes bounds a single artifact upload (default 256 MiB).
	MaxUploadBytes int64
}

// Backend is the in-memory mock. Safe for concurrent use.
type Backend struct {
	opts  Options
	clock c.Clock

	mu      sync.Mutex
	changed chan struct{}

	nodes       map[string]*c.Node
	clients     map[string]*c.ClientInfo
	pairings    map[string]*pairing
	sessions    map[string]*c.Session
	runs        map[string]*c.Run
	transcripts map[string][]c.TranscriptEntry
	messages    map[string]*c.MessageView
	msgSeq      int64
	approvals   map[string]*c.Approval
	leases      map[string]*c.InputLease
	acctLeases  map[string]*c.AccountLease
	cmdResults  map[string]cmdResult
	projects    map[string]*c.Project
	documents   map[string]*c.Document
	revisions   map[string]*c.DocumentRevision
	proposals   map[string]*c.DocumentProposal
	artifacts   map[string]*c.Artifact
	versions    map[string]*c.ArtifactVersion
	contents    map[string][]byte
	uploads     map[string]*upload
	comments    map[string]*c.ReviewComment
	notes       map[string]*c.Note
	accounts    map[string]*c.AccountView
	fallbacks   []c.FallbackPlan
	attention   map[string]*c.AttentionItem
	events      []c.Event
	eventHead   int64
	eventFloor  int64 // events with cursor <= floor were trimmed
	ops         map[string]opRecord
	prefs       c.NotificationPrefs
	pushRegs    []c.PushRegistration

	envs        map[string][]c.EnvironmentGeneration // per node, oldest first
	tools       map[string][]c.ToolVersion           // per node
	drift       []c.DriftItem
	plans       map[string]c.ChangePlan
	connections map[string]*c.Connection

	// simulation state
	busy       map[string]int    // run -> remaining busy ticks
	pendingCmd map[string]string // run -> "start" | "interrupt" | "stop"
	lastState  map[string]c.RunState
	tickCount  int64
}

type pairing struct {
	ticket   c.PairingTicket
	kind     string
	name     string
	redeemed bool
}

type upload struct {
	up      c.Upload
	req     c.BeginUploadRequest
	owner   c.Principal
	data    []byte
	receipt *c.PublishReceipt
}

type opRecord struct {
	op, who, digest string
	resp            []byte
	err             *c.Error
}

// FixtureEpoch is the synthetic "now" of the shared contract fixtures.
var FixtureEpoch = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// New returns a Backend seeded per opts. Fixture load errors panic only if
// FixtureDirs/Fixtures were given explicitly; use NewE to handle them.
func New(opts Options) *Backend {
	b, err := NewE(opts)
	if err != nil {
		panic("mock: " + err.Error())
	}
	return b
}

// NewE is New returning fixture errors.
func NewE(opts Options) (*Backend, error) {
	if opts.Clock == nil {
		opts.Clock = c.SystemClock{}
	}
	if opts.HubID == "" {
		opts.HubID = "hub_mock"
	}
	if opts.HubURL == "" {
		opts.HubURL = "http://127.0.0.1:7788"
	}
	if opts.EventRetention <= 0 {
		opts.EventRetention = 10000
	}
	if opts.BusyTicks <= 0 {
		opts.BusyTicks = 1
	}
	if opts.MaxUploadBytes <= 0 {
		opts.MaxUploadBytes = 256 << 20
	}
	b := &Backend{
		opts: opts, clock: opts.Clock, changed: make(chan struct{}),
		nodes: map[string]*c.Node{}, clients: map[string]*c.ClientInfo{}, pairings: map[string]*pairing{},
		sessions: map[string]*c.Session{}, runs: map[string]*c.Run{}, transcripts: map[string][]c.TranscriptEntry{},
		messages: map[string]*c.MessageView{}, approvals: map[string]*c.Approval{}, leases: map[string]*c.InputLease{},
		projects: map[string]*c.Project{}, documents: map[string]*c.Document{}, revisions: map[string]*c.DocumentRevision{},
		proposals: map[string]*c.DocumentProposal{}, artifacts: map[string]*c.Artifact{}, versions: map[string]*c.ArtifactVersion{},
		contents: map[string][]byte{}, uploads: map[string]*upload{}, comments: map[string]*c.ReviewComment{}, notes: map[string]*c.Note{},
		accounts: map[string]*c.AccountView{}, acctLeases: map[string]*c.AccountLease{}, cmdResults: map[string]cmdResult{}, attention: map[string]*c.AttentionItem{}, ops: map[string]opRecord{},
		envs: map[string][]c.EnvironmentGeneration{}, tools: map[string][]c.ToolVersion{}, plans: map[string]c.ChangePlan{},
		connections: map[string]*c.Connection{},
		busy:        map[string]int{}, pendingCmd: map[string]string{}, lastState: map[string]c.RunState{},
	}
	if !opts.Empty {
		var bundles []*c.FixtureBundle
		dirs := opts.FixtureDirs
		if dirs == nil {
			dirs = DefaultFixtureDirs()
		}
		for _, d := range dirs {
			bs, err := c.LoadFixtureDir(d)
			if err != nil {
				return nil, err
			}
			bundles = append(bundles, bs...)
		}
		bundles = append(bundles, opts.Fixtures...)
		for _, f := range bundles {
			if err := f.Validate(); err != nil {
				return nil, err
			}
			b.seed(f)
		}
		b.finishSeed()
	}
	return b, nil
}

// Clock returns the backend's clock.
func (b *Backend) Clock() c.Clock { return b.clock }

func (b *Backend) now() time.Time { return b.clock.Now().UTC() }

// ---- principal & authorization ----

func (b *Backend) principal(ctx context.Context) (c.Principal, error) {
	if p, ok := c.PrincipalFrom(ctx); ok && p.Kind != "" {
		return p, nil
	}
	if b.opts.DefaultPrincipal != nil {
		return *b.opts.DefaultPrincipal, nil
	}
	return c.Principal{}, c.Errorf(c.ErrUnauthorized, "no authenticated principal")
}

func who(p c.Principal) string { return string(p.Kind) + ":" + p.ID }

func samePrincipal(a, b c.Principal) bool { return a.Kind == b.Kind && a.ID == b.ID }

func requireOwner(p c.Principal) error {
	if p.Kind != c.PrincipalOwner && p.Kind != c.PrincipalSystem {
		return c.Errorf(c.ErrForbidden, "%s principals may not perform this operation", p.Kind)
	}
	return nil
}

func requireNotNode(p c.Principal) error {
	if p.Kind == c.PrincipalNode {
		return c.Errorf(c.ErrForbidden, "node principals report through the node channel")
	}
	return nil
}

// ---- idempotent mutations ----

func digestOf(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// mutate runs fn at most once per operation id with the same semantics as
// the hub's sqlitex.Idempotent: identical retry replays the stored result
// (or stored contract error); a different op/principal/body is a conflict.
// fn runs with b.mu held.
func mutate[Resp any](b *Backend, ctx context.Context, op string, m c.Mutation, req any, fn func(p c.Principal) (Resp, error)) (Resp, error) {
	var zero Resp
	p, err := b.principal(ctx)
	if err != nil {
		return zero, err
	}
	if m.OperationID == "" {
		return zero, c.Errorf(c.ErrInvalid, "operation_id is required")
	}
	if len(m.OperationID) > 200 {
		return zero, c.Errorf(c.ErrInvalid, "operation_id too long")
	}
	digest := digestOf(req)
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec, ok := b.ops[m.OperationID]; ok {
		if rec.op != op || rec.who != who(p) || rec.digest != digest {
			return zero, c.Errorf(c.ErrConflict, "operation_id reused with a different request body").WithRef(c.KindOperation, m.OperationID)
		}
		if rec.err != nil {
			e := *rec.err
			return zero, &e
		}
		var out Resp
		if err := json.Unmarshal(rec.resp, &out); err != nil {
			return zero, c.Errorf(c.ErrInternal, "mock: replay decode: %v", err)
		}
		return out, nil
	}
	resp, err := fn(p)
	if err != nil {
		var ce *c.Error
		if errors.As(err, &ce) && ce.Code != c.ErrInternal && ce.Code != c.ErrUnavailable && !ce.Retryable {
			stored := *ce
			b.ops[m.OperationID] = opRecord{op: op, who: who(p), digest: digest, err: &stored}
		}
		return zero, err
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return zero, c.Errorf(c.ErrInternal, "mock: encode: %v", err)
	}
	b.ops[m.OperationID] = opRecord{op: op, who: who(p), digest: digest, resp: raw}
	var out Resp
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// query runs a read with the lock held and returns a deep copy.
func query[Resp any](b *Backend, ctx context.Context, fn func(p c.Principal) (Resp, error)) (Resp, error) {
	var zero Resp
	p, err := b.principal(ctx)
	if err != nil {
		return zero, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	resp, err := fn(p)
	if err != nil {
		return zero, err
	}
	return clone(resp), nil
}

// clone deep-copies a value through JSON so callers never alias state.
func clone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}

// ---- change notification (long-poll) ----

// notifyLocked wakes long-pollers. Caller holds b.mu.
func (b *Backend) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// waitChange blocks until state changes, ctx ends, or d elapses.
func (b *Backend) waitChange(ctx context.Context, ch <-chan struct{}, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
	case <-ctx.Done():
	case <-t.C:
	}
}

// ---- events ----

// emit appends a durable event and wakes waiters. Caller holds b.mu.
func (b *Backend) emit(kind c.EventKind, ref c.ObjectRef, sessionID string, projectID *string, payload any) c.Event {
	return b.emitFrom("hub", kind, ref, sessionID, projectID, payload)
}

func (b *Backend) emitFrom(origin string, kind c.EventKind, ref c.ObjectRef, sessionID string, projectID *string, payload any) c.Event {
	now := b.now()
	b.eventHead++
	ev := c.Event{
		EventID: c.NewID(c.PrefixEvent), Cursor: b.eventHead, Origin: origin, Kind: kind, Object: ref,
		ProjectID: cloneStrPtr(projectID), SessionID: sessionID, PayloadVersion: 1, OccurredAt: now, IngestedAt: now,
	}
	if payload != nil {
		raw, _ := json.Marshal(payload)
		ev.Payload = raw
	}
	b.events = append(b.events, ev)
	if over := len(b.events) - b.opts.EventRetention; over > 0 {
		b.eventFloor = b.events[over-1].Cursor
		b.events = append([]c.Event(nil), b.events[over:]...)
	}
	b.notifyLocked()
	return ev
}

// ---- pagination ----

const (
	defaultLimit = 50
	maxLimit     = 500
)

func paginate[T any](items []T, req c.PageRequest, now time.Time) (c.Page[T], error) {
	start := 0
	if req.Cursor != "" {
		n, err := strconv.Atoi(req.Cursor)
		if err != nil || n < 0 {
			return c.Page[T]{}, c.Errorf(c.ErrInvalid, "invalid cursor")
		}
		start = n
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	p := c.Page[T]{Items: []T{}, SnapshotAt: now}
	if start >= len(items) {
		return p, nil
	}
	end := min(start+limit, len(items))
	p.Items = append(p.Items, items[start:end]...)
	if end < len(items) {
		p.NextCursor = strconv.Itoa(end)
	}
	return p, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- small helpers ----

func strPtr(s string) *string { return &s }

func cloneStrPtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := *p
	return &s
}

func timePtr(t time.Time) *time.Time { return &t }

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func ref(kind c.ObjectKind, id string) c.ObjectRef { return c.ObjectRef{Kind: kind, ID: id} }

func notFound(kind c.ObjectKind, id string) *c.Error {
	return c.Errorf(c.ErrNotFound, "%s %s not found", kind, id).WithRef(kind, id)
}

func mockFreshness(now time.Time) c.Freshness {
	return c.Freshness{ObservedAt: now, Source: "mock"}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
