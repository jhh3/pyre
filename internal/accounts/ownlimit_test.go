package accounts

import (
	"context"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// An agy run's quota limit marks the machine's own agy login limited until
// the reset, for the node's harness inventory (decision I72); it ends at
// the reset, and Claude limits never read as one.
func TestOwnLimitUntilReset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if l := e.svc.OwnLimit(ctx, contracts.HarnessAgy); l != nil {
		t.Fatalf("no limit yet: %+v", l)
	}
	e.startRun("ses_agy", "run_agy", contracts.HarnessAgy, contracts.AccountPolicy{}, nil)
	reset := e.clock.Now().Add(13 * time.Hour)
	if _, err := e.svc.HandleLimitEvent(ctx, "ses_agy", "run_agy", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset,
		Detail: "RESOURCE_EXHAUSTED (code 429): Individual quota reached."}); err != nil {
		t.Fatal(err)
	}
	l := e.svc.OwnLimit(ctx, contracts.HarnessAgy)
	if l == nil || l.ResetAt == nil || !l.ResetAt.Equal(reset) || l.Kind != LimitUsage {
		t.Fatalf("limit %+v", l)
	}
	if l := e.svc.OwnLimit(ctx, contracts.HarnessClaude); l != nil {
		t.Fatalf("claude has managed logins: %+v", l)
	}
	// A second run's later limit with a later reset wins.
	e.startRun("ses_agy2", "run_agy2", contracts.HarnessAgy, contracts.AccountPolicy{}, nil)
	e.clock.Advance(time.Hour)
	later := reset.Add(time.Hour)
	if _, err := e.svc.HandleLimitEvent(ctx, "ses_agy2", "run_agy2", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &later}); err != nil {
		t.Fatal(err)
	}
	if l := e.svc.OwnLimit(ctx, contracts.HarnessAgy); l == nil || !l.ResetAt.Equal(later) {
		t.Fatalf("newest limit %+v", l)
	}
	e.clock.Advance(later.Sub(e.clock.Now()) + time.Second)
	if l := e.svc.OwnLimit(ctx, contracts.HarnessAgy); l != nil {
		t.Fatalf("limit after its reset: %+v", l)
	}
}

// A limit without a reset says nothing about when the login has room: it
// is not reported.
func TestOwnLimitNeedsReset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.startRun("ses_agy", "run_agy", contracts.HarnessAgy, contracts.AccountPolicy{}, nil)
	if _, err := e.svc.HandleLimitEvent(ctx, "ses_agy", "run_agy", contracts.LimitEvent{Kind: LimitUsage}); err != nil {
		t.Fatal(err)
	}
	if l := e.svc.OwnLimit(ctx, contracts.HarnessAgy); l != nil {
		t.Fatalf("limit without a reset: %+v", l)
	}
}
