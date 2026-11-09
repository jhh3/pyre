package daemon

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// TestLimitHold: a usage or rate limit naming a reset ahead holds Pyre's
// deliveries to the run until that reset (iss_…);
// no event (a turn ended normally since), a login failure, a reset already
// past, no reset, or a limit replayed from before the run started do not.
func TestLimitHold(t *testing.T) {
	now := time.Date(2026, 10, 24, 23, 40, 0, 0, time.UTC)
	started := now.Add(-25 * time.Minute)
	reset := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	ev := func(kind string, at time.Time, r *time.Time) *contracts.LimitEvent {
		return &contracts.LimitEvent{Kind: kind, At: at, ResetAt: r}
	}
	past := now.Add(-time.Minute)
	far := now.Add(30 * 24 * time.Hour)
	for _, c := range []struct {
		name string
		ev   *contracts.LimitEvent
		want time.Time
	}{
		{"spend limit", ev("usage_limit", now.Add(-10*time.Minute), &reset), reset},
		{"rate limit", ev("rate_limited", now.Add(-10*time.Minute), &reset), reset},
		{"cleared", nil, time.Time{}},
		{"login", ev("auth_invalid", now, &reset), time.Time{}},
		{"no reset", ev("usage_limit", now, nil), time.Time{}},
		{"reset passed", ev("usage_limit", now.Add(-time.Hour), &past), time.Time{}},
		{"replayed", ev("usage_limit", started.Add(-time.Minute), &reset), time.Time{}},
		{"bounded", ev("usage_limit", now, &far), now.Add(limitHoldMax)},
	} {
		if got := limitHold(c.ev, started, now); !got.Equal(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
