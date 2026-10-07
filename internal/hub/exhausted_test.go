package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// One usage limit is one account.exhausted event in the hub's stream: the
// node's accounts service reports one (with earliest reset and reasons) just
// before its fallback plan, and the hub no longer derives a second from the
// plan. A plan that arrives without a node event still yields exactly one,
// and a node event arriving after it is dropped.
func TestOneLimitOneExhaustedEvent(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_s", nodeMac)
	reset := x.clock.Now().Add(3 * time.Hour).UTC()
	nodeEvent := func(runID string) contracts.Event {
		b, _ := json.Marshal(map[string]any{"provider": "anthropic", "session_id": v.Session.SessionID, "from_run_id": runID,
			"outcome": "waiting", "earliest_reset": reset, "reasons": []string{"synthetic-account-b: cooling down"}})
		return contracts.Event{Kind: contracts.EvAccountExhausted, Object: contracts.ObjectRef{Kind: contracts.KindSession, ID: v.Session.SessionID},
			SessionID: v.Session.SessionID, Payload: b}
	}
	plan := func(runID string) contracts.FallbackPlan {
		return contracts.FallbackPlan{SessionID: v.Session.SessionID, FromRunID: runID, Outcome: contracts.FallbackWaiting, Reason: "no eligible anthropic subscription"}
	}

	// Node order: its event, then the plan.
	x.report(nodeMac, contracts.ReportEvent, nodeEvent(v.Run.RunID))
	x.report(nodeMac, contracts.ReportFallback, plan(v.Run.RunID))
	if n := x.countEvents(contracts.EvAccountExhausted); n != 1 {
		t.Fatalf("account.exhausted events for one limit = %d, want 1", n)
	}
	if n := x.countEvents(contracts.EvFallbackPlanned); n != 1 {
		t.Fatalf("account.fallback events = %d, want 1", n)
	}
	var payload string
	x.db.QueryRow(`SELECT payload FROM events WHERE kind='account.exhausted'`).Scan(&payload)
	if !strings.Contains(payload, `"earliest_reset"`) || !strings.Contains(payload, `"from_run_id":"`+v.Run.RunID+`"`) {
		t.Fatalf("kept event should be the node's (with earliest_reset): %s", payload)
	}

	// Plan first (no node event yet): the hub derives one; a late node
	// event for the same run is dropped.
	const run2 = "run_synthetic-second-limit"
	x.report(nodeMac, contracts.ReportFallback, plan(run2))
	x.report(nodeMac, contracts.ReportEvent, nodeEvent(run2))
	if n := x.countEvents(contracts.EvAccountExhausted); n != 2 {
		t.Fatalf("account.exhausted events after a second limit = %d, want 2", n)
	}
	// Unrelated node events still pass through.
	x.report(nodeMac, contracts.ReportEvent, contracts.Event{Kind: contracts.EvAccountExhausted, Object: contracts.ObjectRef{Kind: contracts.KindAccount, ID: "acct_synthetic"}, Payload: json.RawMessage(`{"account_id":"acct_synthetic"}`)})
	if n := x.countEvents(contracts.EvAccountExhausted); n != 3 {
		t.Fatalf("account-level exhausted event dropped: %d", n)
	}
}
