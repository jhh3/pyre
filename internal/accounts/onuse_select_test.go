package accounts

import (
	"context"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Selection agrees with the display (usage.Judge): among expired-at-rest
// accounts the one with the most last-known headroom is chosen, and one
// whose last-known limit has not reset yet is not checked out at all.
func TestExpiredAtRestRanksByLastKnownWindows(t *testing.T) {
	r, fr := newRefreshEnv(t)
	r.svc.cfg.Rota.Independent = map[string]bool{}
	ctx := context.Background()
	r.addExpiredClaude(fr, "acct_synth-a", "A")
	r.addExpiredClaude(fr, "acct_synth-b", "B")
	r.svc.ObserveDue(ctx, "")
	lastKnown := func(id string, ws ...contracts.UsageWindow) {
		o := latest(t, r.testEnv, id)
		o.Windows = ws
		if err := putObservation(ctx, r.svc.db, o); err != nil {
			t.Fatal(err)
		}
	}
	reset := r.clock.Now().Add(3 * time.Hour)
	lastKnown("acct_synth-a", contracts.UsageWindow{Name: "five_hour"}, contracts.UsageWindow{Name: "seven_day", UsedPercent: 97})
	lastKnown("acct_synth-b", contracts.UsageWindow{Name: "five_hour"}, contracts.UsageWindow{Name: "seven_day", UsedPercent: 2})
	sel, err := r.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic})
	if err != nil || sel.AccountID != "acct_synth-b" {
		t.Fatalf("most last-known headroom: %+v %v", sel, err)
	}
	// B's weekly limit is spent until the reset: A is chosen, B is never refreshed.
	lastKnown("acct_synth-b", contracts.UsageWindow{Name: "seven_day", UsedPercent: 100, ResetAt: &reset})
	sel, err = r.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic})
	if err != nil || sel.AccountID != "acct_synth-a" {
		t.Fatalf("limited by last-known windows: %+v %v", sel, err)
	}
	if fr.n() != 0 {
		t.Fatalf("dry runs never refresh (calls %d)", fr.n())
	}
}
