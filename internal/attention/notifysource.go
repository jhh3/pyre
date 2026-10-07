package attention

import (
	"context"
	"database/sql"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/notifications"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// This file implements notifications.Source over the attention tables.

// DueJobs returns unfinished notification jobs due at or before now.
func (s *Store) DueJobs(ctx context.Context, now time.Time, limit int) ([]notifications.Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT attention_id,seq,action,attempts,created_at FROM attention_notify
		WHERE done=0 AND next_attempt_at<=? ORDER BY next_attempt_at, created_at LIMIT ?`, sqlitex.FormatTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notifications.Job
	for rows.Next() {
		var j notifications.Job
		var created string
		if err := rows.Scan(&j.AttentionID, &j.Seq, &j.Action, &j.Attempts, &created); err != nil {
			return nil, err
		}
		j.EnqueuedAt, _ = sqlitex.ParseTime(created)
		out = append(out, j)
	}
	return out, rows.Err()
}

// ItemForJob returns the current item and its current transition seq.
func (s *Store) ItemForJob(ctx context.Context, attentionID string) (contracts.AttentionItem, int64, error) {
	r, err := s.getQ(ctx, s.db, attentionID)
	return r.item, r.seq, err
}

// Targets lists push registrations for provider.
func (s *Store) Targets(ctx context.Context, provider string) ([]notifications.Target, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider,client_id,device_token FROM attention_push WHERE provider=? ORDER BY client_id`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notifications.Target
	for rows.Next() {
		var t notifications.Target
		if err := rows.Scan(&t.Provider, &t.ClientID, &t.DeviceToken); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RemoveTarget deletes a registration the provider rejected (for example an
// FCM token reported UNREGISTERED). The token must still match so a fresh
// re-registration is not removed by a stale result.
func (s *Store) RemoveTarget(ctx context.Context, t notifications.Target, reason string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM attention_push WHERE provider=? AND client_id=? AND device_token=?`, t.Provider, t.ClientID, t.DeviceToken)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.log.Info("attention: removed push registration", "provider", t.Provider, "client_id", t.ClientID, "reason", reason)
	}
	return nil
}

// RecordStatus stores delivery status on the item when seq is still the
// item's current transition, and publishes attention.changed so stream
// clients see the status. It does not change state, updated_at, or queue
// further work, so recording status can never trigger another notification.
func (s *Store) RecordStatus(ctx context.Context, attentionID string, seq int64, st contracts.NotificationStatus) error {
	now := s.now()
	return sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		r, err := s.getTx(ctx, tx, attentionID)
		if err != nil {
			return err
		}
		if r.seq != seq {
			return nil
		}
		if cur := r.item.Notification; cur != nil && *cur == st {
			return nil
		}
		st := st
		r.item.Notification = &st
		if err := s.saveTx(ctx, tx, r); err != nil {
			return err
		}
		return s.emitTx(ctx, tx, r.item, now)
	})
}

// FinishJob marks a job done and occasionally prunes old finished jobs.
func (s *Store) FinishJob(ctx context.Context, j notifications.Job) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE attention_notify SET done=1 WHERE attention_id=? AND seq=? AND action=?`, j.AttentionID, j.Seq, j.Action); err != nil {
		return err
	}
	if s.applied.Add(1)%128 == 0 {
		cutoff := sqlitex.FormatTime(s.now().Add(-7 * 24 * time.Hour))
		if _, err := s.db.ExecContext(ctx, `DELETE FROM attention_notify WHERE done=1 AND created_at<?`, cutoff); err != nil {
			s.log.Warn("attention: prune notification jobs", "err", err)
		}
	}
	return nil
}

// RetryJob reschedules a job.
func (s *Store) RetryJob(ctx context.Context, j notifications.Job, next time.Time, countAttempt bool) error {
	inc := 0
	if countAttempt {
		inc = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE attention_notify SET next_attempt_at=?, attempts=attempts+? WHERE attention_id=? AND seq=? AND action=? AND done=0`,
		sqlitex.FormatTime(next), inc, j.AttentionID, j.Seq, j.Action)
	return err
}
