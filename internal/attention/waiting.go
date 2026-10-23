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
// and raises an item per question here (one active per session); the
// deriver withdraws it when the session's next turn starts, it files an
// ask, or it is archived or its run ends, and the owner engaging with the
// session handles it.

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

// RaiseWaiting raises or refreshes sessionID's "waiting on you" item. An
// active item (the session's next turn has not withdrawn it yet) is
// updated: count, updated_at, title, summary; a read one reopens.
// Otherwise a new item is created: a new question after the last one was
// handled, dismissed or withdrawn is a new conversation for the owner
// (owner report 2026-10-19: one item per session, reopened 56 times since
// Oct 15, showed the session's whole history as one thread). A repeat of
// the question already raised is RepeatWaiting's, not this. Summary is a
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
		cur, err := cursorQ(ctx, tx, DefaultConsumer)
		if err != nil {
			return err
		}
		if changed, err = s.applyChangeTx(ctx, tx, ch, cur, s.now()); err != nil {
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

// RepeatWaiting records that sessionID ended another turn still waiting on
// the question its "waiting on you" item attentionID already holds (the
// hub compares the asking sentence and remembers the item it raised for
// it; owner report 2026-10-19: a coordinator's idle turns ending "still
// waiting on your yes" reopened its item 27 times). Nothing is counted,
// reopened or pushed: an active item stays as it is, and one the owner
// handled (dismissed, or he engaged with the session) stays handled until
// the session asks something new (a new item). Only an item withdrawn
// because the session moved on (its next turn started) comes back, open
// but without a push or a count: the session still waits on that same
// question, so it keeps its conversation. ok is false when the session's
// latest item is not that one (the caller raises one): there is none, a
// newer one exists, or attentionID is "" (a question recorded before items
// were linked) and the latest is withdrawn, since an item reopened in
// place before 2026-10-19 stood for earlier questions too and is not
// brought back with all their history.
func (s *Store) RepeatWaiting(ctx context.Context, sessionID, attentionID string) (contracts.AttentionItem, bool, error) {
	var out contracts.AttentionItem
	found, changed := false, false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		latest, err := queryItems(ctx, tx, `WHERE group_key=? ORDER BY created_at DESC, attention_id DESC LIMIT 1`, WaitingGroup(sessionID))
		if err != nil || len(latest) == 0 {
			return err
		}
		if it := latest[0].item; attentionID != "" && it.AttentionID != attentionID || attentionID == "" && it.State == contracts.AttnWithdrawn {
			return nil // not the item raised for this question
		}
		found = true
		r := latest[0]
		out = r.item
		if r.item.State != contracts.AttnWithdrawn {
			return nil
		}
		cur, err := cursorQ(ctx, tx, DefaultConsumer)
		if err != nil {
			return err
		}
		now := s.now()
		r.item.UpdatedAt = now
		r.cursor = cur
		if err := s.setStateTx(ctx, tx, &r, contracts.AttnOpen, nil, now, false); err != nil { // no push: not new
			return err
		}
		if err := s.saveTx(ctx, tx, r); err != nil {
			return err
		}
		changed, out = true, r.item
		return s.emitTx(ctx, tx, r.item, now)
	})
	if err != nil {
		return contracts.AttentionItem{}, false, err
	}
	if changed {
		s.signal()
	}
	return out, found, nil
}
