// Package svctest provides test doubles for the WP11 domain services: a
// transactional fake contracts.EventSink and a principal-carrying context.
// It is imported only by tests.
package svctest

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Sink is a fake EventSink that writes events into a test table inside the
// caller's transaction, so only committed events are visible (exactly like
// the hub's events table).
type Sink struct {
	db *sql.DB
	mu sync.Mutex
}

// NewSink creates the fake events table in db.
func NewSink(t testing.TB, db *sql.DB) *Sink {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS test_events (cursor INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return &Sink{db: db}
}

// AppendTx implements contracts.EventSink.
func (s *Sink) AppendTx(ctx context.Context, tx *sql.Tx, ev contracts.Event) (contracts.Event, error) {
	ev.IngestedAt = time.Now().UTC()
	b, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO test_events(body) VALUES(?)`, string(b))
	if err != nil {
		return ev, err
	}
	ev.Cursor, _ = res.LastInsertId()
	return ev, nil
}

// Events returns committed events, optionally filtered by kind.
func (s *Sink) Events(t testing.TB, kinds ...contracts.EventKind) []contracts.Event {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT cursor, body FROM test_events ORDER BY cursor`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[contracts.EventKind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var out []contracts.Event
	for rows.Next() {
		var c int64
		var body string
		if err := rows.Scan(&c, &body); err != nil {
			t.Fatal(err)
		}
		var ev contracts.Event
		if err := json.Unmarshal([]byte(body), &ev); err != nil {
			t.Fatal(err)
		}
		ev.Cursor = c
		if len(want) == 0 || want[ev.Kind] {
			out = append(out, ev)
		}
	}
	return out
}

// OpenDB opens a fresh file-backed SQLite database in a temp dir.
func OpenDB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Owner returns a ctx authenticated as the synthetic owner.
func Owner() context.Context {
	return contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"})
}

// Session returns a ctx authenticated as a synthetic agent session.
func Session(id string) context.Context {
	return contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: id})
}

// Op returns a fresh operation ID.
func Op() contracts.Mutation {
	return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
}

// FixedClock is a settable test clock.
type FixedClock struct {
	mu sync.Mutex
	T  time.Time
}

// Now implements contracts.Clock.
func (c *FixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.T
}

// Advance moves the clock forward.
func (c *FixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.T = c.T.Add(d)
	c.mu.Unlock()
}
