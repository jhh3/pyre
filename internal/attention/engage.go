package attention

import (
	"context"
	"database/sql"

	"github.com/jhh3/pyre/internal/attention/sessionflag"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Session flags (v1.3): a session's attention summary, the owner engaging
// with a session, and a session flagging itself.

// EngageKinds are the kinds the owner engaging with a session (typing into
// it, messaging it) answers: its messages to the owner, its own flag and
// its "waiting on you". Approvals and questions resolve only through their
// own answer.
var EngageKinds = []contracts.AttentionKind{contracts.AttnMessage, contracts.AttnFlag, contracts.AttnWaiting}

// ReadKinds are the kinds viewing a session's message answers (the owner
// has read what it said, including the question it is waiting on).
var ReadKinds = []contracts.AttentionKind{contracts.AttnMessage, contracts.AttnWaiting}

func flagGroup(sessionID string) string { return "flag:session:" + sessionID }

// SessionAttention returns the open item count (SessionView.Attention) and
// the session's flag (nil when nothing flags). No authorization check;
// hub-internal.
func (s *Store) SessionAttention(ctx context.Context, sessionID string) (int, *contracts.SessionFlag, error) {
	return s.SessionAttentionRun(ctx, sessionID, nil)
}

// SessionAttentionRun is SessionAttention for a session whose current run
// is run (sessionflag.ComputeRun: a launch failure is not "needs you").
func (s *Store) SessionAttentionRun(ctx context.Context, sessionID string, run *contracts.Run) (int, *contracts.SessionFlag, error) {
	rows, err := queryItems(ctx, s.db, `WHERE session_id=? AND state IN (?,?) ORDER BY updated_at DESC, attention_id DESC`,
		sessionID, string(contracts.AttnOpen), string(contracts.AttnRead))
	if err != nil {
		return 0, nil, err
	}
	failed, err := s.failedMemberItems(ctx, sessionID)
	if err != nil {
		return 0, nil, err
	}
	items := append(failed, itemsOf(rows)...)
	n := 0
	for _, it := range items {
		if it.State == contracts.AttnOpen {
			n++
		}
	}
	return n, sessionflag.ComputeRun(items, run), nil
}

func itemsOf(rows []itemRow) []contracts.AttentionItem {
	out := make([]contracts.AttentionItem, len(rows))
	for i, r := range rows {
		out[i] = r.item
	}
	return out
}

// failedMemberItems returns the open or read failure group item a session
// is in (members.go), titled for that session alone ("'x' failed to start:
// working folder missing"), so it still flags the session as its own
// failure item did. Capacity groups do not flag their sessions.
func (s *Store) failedMemberItems(ctx context.Context, sessionID string) ([]contracts.AttentionItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.attention_id, m.label FROM attention_members m JOIN attention_items i ON i.attention_id=m.attention_id
		WHERE m.session_id=? AND i.group_key LIKE ? AND i.state IN (?,?)`, sessionID, failedPrefix+"%", string(contracts.AttnOpen), string(contracts.AttnRead))
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	var ids []string
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			rows.Close()
			return nil, err
		}
		labels[id] = firstNonEmpty(label, sessionID)
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []contracts.AttentionItem
	for _, id := range ids {
		r, err := s.getQ(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		r.item.Title, _ = renderGroup(r.item.GroupKey, []string{labels[id]})
		r.item.SessionID = sessionID
		out = append(out, r.item)
	}
	return out, nil
}

// OwnerEngaged marks the session's active items of the given kinds handled
// (never deleted): the owner typed into the session, messaged it, or read
// its message. It reports how many items changed. No authorization check;
// the hub calls it only for owner principals.
func (s *Store) OwnerEngaged(ctx context.Context, sessionID, summary string, kinds ...contracts.AttentionKind) (int, error) {
	if sessionID == "" || len(kinds) == 0 {
		return 0, nil
	}
	// Cheap pre-check: typing calls this every few seconds.
	var pending int
	args := []any{sessionID}
	for _, k := range kinds {
		args = append(args, string(k))
	}
	args = append(args, activeStates...)
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE session_id=? AND kind IN (`+placeholders(len(kinds))+`) AND state IN (?,?,?)`, args...).Scan(&pending); err != nil || pending == 0 {
		return 0, err
	}
	n := 0
	now := s.now()
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		cur, err := cursorQ(ctx, tx, DefaultConsumer)
		if err != nil {
			return err
		}
		active, err := activeBySessionKindsTx(ctx, tx, sessionID, kinds)
		if err != nil {
			return err
		}
		n = len(active)
		if n == 0 {
			return nil
		}
		_, err = s.applyChangeTx(ctx, tx, change{op: opHandle, sessionID: sessionID, kinds: kinds, summary: summary}, cur, now)
		return err
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.signal()
	}
	return n, nil
}

// FlagAttention raises or clears a session's own flag. A session principal
// may flag only itself; the owner (or system) may flag or clear any
// session. The text is the agent's own short reason, sanitized and
// truncated; it is never read from a transcript.
func (s *Store) FlagAttention(ctx context.Context, req contracts.FlagAttentionRequest) (contracts.AttentionItem, error) {
	var out contracts.AttentionItem
	p, ok := contracts.PrincipalFrom(ctx)
	if !ok || p.Kind == "" {
		return out, contracts.Errorf(contracts.ErrUnauthorized, "no authenticated principal")
	}
	sid := req.SessionID
	switch p.Kind {
	case contracts.PrincipalSession:
		if sid == "" {
			sid = p.ID
		}
		if sid != p.ID {
			return out, contracts.Errorf(contracts.ErrForbidden, "a session may only flag itself")
		}
	case contracts.PrincipalOwner, contracts.PrincipalSystem:
		if sid == "" {
			return out, contracts.Errorf(contracts.ErrInvalid, "session_id is required")
		}
	default:
		return out, contracts.Errorf(contracts.ErrForbidden, "%s principals may not flag sessions", p.Kind)
	}
	text := short(req.Text, maxTitle)
	if text == "" {
		text = "Asks for you"
	}
	group := flagGroup(sid)
	changed := false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := sqlitex.Idempotent(ctx, tx, req.OperationID, contracts.OpFlagAttention, p, req, &out, func() (any, error) {
			now := s.now()
			cur, err := cursorQ(ctx, tx, DefaultConsumer)
			if err != nil {
				return nil, err
			}
			ch := change{op: opRaise, groupKey: group, kind: contracts.AttnFlag, severity: "urgent",
				object: contracts.ObjectRef{Kind: contracts.KindSession, ID: sid}, sessionID: sid, title: text}
			if req.Clear {
				summary := "Cleared by the session."
				if p.Kind != contracts.PrincipalSession {
					summary = "Cleared."
				}
				ch = change{op: opHandle, groupKey: group, summary: summary}
			}
			before, err := activeByGroupTx(ctx, tx, group)
			if err != nil {
				return nil, err
			}
			c, err := s.applyChangeTx(ctx, tx, ch, cur, now)
			if err != nil {
				return nil, err
			}
			changed = c
			if req.Clear {
				if len(before) == 0 {
					return contracts.AttentionItem{}, nil
				}
				r, err := s.getTx(ctx, tx, before[0].item.AttentionID)
				return r.item, err
			}
			after, err := activeByGroupTx(ctx, tx, group)
			if err != nil || len(after) == 0 {
				return contracts.AttentionItem{}, err
			}
			return after[0].item, nil
		})
		return err
	})
	if err != nil {
		return contracts.AttentionItem{}, err
	}
	if changed {
		s.signal()
	}
	return out, nil
}
