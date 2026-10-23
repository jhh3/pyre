package accounts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

func win(name string, used float64) contracts.UsageWindow {
	return contracts.UsageWindow{Name: name, UsedPercent: used}
}

// rank prefers the lowest binding window (the higher of 5-hour and weekly
// use), then the lower weekly use; an unreported window never counts as
// headroom.
func TestRankMostHeadroomBindingWindow(t *testing.T) {
	mk := func(id string, ws ...contracts.UsageWindow) *candidate {
		return &candidate{acct: contracts.Account{AccountID: id}, room: usage.HeadroomOf(ws)}
	}
	cs := []*candidate{
		mk("acct_synth-busy", win("five_hour", 60), win("seven_day", 5)),
		mk("acct_synth-weekly-high", win("five_hour", 12), win("seven_day", 90)),
		mk("acct_synth-best", win("five_hour", 12), win("seven_day", 40)),
		mk("acct_synth-no-windows"),
		// 12.4% rounds to 12: whole percents only.
		mk("acct_synth-fraction", win("five_hour", 12.4), win("seven_day", 50)),
		// The extra-usage budget is not a limit and does not rank.
		mk("acct_synth-extra", win("five_hour", 30), win("seven_day", 30), win("extra_usage", 100)),
		// A model-scoped weekly window counts as the weekly limit.
		mk("acct_synth-scoped", win("five_hour", 12), win("seven_day", 10), win("seven_day_opus", 70)),
	}
	rank(cs)
	var got []string
	for _, c := range cs {
		got = append(got, c.acct.AccountID)
	}
	want := []string{"acct_synth-extra", "acct_synth-best", "acct_synth-fraction", "acct_synth-busy", "acct_synth-scoped", "acct_synth-weekly-high", "acct_synth-no-windows"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rank\n got %v\nwant %v", got, want)
	}
	// Preference still comes first; ties fall to fewer active assignments.
	a := mk("acct_synth-a", win("five_hour", 50))
	b := mk("acct_synth-b", win("five_hour", 50))
	p := mk("acct_synth-p", win("five_hour", 90))
	a.active, b.active, a.pref, b.pref, p.pref = 2, 0, 1, 1, 0
	cs = []*candidate{a, b, p}
	rank(cs)
	if cs[0] != p || cs[1] != b || cs[2] != a {
		t.Fatalf("pref/active order: %s %s %s", cs[0].acct.AccountID, cs[1].acct.AccountID, cs[2].acct.AccountID)
	}
}

// End to end: automatic selection picks the account with the lowest binding
// window (an empty 5-hour window does not hide a spent week) and says why in
// plain words.
func TestSelectPrefersMostHeadroomAndExplains(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, l := range []string{"A", "B", "C"} {
		e.addAccount("acct_synth-"+strings.ToLower(l), contracts.HarnessClaude, l)
	}
	obs := func(id string, ws ...contracts.UsageWindow) {
		t.Helper()
		o := contracts.AccountObservation{AccountID: id, Provider: contracts.ProviderAnthropic, ReportingNode: "node_synth",
			ObservedAt: e.clock.Now().Add(-time.Minute), Source: "mock", Confidence: "high", Eligibility: contracts.EligibleYes, Windows: ws}
		if err := e.svc.IngestObservation(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	obs("acct_synth-a", win("five_hour", 12), win("seven_day", 80))
	obs("acct_synth-b", win("five_hour", 12), win("seven_day", 40))
	obs("acct_synth-c", win("five_hour", 3), win("seven_day", 99), win("extra_usage", 100))
	sel, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic, Policy: eligible})
	if err != nil || sel.AccountID != "acct_synth-b" {
		t.Fatalf("want B (C's week is 99%% spent despite its empty 5h window), got %+v %v", sel, err)
	}
	if sel.Reason != "most headroom (5h 12% used, weekly 40%)" {
		t.Fatalf("reason %q", sel.Reason)
	}
	// C with room in both windows wins (extra usage is not a limit).
	e.clock.Advance(time.Second)
	obs("acct_synth-c", win("five_hour", 3), win("seven_day", 30), win("extra_usage", 100))
	_, _, asg, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_1")
	if err != nil || asg.AccountID != "acct_synth-c" {
		t.Fatalf("want C, got %+v %v", asg, err)
	}
	if asg.Reason != "most headroom (5h 3% used, weekly 30%)" {
		t.Fatalf("assignment reason %q", asg.Reason)
	}
}

// An observation whose only full window is the extra-usage budget stays
// eligible (real usage-endpoint data: extra_usage 100%, five_hour 5%).
func TestExtraUsageIsNotALimit(t *testing.T) {
	e := newEnv(t)
	a := contracts.Account{AccountID: "acct_synth-x", Provider: contracts.ProviderAnthropic}
	o := e.svc.goodObs(a, []contracts.UsageWindow{win("five_hour", 5), win("seven_day", 28), win("extra_usage", 100)}, "")
	if o.Eligibility != contracts.EligibleYes {
		t.Fatalf("eligibility %s", o.Eligibility)
	}
	o = e.svc.goodObs(a, []contracts.UsageWindow{win("five_hour", 100), win("extra_usage", 0)}, "")
	if o.Eligibility != contracts.EligibleExhausted {
		t.Fatalf("five_hour at 100%% must be exhausted, got %s", o.Eligibility)
	}
}
