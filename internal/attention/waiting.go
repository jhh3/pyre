package attention

import (
	"context"
	"database/sql"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// "Waiting on you" (owner request 2026-10-14): a top-level session ended
// its turn with a question or decision for the owner in its own chat and
// filed no ask. The hub detects it at turn end (internal/hub/waiting.go)
// and raises one item per session here; the deriver withdraws it when the
// session's next turn starts, it files an ask, or it is archived or its run
// ends, and the owner engaging with the session handles it.

// WaitingGroup is the group key of a session's "waiting on you" item.
func WaitingGroup(sessionID string) string { return "waiting:" + sessionID }

const (
	// waitingQuestionMax bounds the question in the item title (runes).
	waitingQuestionMax = 100
	// waitingTitlePrefix starts every waiting item's title.
	waitingTitlePrefix = "Waiting on you: "
)

// WaitingTitle is the item title for question: its first line, at most
// waitingQuestionMax runes.
func WaitingTitle(question string) string {
	q, _, _ := strings.Cut(strings.TrimSpace(question), "\n")
	q = short(q, waitingQuestionMax)
	if q == "" {
		q = "a question in its chat"
	}
	return waitingTitlePrefix + q
}

// RaiseWaiting raises or refreshes sessionID's "waiting on you" item: one
// item per session, ever. An active item is updated (count, updated_at,
// title, summary; a read one reopens); otherwise the session's latest
// waiting item is reopened rather than a new one created. Summary is a
// sanitized excerpt (at most maxExcerpt runes). No authorization check;
// hub-internal (the hub's turn-end worker is the only caller).
func (s *Store) RaiseWaiting(ctx context.Context, sessionID string, projectID *string, question, summary string) (contracts.AttentionItem, error) {
	var out contracts.AttentionItem
	if sessionID == "" {
		return out, contracts.Errorf(contracts.ErrInvalid, "session_id is required")
	}
	group := WaitingGroup(sessionID)
	ch := change{op: opRaise, groupKey: group, kind: contracts.AttnWaiting, severity: "normal",
		object: contracts.ObjectRef{Kind: contracts.KindSession, ID: sessionID}, sessionID: sessionID, projectID: projectID,
		title: WaitingTitle(question), summary: MessageExcerpt(summary)}
	changed := false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		now := s.now()
		cur, err := cursorQ(ctx, tx, DefaultConsumer)
		if err != nil {
			return err
		}
		active, err := activeByGroupTx(ctx, tx, group)
		if err != nil {
			return err
		}
		if len(active) == 0 {
			latest, err := queryItems(ctx, tx, `WHERE group_key=? ORDER BY updated_at DESC, attention_id DESC LIMIT 1`, group)
			if err != nil {
				return err
			}
			if len(latest) == 1 {
				// Reopen the session's one item (resolved earlier).
				r := latest[0]
				r.item.Count++
				r.item.UpdatedAt = now
				r.cursor = cur
				r.item.Title, r.item.Summary, r.item.Severity = ch.title, ch.summary, ch.severity
				if projectID != nil {
					r.item.ProjectID = projectID
				}
				if err := s.setStateTx(ctx, tx, &r, contracts.AttnOpen, nil, now, true); err != nil {
					return err
				}
				if err := s.saveTx(ctx, tx, r); err != nil {
					return err
				}
				changed, out = true, r.item
				return s.emitTx(ctx, tx, r.item, now)
			}
		}
		if changed, err = s.applyChangeTx(ctx, tx, ch, cur, now); err != nil {
			return err
		}
		after, err := activeByGroupTx(ctx, tx, group)
		if err == nil && len(after) > 0 {
			out = after[0].item
		}
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

// WithdrawWaiting withdraws sessionID's active "waiting on you" item (the
// question went stale before it was raised). It reports whether one was.
func (s *Store) WithdrawWaiting(ctx context.Context, sessionID, summary string) (bool, error) {
	changed := false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		cur, err := cursorQ(ctx, tx, DefaultConsumer)
		if err != nil {
			return err
		}
		changed, err = s.applyChangeTx(ctx, tx, change{op: opWithdraw, groupKey: WaitingGroup(sessionID), summary: summary}, cur, s.now())
		return err
	})
	if changed {
		s.signal()
	}
	return changed, err
}
