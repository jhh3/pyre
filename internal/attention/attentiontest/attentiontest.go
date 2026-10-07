// Package attentiontest provides in-memory fakes for testing the attention
// inbox and notification dispatch without a hub: an event log implementing
// both contracts.EventAPI and contracts.EventSink, and a settable clock.
package attentiontest

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Clock is a settable contracts.Clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock at t.
func NewClock(t time.Time) *Clock { return &Clock{t: t.UTC()} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Set sets the clock.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t.UTC()
	c.mu.Unlock()
}

// Log is an in-memory event log. AppendTx assigns cursors; ListEvents
// returns events after a cursor, honoring Kinds unless IgnoreKindFilter is
// set (to simulate a hub that returns everything). Events below Oldest are
// treated as expired and produce reset_required.
type Log struct {
	mu     sync.Mutex
	events []contracts.Event
	next   int64
	// Oldest, when > 0, is the oldest retained cursor.
	Oldest int64
	// SnapshotCursor is reported in reset_required details (default head).
	SnapshotCursor int64
	// IgnoreKindFilter returns all kinds regardless of the request filter.
	IgnoreKindFilter bool
	// Requests records ListEvents requests.
	Requests []contracts.ListEventsRequest
}

// NewLog returns an empty log.
func NewLog() *Log { return &Log{} }

// AppendTx implements contracts.EventSink (the transaction is ignored).
func (l *Log) AppendTx(_ context.Context, _ *sql.Tx, ev contracts.Event) (contracts.Event, error) {
	return l.Append(ev), nil
}

// Append assigns a cursor and stores ev.
func (l *Log) Append(ev contracts.Event) contracts.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	ev.Cursor = l.next
	if ev.EventID == "" {
		ev.EventID = contracts.NewID(contracts.PrefixEvent)
	}
	if ev.PayloadVersion == 0 {
		ev.PayloadVersion = 1
	}
	l.events = append(l.events, ev)
	return ev
}

// AppendPayload appends an event of kind with payload v (JSON-encoded).
func (l *Log) AppendPayload(kind contracts.EventKind, obj contracts.ObjectRef, sessionID string, project *string, v any) contracts.Event {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return l.Append(contracts.Event{Origin: "hub", Kind: kind, Object: obj, SessionID: sessionID, ProjectID: project, Payload: b})
}

// Resend re-appends an existing event with the same EventID (a duplicate
// delivery that receives a new cursor).
func (l *Log) Resend(ev contracts.Event) contracts.Event {
	return l.Append(ev)
}

// Head returns the latest cursor.
func (l *Log) Head() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next
}

// Events returns a copy of all events, optionally filtered by kind.
func (l *Log) Events(kinds ...contracts.EventKind) []contracts.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []contracts.Event
	for _, e := range l.events {
		if len(kinds) == 0 || slices.Contains(kinds, e.Kind) {
			out = append(out, e)
		}
	}
	return out
}

// ListEvents implements contracts.EventAPI without blocking.
func (l *Log) ListEvents(_ context.Context, req contracts.ListEventsRequest) (contracts.ListEventsResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Requests = append(l.Requests, req)
	if l.Oldest > 0 && req.AfterCursor < l.Oldest-1 {
		snap := l.SnapshotCursor
		if snap == 0 {
			snap = l.next
		}
		return contracts.ListEventsResponse{}, contracts.Errorf(contracts.ErrResetRequired, "cursor older than retention").
			WithDetail("oldest_cursor", l.Oldest).WithDetail("snapshot_cursor", snap)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	resp := contracts.ListEventsResponse{NextCursor: req.AfterCursor, HeadCursor: l.next}
	for _, e := range l.events {
		if e.Cursor <= req.AfterCursor {
			continue
		}
		if len(resp.Events) >= limit {
			break
		}
		resp.NextCursor = e.Cursor
		if !l.IgnoreKindFilter && len(req.Kinds) > 0 && !slices.Contains(req.Kinds, e.Kind) {
			continue
		}
		resp.Events = append(resp.Events, e)
	}
	return resp, nil
}
