package usage

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// "openai 0 ready" while Codex ran fine on every machine's own login: the
// matching account now shows as one own-login card and counts as ready.
func TestOwnLoginCardCountsReady(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	login := &contracts.HarnessLogin{Email: "synth-owner@example.invalid", ProviderRef: "synth-chatgpt-1"}
	codex := func(l *contracts.HarnessLogin) []contracts.HarnessInfo {
		return []contracts.HarnessInfo{{Harness: contracts.HarnessCodex, Available: true, Login: l}}
	}
	nodes := []contracts.Node{
		{NodeID: "node_a", Name: "synth-a", Harnesses: codex(login)},
		{NodeID: "node_b", Name: "synth-b", Harnesses: codex(login)},
		{NodeID: "node_c", Name: "synth-c", Harnesses: codex(&contracts.HarnessLogin{Email: "someone-else@example.invalid"})},
	}
	views := []contracts.AccountView{
		{Account: contracts.Account{AccountID: "acct_own", Provider: contracts.ProviderOpenAI, Label: "synth-owner", RotaRef: "synth-owner@example.invalid"}},
		{Account: contracts.Account{AccountID: "acct_dead", Provider: contracts.ProviderOpenAI, Label: "synth-old", Disabled: true}},
	}
	cards := Cards(views, nodes, now)
	var own *Card
	ambient := 0
	for i := range cards {
		c := &cards[i]
		switch {
		case c.Own:
			own = c
		case c.Account == nil && c.Provider == contracts.ProviderOpenAI:
			ambient++
			if c.NodeID != "node_c" {
				t.Errorf("ambient card for a matched machine: %+v", c)
			}
		}
	}
	if own == nil || own.Label() != "synth-owner" || own.Machines != "synth-a, synth-b" || own.Kind() != KindOwn ||
		own.State != Healthy || own.Assessment.Label() != "ready · own login" {
		t.Fatalf("own card %+v", own)
	}
	if ambient != 1 {
		t.Fatalf("ambient cards %d", ambient)
	}
	pools := Pools(cards)
	if len(pools) != 1 || pools[0].Ready != 1 || pools[0].Total != 1 || pools[0].Brief() != "1 ready" || pools[0].Tight() {
		t.Fatalf("pools %+v", pools)
	}

	// Usage reported from a run on it decides the state.
	views[0].ByNode = []contracts.AccountObservation{{AccountID: "acct_own", ReportingNode: "node_b", ObservedAt: now.Add(-time.Minute),
		Eligibility: contracts.EligibleYes, Confidence: "high",
		Windows: []contracts.UsageWindow{{Name: "five_hour", UsedPercent: 92}, {Name: "seven_day", UsedPercent: 30}}}}
	for _, c := range Cards(views, nodes, now) {
		if c.Own && c.State != NearLimit {
			t.Fatalf("with a near-limit reading: %+v", c.Assessment)
		}
	}
	// Matching by provider ref when the account has no rota_ref.
	if !MatchesLogin(contracts.Account{ProviderRef: "synth-chatgpt-1"}, login) || MatchesLogin(contracts.Account{RotaRef: "x"}, login) {
		t.Fatal("MatchesLogin")
	}
}
