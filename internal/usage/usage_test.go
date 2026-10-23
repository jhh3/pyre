package usage

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

var now = time.Date(2026, 10, 5, 14, 46, 0, 0, time.UTC) // a Monday

func w(name string, used float64, reset ...time.Time) contracts.UsageWindow {
	uw := contracts.UsageWindow{Name: name, UsedPercent: used}
	if len(reset) > 0 {
		uw.ResetAt = &reset[0]
	}
	return uw
}

func obs(el contracts.Eligibility, age time.Duration, ws ...contracts.UsageWindow) *contracts.AccountObservation {
	return &contracts.AccountObservation{AccountID: "acct_synth", ReportingNode: "node_synth-a", ObservedAt: now.Add(-age), Eligibility: el, Windows: ws}
}

func TestAssessStates(t *testing.T) {
	acct := contracts.Account{AccountID: "acct_synth", Label: "synth"}
	reset := now.Add(3 * time.Hour)
	expired := &contracts.DeviceGrant{State: contracts.GrantExpired, Detail: "refresh failed"}
	stale := obs(contracts.EligibleUnknown, time.Minute, w("five_hour", 0), w("seven_day", 97))
	stale.Stale, stale.Detail = true, "access token expired; usage probe never refreshes (the harness refreshes on its next use)"
	exhausted := obs(contracts.EligibleExhausted, time.Minute, w("seven_day", 100, reset))
	exhausted.ResetAt = &reset
	for _, tc := range []struct {
		name  string
		acct  contracts.Account
		obs   *contracts.AccountObservation
		grant *contracts.DeviceGrant
		want  State
		why   string
		last  bool
	}{
		{"healthy", acct, obs(contracts.EligibleYes, time.Minute, w("five_hour", 12), w("seven_day", 40)), nil, Healthy, "", false},
		{"near limit weekly", acct, obs(contracts.EligibleYes, time.Minute, w("five_hour", 5), w("seven_day", 89)), nil, NearLimit, "", false},
		{"near limit scoped weekly", acct, obs(contracts.EligibleYes, time.Minute, w("five_hour", 5), w("seven_day", 10), w("seven_day_opus", 85)), nil, NearLimit, "", false},
		{"extra usage is not a limit", acct, obs(contracts.EligibleYes, time.Minute, w("five_hour", 5), w("seven_day", 28), w("extra_usage", 100)), nil, Healthy, "", false},
		{"at limit until reset", acct, exhausted, nil, AtLimit, "limit reached", false},
		{"fresh window at 100", acct, obs(contracts.EligibleYes, time.Minute, w("five_hour", 100, reset)), nil, AtLimit, "limit reached", false},
		{"needs re-auth grant", acct, obs(contracts.EligibleYes, time.Minute, w("five_hour", 1)), expired, NeedsReauth, "login expired: refresh failed", false},
		{"needs re-auth unusable", acct, obs(contracts.EligibleUnusable, time.Minute), nil, NeedsReauth, "login unusable", false},
		{"token expired: near limit on use", acct, stale, nil, NearLimit, "refreshes on use", true},
		{"unknown old observation", acct, obs(contracts.EligibleYes, 40*time.Minute, w("five_hour", 1)), nil, Unknown, "last observed 40m ago", true},
		{"unknown none", acct, nil, nil, Unknown, "no usage observation yet", false},
		{"disabled", contracts.Account{Disabled: true}, nil, nil, Disabled, "disabled", false},
	} {
		got := Assess(tc.acct, tc.obs, tc.grant, now)
		if got.State != tc.want || got.Why != tc.why || got.LastKnown != tc.last {
			t.Errorf("%s: got %v %q last=%v, want %v %q last=%v", tc.name, got.State, got.Why, got.LastKnown, tc.want, tc.why, tc.last)
		}
	}
	if got := Assess(acct, exhausted, nil, now); got.Until == nil || !got.Until.Equal(reset) {
		t.Errorf("at limit until: %+v", got.Until)
	}
	// A reset that already passed is not "at limit" any more: unknown until re-observed.
	past := now.Add(-time.Minute)
	gone := obs(contracts.EligibleExhausted, time.Hour)
	gone.ResetAt = &past
	if got := Assess(acct, gone, nil, now); got.State != Unknown {
		t.Errorf("reset passed: %v", got.State)
	}
}

func TestCardsGroupPerNodeAndAmbient(t *testing.T) {
	nodes := []contracts.Node{
		{NodeID: "node_synth-b", Name: "synth-linux", Harnesses: []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Available: true}, {Harness: contracts.HarnessCodex, Available: true}}},
		{NodeID: "node_synth-a", Name: "synth-mac", Harnesses: []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Available: true}}},
		{NodeID: "node_synth-c", Name: "synth-mini", Harnesses: []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Available: true}, {Harness: contracts.HarnessCodex, Available: false}}},
	}
	two := contracts.AccountView{
		Account: contracts.Account{AccountID: "acct_synth-two", Provider: contracts.ProviderAnthropic, Label: "two"},
		ByNode: []contracts.AccountObservation{
			*obs(contracts.EligibleYes, time.Minute, w("five_hour", 5)),
			{AccountID: "acct_synth-two", ReportingNode: "node_synth-b", ObservedAt: now, Eligibility: contracts.EligibleUnknown, Stale: true},
		},
		Grants:      []contracts.DeviceGrant{{NodeID: "node_synth-a", State: contracts.GrantValid}, {NodeID: "node_synth-b", State: contracts.GrantValid}},
		Assignments: []contracts.AccountAssignment{{SessionID: "ses_synth-1", NodeID: "node_synth-a"}},
	}
	codex := contracts.AccountView{
		Account: contracts.Account{AccountID: "acct_synth-cx", Provider: contracts.ProviderOpenAI, Label: "cx"},
		Latest:  obs(contracts.EligibleYes, time.Minute, w("five_hour", 1)),
		Grants:  []contracts.DeviceGrant{{NodeID: "node_synth-a", State: contracts.GrantValid}},
	}
	cards := Cards([]contracts.AccountView{codex, two}, nodes, now)
	type row struct {
		p     contracts.Provider
		node  string
		label string
		state State
		asg   int
	}
	var got []row
	for _, c := range cards {
		got = append(got, row{c.Provider, c.NodeID, c.Label(), c.State, len(c.Assignments)})
	}
	want := []row{
		{contracts.ProviderAnthropic, "node_synth-b", "two", Unknown, 0},
		{contracts.ProviderAnthropic, "node_synth-a", "two", Healthy, 1},
		{contracts.ProviderAnthropic, "node_synth-c", "your own login (not managed)", Ambient, 0},
		{contracts.ProviderOpenAI, "node_synth-b", "your own login (not managed)", Ambient, 0},
		{contracts.ProviderOpenAI, "node_synth-a", "cx", Healthy, 0},
	}
	// Order is provider, then node NAME (synth-linux < synth-mac < synth-mini).
	if len(got) != len(want) {
		t.Fatalf("cards %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("card %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestPools(t *testing.T) {
	a := func(id string, st State) Card {
		return Card{Provider: contracts.ProviderAnthropic, Account: &contracts.Account{AccountID: id}, Assessment: Assessment{State: st}}
	}
	o := func(id string, st State) Card {
		c := a(id, st)
		c.Provider = contracts.ProviderOpenAI
		return c
	}
	cards := []Card{
		a("x", Healthy), a("x", Unknown), // one account on two nodes counts once
		a("y", NearLimit), a("z", AtLimit), a("d", Disabled),
		{Provider: contracts.ProviderAnthropic, Assessment: Assessment{State: Ambient}},
		o("n", NearLimit),
	}
	ps := Pools(cards)
	if len(ps) != 2 {
		t.Fatalf("%+v", ps)
	}
	if p := ps[0]; p.Provider != contracts.ProviderAnthropic || p.Ready != 1 || p.Near != 1 || p.AtLimit != 1 || p.Total != 3 || p.Tight() {
		t.Errorf("anthropic %+v", p)
	}
	if got := ps[0].Summary(); got != "1 ready · 2 near or at limit" {
		t.Errorf("summary %q", got)
	}
	// A near-limit login is still available: calm, not tight.
	if p := ps[1]; p.Ready != 0 || p.Near != 1 || p.Total != 1 || p.Tight() || p.Brief() != "0 ready · 1 near limit" {
		t.Errorf("openai %+v %q", p, p.Brief())
	}
	if p := Pools([]Card{a("z", AtLimit), a("r", NeedsReauth), a("u", Unknown)})[0]; p.Available() != 0 || !p.Tight() ||
		p.Summary() != "0 ready · 1 at limit · 1 needs re-auth · 1 unknown" || p.Brief() != "0 ready · 1 at limit" {
		t.Errorf("exhausted %+v %q %q", p, p.Summary(), p.Brief())
	}
}

func TestTimeWords(t *testing.T) {
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(2*time.Hour + 14*time.Minute), "resets 5:00 PM · in 2h 14m"},
		{now.Add(14 * time.Minute), "resets 3:00 PM · in 14m"},
		{time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC), "resets Thu 8:00 PM · in 3d 5h"},
		{time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), "resets Oct 20 9:00 AM · in 14d 18h"},
		{now.Add(-time.Minute), "reset passed"},
	} {
		if got := Resets(now, tc.t); got != tc.want {
			t.Errorf("Resets(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestHeadroomWords(t *testing.T) {
	h := HeadroomOf([]contracts.UsageWindow{w("five_hour", 12), w("seven_day", 40), w("extra_usage", 100)})
	if got := Explain(h, 3); got != "most headroom (5h 12% used, weekly 40%)" {
		t.Errorf("explain %q", got)
	}
	if got := Explain(h, 1); got != "only eligible account (5h 12% used, weekly 40%)" {
		t.Errorf("explain one %q", got)
	}
	if got := HeadroomOf(nil).Numbers(); got != "no usage windows reported" {
		t.Errorf("none %q", got)
	}
	if got := HeadroomOf([]contracts.UsageWindow{w("seven_day", 7)}).Numbers(); got != "weekly 7% used" {
		t.Errorf("weekly only %q", got)
	}
}

// A node where agy is installed shows one "your own login" card under
// google (agy has no Pyre-managed logins); nodes without agy show none.
func TestCardsAgyOwnLogin(t *testing.T) {
	nodes := []contracts.Node{
		{NodeID: "node_synth-b", Name: "synth-linux", Harnesses: []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Available: true}, {Harness: contracts.HarnessAgy, Available: true}}},
		{NodeID: "node_synth-a", Name: "synth-mac", Harnesses: []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Available: true}}},
	}
	var google []Card
	for _, c := range Cards(nil, nodes, now) {
		if c.Provider == contracts.ProviderGoogle {
			google = append(google, c)
		}
	}
	if len(google) != 1 || google[0].NodeID != "node_synth-b" || google[0].State != Ambient || google[0].Label() != "your own login (not managed)" {
		t.Fatalf("google cards %+v", google)
	}
}
