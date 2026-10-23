package mock

import (
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// The mock raises and resolves "waiting on you" like the hub, so TUI and
// Android fixtures can show a lead waiting on the owner.
func TestRaiseWaitingLikeTheHub(t *testing.T) {
	b, clk, node := newEmpty(t)
	sv := start(t, b, node)
	sid, runID := sv.Session.SessionID, sv.Run.RunID
	waiting := func() []c.AttentionItem {
		t.Helper()
		page, err := b.ListAttention(as(owner), c.ListAttentionRequest{SessionID: sid, Kinds: []c.AttentionKind{c.AttnWaiting},
			States: []c.AttentionState{c.AttnOpen, c.AttnRead, c.AttnHandled, c.AttnWithdrawn}})
		if err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	raise := func() c.AttentionItem {
		t.Helper()
		clk.Advance(1)
		it, err := b.RaiseWaiting(sid, "Should I ship the synthetic build?\nmore", "Synthetic report done. Should I ship the synthetic build?")
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	it := raise()
	v, err := b.GetSession(as(owner), c.GetRequest{ID: sid})
	if err != nil || it.Title != "Waiting on you: Should I ship the synthetic build?" || it.Kind != c.AttnWaiting || it.GroupKey != "waiting:"+sid ||
		v.Flag == nil || v.Flag.Level != c.FlagNeedsYou || v.Flag.Reason != "waiting" {
		t.Fatalf("raise: %+v flag %+v %v", it, v.Flag, err)
	}
	resolvers := []struct {
		name  string
		state c.AttentionState
		fn    func()
	}{
		{"owner message", c.AttnHandled, func() {
			if _, err := b.SendMessage(as(owner), c.SendMessageRequest{Mutation: op(), Recipient: c.Address{Kind: c.AddrSession, ID: sid}, Body: c.MessageBody{Text: "synthetic"}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"owner typing", c.AttnHandled, func() {
			l, err := b.AcquireLease(as(owner), c.LeaseRequest{Mutation: op(), RunID: runID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.ReleaseLease(as(owner), c.LeaseReleaseRequest{Mutation: op(), LeaseID: l.LeaseID}); err != nil {
				t.Fatal(err)
			}
		}},
		{"next turn", c.AttnWithdrawn, func() {
			if err := b.SetRunState(runID, c.RunRunning, ""); err != nil {
				t.Fatal(err)
			}
			if err := b.SetRunState(runID, c.RunWaitingInput, ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"ask", c.AttnWithdrawn, func() {
			if _, err := b.CreateAsk(as(c.Principal{Kind: c.PrincipalSession, ID: sid}), askReq("synthetic-ship", "Ship the synthetic build?", "a")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	// Each new question after the last was resolved is a new item.
	ids := map[string]bool{}
	for i, r := range resolvers {
		if i > 0 {
			it = raise()
		}
		ids[it.AttentionID] = true
		r.fn()
		got := waiting()
		if len(got) != i+1 || len(ids) != i+1 || it.Count != 1 {
			t.Fatalf("%s: %d items for %d questions: %+v", r.name, len(got), i+1, got)
		}
		for _, g := range got {
			if g.AttentionID == it.AttentionID && g.State != r.state {
				t.Fatalf("%s: %+v", r.name, g)
			}
		}
	}
}
