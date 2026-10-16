package attention

import (
	"context"
	"database/sql"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Message items raised before their summary carried the message excerpt
// (2026-10-12) have an empty summary. The hub fills the open ones once
// from the stored messages (hub maintenance); these are its two halves.

// MessageItemsWithoutSummary lists up to limit active "message" items with
// an empty summary, by attention ID after the given one. No authorization
// check; hub-internal.
func (s *Store) MessageItemsWithoutSummary(ctx context.Context, after string, limit int) ([]contracts.AttentionItem, error) {
	args := []any{string(contracts.AttnMessage), after}
	args = append(args, activeStates...)
	args = append(args, limit)
	rows, err := queryItems(ctx, s.db, `WHERE kind=? AND summary='' AND attention_id>? AND state IN (?,?,?) ORDER BY attention_id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	out := make([]contracts.AttentionItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.item)
	}
	return out, nil
}

// FillMessageSummary sets an active message item's summary to the excerpt
// of body when it is still empty. Not new activity: the state, order and
// notifications are unchanged. It reports whether it changed the item.
func (s *Store) FillMessageSummary(ctx context.Context, attentionID, body string) (bool, error) {
	summary := MessageExcerpt(body)
	if summary == "" {
		return false, nil
	}
	changed := false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		r, err := s.getTx(ctx, tx, attentionID)
		if err != nil || r.item.Kind != contracts.AttnMessage || r.item.Summary != "" {
			return err
		}
		switch r.item.State {
		case contracts.AttnOpen, contracts.AttnRead, contracts.AttnSnoozed:
		default:
			return nil
		}
		r.item.Summary = summary
		if err := s.saveTx(ctx, tx, r); err != nil {
			return err
		}
		changed = true
		return s.emitTx(ctx, tx, r.item, s.now())
	})
	return changed, err
}
