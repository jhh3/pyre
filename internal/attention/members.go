package attention

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Grouped items (owner report 2026-10-12: 53 separate "Session run failed"
// items for Codex launches that all waited on one reset). A raise with a
// member joins that session to the active item of its group; the item's
// count, title and summary are its member sessions. A session leaves every
// group when it is archived or a later run of it goes live; the item is
// withdrawn when its last member leaves.

const capacityPrefix = "capacity:"

// capacityGroup is the group of owner-started launches that failed only for
// lack of a provider account with capacity.
func capacityGroup(p contracts.Provider) string {
	if p == "" {
		return capacityPrefix + "unknown"
	}
	return capacityPrefix + string(p)
}

// maxGroupSummary bounds the session list of a grouped item (runes).
const maxGroupSummary = 240

// renderGroup returns a grouped item's title and summary from its member
// labels (newest first).
func renderGroup(group string, labels []string) (title, summary string) {
	n := len(labels)
	if strings.HasPrefix(group, capacityPrefix) {
		prov := providerLabel(contracts.Provider(strings.TrimPrefix(group, capacityPrefix)))
		if prov == "Unknown-provider" {
			prov = "Provider"
		}
		noun := "launches"
		if n == 1 {
			noun = "launch"
		}
		title = fmt.Sprintf("%s out of capacity: %d %s waiting for the reset", prov, n, noun)
	} else {
		title = fmt.Sprintf("%d sessions", n)
	}
	var b strings.Builder
	b.WriteString("Waiting: ")
	for i, l := range labels {
		part := quoteTitle(l)
		if i > 0 {
			part = ", " + part
		}
		more := fmt.Sprintf(" and %d more", n-i)
		if i > 0 && b.Len()+len(part)+len(more) > maxGroupSummary {
			b.WriteString(more)
			break
		}
		b.WriteString(part)
	}
	b.WriteString(". Resume or retry after the reset; archive the ones you no longer need.")
	return title, short(b.String(), maxGroupSummary+80)
}

func memberLabelsTx(ctx context.Context, tx *sql.Tx, attentionID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id, label FROM attention_members WHERE attention_id=? ORDER BY added_at DESC, session_id`, attentionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		out = append(out, firstNonEmpty(label, id))
	}
	return out, rows.Err()
}

// raiseMemberTx applies a raise with a member: the session joins the active
// item of the group (created when there is none). A new member is new
// activity (a read item reopens); a session already in the group only
// refreshes its label.
func (s *Store) raiseMemberTx(ctx context.Context, tx *sql.Tx, ch change, cursor int64, now time.Time) (bool, error) {
	active, err := activeByGroupTx(ctx, tx, ch.groupKey)
	if err != nil {
		return false, err
	}
	var r itemRow
	isNew := len(active) == 0
	if isNew {
		r = s.newRow(ch, cursor, now)
		r.item.State = contracts.AttnOpen
		r.seq = 1
		if err := s.queueNotifyTx(ctx, tx, &r, now); err != nil {
			return false, err
		}
	} else {
		r = active[0]
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO attention_members(attention_id,session_id,label,added_at) VALUES(?,?,?,?)
		ON CONFLICT(attention_id,session_id) DO UPDATE SET label=excluded.label`,
		r.item.AttentionID, ch.member, ch.memberLabel, sqlitex.FormatTime(now))
	if err != nil {
		return false, err
	}
	var joined bool
	if n, _ := res.RowsAffected(); n > 0 {
		var added string
		if err := tx.QueryRowContext(ctx, `SELECT added_at FROM attention_members WHERE attention_id=? AND session_id=?`, r.item.AttentionID, ch.member).Scan(&added); err != nil {
			return false, err
		}
		joined = added == sqlitex.FormatTime(now)
	}
	labels, err := memberLabelsTx(ctx, tx, r.item.AttentionID)
	if err != nil {
		return false, err
	}
	r.item.Title, r.item.Summary = renderGroup(ch.groupKey, labels)
	r.item.Count = len(labels)
	r.cursor = cursor
	if isNew {
		if err := s.insertTx(ctx, tx, r); err != nil {
			return false, err
		}
		return true, s.emitTx(ctx, tx, r.item, now)
	}
	if joined {
		r.item.UpdatedAt = now
		if ch.object.ID != "" {
			r.item.Object = ch.object
			r.item.DeepLink = contracts.DeepLink(ch.object)
		}
		if r.item.State == contracts.AttnRead {
			if err := s.setStateTx(ctx, tx, &r, contracts.AttnOpen, nil, now, true); err != nil {
				return false, err
			}
		}
	}
	if err := s.saveTx(ctx, tx, r); err != nil {
		return false, err
	}
	return true, s.emitTx(ctx, tx, r.item, now)
}

// leaveTx removes a session from every active grouped item. An item whose
// last member left is withdrawn; the others shrink without moving in the
// inbox (it is not new activity).
func (s *Store) leaveTx(ctx context.Context, tx *sql.Tx, ch change, cursor int64, now time.Time) (bool, error) {
	if ch.sessionID == "" {
		return false, nil
	}
	ids, err := memberItemsTx(ctx, tx, ch.sessionID)
	if err != nil || len(ids) == 0 {
		return false, err
	}
	changed := false
	for _, id := range ids {
		q, args := `DELETE FROM attention_members WHERE attention_id=? AND session_id=?`, []any{id, ch.sessionID}
		if !ch.before.IsZero() {
			// A sweep decided on older state: a session that joined again
			// since stays.
			q, args = q+` AND added_at<=?`, append(args, sqlitex.FormatTime(ch.before))
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		changed = true
		r, err := s.getTx(ctx, tx, id)
		if err != nil {
			return false, err
		}
		labels, err := memberLabelsTx(ctx, tx, id)
		if err != nil {
			return false, err
		}
		r.cursor = cursor
		if len(labels) == 0 {
			if err := s.setStateTx(ctx, tx, &r, contracts.AttnWithdrawn, nil, now, false); err != nil {
				return false, err
			}
			r.item.UpdatedAt = now
			r.item.Count = 0
			r.item.Summary = "No launches are waiting any more."
		} else {
			r.item.Title, r.item.Summary = renderGroup(r.item.GroupKey, labels)
			r.item.Count = len(labels)
		}
		if err := s.saveTx(ctx, tx, r); err != nil {
			return false, err
		}
		if err := s.emitTx(ctx, tx, r.item, now); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// memberItemsTx lists the active grouped items a session belongs to.
func memberItemsTx(ctx context.Context, q querier, sessionID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT m.attention_id FROM attention_members m JOIN attention_items i ON i.attention_id=m.attention_id
		WHERE m.session_id=? AND i.state IN (?,?,?) ORDER BY m.attention_id`, append([]any{sessionID}, activeStates...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- hub sweep (RunMaintenance) ----

// SessionItems summarizes one session's open session-scoped problems for the
// hub's sweep: open failure / account_exhausted items and memberships of
// grouped items.
type SessionItems struct {
	SessionID string
	Failure   bool
	Account   bool
	// AccountUpdated is the oldest last update of its account_exhausted
	// items.
	AccountUpdated time.Time
	Member         bool
}

// ResolvableKinds are the item kinds the sweep (and session lifecycle
// events) resolve automatically.
var ResolvableKinds = []contracts.AttentionKind{contracts.AttnFailure, contracts.AttnAccountPool}

// ResolvableSessions returns, ordered by session ID and after the given
// one, up to limit sessions that have active failure / account_exhausted
// items or belong to an active grouped item. No authorization check;
// hub-internal.
func (s *Store) ResolvableSessions(ctx context.Context, after string, limit int) ([]SessionItems, error) {
	if limit <= 0 {
		limit = 100
	}
	f, a := string(contracts.AttnFailure), string(contracts.AttnAccountPool)
	args := []any{f, a, a, after, f, a}
	args = append(args, activeStates...)
	args = append(args, after)
	args = append(args, activeStates...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, MAX(failure), MAX(account), MIN(account_at), MAX(member) FROM (
			SELECT session_id, kind=? AS failure, kind=? AS account, CASE WHEN kind=? THEN updated_at END AS account_at, 0 AS member
				FROM attention_items WHERE session_id>? AND kind IN (?,?) AND state IN (?,?,?)
			UNION ALL
			SELECT m.session_id, 0, 0, NULL, 1 FROM attention_members m JOIN attention_items i ON i.attention_id=m.attention_id
				WHERE m.session_id>? AND i.state IN (?,?,?)
		) GROUP BY session_id ORDER BY session_id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionItems
	for rows.Next() {
		var si SessionItems
		var failure, account, member int
		var at sql.NullString
		if err := rows.Scan(&si.SessionID, &failure, &account, &at, &member); err != nil {
			return nil, err
		}
		si.Failure, si.Account, si.Member = failure != 0, account != 0, member != 0
		if at.Valid && at.String != "" {
			si.AccountUpdated, _ = sqlitex.ParseTime(at.String)
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// ResolveSession withdraws a session's active items of the given kinds
// last updated at or before before (zero: any) and, with leave, removes it
// from grouped items it joined at or before before. Each call is one short
// transaction (decision I27). It reports whether anything changed. No
// authorization check; hub-internal.
func (s *Store) ResolveSession(ctx context.Context, sessionID string, kinds []contracts.AttentionKind, leave bool, before time.Time, summary string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	changed := false
	now := s.now()
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		cur, err := cursorQ(ctx, tx, DefaultConsumer)
		if err != nil {
			return err
		}
		if len(kinds) > 0 {
			c, err := s.applyChangeTx(ctx, tx, change{op: opWithdraw, sessionID: sessionID, kinds: kinds, before: before, summary: summary}, cur, now)
			if err != nil {
				return err
			}
			changed = changed || c
		}
		if leave {
			c, err := s.applyChangeTx(ctx, tx, change{op: opLeave, sessionID: sessionID, before: before}, cur, now)
			if err != nil {
				return err
			}
			changed = changed || c
		}
		return nil
	})
	if err == nil && changed {
		s.signal()
	}
	return changed, err
}

// PruneMembers deletes up to limit member rows of items that are no longer
// active and reports how many it deleted.
func (s *Store) PruneMembers(ctx context.Context, limit int) (int64, error) {
	args := append([]any{}, activeStates...)
	args = append(args, limit)
	res, err := s.db.ExecContext(ctx, `DELETE FROM attention_members WHERE rowid IN (SELECT m.rowid FROM attention_members m
		JOIN attention_items i ON i.attention_id=m.attention_id WHERE i.state NOT IN (?,?,?) LIMIT ?)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
