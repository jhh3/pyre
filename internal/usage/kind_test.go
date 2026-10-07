package usage

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestTokenCardKindsAndOnUse(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tokGrant := contracts.DeviceGrant{NodeID: "node_a", Kind: contracts.CredOAuthToken, State: contracts.GrantValid}
	rotaGrant := contracts.DeviceGrant{NodeID: "node_a", Kind: contracts.CredRotaCheckout, State: contracts.GrantValid}
	old := &contracts.AccountObservation{AccountID: "acct_t", ReportingNode: "node_a", ObservedAt: now.Add(-2 * time.Hour),
		Eligibility: contracts.EligibleYes, Confidence: "high", Windows: []contracts.UsageWindow{{Name: FiveHour, UsedPercent: 30}}}

	// An idle token account with an aged reading is ready, checked on use.
	a := Assess(contracts.Account{AccountID: "acct_t"}, old, &tokGrant, now)
	if a.State != Healthy || a.OnUse != ProbeOnUse || !a.LastKnown || a.Label() != "ready · checked on use" {
		t.Fatalf("token: %+v %q", a, a.Label())
	}
	// The same reading on a rota login is unknown (unchanged behaviour).
	if a := Assess(contracts.Account{AccountID: "acct_r"}, old, &rotaGrant, now); a.State != Unknown {
		t.Fatalf("rota: %+v", a)
	}
	// Near limit and at limit come from the last-known windows.
	near := *old
	near.Windows = []contracts.UsageWindow{{Name: FiveHour, UsedPercent: 90}}
	if a := Assess(contracts.Account{}, &near, &tokGrant, now); a.State != NearLimit || a.Label() != "near limit · checked on use" {
		t.Fatalf("near: %+v", a)
	}
	// A rejected token needs re-auth.
	dead := tokGrant
	dead.State, dead.Detail = contracts.GrantDead, "token revoked or expired"
	if a := Assess(contracts.Account{}, old, &dead, now); a.State != NeedsReauth {
		t.Fatalf("dead: %+v", a)
	}

	views := []contracts.AccountView{
		{Account: contracts.Account{AccountID: "acct_t", Provider: contracts.ProviderAnthropic, Label: "synth"}, Grants: []contracts.DeviceGrant{tokGrant}, ByNode: []contracts.AccountObservation{*old}},
		{Account: contracts.Account{AccountID: "acct_r", Provider: contracts.ProviderAnthropic, Label: "synth"}, Grants: []contracts.DeviceGrant{rotaGrant}},
		{Account: contracts.Account{AccountID: "acct_o", Provider: contracts.ProviderOpenAI, Label: "synth"}, Grants: []contracts.DeviceGrant{{NodeID: "node_a", Kind: contracts.CredRotaCheckout}}},
	}
	cards := Cards(views, nil, now)
	mixed := MixedKinds(cards)
	if !mixed[contracts.ProviderAnthropic] || mixed[contracts.ProviderOpenAI] {
		t.Fatalf("mixed: %v", mixed)
	}
	tags := map[string]string{}
	for _, c := range cards {
		tags[c.Account.AccountID] = c.KindTag(mixed[c.Provider])
	}
	if tags["acct_t"] != KindToken || tags["acct_r"] != KindRota || tags["acct_o"] != "" {
		t.Fatalf("tags: %v", tags)
	}
	// The pool counts the idle token account as ready.
	for _, p := range Pools(cards) {
		if p.Provider == contracts.ProviderAnthropic && p.Ready != 1 {
			t.Fatalf("pool: %+v", p)
		}
	}
}
