//go:build !windows

package resilience

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/diagnostics"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
	"github.com/jhh3/pyre/tests/resilience/refhub"
)

func waitFor(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func ctxT(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newHub starts a reference hub in a temp dir with a redacting log ring.
func newHub(t testing.TB, mod func(*refhub.Options)) (*refhub.RefHub, *diagnostics.Ring) {
	t.Helper()
	ring := diagnostics.NewRing(5000, 4<<20, nil)
	opts := refhub.Options{Dir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(ring, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	if mod != nil {
		mod(&opts)
	}
	h, err := refhub.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, ring
}

// countingHandler executes commands, counting invocations per command ID.
// Block, when set for a command kind, parks that command until released.
type countingHandler struct {
	mu      sync.Mutex
	calls   map[string]int
	block   map[contracts.CommandKind]chan struct{}
	started chan string
	// ignoreCtx makes blocked commands ignore cancellation (hard-crash
	// emulation needs work still "executing" when the database goes away).
	ignoreCtx bool
}

func newCountingHandler() *countingHandler {
	return &countingHandler{calls: map[string]int{}, block: map[contracts.CommandKind]chan struct{}{}, started: make(chan string, 1024)}
}

func (h *countingHandler) HandleCommand(ctx context.Context, cmd contracts.NodeCommand) (contracts.CommandResult, error) {
	h.mu.Lock()
	h.calls[cmd.CommandID]++
	ch := h.block[cmd.Kind]
	h.mu.Unlock()
	select {
	case h.started <- cmd.CommandID:
	default:
	}
	if ch != nil {
		if h.ignoreCtx {
			<-ch // a runtime call that does not notice cancellation
		} else {
			select {
			case <-ch:
			case <-ctx.Done():
				return contracts.CommandResult{}, ctx.Err()
			}
		}
	}
	res, _ := json.Marshal(map[string]string{"executed": cmd.CommandID})
	return contracts.CommandResult{Outcome: contracts.OutcomeSucceeded, Result: res}, nil
}

func (h *countingHandler) count(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[id]
}

// testNode is a node daemon stand-in: a NodeLink over an on-disk SQLite
// database that can be crashed and restarted like a process.
type testNode struct {
	t       testing.TB
	dbPath  string
	id      string
	token   string
	url     func() string
	handler transport.CommandHandler
	mod     func(*transport.NodeLinkConfig)
	logs    *diagnostics.Ring

	mu     sync.Mutex
	db     *sql.DB
	link   *transport.NodeLink
	cancel context.CancelFunc
	done   chan struct{}
	states []transport.LinkState
}

func newNode(t testing.TB, id, token string, url func() string, h transport.CommandHandler, mod func(*transport.NodeLinkConfig)) *testNode {
	n := &testNode{t: t, dbPath: filepath.Join(t.TempDir(), "node.db"), id: id, token: token, url: url, handler: h, mod: mod,
		logs: diagnostics.NewRing(5000, 4<<20, nil)}
	t.Cleanup(n.stop)
	return n
}

func (n *testNode) start() {
	n.t.Helper()
	db, err := sqlitex.Open(n.dbPath)
	if err != nil {
		n.t.Fatal(err)
	}
	cfg := transport.NodeLinkConfig{
		HubURL: n.url(), NodeID: n.id, Token: n.token, DB: db, Handler: n.handler,
		Logger: slog.New(slog.NewTextHandler(n.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Hello: func(context.Context) (contracts.NodeHello, error) {
			return contracts.NodeHello{Hostname: "resilience-node", Platform: "linux", Arch: "amd64", PyredVersion: "test"}, nil
		},
		Backoff:     transport.BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond},
		StableAfter: time.Hour,
		OnStateChange: func(s transport.LinkStatus) {
			n.mu.Lock()
			n.states = append(n.states, s.State)
			n.mu.Unlock()
		},
	}
	if n.mod != nil {
		n.mod(&cfg)
	}
	link, err := transport.NewNodeLink(cfg)
	if err != nil {
		db.Close()
		n.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := link.Run(ctx); err != nil {
			n.t.Errorf("node link run: %v", err)
		}
	}()
	n.mu.Lock()
	n.db, n.link, n.cancel, n.done = db, link, cancel, done
	n.mu.Unlock()
}

// stop cancels the link and closes the database (a process exit).
func (n *testNode) stop() {
	n.mu.Lock()
	cancel, done, db := n.cancel, n.done, n.db
	n.cancel, n.done, n.db = nil, nil, nil
	n.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		n.t.Errorf("node link did not stop")
	}
	_ = db.Close()
}

// crash emulates a hard process kill: the database is closed underneath the
// running link without waiting for in-flight work, so the command journal
// keeps whatever state it had ("executing"). The abandoned goroutines exit
// once their blocked work returns; wait() blocks until then.
func (n *testNode) crash() (wait func()) {
	n.mu.Lock()
	cancel, done, db := n.cancel, n.done, n.db
	n.cancel, n.done, n.db = nil, nil, nil
	n.mu.Unlock()
	_ = db.Close()
	cancel()
	return func() {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			n.t.Errorf("crashed node link did not exit")
		}
	}
}

func (n *testNode) Link() *transport.NodeLink {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.link
}

func (n *testNode) enqueue(ctx context.Context, i int) error {
	_, err := n.Link().Enqueue(ctx, contracts.ReportEvent, map[string]any{"kind": "test.tick", "i": i})
	return err
}

func (n *testNode) outbox(ctx context.Context) int {
	c, err := n.Link().OutboxLen(ctx)
	if err != nil {
		return -1
	}
	return c
}

// assertContiguous checks the hub committed exactly origin seqs 1..want.
func assertContiguous(t testing.TB, reports []contracts.NodeReport, want int) {
	t.Helper()
	if len(reports) != want {
		t.Fatalf("hub committed %d reports, want %d", len(reports), want)
	}
	for i, r := range reports {
		if r.OriginSeq != uint64(i+1) {
			t.Fatalf("report %d has origin_seq %d (gap or reorder)", i, r.OriginSeq)
		}
	}
}

// eventIndexes returns the "i" payload field of committed test.tick reports.
func eventIndexes(t testing.TB, reports []contracts.NodeReport) []int {
	t.Helper()
	var out []int
	for _, r := range reports {
		if r.Kind != contracts.ReportEvent {
			continue
		}
		var p struct {
			I int `json:"i"`
		}
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatalf("payload: %v", err)
		}
		out = append(out, p.I)
	}
	return out
}

func assertEachOnce(t testing.TB, got []int, n int) {
	t.Helper()
	seen := make(map[int]int, n)
	for _, i := range got {
		seen[i]++
	}
	for i := 0; i < n; i++ {
		if seen[i] != 1 {
			t.Fatalf("report payload %d committed %d times", i, seen[i])
		}
	}
	if len(got) != n {
		t.Fatalf("committed %d test reports, want %d", len(got), n)
	}
}

func evidence(t testing.TB, format string, args ...any) {
	t.Helper()
	t.Logf("EVIDENCE %s", fmt.Sprintf(format, args...))
}
