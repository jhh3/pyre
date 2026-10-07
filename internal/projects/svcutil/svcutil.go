// Package svcutil holds the small amount of plumbing shared by the WP11 hub
// domain services (internal/projects, internal/artifacts, internal/review):
// idempotent mutation transactions, event emission through
// contracts.EventSink, keyset pagination cursors, and nullable project IDs.
//
// It is deliberately tiny and has no state; each domain owns its own tables.
package svcutil

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// RequirePrincipal returns the authenticated principal carried in ctx or
// ErrUnauthorized. Request bodies never claim identity.
func RequirePrincipal(ctx context.Context) (contracts.Principal, error) {
	p, ok := contracts.PrincipalFrom(ctx)
	if !ok || p.Kind == "" || p.ID == "" {
		return contracts.Principal{}, contracts.Errorf(contracts.ErrUnauthorized, "no authenticated principal")
	}
	return p, nil
}

// commitErr marks a contract error whose writes must be kept (for example
// a document conflict that preserved the caller's proposal, or an upload
// that is now permanently failed). Idempotent sees the wrapped
// *contracts.Error via errors.As, keeps fn's writes (sqlitex.WriteKeeper)
// and stores the error for replay.
type commitErr struct{ err *contracts.Error }

func (c *commitErr) Error() string    { return c.err.Error() }
func (c *commitErr) Unwrap() error    { return c.err }
func (c *commitErr) KeepWrites() bool { return true }

// CommitWith returns err so that Mutate commits the transaction (keeping
// any writes fn made, plus the stored error for idempotent replay) and then
// returns err to the caller.
func CommitWith(err *contracts.Error) error { return &commitErr{err: err} }

// Mutate runs fn at most once per operation ID inside one write transaction
// using sqlitex.Idempotent with the ctx principal.
//
//   - fn returns (result, nil): commit; result is JSON-decoded into out.
//   - fn returns CommitWith(e): commit (writes kept, e stored unless
//     retryable); e returned.
//   - fn returns a non-retryable *contracts.Error: fn's writes are discarded
//     but the rejection is stored and committed (sqlitex.StoredRejection),
//     so a retry of the operation replays it.
//   - retryable contract errors (capacity_exhausted, unavailable, or
//     Retryable=true), internal and non-contract errors: rollback; nothing
//     is stored, so a retry re-executes.
//   - replay of an identical request: the stored result/error is returned.
func Mutate(ctx context.Context, db *sql.DB, m contracts.Mutation, op string, req any, out any, fn func(tx *sql.Tx, p contracts.Principal) (any, error)) error {
	p, err := RequirePrincipal(ctx)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	_, ierr := sqlitex.Idempotent(ctx, tx, m.OperationID, op, p, req, out, func() (any, error) {
		return fn(tx, p)
	})
	var sr *sqlitex.StoredRejection
	var ce *commitErr
	switch {
	case ierr == nil:
		return tx.Commit()
	case errors.As(ierr, &sr):
		if err := tx.Commit(); err != nil {
			return err
		}
		return sr.Err
	case errors.As(ierr, &ce):
		// CommitWith of a retryable error: writes kept, error not stored.
		if err := tx.Commit(); err != nil {
			return err
		}
		return ce.err
	default:
		_ = tx.Rollback()
		return ierr
	}
}

// Emit appends one durable hub event in tx.
func Emit(ctx context.Context, tx *sql.Tx, sink contracts.EventSink, now time.Time, kind contracts.EventKind, obj contracts.ObjectRef, projectID *string, sessionID string, payload any) error {
	if sink == nil {
		return nil
	}
	var raw contracts.RawJSON
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = b
	}
	_, err := sink.AppendTx(ctx, tx, contracts.Event{
		EventID:        contracts.NewID(contracts.PrefixEvent),
		Origin:         "hub",
		Kind:           kind,
		Object:         obj,
		ProjectID:      CopyProject(projectID),
		SessionID:      sessionID,
		PayloadVersion: 1,
		Payload:        raw,
		OccurredAt:     now,
	})
	return err
}

// ---- pagination ----

// DefaultLimit and MaxLimit bound list pages.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Limit clamps a requested page size.
func Limit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// EncodeCursor makes an opaque cursor from a keyset key.
func EncodeCursor(key string) string {
	if key == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

// DecodeCursor reverses EncodeCursor ("" -> "").
func DecodeCursor(c string) (string, error) {
	if c == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(b) == 0 {
		return "", contracts.Errorf(contracts.ErrInvalid, "malformed cursor")
	}
	return string(b), nil
}

// ---- nullable project IDs ----

// ProjectFilter interprets a *string project filter used by list requests:
// nil = no filter, "" = standalone (no project) only, otherwise that project.
// It returns a SQL fragment over column col and its args.
func ProjectFilter(col string, p *string) (string, []any) {
	if p == nil {
		return "", nil
	}
	if *p == "" {
		return col + " IS NULL", nil
	}
	return col + " = ?", []any{*p}
}

// NullProject converts *string to a nullable SQL value ("" counts as NULL).
func NullProject(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

// ScanProject converts a scanned nullable project into *string.
func ScanProject(ns sql.NullString) *string {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	s := ns.String
	return &s
}

// CopyProject returns a defensive copy, normalizing "" to nil.
func CopyProject(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	s := *p
	return &s
}

// ---- JSON columns ----

// JSON marshals v for a TEXT column.
func JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// FromJSON unmarshals a TEXT column ("" leaves v untouched).
func FromJSON(s string, v any) error {
	if s == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), v)
}

// ---- text helpers ----

// LikeEscape escapes s for use in `LIKE ? ESCAPE '\'`.
func LikeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// TruncateUTF8 cuts s to at most n bytes without splitting a rune.
func TruncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Time formats/parses stored times.
func Time(t time.Time) string { return sqlitex.FormatTime(t) }

// ParseTime parses a stored time, returning zero on error.
func ParseTime(s string) time.Time {
	t, _ := sqlitex.ParseTime(s)
	return t
}

// IsUniqueViolation reports whether err is a SQLite UNIQUE constraint error.
func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
