package usage

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func synthNode(id, name string, harness contracts.Harness) contracts.Node {
	n := contracts.Node{NodeID: id, Name: name}
	if harness != "" {
		n.Harnesses = []contracts.HarnessInfo{{Harness: harness, Available: true}}
	}
	return n
}

func TestTokenAccountIsOneCard(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	nodes := []contracts.Node{synthNode("node_a", "synth-mac", contracts.HarnessClaude), synthNode("node_b", "synth-linux", contracts.HarnessClaude)}
	tok := func(node string, st contracts.GrantState, at time.Time) contracts.DeviceGrant {
		return contracts.DeviceGrant{GrantID: "grant_" + node, NodeID: node, Kind: contracts.CredOAuthToken, State: st, ObservedAt: at}
	}
	obsA := contracts.AccountObservation{AccountID: "acct_t", ReportingNode: "node_a", ObservedAt: now.Add(-5 * time.Minute),
		Eligibility: contracts.EligibleYes, Confidence: "high", Windows: []contracts.UsageWindow{{Name: FiveHour, UsedPercent: 10}}}
	obsB := obsA
	obsB.ReportingNode, obsB.ObservedAt = "node_b", now.Add(-time.Minute)
	obsB.Windows = []contracts.UsageWindow{{Name: FiveHour, UsedPercent: 20}}
	view := contracts.AccountView{
		Account: contracts.Account{AccountID: "acct_t", Provider: contracts.ProviderAnthropic, Label: "synth-tok"},
		Grants:  []contracts.DeviceGrant{tok("node_a", contracts.GrantValid, now.Add(-time.Hour)), tok("node_b", contracts.GrantValid, now)},
		ByNode:  []contracts.AccountObservation{obsA, obsB},
		Assignments: []contracts.AccountAssignment{{AssignmentID: "asg_1", NodeID: "node_a", SessionID: "ses_1"},
			{AssignmentID: "asg_2", NodeID: "node_b", SessionID: "ses_2"}},
	}

	t.Run("two machines, all covered", func(t *testing.T) {
		cards := Cards([]contracts.AccountView{view}, nodes, now)
		if len(cards) != 1 {
			t.Fatalf("want 1 card (no ambient either), got %d: %+v", len(cards), cards)
		}
		c := cards[0]
		if !c.Collapsed() || c.NodeID != "" || c.Machines != AllMachines || c.MachineCell(func(string) string { return "x" }) != AllMachines {
			t.Fatalf("card: %+v", c)
		}
		if c.Obs == nil || c.Obs.ReportingNode != "node_b" || c.Grant.NodeID != "node_b" || len(c.Assignments) != 2 || c.KindTag(false) != KindToken {
			t.Fatalf("card details: obs=%+v grant=%+v asg=%d", c.Obs, c.Grant, len(c.Assignments))
		}
	})

	t.Run("token on one of two harness machines", func(t *testing.T) {
		v := view
		v.Grants = []contracts.DeviceGrant{tok("node_a", contracts.GrantValid, now)}
		cards := Cards([]contracts.AccountView{v}, nodes, now)
		var tc *Card
		ambient := 0
		for i := range cards {
			if cards[i].Account == nil {
				ambient++
			} else {
				tc = &cards[i]
			}
		}
		if tc == nil || tc.Machines != "synth-mac" || ambient != 1 {
			t.Fatalf("cards: %+v", cards)
		}
		// Without a machine list: a count.
		if c := Cards([]contracts.AccountView{v}, nil, now); len(c) != 1 || c[0].Machines != "1 machine" {
			t.Fatalf("no nodes: %+v", c)
		}
	})

	t.Run("token plus rota fallback is one card", func(t *testing.T) {
		v := view
		v.Grants = []contracts.DeviceGrant{
			tok("node_a", contracts.GrantRevoked, now),
			{GrantID: "grant_rota_b", NodeID: "node_b", Kind: contracts.CredRotaCheckout, State: contracts.GrantValid},
			tok("node_b", contracts.GrantValid, now.Add(-time.Hour)),
		}
		cards := Cards([]contracts.AccountView{v}, nodes, now)
		if len(cards) != 1 || cards[0].Grant.Kind != contracts.CredOAuthToken || cards[0].Grant.State != contracts.GrantValid {
			t.Fatalf("cards: %+v", cards)
		}
		p := Pools(cards)
		if len(p) != 1 || p[0].Total != 1 {
			t.Fatalf("pool: %+v", p)
		}
	})

	t.Run("non-token account keeps one card per machine", func(t *testing.T) {
		rota := contracts.AccountView{
			Account: contracts.Account{AccountID: "acct_r", Provider: contracts.ProviderAnthropic, Label: "synth-rota"},
			Grants: []contracts.DeviceGrant{{NodeID: "node_a", Kind: contracts.CredRotaCheckout, State: contracts.GrantValid},
				{NodeID: "node_b", Kind: contracts.CredRotaCheckout, State: contracts.GrantValid}},
		}
		cards := Cards([]contracts.AccountView{rota, view}, nodes, now)
		if len(cards) != 3 {
			t.Fatalf("want 3 cards, got %+v", cards)
		}
		// Token card first, then the per-machine cards by machine name.
		if !cards[0].Collapsed() || cards[1].NodeID != "node_b" || cards[2].NodeID != "node_a" || cards[1].Collapsed() {
			t.Fatalf("order: %+v", cards)
		}
		if cards[1].MachineCell(func(id string) string { return "n:" + id }) != "n:node_b" {
			t.Fatalf("machine cell: %+v", cards[1])
		}
	})
}
