package hub

import (
	"context"
	"errors"
	"time"

	"github.com/jhh3/pyre/internal/attention"
	"github.com/jhh3/pyre/internal/contracts"
)

// Stale failure items (owner report 2026-10-12: 84 open "Session run
// failed" items, most for sessions started by lead sessions that never
// ran). The attention deriver resolves them from events (archive, a later
// live run, the run ending); this sweep catches what it missed (items from
// before the rule, events past retention) on every maintenance pass. It is
// idempotent and bounded: one short transaction per session it changes.

// SessionRecord returns the stored session (no presentation view, so no
// attention reads). The attention deriver's lookup uses it: GetSession
// reads session flags, which wait for the deriver.
func (h *Hub) SessionRecord(ctx context.Context, sessionID string) (contracts.Session, error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Session{}, err
	}
	return h.getSession(ctx, h.db, sessionID)
}

// attentionSweeper is implemented by internal/attention.Store.
type attentionSweeper interface {
	ResolvableSessions(ctx context.Context, after string, limit int) ([]attention.SessionItems, error)
	ResolveSession(ctx context.Context, sessionID string, kinds []contracts.AttentionKind, leave bool, before time.Time, summary string) (bool, error)
	PruneMembers(ctx context.Context, limit int) (int64, error)
}

const (
	sweepPage  = 100
	sweepPages = 10 // at most 1000 sessions per pass
)

// sweepAttention resolves open failure / account_exhausted items (and
// grouped capacity memberships) whose session was archived, runs again,
// sits under a parent session (its parent hears of failures), or, for
// account items, ended after the item was raised.
func (h *Hub) sweepAttention(ctx context.Context) error {
	sw, ok := h.cfg.Attention.(attentionSweeper)
	if !ok {
		return nil
	}
	var errs []error
	after := ""
	for range sweepPages {
		page, err := sw.ResolvableSessions(ctx, after, sweepPage)
		if err != nil {
			return err
		}
		for _, si := range page {
			after = si.SessionID
			if err := h.sweepSession(ctx, sw, si); err != nil {
				errs = append(errs, err)
			}
		}
		if len(page) < sweepPage {
			break
		}
	}
	if _, err := sw.PruneMembers(ctx, 500); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (h *Hub) sweepSession(ctx context.Context, sw attentionSweeper, si attention.SessionItems) error {
	// Items raised after the state below was read are kept: the deriver may
	// be raising a newer failure meanwhile.
	before := h.now()
	s, err := h.getSession(ctx, h.db, si.SessionID)
	if contracts.IsCode(err, contracts.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var run *contracts.Run
	if s.CurrentRunID != "" {
		if r, err := h.getRun(ctx, h.db, s.CurrentRunID); err == nil {
			run = &r
		}
	}
	resolve := func(kinds []contracts.AttentionKind, leave bool, before time.Time, summary string) error {
		_, err := sw.ResolveSession(ctx, si.SessionID, kinds, leave, before, summary)
		return err
	}
	switch {
	case s.Archived:
		return resolve(attention.ResolvableKinds, true, before, "The session was archived.")
	case run != nil && attention.LiveRun(run.State):
		return resolve(attention.ResolvableKinds, true, before, "A later run of this session is running.")
	}
	var errs []error
	if (si.Failure || si.Member) && s.Parent() != "" {
		errs = append(errs, resolve([]contracts.AttentionKind{contracts.AttnFailure}, true, before, "It runs under another session; that session hears of its failures."))
	}
	if si.Account && (run == nil || run.State.Terminal()) {
		// Only limits hit before the session ended: a "waiting for the
		// reset" item raised after it stopped stays.
		ended := before
		if run != nil && run.EndedAt != nil {
			ended = *run.EndedAt
		}
		if !si.AccountUpdated.After(ended) {
			errs = append(errs, resolve([]contracts.AttentionKind{contracts.AttnAccountPool}, false, ended, "The session has ended."))
		}
	}
	return errors.Join(errs...)
}

// messageSummaryFiller is implemented by internal/attention.Store.
type messageSummaryFiller interface {
	MessageItemsWithoutSummary(ctx context.Context, after string, limit int) ([]contracts.AttentionItem, error)
	FillMessageSummary(ctx context.Context, attentionID, body string) (bool, error)
}

// backfillMessageSummaries fills, once per hub process, the empty summary
// of open message items raised before items carried the message excerpt,
// from the newest stored message to the owner the item stands for (its
// thread, else its message). Idempotent: only empty summaries are filled.
func (h *Hub) backfillMessageSummaries(ctx context.Context) error {
	if h.summaryFill.Load() {
		return nil
	}
	f, ok := h.cfg.Attention.(messageSummaryFiller)
	if !ok {
		h.summaryFill.Store(true)
		return nil
	}
	after := ""
	for range sweepPages {
		items, err := f.MessageItemsWithoutSummary(ctx, after, sweepPage)
		if err != nil {
			return err
		}
		for _, it := range items {
			after = it.AttentionID
			msgID := it.Object.ID
			if it.Object.Kind == contracts.KindThread {
				msgID = ""
				_ = h.db.QueryRowContext(ctx, `SELECT message_id FROM messages WHERE thread_id=? AND recipient_kind=? ORDER BY seq DESC LIMIT 1`,
					it.Object.ID, string(contracts.AddrOwner)).Scan(&msgID)
			}
			if msgID == "" {
				continue
			}
			mv, err := h.msgs.GetMessage(h.systemCtx(ctx), contracts.GetRequest{ID: msgID})
			if err != nil {
				continue
			}
			if _, err := f.FillMessageSummary(ctx, it.AttentionID, mv.Envelope.Body.Text); err != nil {
				return err
			}
		}
		if len(items) < sweepPage {
			h.summaryFill.Store(true)
			return nil
		}
	}
	return nil // more next pass
}
