// Package diagnostics holds shared measurement and safety helpers:
//
//   - process resource sampling (RSS/CPU of the current process, another
//     PID, or a process tree; portable via /proc on Linux and ps elsewhere)
//     and a best-effort host summary;
//   - Ring, a bounded log/line buffer usable as a slog sink that also counts
//     total log volume (log-growth budgets);
//   - Scanner, which greps bytes, files, SQLite databases (+WAL) and
//     directories for Pyre tokens (pyc_/pyn_/pys_/pyt_/pye_/pyp_), bearer
//     values, OAuth/provider-token shapes, JWTs, private keys and email
//     addresses, plus exact registered values; and Redact built on it;
//   - Builder, which assembles the redacted contracts.Diagnostics payload
//     for the node action "diagnostics".
//
// Tests under tests/security and tests/resilience use these helpers; the
// node daemon (WP06) is expected to register Builder.Handler for
// contracts.ActionDiagnostics.
package diagnostics

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// QueueFunc reports the current length of one queue (outbox, inbox, ...).
type QueueFunc func(ctx context.Context) (int, error)

// Builder assembles contracts.Diagnostics. It is safe for concurrent use.
// Recent errors are kept in a bounded ring and redacted on the way in and
// again on the way out.
type Builder struct {
	NodeID string
	Start  time.Time
	Clock  contracts.Clock
	// MaxRuns is the advertised launch capacity copied into resources.
	MaxRuns int
	// ActiveRuns, if set, reports supervised run count.
	ActiveRuns func() int

	mu     sync.Mutex
	queues map[string]QueueFunc
	limits map[string]int
	errs   *Ring
	scan   *Scanner
	last   *ProcSample
}

// NewBuilder returns a builder for nodeID that keeps the last maxErrors
// redacted error lines (default 50).
func NewBuilder(nodeID string, maxErrors int) *Builder {
	if maxErrors <= 0 {
		maxErrors = 50
	}
	s := NewScanner()
	return &Builder{
		NodeID: nodeID,
		Start:  time.Now(),
		Clock:  contracts.SystemClock{},
		queues: map[string]QueueFunc{},
		limits: map[string]int{},
		errs:   NewRing(maxErrors, 64<<10, s.Redact),
		scan:   s,
	}
}

// AddKnownSecret registers exact values (e.g. the node token) that are
// always redacted from reported errors.
func (b *Builder) AddKnownSecret(values ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.scan.AddKnown("configured", values...)
}

// SetQueue registers a queue length reporter.
func (b *Builder) SetQueue(name string, fn QueueFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queues[name] = fn
}

// SetLimit records a configured bound (e.g. "max_outbox": 100000).
func (b *Builder) SetLimit(name string, v int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limits[name] = v
}

// RecordError keeps a redacted line describing a recent error.
func (b *Builder) RecordError(msg string) {
	b.mu.Lock()
	s := b.scan
	b.mu.Unlock()
	b.errs.Add(s.Redact(msg))
}

// Errors returns the ring for wiring as a slog sink of warnings/errors.
func (b *Builder) Errors() *Ring { return b.errs }

// Build samples resources and queue lengths. A failing queue reporter is
// reported as -1 with its (redacted) error appended to recent_errors.
func (b *Builder) Build(ctx context.Context) (contracts.Diagnostics, error) {
	b.mu.Lock()
	queues := make(map[string]QueueFunc, len(b.queues))
	for k, v := range b.queues {
		queues[k] = v
	}
	limits := make(map[string]int, len(b.limits))
	for k, v := range b.limits {
		limits[k] = v
	}
	scan := b.scan
	b.mu.Unlock()

	d := contracts.Diagnostics{
		NodeID: b.NodeID,
		Uptime: int64(time.Since(b.Start) / time.Second),
		Queues: map[string]int{},
		Limits: limits,
	}
	var extraErrs []string
	names := make([]string, 0, len(queues))
	for k := range queues {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		n, err := queues[k](ctx)
		if err != nil {
			n = -1
			extraErrs = append(extraErrs, "queue "+k+": "+err.Error())
		}
		d.Queues[k] = n
	}
	d.Resources = b.resources()
	errs := b.errs.Lines()
	errs = append(errs, extraErrs...)
	for i := range errs {
		errs[i] = scan.Redact(errs[i])
	}
	if errs == nil {
		errs = []string{}
	}
	d.Errors = errs
	return d, nil
}

func (b *Builder) resources() contracts.NodeResources {
	h := SampleHost()
	r := contracts.NodeResources{
		CPUCount:   h.CPUCount,
		Load1:      h.Load1,
		MemTotalMB: h.MemTotalMB,
		MemAvailMB: h.MemAvailMB,
		MaxRuns:    b.MaxRuns,
		ObservedAt: b.Clock.Now(),
	}
	if b.ActiveRuns != nil {
		r.ActiveRuns = b.ActiveRuns()
	}
	if s, err := SampleSelf(); err == nil {
		r.PyreRSSMB = round1(s.RSSMB())
		b.mu.Lock()
		if b.last != nil {
			r.PyreCPUPct = round1(CPUPercent(*b.last, s))
		}
		b.last = &s
		b.mu.Unlock()
	}
	return r
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// Handler returns a node-action handler for contracts.ActionDiagnostics
// with the signature used by node action registries
// (func(ctx, params) (any, error)). Params are ignored.
func (b *Builder) Handler() func(ctx context.Context, params json.RawMessage) (any, error) {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		return b.Build(ctx)
	}
}
