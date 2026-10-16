package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Provider names (contracts.NotificationStatus.Provider).
const (
	ProviderFCM       = "fcm"
	ProviderHubStream = "hub-stream"
	ProviderTerminal  = "terminal"
	ProviderNone      = "none"
)

// Message is the minimal notification content handed to a provider. It
// carries identifiers and a generic title only.
type Message struct {
	AttentionID string
	Kind        contracts.AttentionKind
	Severity    string
	DeepLink    string
	GroupKey    string
	Title       string
	Action      string // ActionNotify | ActionDismiss
}

// Data is the complete push data payload. Nothing else is sent.
func (m Message) Data() map[string]string {
	return map[string]string{
		"attention_id": m.AttentionID,
		"kind":         string(m.Kind),
		"severity":     m.Severity,
		"deep_link":    m.DeepLink,
		"group_key":    m.GroupKey,
		"title":        m.Title,
		"action":       m.Action,
	}
}

func messageFor(it contracts.AttentionItem, action string) Message {
	return Message{
		AttentionID: it.AttentionID,
		Kind:        it.Kind,
		Severity:    it.Severity,
		DeepLink:    it.DeepLink,
		GroupKey:    it.GroupKey,
		Title:       GenericTitle(it.Kind),
		Action:      action,
	}
}

// Result is a provider's outcome for one target.
type Result struct {
	Target Target
	Err    error
	// Unregistered means the target can never succeed (token invalid or
	// unregistered); the registration is removed.
	Unregistered bool
	// Retryable means a later attempt may succeed.
	Retryable  bool
	RetryAfter time.Duration
}

// Provider delivers messages to registered device targets.
type Provider interface {
	Name() string
	Deliver(ctx context.Context, m Message, targets []Target) []Result
}

// DispatcherOptions configures a Dispatcher.
type DispatcherOptions struct {
	// FCM is the push provider; nil when the hub has no FCM service account.
	FCM   Provider
	Clock contracts.Clock
	// Debounce delays a notify job so a burst of updates, or a request
	// resolved within moments, produces at most one push (default 2s).
	Debounce time.Duration
	// MaxAttempts bounds provider retries per job (default 5).
	MaxAttempts int
	BaseBackoff time.Duration // default 2s, doubling
	MaxBackoff  time.Duration // default 5m
	// RateBurst/RateInterval form a token bucket over pushes: at most
	// RateBurst immediately, then one per RateInterval (defaults 6, 10s).
	// Rate-limited pushes are deferred, not dropped.
	RateBurst    int
	RateInterval time.Duration
	PollInterval time.Duration // default 1s
	BatchSize    int           // default 50
	Logger       *slog.Logger
}

// Dispatcher turns queued attention transitions into best-effort pushes and
// records NotificationStatus on each item. Delivery state is separate from
// the human acting on the item: the item's State is never changed here.
type Dispatcher struct {
	src  Source
	opts DispatcherOptions
	log  *slog.Logger

	mu       sync.Mutex
	tokens   float64
	tokensAt time.Time
	// lastPush is when a group with a RepeatWindow last pushed (in
	// memory: a hub restart allows one more push).
	lastPush map[string]time.Time
}

// NewDispatcher returns a Dispatcher over src.
func NewDispatcher(src Source, opts DispatcherOptions) *Dispatcher {
	if opts.Clock == nil {
		opts.Clock = contracts.SystemClock{}
	}
	if opts.Debounce < 0 {
		opts.Debounce = 0
	} else if opts.Debounce == 0 {
		opts.Debounce = 2 * time.Second
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = 2 * time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 5 * time.Minute
	}
	if opts.RateBurst <= 0 {
		opts.RateBurst = 6
	}
	if opts.RateInterval <= 0 {
		opts.RateInterval = 10 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 50
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{src: src, opts: opts, log: log, tokens: float64(opts.RateBurst)}
}

// Run processes jobs until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	var wake <-chan struct{}
	if w, ok := d.src.(Waker); ok {
		wake = w.Wake()
	}
	t := time.NewTicker(d.opts.PollInterval)
	defer t.Stop()
	for {
		if _, err := d.ProcessOnce(ctx); err != nil && ctx.Err() == nil {
			d.log.Warn("notifications: dispatch failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-t.C:
		}
	}
}

// ProcessOnce handles every job due now and returns how many it examined.
func (d *Dispatcher) ProcessOnce(ctx context.Context) (int, error) {
	now := d.opts.Clock.Now()
	jobs, err := d.src.DueJobs(ctx, now, d.opts.BatchSize)
	if err != nil {
		return 0, err
	}
	var firstErr error
	for _, j := range jobs {
		if err := d.handle(ctx, j, now); err != nil {
			d.log.Warn("notifications: job failed", "attention_id", j.AttentionID, "action", j.Action, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return len(jobs), firstErr
}

func (d *Dispatcher) handle(ctx context.Context, j Job, now time.Time) error {
	it, seq, err := d.src.ItemForJob(ctx, j.AttentionID)
	if contracts.IsCode(err, contracts.ErrNotFound) {
		return d.src.FinishJob(ctx, j)
	}
	if err != nil {
		return err
	}
	if j.Action == ActionDismiss {
		return d.dismiss(ctx, j, it)
	}
	// One push per transition: a superseded transition, or an item the
	// owner already read/handled or that was withdrawn, is not pushed.
	if seq != j.Seq || it.State != contracts.AttnOpen {
		return d.src.FinishJob(ctx, j)
	}
	if due := j.EnqueuedAt.Add(d.opts.Debounce); now.Before(due) {
		return d.src.RetryJob(ctx, j, due, false)
	}
	prefs, err := d.src.Prefs(ctx)
	if err != nil {
		return err
	}
	if st, detail := Evaluate(prefs, it, now); st != "" {
		if err := d.src.RecordStatus(ctx, it.AttentionID, seq, contracts.NotificationStatus{Provider: ProviderNone, State: st, Detail: detail}); err != nil {
			return err
		}
		return d.src.FinishJob(ctx, j)
	}
	if !d.repeatAllowed(it, now) {
		if err := d.src.RecordStatus(ctx, it.AttentionID, seq, contracts.NotificationStatus{Provider: ProviderNone, State: StateDisabled,
			Detail: "already pushed for this session within " + RepeatWindow(it.Kind).String()}); err != nil {
			return err
		}
		return d.src.FinishJob(ctx, j)
	}
	if ok, next := d.take(now); !ok {
		if err := d.src.RecordStatus(ctx, it.AttentionID, seq, contracts.NotificationStatus{Provider: ProviderNone, State: StatePending, Detail: "rate limited; deferred"}); err != nil {
			return err
		}
		return d.src.RetryJob(ctx, j, next, false)
	}

	d.notePush(it, now)
	why := "FCM not configured on hub"
	if d.opts.FCM != nil {
		targets, err := d.src.Targets(ctx, ProviderFCM)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			why = "no FCM devices registered"
		} else {
			res := d.opts.FCM.Deliver(ctx, messageFor(it, ActionNotify), targets)
			sum := d.summarize(ctx, res)
			switch {
			case sum.sent > 0:
				t := now
				detail := fmt.Sprintf("delivered to %d of %d devices", sum.sent, len(targets))
				if err := d.src.RecordStatus(ctx, it.AttentionID, seq, contracts.NotificationStatus{Provider: ProviderFCM, State: StateSent, SentAt: &t, Detail: detail}); err != nil {
					return err
				}
				return d.src.FinishJob(ctx, j)
			case sum.unregistered == len(targets):
				why = "all FCM registrations were unregistered and removed"
			case sum.retryable && j.Attempts+1 < d.opts.MaxAttempts:
				next := now.Add(d.backoff(j.Attempts, sum.retryAfter))
				if err := d.src.RecordStatus(ctx, it.AttentionID, seq, contracts.NotificationStatus{Provider: ProviderFCM, State: StatePending,
					Detail: clip(fmt.Sprintf("attempt %d failed (%v); retrying", j.Attempts+1, sum.err), 200)}); err != nil {
					return err
				}
				return d.src.RetryJob(ctx, j, next, true)
			default:
				if err := d.src.RecordStatus(ctx, it.AttentionID, seq, contracts.NotificationStatus{Provider: ProviderFCM, State: StateFailed,
					Detail: clip(fmt.Sprintf("FCM delivery failed after %d attempts: %v; item remains in the Inbox and on the event stream", j.Attempts+1, sum.err), 240)}); err != nil {
					return err
				}
				return d.src.FinishJob(ctx, j)
			}
		}
	}
	// hub-stream: the item is published on the event stream (attention.changed);
	// this status tells stream-holding clients (e.g. the Android foreground
	// service) that prefs allow alerting for this transition.
	t := now
	if err := d.src.RecordStatus(ctx, it.AttentionID, seq, contracts.NotificationStatus{Provider: ProviderHubStream, State: StateSent, SentAt: &t,
		Detail: "published to the hub event stream (" + why + ")"}); err != nil {
		return err
	}
	return d.src.FinishJob(ctx, j)
}

// dismiss withdraws a previously pushed alert. It is silent (data-only), so
// it is exempt from quiet hours and rate limiting; failures are retried a
// bounded number of times and never change the item's status.
func (d *Dispatcher) dismiss(ctx context.Context, j Job, it contracts.AttentionItem) error {
	if d.opts.FCM == nil {
		return d.src.FinishJob(ctx, j)
	}
	targets, err := d.src.Targets(ctx, ProviderFCM)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return d.src.FinishJob(ctx, j)
	}
	sum := d.summarize(ctx, d.opts.FCM.Deliver(ctx, messageFor(it, ActionDismiss), targets))
	if sum.sent == 0 && sum.retryable && j.Attempts+1 < d.opts.MaxAttempts {
		return d.src.RetryJob(ctx, j, d.opts.Clock.Now().Add(d.backoff(j.Attempts, sum.retryAfter)), true)
	}
	return d.src.FinishJob(ctx, j)
}

type summary struct {
	sent, unregistered int
	retryable          bool
	retryAfter         time.Duration
	err                error
}

func (d *Dispatcher) summarize(ctx context.Context, res []Result) summary {
	var s summary
	for _, r := range res {
		switch {
		case r.Err == nil:
			s.sent++
		case r.Unregistered:
			s.unregistered++
			if err := d.src.RemoveTarget(ctx, r.Target, "provider reported token unregistered"); err != nil {
				d.log.Warn("notifications: remove target", "err", err)
			}
		default:
			if r.Retryable {
				s.retryable = true
			}
			if r.RetryAfter > s.retryAfter {
				s.retryAfter = r.RetryAfter
			}
			if s.err == nil {
				s.err = r.Err
			}
		}
	}
	if s.err == nil && s.sent == 0 && s.unregistered < len(res) {
		s.err = errors.New("no result")
	}
	return s
}

func (d *Dispatcher) backoff(attempts int, retryAfter time.Duration) time.Duration {
	b := d.opts.BaseBackoff
	for i := 0; i < attempts && b < d.opts.MaxBackoff; i++ {
		b *= 2
	}
	if retryAfter > b {
		b = retryAfter
	}
	return min(b, d.opts.MaxBackoff)
}

// repeatAllowed reports whether it's group may push now (RepeatWindow).
func (d *Dispatcher) repeatAllowed(it contracts.AttentionItem, now time.Time) bool {
	w := RepeatWindow(it.Kind)
	if w <= 0 {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	last, ok := d.lastPush[it.GroupKey]
	return !ok || now.Sub(last) >= w
}

// notePush records a push for it's group when its kind has a
// RepeatWindow, dropping entries past their window (bounded).
func (d *Dispatcher) notePush(it contracts.AttentionItem, now time.Time) {
	w := RepeatWindow(it.Kind)
	if w <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastPush == nil {
		d.lastPush = map[string]time.Time{}
	}
	for k, t := range d.lastPush {
		if now.Sub(t) >= w {
			delete(d.lastPush, k)
		}
	}
	d.lastPush[it.GroupKey] = now
}

// take consumes one rate-limit token, or returns when the next is available.
func (d *Dispatcher) take(now time.Time) (bool, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.tokensAt.IsZero() && now.After(d.tokensAt) {
		d.tokens += float64(now.Sub(d.tokensAt)) / float64(d.opts.RateInterval)
		if max := float64(d.opts.RateBurst); d.tokens > max {
			d.tokens = max
		}
	}
	if d.tokensAt.IsZero() || now.After(d.tokensAt) {
		d.tokensAt = now
	}
	if d.tokens >= 1 {
		d.tokens--
		return true, time.Time{}
	}
	wait := time.Duration((1 - d.tokens) * float64(d.opts.RateInterval))
	return false, now.Add(wait)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// HubStream is the no-outbound-push provider: items are already published
// on the hub event stream (attention.changed), so delivery always succeeds.
// The Dispatcher uses it implicitly when FCM is absent or has no devices.
type HubStream struct{}

// Name returns "hub-stream".
func (HubStream) Name() string { return ProviderHubStream }

// Deliver reports success for every target without network I/O.
func (HubStream) Deliver(_ context.Context, _ Message, targets []Target) []Result {
	out := make([]Result, len(targets))
	for i, t := range targets {
		out[i] = Result{Target: t}
	}
	return out
}
