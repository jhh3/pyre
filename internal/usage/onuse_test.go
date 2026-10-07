package usage

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

const (
	expiredDetail = "access token expired; usage probe never refreshes (the harness refreshes on its next use)"
	codexDetail   = "codex rota account not checked out; usage needs its app-server (unknown until checked out)"
)

func onUseObs(detail string, ws ...contracts.UsageWindow) *contracts.AccountObservation {
	o := obs(contracts.EligibleUnknown, 30*time.Minute, ws...)
	o.Stale, o.Detail = true, detail
	return o
}

// The owner's fleet: logins whose token expired at rest (or Codex usage
// readable only once checked out) are available when their last-known
// windows have headroom, near limit when close, at limit only when a
// limit's reset is still ahead.
func TestAssessOnUse(t *testing.T) {
	acct := contracts.Account{AccountID: "acct_synth"}
	ahead, passed := now.Add(2*time.Hour), now.Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		obs   *contracts.AccountObservation
		want  State
		label string
	}{
		{"weekly 2%", onUseObs(expiredDetail, w("five_hour", 0), w("seven_day", 2), w("extra_usage", 89)), Healthy, "ready · refreshes on use"},
		{"weekly 28%, extra spent", onUseObs(expiredDetail, w("five_hour", 5), w("seven_day", 28), w("extra_usage", 100)), Healthy, "ready · refreshes on use"},
		{"no windows yet", onUseObs(expiredDetail), Healthy, "ready · refreshes on use"},
		{"codex not checked out", onUseObs(codexDetail, w("five_hour", 5), w("seven_day", 10)), Healthy, "ready · refreshes on use"},
		{"codex not checked out, no windows", onUseObs(codexDetail), Healthy, "ready · refreshes on use"},
		{"codex known limited", onUseObs(codexDetail, w("five_hour", 100, ahead)), AtLimit, "at limit"},
		{"weekly 97%", onUseObs(expiredDetail, w("seven_day", 97)), NearLimit, "near limit · refreshes on use"},
		{"weekly 100% until reset", onUseObs(expiredDetail, w("seven_day", 100, ahead)), AtLimit, "at limit"},
		{"weekly 100% reset passed", onUseObs(expiredDetail, w("seven_day", 100, passed)), Healthy, "ready · refreshes on use"},
	} {
		got := Assess(acct, tc.obs, nil, now)
		if got.State != tc.want || got.Label() != tc.label || got.Available() != (tc.want != AtLimit) {
			t.Errorf("%s: got %v %q available=%v, want %v %q", tc.name, got.State, got.Label(), got.Available(), tc.want, tc.label)
		}
	}
	// Another reason for unknown stays unknown.
	if got := Assess(acct, onUseObs("probe failed"), nil, now); got.State != Unknown {
		t.Errorf("other unknown: %v", got.State)
	}
}

func TestJudgeRanksAndLimits(t *testing.T) {
	ahead := now.Add(time.Hour)
	lk := Judge([]contracts.UsageWindow{w("five_hour", 5), w("seven_day", 100, ahead), w("extra_usage", 100)}, now)
	if !lk.Limited || lk.Until == nil || !lk.Until.Equal(ahead) {
		t.Errorf("limited %+v", lk)
	}
	if lk := Judge([]contracts.UsageWindow{w("extra_usage", 100)}, now); lk.Limited || lk.Near {
		t.Errorf("extra usage is not a limit: %+v", lk)
	}
	a := Judge([]contracts.UsageWindow{w("five_hour", 0), w("seven_day", 2)}, now).Room
	b := Judge([]contracts.UsageWindow{w("five_hour", 0), w("seven_day", 97)}, now).Room
	if a.Compare(b) >= 0 {
		t.Errorf("2%% weekly should rank before 97%%")
	}
}

// One account on two machines: the machine without windows shows the
// newest windows another machine saw, and the account counts once.
func TestCardsBorrowWindowsAndPoolCounts(t *testing.T) {
	mk := func(id string, ws ...contracts.UsageWindow) contracts.AccountView {
		o := *onUseObs(expiredDetail, ws...)
		o.AccountID = id
		return contracts.AccountView{Account: contracts.Account{AccountID: id, Provider: contracts.ProviderAnthropic, Label: id},
			ByNode: []contracts.AccountObservation{o}, Grants: []contracts.DeviceGrant{{NodeID: o.ReportingNode, State: contracts.GrantValid}}}
	}
	shared := mk("acct_synth-shared", w("seven_day", 89))
	other := *onUseObs(expiredDetail)
	other.ReportingNode = "node_synth-b"
	shared.ByNode = append(shared.ByNode, other)
	shared.Grants = append(shared.Grants, contracts.DeviceGrant{NodeID: "node_synth-b", State: contracts.GrantValid})
	views := []contracts.AccountView{
		mk("acct_synth-low", w("seven_day", 2)),
		mk("acct_synth-mid", w("seven_day", 28)),
		mk("acct_synth-full", w("seven_day", 100, now.Add(time.Hour))),
		shared,
	}
	cards := Cards(views, nil, now)
	for _, c := range cards {
		if c.Account.AccountID == "acct_synth-shared" && c.NodeID == "node_synth-b" {
			if c.Obs == nil || len(c.Obs.Windows) != 1 || c.State != NearLimit || !c.LastKnown {
				t.Errorf("borrowed: %+v", c)
			}
		}
	}
	p := Pools(cards)[0]
	if p.Summary() != "2 ready · 2 near or at limit" || p.Tight() || p.Warn() {
		t.Errorf("pool %+v %q", p, p.Summary())
	}
}

// Amber only when every available account is near its limit; red only
// when none is available.
func TestPoolTone(t *testing.T) {
	for _, tc := range []struct {
		p         Pool
		warn, bad bool
		brief     string
	}{
		{Pool{Ready: 3, Near: 5, Total: 8}, false, false, "3 ready · 5 near limit"},
		{Pool{Near: 2, AtLimit: 1, Total: 3}, true, false, "0 ready · 3 near or at limit"},
		{Pool{AtLimit: 1, Reauth: 1, Total: 2}, false, true, "0 ready · 1 at limit"},
		{Pool{Ready: 1, Unknown: 2, Total: 3}, false, false, "1 ready"},
	} {
		if tc.p.Warn() != tc.warn || tc.p.Tight() != tc.bad || tc.p.Brief() != tc.brief {
			t.Errorf("%+v: warn=%v bad=%v brief=%q", tc.p, tc.p.Warn(), tc.p.Tight(), tc.p.Brief())
		}
	}
}
