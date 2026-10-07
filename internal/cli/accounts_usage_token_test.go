package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// A token account with grants on every Claude machine (and a rota login
// kept as a fallback on one) prints as one row: "<label> · token" on "all
// machines"; a rota account next to it keeps one row per machine.
func TestPrintAccountsTokenAccountIsOneRow(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	claude := []c.HarnessInfo{{Harness: c.HarnessClaude, Available: true}}
	nodes := []c.Node{{NodeID: "node_synth-linux", Name: "synth-linux", Harnesses: claude}, {NodeID: "node_synth-mac", Name: "synth-mac", Harnesses: claude}}
	tok := c.AccountView{Account: c.Account{AccountID: "acct_synth-t", Provider: c.ProviderAnthropic, Label: "synth-tok"},
		Grants: []c.DeviceGrant{
			{GrantID: "grant_synth-1", NodeID: "node_synth-linux", Kind: c.CredOAuthToken, State: c.GrantValid},
			{GrantID: "grant_synth-2", NodeID: "node_synth-mac", Kind: c.CredOAuthToken, State: c.GrantValid},
			{GrantID: "grant_synth-3", NodeID: "node_synth-mac", Kind: c.CredRotaCheckout, State: c.GrantValid},
		},
		ByNode: []c.AccountObservation{{ReportingNode: "node_synth-mac", ObservedAt: now, Confidence: "high", Eligibility: c.EligibleYes,
			Windows: []c.UsageWindow{{Name: "five_hour", UsedPercent: 12}}}},
	}
	rota := c.AccountView{Account: c.Account{AccountID: "acct_synth-r", Provider: c.ProviderAnthropic, Label: "synth-rota"},
		Grants: []c.DeviceGrant{{NodeID: "node_synth-linux", Kind: c.CredRotaCheckout, State: c.GrantValid}, {NodeID: "node_synth-mac", Kind: c.CredRotaCheckout, State: c.GrantValid}}}
	var b bytes.Buffer
	printAccounts(&b, []c.AccountView{tok, rota}, nodes, now, nil)
	out := b.String()
	if n := strings.Count(out, "synth-tok"); n != 1 {
		t.Fatalf("token account rows = %d, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "synth-rota"); n != 2 {
		t.Fatalf("rota account rows = %d, want 2:\n%s", n, out)
	}
	var row string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "synth-tok") {
			row = l
		}
	}
	if !strings.Contains(row, "synth-tok · token") || !strings.Contains(row, "all machines") {
		t.Fatalf("token row: %q\n%s", row, out)
	}
	if strings.Contains(out, "not managed") {
		t.Fatalf("machines with grants must not get an ambient row:\n%s", out)
	}
}
