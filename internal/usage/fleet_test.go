package usage

import (
	"math"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// The fleet mean covers current readings only: an unknown (stale) account
// and an unobserved one are left out and counted, a disabled one is not an
// account of the pool, a provider without a 5-hour window reports none,
// and agy's own login without a reading counts as missing.
func TestUtilizations(t *testing.T) {
	login := func(node string) []contracts.DeviceGrant {
		return []contracts.DeviceGrant{{NodeID: node, Kind: contracts.CredRotaCheckout, State: contracts.GrantValid}}
	}
	ob := func(age time.Duration, el contracts.Eligibility, ws ...contracts.UsageWindow) []contracts.AccountObservation {
		o := obs(el, age, ws...)
		return []contracts.AccountObservation{*o}
	}
	acct := func(id string, p contracts.Provider) contracts.Account {
		return contracts.Account{AccountID: id, Provider: p, Label: id}
	}
	passed := now.Add(-time.Minute)
	views := []contracts.AccountView{
		{Account: acct("acct_synth-a", contracts.ProviderAnthropic), Grants: login("node_synth-a"),
			ByNode: ob(time.Minute, contracts.EligibleYes, w(FiveHour, 20), w(Weekly, 40), w(Weekly+"_fable", 99))},
		{Account: acct("acct_synth-b", contracts.ProviderAnthropic), Grants: login("node_synth-a"),
			ByNode: ob(2*time.Minute, contracts.EligibleYes, w(FiveHour, 70, passed), w(Weekly, 61))},
		// An hour old: not current for the 5-hour mean, still counted for
		// the week (idle accounts' weeks move slowly).
		{Account: acct("acct_synth-c", contracts.ProviderAnthropic), Grants: login("node_synth-a"),
			ByNode: ob(time.Hour, contracts.EligibleYes, w(FiveHour, 95), w(Weekly, 95))},
		// Over 3 hours old: in neither mean.
		{Account: acct("acct_synth-e", contracts.ProviderAnthropic), Grants: login("node_synth-a"),
			ByNode: ob(4*time.Hour, contracts.EligibleYes, w(FiveHour, 50), w(Weekly, 50))},
		// A failed probe's carried-over windows (Stale): in neither mean.
		{Account: acct("acct_synth-f", contracts.ProviderAnthropic), Grants: login("node_synth-a"),
			ByNode: func() []contracts.AccountObservation {
				o := ob(time.Minute, contracts.EligibleUnknown, w(FiveHour, 50), w(Weekly, 50))
				o[0].Stale = true
				return o
			}()},
		// No observation yet: missing too.
		{Account: acct("acct_synth-d", contracts.ProviderAnthropic), Grants: login("node_synth-a")},
		{Account: contracts.Account{AccountID: "acct_synth-off", Provider: contracts.ProviderAnthropic, Disabled: true}, Grants: login("node_synth-a"),
			ByNode: ob(time.Minute, contracts.EligibleYes, w(FiveHour, 100), w(Weekly, 100))},
		// Codex reports only the week.
		{Account: acct("acct_synth-x", contracts.ProviderOpenAI), Grants: login("node_synth-a"),
			ByNode: ob(time.Minute, contracts.EligibleYes, w(Weekly, 30))},
		{Account: acct("acct_synth-y", contracts.ProviderOpenAI), Grants: login("node_synth-a"),
			ByNode: ob(time.Minute, contracts.EligibleExhausted, w(Weekly, 100, now.Add(time.Hour)))},
	}
	nodes := []contracts.Node{{NodeID: "node_synth-a", Name: "synth-a", Harnesses: []contracts.HarnessInfo{
		{Harness: contracts.HarnessClaude, Available: true}, {Harness: contracts.HarnessCodex, Available: true}, {Harness: contracts.HarnessAgy, Available: true}}}}
	got := Utilizations(Cards(views, nodes, now), now)
	if len(got) != 3 || got[0].Provider != contracts.ProviderAnthropic || got[1].Provider != contracts.ProviderOpenAI || got[2].Provider != contracts.ProviderGoogle {
		t.Fatalf("providers/order: %+v", got)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	c := got[0]
	// a and b are current, c counts for the week only, d (unobserved), e
	// (too old) and f (failed probe) are missing; the passed 5-hour reset
	// counts as 0; the model-scoped week is not the weekly window.
	if c.Current != 3 || c.Total != 6 || !c.Partial() {
		t.Fatalf("claude counts: %+v", c)
	}
	if !c.FiveHour.Has || c.FiveHour.N != 2 || c.FiveHour.Of != 3 || !c.FiveHour.Partial() || !near(c.FiveHour.Percent, 10) {
		t.Fatalf("claude 5h: %+v", c.FiveHour)
	}
	if !c.Weekly.Has || c.Weekly.N != 3 || c.Weekly.Partial() || !near(c.Weekly.Percent, (40+61+95)/3.0) {
		t.Fatalf("claude week: %+v", c.Weekly)
	}
	x := got[1]
	// An at-limit account with a fresh reading is current (100%).
	if x.FiveHour.Has || x.FiveHour.N != 0 || x.Current != 2 || x.Total != 2 || x.Partial() || !near(x.Weekly.Percent, 65) {
		t.Fatalf("codex: %+v", x)
	}
	g := got[2]
	// agy's own login is counted (it reports its own usage) but has no
	// reading yet.
	if g.Total != 1 || g.Current != 0 || !g.Partial() || g.FiveHour.Has || g.Weekly.Has || g.Weekly.N != 0 {
		t.Fatalf("agy ambient: %+v", g)
	}

	// A provider with only disabled accounts and no ambient login is absent.
	only := []contracts.AccountView{views[6]} // acct_synth-off
	if u := Utilizations(Cards(only, nil, now), now); len(u) != 0 {
		t.Fatalf("disabled only: %+v", u)
	}
	// Usage is per account: two logins of one account count once, by the
	// newest current reading.
	two := []contracts.AccountView{{Account: acct("acct_synth-r", contracts.ProviderAnthropic),
		Grants: append(login("node_synth-a"), login("node_synth-b")...),
		ByNode: []contracts.AccountObservation{
			{AccountID: "acct_synth-r", ReportingNode: "node_synth-a", ObservedAt: now.Add(-3 * time.Minute), Eligibility: contracts.EligibleYes, Windows: []contracts.UsageWindow{w(Weekly, 10)}},
			{AccountID: "acct_synth-r", ReportingNode: "node_synth-b", ObservedAt: now.Add(-time.Minute), Eligibility: contracts.EligibleYes, Windows: []contracts.UsageWindow{w(Weekly, 20)}},
		}}}
	if u := Utilizations(Cards(two, nil, now), now); len(u) != 1 || u[0].Total != 1 || u[0].Weekly.N != 1 || !near(u[0].Weekly.Percent, 20) {
		t.Fatalf("one account, two logins: %+v", u)
	}
}

// agy's own login reads its usage itself (HarnessInfo.Usage): a fresh
// reading is averaged, one Google login on two machines is one account,
// and a stale reading is missing. Claude's unmanaged own login is shown
// but never counted.
func TestUtilizationsAgyOwnLogin(t *testing.T) {
	reading := func(age time.Duration, five, week float64) *contracts.HarnessUsage {
		return &contracts.HarnessUsage{ObservedAt: now.Add(-age), Source: "agy:/usage", Windows: []contracts.UsageWindow{w(FiveHour, five), w(Weekly, week)}}
	}
	node := func(id, email string, u *contracts.HarnessUsage) contracts.Node {
		return contracts.Node{NodeID: id, Name: id, Harnesses: []contracts.HarnessInfo{
			{Harness: contracts.HarnessAgy, Available: true, Login: &contracts.HarnessLogin{Email: email}, Usage: u},
			{Harness: contracts.HarnessClaude, Available: true}}}
	}
	nodes := []contracts.Node{
		node("node_synth-a", "synth-agy@example.invalid", reading(time.Minute, 10, 40)),
		node("node_synth-b", "Synth-Agy@example.invalid", reading(2*time.Minute, 12, 41)),
		node("node_synth-c", "synth-other@example.invalid", reading(time.Hour, 90, 90)),
		node("node_synth-d", "synth-third@example.invalid", reading(4*time.Hour, 70, 70)), // too old
	}
	cards := Cards(nil, nodes, now)
	got := Utilizations(cards, now)
	if len(got) != 2 || got[0].Provider != contracts.ProviderAnthropic || got[1].Provider != contracts.ProviderGoogle {
		t.Fatalf("providers: %+v", got)
	}
	if c := got[0]; c.Total != 0 || c.Weekly.Has {
		t.Fatalf("claude own login counted: %+v", c)
	}
	g := got[1]
	// a and b are one identity (newest reading: a); c's hour-old reading
	// counts for the week only.
	if g.Total != 3 || g.Current != 2 || g.FiveHour.N != 1 || g.FiveHour.Percent != 10 || g.Weekly.N != 2 || g.Weekly.Percent != 65 {
		t.Fatalf("agy: %+v", g)
	}
	for _, c := range cards {
		if c.Provider == contracts.ProviderGoogle && (c.Obs == nil || c.State != Ambient) {
			t.Fatalf("agy card without its reading: %+v", c)
		}
	}
	// A registered Google account that is the machines' own login carries
	// the reading on its own-login card.
	views := []contracts.AccountView{{Account: contracts.Account{AccountID: "acct_synth-g", Provider: contracts.ProviderGoogle, RotaRef: "synth-agy@example.invalid"}}}
	got = Utilizations(Cards(views, nodes[:1], now), now)
	if len(got) != 2 || got[1].Total != 1 || got[1].Current != 1 || got[1].Weekly.Percent != 40 {
		t.Fatalf("registered own login: %+v", got)
	}
}
