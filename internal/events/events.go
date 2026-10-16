// Package events is the hub's durable event log (migration namespace
// "events"). Every domain appends events inside its own write transaction
// through AppendTx (contracts.EventSink); readers page through them by
// cursor with ListEvents, optionally long-polling.
//
// Cursors are SQLite AUTOINCREMENT row IDs: strictly increasing, never
// reused, and assigned by serialized writers, so every event at or below the
// observed head is already committed. A reader that pages up to the head it
// observed therefore never skips a committed event.
//
// Retention prunes the oldest events by count and/or age. A reader whose
// cursor is older than the pruned range gets ErrResetRequired with details
// {"oldest_cursor", "snapshot_cursor"} instead of silently skipping events.
package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Migrations for namespace "events".
var Migrations = []sqlitex.Migration{{Namespace: "events", Version: 1, Name: "events", SQL: `
CREATE TABLE events (
	cursor INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id TEXT NOT NULL UNIQUE,
	origin TEXT NOT NULL,
	origin_seq INTEGER NOT NULL DEFAULT 0,
	kind TEXT NOT NULL,
	object_kind TEXT NOT NULL,
	object_id TEXT NOT NULL,
	project_id TEXT,
	session_id TEXT,
	payload_version INTEGER NOT NULL,
	payload TEXT,
	occurred_at TEXT NOT NULL,
	ingested_at TEXT NOT NULL
);
CREATE INDEX events_session ON events(session_id, cursor);
CREATE INDEX events_kind ON events(kind, cursor);
CREATE INDEX events_ingested ON events(ingested_at);
CREATE TABLE events_meta (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	pruned_through INTEGER NOT NULL DEFAULT 0
);
INSERT INTO events_meta(id, pruned_through) VALUES (1, 0);
`}}

// Retention bounds the event log. Zero values disable that bound.
type Retention struct {
	MaxEvents int           // keep at most this many newest events
	MaxAge    time.Duration // drop events ingested longer ago than this
}

// DefaultRetention keeps a week or 200k events, whichever is smaller.
var DefaultRetention = Retention{MaxEvents: 200_000, MaxAge: 7 * 24 * time.Hour}

// Options configures a Log.
type Options struct {
	Clock     contracts.Clock
	Retention Retention
	// PollInterval bounds how long a long poll may miss a commit made by a
	// package that does not call Notify. Default 5s (waiters wake on Notify).
	PollInterval time.Duration
}

// Log is the durable event log. It implements contracts.EventSink and
// contracts.EventAPI.
type Log struct {
	db    *sql.DB
	clock contracts.Clock
	ret   Retention
	poll  time.Duration
	bc    persistence.Broadcaster
	hook  atomic.Pointer[AppendHook]
}

// AppendHook observes every appended event inside the appending
// transaction (after the insert succeeded). It must be cheap and must not
// fail the caller's write: the hub uses it to derive audit entries
// (W-audit). Its own errors are its business.
type AppendHook func(ctx context.Context, tx *sql.Tx, ev contracts.Event)

// OnAppend installs (or, with nil, removes) the append hook.
func (l *Log) OnAppend(fn AppendHook) {
	if fn == nil {
		l.hook.Store(nil)
		return
	}
	l.hook.Store(&fn)
}

var (
	_ contracts.EventSink = (*Log)(nil)
	_ contracts.EventAPI  = (*Log)(nil)
)

// Open migrates the events namespace and returns a Log.
func Open(ctx context.Context, db *sql.DB, opt Options) (*Log, error) {
	if err := sqlitex.Migrate(ctx, db, Migrations...); err != nil {
		return nil, err
	}
	if opt.Clock == nil {
		opt.Clock = contracts.SystemClock{}
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = 5 * time.Second
	}
	return &Log{db: db, clock: opt.Clock, ret: opt.Retention, poll: opt.PollInterval}, nil
}

// AppendTx inserts ev inside tx and returns it with its cursor. EventID,
// Origin, PayloadVersion, OccurredAt and IngestedAt are defaulted.
// Call Notify after commit to wake long polls promptly. Deduplication of
// re-sent node reports happens before events are derived from them.
func (l *Log) AppendTx(ctx context.Context, tx *sql.Tx, ev contracts.Event) (contracts.Event, error) {
	if ev.Kind == "" {
		return ev, contracts.Errorf(contracts.ErrInvalid, "event kind is required")
	}
	now := l.clock.Now().UTC()
	if ev.EventID == "" {
		ev.EventID = contracts.NewID(contracts.PrefixEvent)
	}
	if ev.Origin == "" {
		ev.Origin = "hub"
	}
	if ev.PayloadVersion == 0 {
		ev.PayloadVersion = 1
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = now
	}
	ev.IngestedAt = now
	var payload any
	if len(ev.Payload) > 0 {
		payload = string(ev.Payload)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO events(event_id,origin,origin_seq,kind,object_kind,object_id,project_id,session_id,payload_version,payload,occurred_at,ingested_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.EventID, ev.Origin, int64(ev.OriginSeq), string(ev.Kind), string(ev.Object.Kind), ev.Object.ID,
		persistence.ProjectArg(ev.ProjectID), persistence.NullStr(ev.SessionID), ev.PayloadVersion, payload,
		persistence.Time(ev.OccurredAt), persistence.Time(ev.IngestedAt))
	if err != nil {
		return ev, fmt.Errorf("events: append: %w", err)
	}
	ev.Cursor, err = res.LastInsertId()
	if err != nil {
		return ev, err
	}
	if h := l.hook.Load(); h != nil {
		(*h)(ctx, tx, ev)
	}
	return ev, nil
}

// Notify wakes long-poll readers; call after committing appended events.
func (l *Log) Notify() { l.bc.Broadcast() }

const cols = `cursor,event_id,origin,origin_seq,kind,object_kind,object_id,project_id,session_id,payload_version,payload,occurred_at,ingested_at`

type scanner interface{ Scan(...any) error }

func scanEvent(s scanner) (contracts.Event, error) {
	var ev contracts.Event
	var originSeq int64
	var kind, okind string
	var project, session, payload sql.NullString
	var occ, ing string
	if err := s.Scan(&ev.Cursor, &ev.EventID, &ev.Origin, &originSeq, &kind, &okind, &ev.Object.ID, &project, &session, &ev.PayloadVersion, &payload, &occ, &ing); err != nil {
		return ev, err
	}
	ev.OriginSeq = uint64(originSeq)
	ev.Kind = contracts.EventKind(kind)
	ev.Object.Kind = contracts.ObjectKind(okind)
	ev.ProjectID = persistence.ProjectPtr(project)
	ev.SessionID = session.String
	if payload.Valid {
		ev.Payload = contracts.RawJSON(payload.String)
	}
	ev.OccurredAt = persistence.ParseTime(occ)
	ev.IngestedAt = persistence.ParseTime(ing)
	return ev, nil
}

// Head returns the newest committed cursor (0 if none).
func (l *Log) Head(ctx context.Context) (int64, error) {
	var h sql.NullInt64
	if err := l.db.QueryRowContext(ctx, `SELECT MAX(cursor) FROM events`).Scan(&h); err != nil {
		return 0, err
	}
	if h.Valid {
		return h.Int64, nil
	}
	// All events pruned: the head is the pruned boundary.
	return l.prunedThrough(ctx)
}

func (l *Log) prunedThrough(ctx context.Context) (int64, error) {
	var p int64
	err := l.db.QueryRowContext(ctx, `SELECT pruned_through FROM events_meta WHERE id=1`).Scan(&p)
	return p, err
}

// Limits for ListEvents.
const (
	DefaultListLimit = 100
	MaxListLimit     = 1000
)

// ListEvents returns events after req.AfterCursor matching the filters. With
// WaitSeconds (<= 60) it long-polls until a match commits. NextCursor is the
// cursor to resume from; it advances past scanned non-matching events but
// never past an uncommitted one.
func (l *Log) ListEvents(ctx context.Context, req contracts.ListEventsRequest) (contracts.ListEventsResponse, error) {
	if req.AfterCursor < 0 {
		return contracts.ListEventsResponse{}, contracts.Errorf(contracts.ErrInvalid, "after_cursor must be >= 0")
	}
	limit := persistence.ClampLimit(req.Limit, DefaultListLimit, MaxListLimit)
	var resp contracts.ListEventsResponse
	err := persistence.LongPoll(ctx, &l.bc, persistence.WaitSeconds(req.WaitSeconds), l.poll, func() (bool, error) {
		r, err := l.listOnce(ctx, req, limit)
		if err != nil {
			return false, err
		}
		resp = r
		return len(r.Events) > 0, nil
	})
	if resp.Events == nil {
		resp.Events = []contracts.Event{}
	}
	return resp, err
}

func (l *Log) listOnce(ctx context.Context, req contracts.ListEventsRequest, limit int) (contracts.ListEventsResponse, error) {
	head, err := l.Head(ctx)
	if err != nil {
		return contracts.ListEventsResponse{}, err
	}
	if err := l.checkStale(ctx, req.AfterCursor, head); err != nil {
		return contracts.ListEventsResponse{}, err
	}
	where := []string{"cursor > ?", "cursor <= ?"}
	args := []any{req.AfterCursor, head}
	if len(req.Kinds) > 0 {
		ph := make([]string, len(req.Kinds))
		for i, k := range req.Kinds {
			ph[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "kind IN ("+strings.Join(ph, ",")+")")
	}
	if req.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, req.SessionID)
	}
	if req.ProjectID != nil {
		if *req.ProjectID == "" {
			where = append(where, "project_id IS NULL")
		} else {
			where = append(where, "project_id = ?")
			args = append(args, *req.ProjectID)
		}
	}
	args = append(args, limit)
	rows, err := l.db.QueryContext(ctx, `SELECT `+cols+` FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY cursor LIMIT ?`, args...)
	if err != nil {
		return contracts.ListEventsResponse{}, err
	}
	defer rows.Close()
	resp := contracts.ListEventsResponse{Events: []contracts.Event{}, HeadCursor: head}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return resp, err
		}
		resp.Events = append(resp.Events, ev)
	}
	if err := rows.Err(); err != nil {
		return resp, err
	}
	// Events may have been pruned while we read; re-check before answering.
	if err := l.checkStale(ctx, req.AfterCursor, head); err != nil {
		return contracts.ListEventsResponse{}, err
	}
	if len(resp.Events) < limit {
		resp.NextCursor = head
	} else {
		resp.NextCursor = resp.Events[len(resp.Events)-1].Cursor
	}
	if resp.NextCursor < req.AfterCursor {
		resp.NextCursor = req.AfterCursor
	}
	return resp, nil
}

func (l *Log) checkStale(ctx context.Context, after, head int64) error {
	pruned, err := l.prunedThrough(ctx)
	if err != nil {
		return err
	}
	if after < pruned {
		var oldest sql.NullInt64
		if err := l.db.QueryRowContext(ctx, `SELECT MIN(cursor) FROM events`).Scan(&oldest); err != nil {
			return err
		}
		o := pruned + 1
		if oldest.Valid {
			o = oldest.Int64
		}
		return contracts.Errorf(contracts.ErrResetRequired, "cursor %d is older than retained events (oldest %d); re-snapshot and resume from snapshot_cursor", after, o).
			WithDetail("oldest_cursor", o).WithDetail("snapshot_cursor", head)
	}
	if after > head {
		// A cursor from the future (e.g. after restoring an older backup).
		return contracts.Errorf(contracts.ErrResetRequired, "cursor %d is ahead of the hub head %d (hub restored?); re-snapshot", after, head).
			WithDetail("oldest_cursor", pruned+1).WithDetail("snapshot_cursor", head)
	}
	return nil
}

// Retention queries. Both must stay cheap at the retention limits (they
// run inside a write transaction on every maintenance pass, every 10 s on
// the hub): the A20 soak measured the previous forms (OFFSET over the
// newest MaxEvents rows; MAX(cursor) walked the table backwards checking
// ingested_at) at 13-14 ms each with 54k events, growing linearly with the
// log. Cursors only ever grow and are deleted from the bottom, so the
// MaxEvents boundary is MAX(cursor) - MaxEvents.
const (
	pruneMaxCursorQuery = `SELECT MAX(cursor) FROM events`
	pruneAgeQuery       = `SELECT MAX(cursor) FROM events INDEXED BY events_ingested WHERE ingested_at < ?`
)

// pruneBatchBound is the cursor at which one prune batch ends: the
// PruneBatch-th oldest event at or below the retention boundary.
const pruneBatchBound = `SELECT cursor FROM events WHERE cursor <= ? ORDER BY cursor LIMIT 1 OFFSET ?`

// Prune applies the retention policy and returns how many events were
// deleted. It records the highest deleted cursor so stale readers get
// reset_required. It deletes from the oldest end in short write
// transactions of at most sqlitex.PruneBatch events (W-dbhygiene: one
// statement deleting a backlog of large events held the write lock for
// seconds), so pruned_through only ever covers deleted events.
func (l *Log) Prune(ctx context.Context) (int64, error) {
	if l.ret.MaxEvents <= 0 && l.ret.MaxAge <= 0 {
		return 0, nil
	}
	// The boundary is read outside the write transactions: cursors only
	// grow, so it stays valid.
	var through int64
	if l.ret.MaxEvents > 0 {
		var c sql.NullInt64
		if err := l.db.QueryRowContext(ctx, pruneMaxCursorQuery).Scan(&c); err != nil {
			return 0, err
		}
		if c.Valid && c.Int64-int64(l.ret.MaxEvents) > through {
			through = c.Int64 - int64(l.ret.MaxEvents)
		}
	}
	if l.ret.MaxAge > 0 {
		cutoff := persistence.Time(l.clock.Now().Add(-l.ret.MaxAge))
		var c sql.NullInt64
		if err := l.db.QueryRowContext(ctx, pruneAgeQuery, cutoff).Scan(&c); err != nil {
			return 0, err
		}
		if c.Valid && c.Int64 > through {
			through = c.Int64
		}
	}
	if through == 0 {
		return 0, nil
	}
	return sqlitex.Batched(ctx, l.db, sqlitex.PruneBatch, 0, func(tx *sql.Tx, limit int) (int64, error) {
		upTo := through
		var b sql.NullInt64
		if err := tx.QueryRowContext(ctx, pruneBatchBound, through, limit-1).Scan(&b); err == nil && b.Valid {
			upTo = b.Int64
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE cursor <= ?`, upTo)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `UPDATE events_meta SET pruned_through = MAX(pruned_through, ?) WHERE id=1`, upTo); err != nil {
			return 0, err
		}
		return n, nil
	})
}

// Get returns one event by cursor.
func (l *Log) Get(ctx context.Context, cursor int64) (contracts.Event, error) {
	ev, err := scanEvent(l.db.QueryRowContext(ctx, `SELECT `+cols+` FROM events WHERE cursor=?`, cursor))
	if errors.Is(err, sql.ErrNoRows) {
		return ev, contracts.Errorf(contracts.ErrNotFound, "event %s not found", strconv.FormatInt(cursor, 10))
	}
	return ev, err
}
