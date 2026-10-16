package hub

import (
	"context"
	"sync"

	"github.com/jhh3/pyre/internal/contracts"
)

// nodeMemo caches per-node lookups for one list request. Without it a
// session list re-read and re-parsed the node's status and fleet record once
// per session: ~380 sessions on a node with a large status blob took 3-6s and
// read hundreds of MB per call (server, 2026-10-08).
type nodeMemo struct {
	mu     sync.Mutex
	status map[string]*contracts.NodeStatusReport // nil value: no usable status
	names  map[string]string
	conn   map[string]nodeConn
	once   map[string]bool // FirstInRequest keys already seen
	batch  *listBatch      // prefetched runs/approvals/leases (listbatch.go)
}

// nodeConn is a memoized nodeConnected answer.
type nodeConn struct {
	connected bool
	fresh     *contracts.Freshness
}

// FirstInRequest reports whether this is the first call with key during the
// multi-session read (a session list) ctx belongs to. Outside such a read it
// is always true. Domains use it to do per-request work once instead of once
// per listed session: the attention catch-up before reading session flags
// (W-dbhygiene: done per session it made sessions.list take 2-12s on a busy
// hub).
func FirstInRequest(ctx context.Context, key string) bool {
	m := nodeMemoFrom(ctx)
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.once[key] {
		return false
	}
	m.once[key] = true
	return true
}

type nodeMemoKey struct{}

func withNodeMemo(ctx context.Context) context.Context {
	if _, ok := ctx.Value(nodeMemoKey{}).(*nodeMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, nodeMemoKey{}, &nodeMemo{status: map[string]*contracts.NodeStatusReport{}, names: map[string]string{}, conn: map[string]nodeConn{}, once: map[string]bool{}})
}

func nodeMemoFrom(ctx context.Context) *nodeMemo {
	m, _ := ctx.Value(nodeMemoKey{}).(*nodeMemo)
	return m
}
