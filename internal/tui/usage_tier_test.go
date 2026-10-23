package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// A reserve account's row says "reserve"; selected, its details explain it.
func TestAccountsViewShowsReserveTier(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	m := New(context.Background(), nil, Options{Keymap: DefaultKeymap(), StateFile: "none", NoColor: true, Clock: fixedClock(now)})
	defer m.Close()
	claude := []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Available: true}}
	nodes := []contracts.Node{{NodeID: "node_synth-mac", Name: "synth-mac", Harnesses: claude}}
	grant := []contracts.DeviceGrant{{NodeID: "node_synth-mac", Kind: contracts.CredClaudeProfileStore, State: contracts.GrantValid}}
	views := []contracts.AccountView{
		{Account: contracts.Account{AccountID: "acct_synth-r", Provider: contracts.ProviderAnthropic, Label: "synth-spare", Tier: contracts.TierReserve}, Grants: grant},
		{Account: contracts.Account{AccountID: "acct_synth-n", Provider: contracts.ProviderAnthropic, Label: "synth-main"}, Grants: grant},
	}
	m.loads["accounts"] = &loadState{val: views, have: true, at: now}
	m.loads["nodes"] = &loadState{val: nodes, have: true, at: now}
	m.mgmtList.sel = "acct_synth-r"
	out := strings.Join(m.viewAccounts(160, 80), "\n")
	var spare, main string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(l, "synth-spare"):
			spare = l
		case strings.Contains(l, "synth-main"):
			main = l
		}
	}
	if !strings.Contains(spare, "reserve") || strings.Contains(main, "reserve") {
		t.Fatalf("rows:\n%s", out)
	}
	if !strings.Contains(out, "new work only when every normal account is near (80%) or at its limit") {
		t.Fatalf("details:\n%s", out)
	}
}
