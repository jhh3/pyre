package attention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/notifications"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// itemRow is an item plus internal bookkeeping columns.
type itemRow struct {
	item    contracts.AttentionItem
	oneShot bool
	seq     int64
	cursor  int64
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const itemColumns = `attention_id,kind,severity,group_key,object_kind,object_id,session_id,project_id,title,summary,state,count,created_at,updated_at,snoozed_until,deep_link,notification,one_shot,transition_seq,last_cursor`

type scanner interface{ Scan(dest ...any) error }

func scanItem(sc scanner) (itemRow, error) {
	var r itemRow
	var it = &r.item
	var kind, state, objKind string
	var project, snoozed, notif sql.NullString
	var created, updated string
	var oneShot int
	if err := sc.Scan(&it.AttentionID, &kind, &it.Severity, &it.GroupKey, &objKind, &it.Object.ID, &it.SessionID, &project,
		&it.Title, &it.Summary, &state, &it.Count, &created, &updated, &snoozed, &it.DeepLink, &notif, &oneShot, &r.seq, &r.cursor); err != nil {
		return r, err
	}
	it.Kind = contracts.AttentionKind(kind)
	it.State = contracts.AttentionState(state)
	it.Object.Kind = contracts.ObjectKind(objKind)
	if project.Valid {
		p := project.String
		it.ProjectID = &p
	}
	var err error
	if it.CreatedAt, err = sqlitex.ParseTime(created); err != nil {
		return r, err
	}
	if it.UpdatedAt, err = sqlitex.ParseTime(updated); err != nil {
		return r, err
	}
	if snoozed.Valid && snoozed.String != "" {
		t, err := sqlitex.ParseTime(snoozed.String)
		if err != nil {
			return r, err
		}
		it.SnoozedUntil = &t
	}
	if notif.Valid && notif.String != "" {
		var ns contracts.NotificationStatus
		if err := json.Unmarshal([]byte(notif.String), &ns); err != nil {
			return r, fmt.Errorf("attention: decode notification status: %w", err)
		}
		it.Notification = &ns
	}
	r.oneShot = oneShot != 0
	return r, nil
}

func queryItems(ctx context.Context, q querier, where string, args ...any) ([]itemRow, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+itemColumns+" FROM attention_items "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []itemRow
	for rows.Next() {
		r, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) getQ(ctx context.Context, q querier, id string) (itemRow, error) {
	r, err := scanItem(q.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM attention_items WHERE attention_id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, contracts.Errorf(contracts.ErrNotFound, "attention item not found").WithRef(contracts.KindAttention, id)
	}
	return r, err
}

func (s *Store) getTx(ctx context.Context, tx *sql.Tx, id string) (itemRow, error) {
	return s.getQ(ctx, tx, id)
}

// activeStates are the states in which an item is still collecting updates.
var activeStates = []any{string(contracts.AttnOpen), string(contracts.AttnRead), string(contracts.AttnSnoozed)}

func activeByGroupTx(ctx context.Context, tx *sql.Tx, group string) ([]itemRow, error) {
	return queryItems(ctx, tx, `WHERE group_key=? AND state IN (?,?,?) ORDER BY created_at DESC, attention_id DESC`,
		append([]any{group}, activeStates...)...)
}

func anyByGroupTx(ctx context.Context, tx *sql.Tx, group string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE group_key=?`, group).Scan(&n)
	return n > 0, err
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func notifJSON(ns *contracts.NotificationStatus) (any, error) {
	if ns == nil {
		return nil, nil
	}
	b, err := json.Marshal(ns)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func snoozeVal(t *time.Time) any {
	if t == nil {
		return nil
	}
	return sqlitex.FormatTime(*t)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) insertTx(ctx context.Context, tx *sql.Tx, r itemRow) error {
	it := r.item
	nj, err := notifJSON(it.Notification)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO attention_items(`+itemColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.AttentionID, string(it.Kind), it.Severity, it.GroupKey, string(it.Object.Kind), it.Object.ID, it.SessionID, nullStr(it.ProjectID),
		it.Title, it.Summary, string(it.State), it.Count, sqlitex.FormatTime(it.CreatedAt), sqlitex.FormatTime(it.UpdatedAt),
		snoozeVal(it.SnoozedUntil), it.DeepLink, nj, boolInt(r.oneShot), r.seq, r.cursor)
	return err
}

func (s *Store) saveTx(ctx context.Context, tx *sql.Tx, r itemRow) error {
	it := r.item
	nj, err := notifJSON(it.Notification)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE attention_items SET kind=?,severity=?,object_kind=?,object_id=?,session_id=?,project_id=?,title=?,summary=?,
		state=?,count=?,updated_at=?,snoozed_until=?,deep_link=?,notification=?,transition_seq=?,last_cursor=? WHERE attention_id=?`,
		string(it.Kind), it.Severity, string(it.Object.Kind), it.Object.ID, it.SessionID, nullStr(it.ProjectID), it.Title, it.Summary,
		string(it.State), it.Count, sqlitex.FormatTime(it.UpdatedAt), snoozeVal(it.SnoozedUntil), it.DeepLink, nj, r.seq, r.cursor, it.AttentionID)
	return err
}

// emitTx appends attention.changed (payload = the item) inside tx.
// The deriver never derives attention from attention.changed, so these
// events cannot loop.
func (s *Store) emitTx(ctx context.Context, tx *sql.Tx, it contracts.AttentionItem, now time.Time) error {
	b, err := json.Marshal(it)
	if err != nil {
		return err
	}
	_, err = s.sink.AppendTx(ctx, tx, contracts.Event{
		EventID:        contracts.NewID(contracts.PrefixEvent),
		Origin:         "hub",
		Kind:           contracts.EvAttentionChanged,
		Object:         contracts.ObjectRef{Kind: contracts.KindAttention, ID: it.AttentionID},
		ProjectID:      it.ProjectID,
		SessionID:      it.SessionID,
		PayloadVersion: 1,
		Payload:        b,
		OccurredAt:     now,
		IngestedAt:     now,
	})
	return err
}

func enqueueTx(ctx context.Context, tx *sql.Tx, id string, seq int64, action string, now time.Time) error {
	ts := sqlitex.FormatTime(now)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_notify(attention_id,seq,action,attempts,next_attempt_at,created_at,done) VALUES(?,?,?,0,?,?,0)`,
		id, seq, action, ts, ts)
	return err
}

// setStateTx changes r's state and queues notification work:
//   - entering open bumps the transition seq; when notify is set (hub-driven
//     transitions, not an owner marking unread) one notify job is queued and
//     the status becomes pending;
//   - leaving open/read after a provider push was sent queues a dismiss job
//     so the phone withdraws a stale alert.
//
// The caller saves and emits.
func (s *Store) setStateTx(ctx context.Context, tx *sql.Tx, r *itemRow, state contracts.AttentionState, until *time.Time, now time.Time, notify bool) error {
	old := r.item.State
	r.item.State = state
	if state == contracts.AttnSnoozed {
		r.item.SnoozedUntil = until
	} else {
		r.item.SnoozedUntil = nil
	}
	if state == contracts.AttnOpen && old != contracts.AttnOpen {
		r.seq++
		if notify {
			return s.queueNotifyTx(ctx, tx, r, now)
		}
		return nil
	}
	if state != contracts.AttnOpen && (old == contracts.AttnOpen || old == contracts.AttnRead) {
		if ns := r.item.Notification; ns != nil && ns.State == "sent" && ns.Provider == ProviderFCM {
			return enqueueTx(ctx, tx, r.item.AttentionID, r.seq, "dismiss", now)
		}
	}
	return nil
}

func (s *Store) queueNotifyTx(ctx context.Context, tx *sql.Tx, r *itemRow, now time.Time) error {
	r.item.Notification = &contracts.NotificationStatus{Provider: "none", State: "pending"}
	return enqueueTx(ctx, tx, r.item.AttentionID, r.seq, "notify", now)
}

// ---- applying derived changes ----

type opKind int

const (
	opRaise    opKind = iota + 1 // create or collapse into the active item for groupKey
	opWithdraw                   // underlying request resolved elsewhere
	opHandle                     // the owner acted (e.g. replied in the thread)
	opLeave                      // sessionID leaves every grouped item (members.go)
)

func (o opKind) String() string {
	switch o {
	case opRaise:
		return "raise"
	case opWithdraw:
		return "withdraw"
	case opHandle:
		return "handle"
	case opLeave:
		return "leave"
	}
	return "?"
}

// change is one derived inbox mutation.
type change struct {
	op        opKind
	groupKey  string
	kind      contracts.AttentionKind
	severity  string
	object    contracts.ObjectRef
	sessionID string
	projectID *string
	title     string
	summary   string
	// oneShot items (approvals, a specific delivery) are never reopened or
	// recreated once handled/withdrawn; a withdraw with no item leaves a
	// withdrawn tombstone so a late duplicate request cannot resurrect it.
	oneShot bool
	// kinds, with an empty groupKey, makes a withdraw/handle apply to every
	// active item of these kinds for sessionID (owner engagement).
	kinds []contracts.AttentionKind
	// before, when set, limits a withdraw/handle to items last updated at
	// or before it, so a late-processed event cannot clear a newer item.
	before time.Time
	// member, with a raise, joins that session to the group's item instead
	// of counting a repeat: the item lists its member sessions (labels are
	// their titles) and is withdrawn when the last one leaves (members.go).
	member      string
	memberLabel string
	// readOnly limits a withdraw/handle to items the owner already read.
	readOnly bool
	// objectID limits a withdraw/handle to items whose object is this one
	// (a sender's item now standing for a newer message survives).
	objectID string
}

func activeBySessionKindsTx(ctx context.Context, tx *sql.Tx, sessionID string, kinds []contracts.AttentionKind) ([]itemRow, error) {
	args := []any{sessionID}
	for _, k := range kinds {
		args = append(args, string(k))
	}
	args = append(args, activeStates...)
	return queryItems(ctx, tx, `WHERE session_id=? AND kind IN (`+placeholders(len(kinds))+`) AND state IN (?,?,?) ORDER BY created_at DESC, attention_id DESC`, args...)
}

// applyChangeTx applies one change. It reports whether anything changed.
func (s *Store) applyChangeTx(ctx context.Context, tx *sql.Tx, ch change, cursor int64, now time.Time) (bool, error) {
	switch ch.op {
	case opLeave:
		return s.leaveTx(ctx, tx, ch, cursor, now)
	case opRaise:
		if ch.member != "" {
			return s.raiseMemberTx(ctx, tx, ch, cursor, now)
		}
		active, err := activeByGroupTx(ctx, tx, ch.groupKey)
		if err != nil {
			return false, err
		}
		if len(active) > 0 {
			r := active[0]
			if r.oneShot {
				return false, nil // duplicate request for the same approval/delivery
			}
			r.item.Count++
			r.item.UpdatedAt = now
			r.cursor = cursor
			if ch.title != "" {
				r.item.Title = ch.title
			}
			r.item.Summary = ch.summary
			if ch.object.ID != "" {
				r.item.Object = ch.object
				r.item.DeepLink = contracts.DeepLink(ch.object)
			}
			if ch.sessionID != "" {
				r.item.SessionID = ch.sessionID
			}
			if ch.projectID != nil {
				r.item.ProjectID = ch.projectID
			}
			escalated := notifications.SeverityRank(ch.severity) > notifications.SeverityRank(r.item.Severity)
			if escalated {
				r.item.Severity = ch.severity
			}
			switch {
			case r.item.State == contracts.AttnRead:
				// New activity on a read incident reopens it.
				if err := s.setStateTx(ctx, tx, &r, contracts.AttnOpen, nil, now, true); err != nil {
					return false, err
				}
			case escalated && r.item.State == contracts.AttnOpen:
				// Escalation is a new transition (e.g. a pool that was
				// limited is now fully exhausted).
				r.seq++
				if err := s.queueNotifyTx(ctx, tx, &r, now); err != nil {
					return false, err
				}
			}
			if err := s.saveTx(ctx, tx, r); err != nil {
				return false, err
			}
			return true, s.emitTx(ctx, tx, r.item, now)
		}
		if ch.oneShot {
			exists, err := anyByGroupTx(ctx, tx, ch.groupKey)
			if err != nil || exists {
				return false, err
			}
		}
		r := s.newRow(ch, cursor, now)
		r.item.State = contracts.AttnOpen
		r.seq = 1
		if err := s.queueNotifyTx(ctx, tx, &r, now); err != nil {
			return false, err
		}
		if err := s.insertTx(ctx, tx, r); err != nil {
			return false, err
		}
		return true, s.emitTx(ctx, tx, r.item, now)

	case opWithdraw, opHandle:
		target := contracts.AttnWithdrawn
		if ch.op == opHandle {
			target = contracts.AttnHandled
		}
		var active []itemRow
		var err error
		if ch.groupKey == "" && len(ch.kinds) > 0 {
			if ch.sessionID == "" {
				return false, nil
			}
			active, err = activeBySessionKindsTx(ctx, tx, ch.sessionID, ch.kinds)
		} else {
			active, err = activeByGroupTx(ctx, tx, ch.groupKey)
		}
		if err != nil {
			return false, err
		}
		if !ch.before.IsZero() || ch.readOnly || ch.objectID != "" {
			kept := active[:0]
			for _, r := range active {
				if (ch.before.IsZero() || !r.item.UpdatedAt.After(ch.before)) && (!ch.readOnly || r.item.State == contracts.AttnRead) &&
					(ch.objectID == "" || r.item.Object.ID == ch.objectID) {
					kept = append(kept, r)
				}
			}
			active = kept
		}
		if len(active) == 0 {
			if ch.op != opWithdraw || !ch.oneShot || ch.groupKey == "" {
				return false, nil
			}
			exists, err := anyByGroupTx(ctx, tx, ch.groupKey)
			if err != nil || exists {
				return false, err
			}
			// Tombstone: the request resolved before we saw it requested.
			r := s.newRow(ch, cursor, now)
			r.item.State = contracts.AttnWithdrawn
			if err := s.insertTx(ctx, tx, r); err != nil {
				return false, err
			}
			return true, s.emitTx(ctx, tx, r.item, now)
		}
		for _, r := range active {
			if err := s.setStateTx(ctx, tx, &r, target, nil, now, false); err != nil {
				return false, err
			}
			r.item.UpdatedAt = now
			r.cursor = cursor
			if ch.summary != "" {
				r.item.Summary = ch.summary
			}
			if err := s.saveTx(ctx, tx, r); err != nil {
				return false, err
			}
			if err := s.emitTx(ctx, tx, r.item, now); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return false, fmt.Errorf("attention: unknown change op %d", ch.op)
}

func (s *Store) newRow(ch change, cursor int64, now time.Time) itemRow {
	sev := ch.severity
	if sev == "" {
		sev = "normal"
	}
	return itemRow{
		item: contracts.AttentionItem{
			AttentionID: contracts.NewID(contracts.PrefixAttention),
			Kind:        ch.kind,
			Severity:    sev,
			GroupKey:    ch.groupKey,
			Object:      ch.object,
			SessionID:   ch.sessionID,
			ProjectID:   ch.projectID,
			Title:       ch.title,
			Summary:     ch.summary,
			Count:       1,
			CreatedAt:   now,
			UpdatedAt:   now,
			DeepLink:    contracts.DeepLink(ch.object),
		},
		oneShot: ch.oneShot,
		cursor:  cursor,
	}
}

// ---- cursor and dedup ----

func cursorQ(ctx context.Context, q querier, consumer string) (int64, error) {
	var c int64
	err := q.QueryRowContext(ctx, `SELECT cursor FROM attention_cursor WHERE consumer=?`, consumer).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return c, err
}

func setCursorTx(ctx context.Context, tx *sql.Tx, consumer string, c int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO attention_cursor(consumer,cursor,updated_at) VALUES(?,?,?)
		ON CONFLICT(consumer) DO UPDATE SET cursor=MAX(cursor, excluded.cursor), updated_at=excluded.updated_at`,
		consumer, c, sqlitex.FormatTime(now))
	return err
}

// applyEvent applies the changes derived from ev exactly once per consumer:
// events at or before the persisted cursor, or with an already-seen event
// ID, are skipped. The cursor advances in the same transaction.
func (s *Store) applyEvent(ctx context.Context, consumer string, ev contracts.Event, changes []change) (bool, error) {
	now := s.now()
	changed := false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		cur, err := cursorQ(ctx, tx, consumer)
		if err != nil {
			return err
		}
		if ev.Cursor > 0 && ev.Cursor <= cur {
			return nil
		}
		track := ev.EventID != "" && len(changes) > 0
		if track {
			var one int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM attention_seen WHERE event_id=?`, ev.EventID).Scan(&one)
			if err == nil {
				return setCursorTx(ctx, tx, consumer, ev.Cursor, now)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		for _, ch := range changes {
			c, err := s.applyChangeTx(ctx, tx, ch, ev.Cursor, now)
			if err != nil {
				return fmt.Errorf("attention: %s %s: %w", ch.op, ch.groupKey, err)
			}
			changed = changed || c
		}
		if track {
			if _, err := tx.ExecContext(ctx, `INSERT INTO attention_seen(event_id,cursor,seen_at) VALUES(?,?,?)`,
				ev.EventID, ev.Cursor, sqlitex.FormatTime(now)); err != nil {
				return err
			}
		}
		return setCursorTx(ctx, tx, consumer, ev.Cursor, now)
	})
	if err != nil {
		return false, err
	}
	if changed {
		s.signal()
	}
	if s.applied.Add(1)%256 == 0 {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM attention_seen WHERE event_id IN
			(SELECT event_id FROM attention_seen ORDER BY cursor DESC LIMIT -1 OFFSET ?)`, s.seenKeep); err != nil {
			s.log.Warn("attention: prune seen events", "err", err)
		}
	}
	return changed, nil
}

// advanceCursor moves the consumer cursor forward without applying events
// (used when the stream skipped filtered kinds).
func (s *Store) advanceCursor(ctx context.Context, consumer string, c int64) error {
	return sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		return setCursorTx(ctx, tx, consumer, c, s.now())
	})
}
