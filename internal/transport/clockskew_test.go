package transport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// skewClock is the system clock shifted by a fixed offset (a node whose
// clock is wrong).
type skewClock struct{ d time.Duration }

func (c skewClock) Now() time.Time { return time.Now().Add(c.d) }

func resultFor(e *env, id string) *contracts.CommandResult {
	var out *contracts.CommandResult
	e.hub.snapshot(func(hb *fakeHub) {
		for i := range hb.results {
			if hb.results[i].CommandID == id {
				r := hb.results[i]
				out = &r
			}
		}
	})
	return out
}

// Command expiry is judged on the hub clock: a node whose clock runs 10
// minutes ahead still runs a command with minutes left, and a node 10
// minutes behind never runs a command the hub already considers expired.
func TestCommandExpiryUsesHubClockUnderSkew(t *testing.T) {
	for _, skew := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		t.Run(skew.String(), func(t *testing.T) {
			e := newEnv(t)
			e.start()
			h := &countingHandler{}
			link := e.newLink(openDB(t, "node.db"), h, func(c *NodeLinkConfig) { c.Clock = skewClock{skew} })
			runLink(t, link)
			waitFor(t, "connected", link.Connected)

			ahead := link.Status().HubClockAhead
			if d := (ahead + skew).Abs(); d > 5*time.Second {
				t.Fatalf("HubClockAhead = %v, want about %v", ahead, -skew)
			}
			if d := link.HubNow().Sub(time.Now()); d < 0 || d > 5*time.Second {
				t.Fatalf("HubNow is %v from the hub clock, want a small non-negative bound", d)
			}

			ctx := context.Background()
			hubNow := time.Now().UTC() // the hub (NodeServer) uses the system clock
			valid := hubNow.Add(2 * time.Minute)
			expired := hubNow.Add(-time.Second)
			ok := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Seq: 1, Kind: contracts.CmdInterrupt,
				IssuedAt: hubNow, ExpiresAt: &valid, Payload: json.RawMessage(`{}`)}
			stale := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Seq: 2, Kind: contracts.CmdStop,
				IssuedAt: hubNow.Add(-time.Minute), ExpiresAt: &expired, Payload: json.RawMessage(`{}`)}
			for _, c := range []contracts.NodeCommand{ok, stale} {
				if err := e.ns.Send(ctx, e.nodeID, c); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, "both results", func() bool { return resultFor(e, ok.CommandID) != nil && resultFor(e, stale.CommandID) != nil })
			if r := resultFor(e, ok.CommandID); r.Outcome != contracts.OutcomeSucceeded || h.count(ok.CommandID) != 1 {
				t.Fatalf("valid command with skew %v: outcome %s err %v, ran %d times", skew, r.Outcome, r.Error, h.count(ok.CommandID))
			}
			if r := resultFor(e, stale.CommandID); r.Error == nil || r.Error.Code != contracts.ErrExpired || h.count(stale.CommandID) != 0 {
				t.Fatalf("expired command with skew %v: outcome %s err %v, ran %d times", skew, r.Outcome, r.Error, h.count(stale.CommandID))
			}
		})
	}
}

// The estimate survives a node restart, so commands recorded before the
// restart are judged on the hub clock even before the next Welcome.
func TestHubClockEstimatePersists(t *testing.T) {
	e := newEnv(t)
	e.start()
	db := openDB(t, "node.db")
	skew := -10 * time.Minute
	link := e.newLink(db, &countingHandler{}, func(c *NodeLinkConfig) { c.Clock = skewClock{skew} })
	stop := runLink(t, link)
	waitFor(t, "connected", link.Connected)
	stop()
	e.stop()

	again := e.newLink(db, &countingHandler{}, func(c *NodeLinkConfig) { c.Clock = skewClock{skew} })
	if d := again.HubNow().Sub(time.Now()); d < 0 || d > 5*time.Second {
		t.Fatalf("restarted link HubNow is %v from the hub clock before connecting", d)
	}
	if d := (again.Status().HubClockAhead + skew).Abs(); d > 5*time.Second {
		t.Fatalf("restarted HubClockAhead = %v", again.Status().HubClockAhead)
	}
}
