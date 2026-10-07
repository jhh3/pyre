package notifications

import (
	"context"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Job actions.
const (
	// ActionNotify asks providers to alert about an item that (re)entered
	// the open state.
	ActionNotify = "notify"
	// ActionDismiss asks providers that already alerted to withdraw the
	// alert because the item was read, handled, snoozed, or withdrawn.
	ActionDismiss = "dismiss"
)

// Job is one queued notification decision for an attention item state
// transition. Seq identifies the transition; at most one push is sent per
// (AttentionID, Seq, Action).
type Job struct {
	AttentionID string
	Seq         int64
	Action      string
	Attempts    int
	EnqueuedAt  time.Time
}

// Target is one registered device endpoint for a provider.
type Target struct {
	Provider    string
	ClientID    string
	DeviceToken string
}

// Source is the durable side of the dispatcher: the attention store
// implements it (internal/attention.Store). It is defined here so this
// package stays free of storage dependencies and can be linked into
// client binaries for Desktop.
type Source interface {
	// DueJobs returns unfinished jobs whose next attempt time is <= now.
	DueJobs(ctx context.Context, now time.Time, limit int) ([]Job, error)
	// ItemForJob returns the current item and its current transition seq.
	ItemForJob(ctx context.Context, attentionID string) (contracts.AttentionItem, int64, error)
	// Prefs returns the stored notification preferences.
	Prefs(ctx context.Context) (contracts.NotificationPrefs, error)
	// Targets lists push registrations for a provider.
	Targets(ctx context.Context, provider string) ([]Target, error)
	// RemoveTarget deletes a registration the provider reported as invalid.
	RemoveTarget(ctx context.Context, t Target, reason string) error
	// RecordStatus stores delivery status on the item if seq is still the
	// item's current transition (stale results are ignored).
	RecordStatus(ctx context.Context, attentionID string, seq int64, st contracts.NotificationStatus) error
	// FinishJob marks a job done.
	FinishJob(ctx context.Context, j Job) error
	// RetryJob reschedules a job; countAttempt increments Attempts.
	RetryJob(ctx context.Context, j Job, next time.Time, countAttempt bool) error
}

// Waker is optionally implemented by a Source to signal new jobs promptly.
type Waker interface {
	Wake() <-chan struct{}
}
