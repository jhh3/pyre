package tui

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

var updateTopbar = flag.Bool("update", false, "rewrite internal/tui/testdata goldens")

// topbarModel is a fleet of three providers: Claude accounts with 5-hour
// and weekly windows (one stale, one at limit), Codex accounts that report
// only the week, and agy's own login (its own /usage report).
func topbarModel(t *testing.T) *Model {
	t.Helper()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m := New(context.Background(), nil, Options{Keymap: DefaultKeymap(), StateFile: "none", NoColor: true, Clock: fixedClock(now)})
	t.Cleanup(m.Close)
	hs := []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Available: true}, {Harness: contracts.HarnessCodex, Available: true}, {Harness: contracts.HarnessAgy, Available: true,
		Usage: &contracts.HarnessUsage{ObservedAt: now.Add(-2 * time.Minute), Source: "agy:/usage",
			Windows: []contracts.UsageWindow{{Name: "five_hour", UsedPercent: 8}, {Name: "seven_day", UsedPercent: 62}}}}}
	nodes := []contracts.Node{{NodeID: "node_synth-a", Name: "synth-a", Harnesses: hs}}
	acct := func(id string, p contracts.Provider, age time.Duration, el contracts.Eligibility, ws ...contracts.UsageWindow) contracts.AccountView {
		v := contracts.AccountView{Account: contracts.Account{AccountID: id, Provider: p, Label: id},
			Grants: []contracts.DeviceGrant{{NodeID: "node_synth-a", Kind: contracts.CredRotaCheckout, State: contracts.GrantValid}}}
		if ws != nil {
			v.ByNode = []contracts.AccountObservation{{AccountID: id, ReportingNode: "node_synth-a", ObservedAt: now.Add(-age), Eligibility: el, Windows: ws}}
		}
		return v
	}
	uw := func(name string, pct float64) contracts.UsageWindow {
		return contracts.UsageWindow{Name: name, UsedPercent: pct}
	}
	views := []contracts.AccountView{
		acct("acct_synth-c1", contracts.ProviderAnthropic, time.Minute, contracts.EligibleYes, uw("five_hour", 12), uw("seven_day", 40)),
		acct("acct_synth-c2", contracts.ProviderAnthropic, time.Minute, contracts.EligibleYes, uw("five_hour", 30), uw("seven_day", 70)),
		acct("acct_synth-c3", contracts.ProviderAnthropic, time.Minute, contracts.EligibleExhausted, uw("five_hour", 100), uw("seven_day", 55)),
		acct("acct_synth-c4", contracts.ProviderAnthropic, 2*time.Hour, contracts.EligibleYes, uw("five_hour", 90), uw("seven_day", 90)), // week only
		acct("acct_synth-c5", contracts.ProviderAnthropic, 5*time.Hour, contracts.EligibleYes, uw("five_hour", 10), uw("seven_day", 10)), // too old
		acct("acct_synth-x1", contracts.ProviderOpenAI, time.Minute, contracts.EligibleYes, uw("seven_day", 20)),
		acct("acct_synth-x2", contracts.ProviderOpenAI, time.Minute, contracts.EligibleYes, uw("seven_day", 40)),
	}
	m.loads["accounts"] = &loadState{val: views, have: true, at: now}
	m.loads["nodes"] = &loadState{val: nodes, have: true, at: now}
	m.loads["sessions"] = &loadState{val: []contracts.SessionView{}, have: true, at: now}
	return m
}

// The header's fleet utilization at widths from wide to narrow: the
// 5-hour window goes first, then the names abbreviate, then it drops.
func TestTopBarUtilizationGolden(t *testing.T) {
	m := topbarModel(t)
	for _, tc := range []struct {
		w      int
		filter string
	}{{200, ""}, {150, ""}, {130, ""}, {110, ""}, {80, ""}, {150, "synth"}} {
		w, name := tc.w, fmt.Sprint(tc.w)
		if tc.filter != "" {
			name += "-filter" // the 5-hour window goes before the filter tag
		}
		t.Run(name, func(t *testing.T) {
			m.filter = tc.filter
			defer func() { m.filter = "" }()
			got := strings.TrimRight(ansiStrip(m.viewHeader(w)), " ") + "\n"
			if n := width(strings.TrimRight(got, "\n")); n > w {
				t.Fatalf("header is %d cells at width %d", n, w)
			}
			path := filepath.Join("testdata", "topbar-"+name+".txt")
			if *updateTopbar {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden (go test ./internal/tui -run TestTopBar -update): %v", err)
			}
			if string(want) != got {
				t.Errorf("width %d\n--- want\n%s--- got\n%s", w, want, got)
			}
		})
	}
}

// Claude's 5-hour mean is over the three current accounts (12, 30, 100,
// so a ~: c4's reading is two hours old), its week over those and c4
// (40, 70, 55, 90); c5 (five hours old) is the missing one of five, and
// c3 is at its limit (!1). Codex has no 5-hour window; agy is its own
// report.
func TestTopBarUtilizationNumbers(t *testing.T) {
	m := topbarModel(t)
	got := ansiStrip(m.utilSummaries()[utilFull])
	want := "claude 5h 47%~ · wk 64% (4/5) !1   codex wk 30%   agy 5h 8% · wk 62%"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if s := ansiStrip(m.utilSummaries()[utilShort]); s != "cl 64%~!1   cx 30%   agy 62%" {
		t.Fatalf("short: %q", s)
	}
}

// The Accounts view shows agy's own reading on its own-login line.
func TestAccountsViewAgyReading(t *testing.T) {
	m := topbarModel(t)
	out := ansiStrip(strings.Join(m.viewAccounts(160, 80), "\n"))
	if !strings.Contains(out, "○ your own login on synth-a (not managed) · 5h 8% · week 62% · read 2m ago via agy:/usage") {
		t.Fatalf("agy reading missing:\n%s", out)
	}
}
