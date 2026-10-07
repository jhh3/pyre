// Package persistence holds small helpers shared by the hub's storage
// packages (internal/hub, internal/events, internal/messaging/store): the
// idempotent mutation wrapper, page-cursor encoding, JSON column helpers, and
// a post-commit change broadcaster for long polls.
package persistence

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Mutate runs fn exactly once per operation ID inside a write transaction
// using sqlitex.Idempotent.
//
//   - fn's writes run inside a savepoint: if fn returns an error its partial
//     writes are discarded.
//   - Non-retryable contract errors (conflict, invalid, expired, ...) are
//     stored with the operation and the transaction commits, so a retry of the
//     same operation returns the same error.
//   - Retryable errors (capacity_exhausted, unavailable) and internal errors
//     roll the transaction back and are not stored; a retry may succeed.
//
// The decoded result is written to out. replayed reports whether a stored
// result was returned without running fn.
func Mutate(ctx context.Context, db *sql.DB, opID, op string, p contracts.Principal, req any, out any, fn func(tx *sql.Tx) (any, error)) (replayed bool, err error) {
	var stored error
	err = sqlitex.Tx(ctx, db, func(tx *sql.Tx) error {
		rep, e := sqlitex.Idempotent(ctx, tx, opID, op, p, req, out, func() (any, error) {
			if _, err := tx.ExecContext(ctx, `SAVEPOINT pyre_op`); err != nil {
				return nil, err
			}
			res, ferr := fn(tx)
			if ferr != nil {
				if _, err := tx.ExecContext(ctx, `ROLLBACK TO pyre_op`); err != nil {
					return nil, err
				}
				if _, err := tx.ExecContext(ctx, `RELEASE pyre_op`); err != nil {
					return nil, err
				}
				return nil, ferr
			}
			if _, err := tx.ExecContext(ctx, `RELEASE pyre_op`); err != nil {
				return nil, err
			}
			return res, nil
		})
		replayed = rep
		if e == nil {
			return nil
		}
		var ce *contracts.Error
		if errors.As(e, &ce) && ce.Code != contracts.ErrInternal && !ce.Retryable && ce.Code != contracts.ErrUnavailable {
			// Stored (or replayed) contract error: commit so it persists.
			stored = ce // bare *contracts.Error, not the sqlitex.StoredRejection wrapper
			return nil
		}
		return e
	})
	if err != nil {
		return replayed, err
	}
	return replayed, stored
}

// ---- page cursors ----

// PageCursor is the decoded form of an opaque list cursor: a sort key and a
// tiebreak ID.
type PageCursor struct {
	K string `json:"k"`
	I string `json:"i"`
	N int64  `json:"n,omitempty"`
}

// EncodeCursor returns an opaque cursor string.
func EncodeCursor(c PageCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses an opaque cursor ("" -> zero, ok).
func DecodeCursor(s string) (PageCursor, error) {
	var c PageCursor
	if s == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, contracts.Errorf(contracts.ErrInvalid, "malformed cursor")
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, contracts.Errorf(contracts.ErrInvalid, "malformed cursor")
	}
	return c, nil
}

// Limits for list calls.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// ClampLimit applies the default and maximum page size.
func ClampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// ---- JSON / time helpers ----

// JSON marshals v for a TEXT column; nil-able values marshal to "null".
func JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("persistence: marshal %T: %v", v, err))
	}
	return string(b)
}

// FromJSON unmarshals a TEXT column ("" leaves v unchanged).
func FromJSON(s string, v any) error {
	if s == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), v)
}

// Time formats t for storage ("" for zero).
func Time(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return sqlitex.FormatTime(t)
}

// TimePtr formats *t for storage (NULL for nil).
func TimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return sqlitex.FormatTime(*t)
}

// ParseTime parses a stored time, returning zero on "" or error.
func ParseTime(s string) time.Time {
	t, _ := sqlitex.ParseTime(s)
	return t
}

// ParseTimePtr parses a nullable stored time.
func ParseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := ParseTime(s.String)
	return &t
}

// NullStr returns NULL for "" and the string otherwise.
func NullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ProjectArg converts an optional project ID to a column value.
func ProjectArg(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

// ProjectPtr converts a nullable project column to *string.
func ProjectPtr(s sql.NullString) *string {
	if !s.Valid || s.String == "" {
		return nil
	}
	v := s.String
	return &v
}

// PrincipalKey is the canonical "kind:id" form of a principal.
func PrincipalKey(p contracts.Principal) string { return string(p.Kind) + ":" + p.ID }

// ---- change broadcaster ----

// Broadcaster wakes long-poll waiters after a commit. Waiters take the
// current channel with C() before checking storage, then block on it; a
// Broadcast closes it and installs a fresh one, so no wakeup is lost.
type Broadcaster struct {
	mu sync.Mutex
	ch chan struct{}
}

// C returns the channel closed by the next Broadcast.
func (b *Broadcaster) C() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch == nil {
		b.ch = make(chan struct{})
	}
	return b.ch
}

// Broadcast wakes all current waiters.
func (b *Broadcaster) Broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch != nil {
		close(b.ch)
	}
	b.ch = make(chan struct{})
}

// WaitSeconds clamps a long-poll wait to [0, 60] seconds.
func WaitSeconds(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	if n > 60 {
		n = 60
	}
	return time.Duration(n) * time.Second
}

// LongPoll repeatedly calls check until it reports done, the wait elapses,
// or ctx ends. It wakes on b's broadcasts and also every poll interval so
// commits made by other packages (which do not broadcast) are observed.
func LongPoll(ctx context.Context, b *Broadcaster, wait, poll time.Duration, check func() (bool, error)) error {
	deadline := time.Now().Add(wait)
	for {
		c := b.C()
		done, err := check()
		if err != nil && ctx.Err() != nil {
			// The caller gave up while check ran: same outcome as a
			// cancellation during the wait (no result, no error).
			return nil
		}
		if err != nil || done {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		if poll <= 0 || poll > remaining {
			poll = remaining
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-c:
		case <-t.C:
		}
		t.Stop()
	}
}
