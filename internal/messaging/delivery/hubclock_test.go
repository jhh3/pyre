package delivery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

type shiftedClock struct{ d time.Duration }

func (c shiftedClock) Now() time.Time { return time.Now().Add(c.d) }

// Input leases are hub-issued and expire on the hub clock: a node clock 10
// minutes ahead must still honour a lease with 2 minutes left, and a node
// 10 minutes behind must not keep honouring a lease the hub let expire.
func TestLeaseExpiryUsesHubClock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		skew    time.Duration
		expires time.Duration // relative to the hub clock
		blocked bool
	}{
		{"node ahead, lease live", 10 * time.Minute, 2 * time.Minute, true},
		{"node behind, lease expired", -10 * time.Minute, -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.engine = New(Config{NodeID: "node_test", Inbox: e.inbox, Runs: e.runs, Reporter: e.rep,
				Clock: shiftedClock{tc.skew}, HubClock: shiftedClock{0}, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond})
			e.launch("ses_a", "run_1", nil)
			e.runs.set(func() {
				e.runs.runs["run_1"].Lease = &contracts.InputLease{LeaseID: "lease_1", RunID: "run_1",
					Holder: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_me"}, ExpiresAt: time.Now().Add(tc.expires)}
			})
			e.put("msg_1", "ses_a", nil)
			e.engine.ProcessSession(context.Background(), "ses_a", true)
			m := e.state("msg_1")
			if tc.blocked {
				if e.ad.Hits() != 0 || m.State != contracts.DeliveryQueued || !strings.Contains(m.Reason, "lease") {
					t.Fatalf("live lease ignored: hits=%d state=%s reason=%q", e.ad.Hits(), m.State, m.Reason)
				}
				return
			}
			if m.State != contracts.DeliveryAccepted {
				t.Fatalf("expired lease still blocks: state=%s reason=%q", m.State, m.Reason)
			}
		})
	}
}
