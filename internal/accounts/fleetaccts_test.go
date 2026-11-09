package accounts

import (
	"context"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// The hub's account list for own-login matching is reused for
// fleetAcctTTL instead of re-read on every usage update and launch; a
// negative FleetCache (always read) keeps reading.
func TestFleetAccountsReused(t *testing.T) {
	for _, tc := range []struct {
		cache int
		want  int
	}{{0, 1}, {-1, 3}} {
		dir := &fakeFleetDir{}
		dir.use("acct_synth-a", "node_synth-mac", contracts.SystemClock{}.Now())
		e := newEnv(t, func(c *Config) {
			c.Fleet, c.NodeName = dir, "synth-node"
			c.FleetCache = 0
			if tc.cache < 0 {
				c.FleetCache = -1
			}
		})
		for i := 0; i < 3; i++ {
			views, err := e.svc.fleetAccounts(context.Background(), contracts.ProviderAnthropic)
			if err != nil || len(views) != 1 {
				t.Fatalf("cache %d read %d: %d views, %v", tc.cache, i, len(views), err)
			}
		}
		dir.mu.Lock()
		calls := dir.calls
		dir.mu.Unlock()
		if calls != tc.want {
			t.Fatalf("FleetCache %d: %d accounts.list calls for 3 reads, want %d", tc.cache, calls, tc.want)
		}
	}
}
