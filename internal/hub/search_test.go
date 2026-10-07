package hub

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakeSearchNodes answers transcript.search (and refuses memory.search,
// as a node without Hive does) for connected nodes, from canned hits.
func fakeSearchNodes(x *harness, hits map[string][]contracts.TranscriptHit, seen *[]string) {
	var mu sync.Mutex
	x.ch.onSend = func(nodeID string, cmd contracts.NodeCommand) {
		if cmd.Kind != contracts.CmdAction {
			return
		}
		var ar contracts.NodeActionRequest
		_ = json.Unmarshal(cmd.Payload, &ar)
		go func() {
			mu.Lock()
			defer mu.Unlock()
			*seen = append(*seen, nodeID+":"+ar.Action+":"+cmd.Actor.ID)
			x.ack(nodeID, cmd)
			switch ar.Action {
			case contracts.ActionTranscriptSearch:
				var p contracts.TranscriptSearchParams
				_ = json.Unmarshal(ar.Params, &p)
				if p.Query == "" || p.BudgetMS <= 0 || p.Limit <= 0 {
					x.t.Errorf("params not forwarded: %+v", p)
				}
				x.result(nodeID, cmd, contracts.OutcomeSucceeded, contracts.TranscriptSearchResult{Hits: hits[nodeID], Runs: 1})
			default:
				x.report(nodeID, contracts.ReportCommandResult, contracts.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind,
					Outcome: contracts.OutcomeFailed, Error: contracts.AsError(contracts.Errorf(contracts.ErrUnsupported, "no hive")), At: x.clock.Now()})
			}
		}()
	}
}

func TestSearchFansOutMergesAndMarksOfflineNodes(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	x.connect("node_synth-offline", 0)
	x.disconnect("node_synth-offline")
	sm := x.start(owner, "op_s1", nodeMac)
	sl := x.start(owner, "op_s2", nodeLin)
	sa := x.start(owner, "op_s3", nodeLin)
	title := "Stripe webhook work (synthetic)"
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_t"}, SessionID: sm.Session.SessionID, Title: &title}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_a"}, SessionID: sa.Session.SessionID, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	now := x.clock.Now()
	hit := func(s contracts.SessionView, node string, seq int64, score float64, age time.Duration) contracts.TranscriptHit {
		return contracts.TranscriptHit{SessionID: s.Session.SessionID, RunID: s.Session.CurrentRunID, NodeID: node, Harness: contracts.HarnessClaude,
			Title: "node-side title", Kind: "assistant", Seq: seq, At: now.Add(-age), Snippet: "the «stripe» «webhook» handler", Score: score}
	}
	var seen []string
	fakeSearchNodes(x, map[string][]contracts.TranscriptHit{
		nodeMac: {hit(sm, nodeMac, 7, 9, time.Hour), hit(sm, nodeMac, 3, 4, 48*time.Hour)},
		nodeLin: {hit(sl, nodeLin, 2, 8, 2*time.Hour), hit(sa, nodeLin, 5, 9.5, time.Hour)},
	}, &seen)

	resp, err := x.h.Search(as(owner), contracts.SearchRequest{Query: "stripe webhook", BudgetMS: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 3 {
		t.Fatalf("hits (archived filtered) %+v", resp.Hits)
	}
	if resp.Hits[0].SessionID != sm.Session.SessionID || resp.Hits[0].Seq != 7 || resp.Hits[0].Title != title || resp.Hits[0].NodeID != nodeMac {
		t.Fatalf("top hit %+v", resp.Hits[0])
	}
	for _, h := range resp.Hits {
		if h.SessionID == sa.Session.SessionID {
			t.Fatalf("archived session leaked: %+v", h)
		}
	}
	states := map[string]contracts.SearchNodeStatus{}
	for _, n := range resp.Nodes {
		states[n.NodeID] = n
	}
	if off := states["node_synth-offline"]; off.State != contracts.SearchNodeOffline || !strings.Contains(off.Detail, "not searched (offline)") {
		t.Fatalf("offline node %+v", off)
	}
	if states[nodeMac].State != contracts.SearchNodeSearched || states[nodeMac].Hits != 2 || states[nodeLin].Hits != 2 {
		t.Fatalf("node states %+v", resp.Nodes)
	}
	if resp.Nodes[len(resp.Nodes)-1].NodeID != "node_synth-offline" {
		t.Fatalf("offline should sort last: %+v", resp.Nodes)
	}
	for _, s := range seen {
		if strings.HasPrefix(s, "node_synth-offline") {
			t.Fatalf("offline node was sent a command: %s", s)
		}
	}

	resp, err = x.h.Search(as(owner), contracts.SearchRequest{Query: "stripe webhook", IncludeArchived: true, BudgetMS: 2000})
	if err != nil || len(resp.Hits) != 4 {
		t.Fatalf("include archived %d %v", len(resp.Hits), err)
	}
	var archived bool
	for _, h := range resp.Hits {
		archived = archived || (h.SessionID == sa.Session.SessionID && h.Archived)
	}
	if !archived {
		t.Fatalf("archived hit not marked: %+v", resp.Hits)
	}

	// One node only.
	resp, err = x.h.Search(as(owner), contracts.SearchRequest{Query: "stripe", NodeID: nodeLin, BudgetMS: 2000})
	if err != nil || len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != nodeLin {
		t.Fatalf("node filter %+v %v", resp.Nodes, err)
	}
}

func TestSearchPrincipalsAndValidation(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	s := x.start(owner, "op_s1", nodeMac)
	var seen []string
	fakeSearchNodes(x, map[string][]contracts.TranscriptHit{}, &seen)
	_, err := x.h.Search(nodeCtx(nodeMac), contracts.SearchRequest{Query: "x"})
	wantCode(t, err, contracts.ErrForbidden)
	_, err = x.h.Search(as(owner), contracts.SearchRequest{Query: "  "})
	wantCode(t, err, contracts.ErrInvalid)
	// A session principal searches as itself (the node sees its identity).
	resp, err := x.h.Search(as(sesPrincipal(s.Session.SessionID)), contracts.SearchRequest{Query: "anything", BudgetMS: 1500})
	if err != nil || resp.Hits == nil || len(resp.Nodes) != 1 {
		t.Fatalf("session search %+v %v", resp, err)
	}
	found := false
	for _, v := range seen {
		found = found || v == nodeMac+":"+contracts.ActionTranscriptSearch+":"+s.Session.SessionID
	}
	if !found {
		t.Fatalf("node did not see the session principal: %v", seen)
	}
	// session agents may also invoke the node action directly
	if !sessionActions[contracts.ActionTranscriptSearch] {
		t.Fatal("transcript.search must be a session action")
	}
}

func TestSearchTimeoutNodeIsReported(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0) // connected, never answers
	resp, err := x.h.Search(as(owner), contracts.SearchRequest{Query: "x", BudgetMS: 300, Sources: []string{contracts.SearchSourceTranscript}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].State != contracts.SearchNodeTimeout {
		t.Fatalf("timeout %+v", resp.Nodes)
	}
}
