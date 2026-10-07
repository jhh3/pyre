package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

// A token account is one row on "all machines"; a rota account on two
// machines is one row too (one row per identity).
func TestAccountsViewTokenAccountIsOneCard(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m := New(context.Background(), nil, Options{Keymap: DefaultKeymap(), StateFile: "none", NoColor: true, Clock: fixedClock(now)})
	defer m.Close()
	claude := []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Available: true}}
	nodes := []contracts.Node{{NodeID: "node_synth-linux", Name: "synth-linux", Harnesses: claude}, {NodeID: "node_synth-mac", Name: "synth-mac", Harnesses: claude}}
	views := []contracts.AccountView{
		{Account: contracts.Account{AccountID: "acct_synth-t", Provider: contracts.ProviderAnthropic, Label: "synth-tok"},
			Grants: []contracts.DeviceGrant{
				{GrantID: "grant_synth-1", NodeID: "node_synth-linux", Kind: contracts.CredOAuthToken, State: contracts.GrantValid},
				{GrantID: "grant_synth-2", NodeID: "node_synth-mac", Kind: contracts.CredOAuthToken, State: contracts.GrantValid},
			},
			ByNode: []contracts.AccountObservation{{ReportingNode: "node_synth-mac", ObservedAt: now, Confidence: "high", Eligibility: contracts.EligibleYes,
				Windows: []contracts.UsageWindow{{Name: "five_hour", UsedPercent: 12}}}},
			Assignments: []contracts.AccountAssignment{{SessionID: "ses_synth-1", NodeID: "node_synth-mac", AssignedAt: now.Add(-time.Minute)}}},
		{Account: contracts.Account{AccountID: "acct_synth-r", Provider: contracts.ProviderAnthropic, Label: "synth-rota"},
			Grants: []contracts.DeviceGrant{{NodeID: "node_synth-linux", Kind: contracts.CredRotaCheckout, State: contracts.GrantValid},
				{NodeID: "node_synth-mac", Kind: contracts.CredRotaCheckout, State: contracts.GrantValid}}},
	}
	m.loads["accounts"] = &loadState{val: views, have: true, at: now}
	m.loads["nodes"] = &loadState{val: nodes, have: true, at: now}
	m.mgmtList.sel = "acct_synth-t" // the token row is selected: its details show
	out := strings.Join(m.viewAccounts(160, 80), "\n")
	if n := strings.Count(out, "synth-tok"); n != 1 {
		t.Fatalf("token rows = %d, want 1:\n%s", n, out)
	}
	// One row per identity: the rota account is one row; its machines
	// show when it is selected.
	if n := strings.Count(out, "synth-rota"); n != 1 {
		t.Fatalf("rota rows = %d, want 1:\n%s", n, out)
	}
	var tokRow string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "synth-tok") {
			tokRow = l
		}
	}
	if !strings.Contains(tokRow, "token") || !strings.Contains(tokRow, "all machines") || !strings.Contains(tokRow, "1 session") {
		t.Fatalf("token row must say token, all machines and its session count:\n%s", out)
	}
	if !strings.Contains(out, "2 machines") {
		t.Fatalf("rota row must count its machines:\n%s", out)
	}
	// Machine affinity: the selected row says which machine holds the account.
	if !strings.Contains(out, "on synth-mac · since") {
		t.Fatalf("in-use machine missing:\n%s", out)
	}
	if strings.Contains(out, "no machine yet") || strings.Contains(out, "not managed") {
		t.Fatalf("unexpected group/ambient line:\n%s", out)
	}
}
