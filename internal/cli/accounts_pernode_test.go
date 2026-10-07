package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// An account with logins on two nodes prints one row per node, so one
// node's "unknown" does not hide another node's state; each row has 5h and
// weekly bars with reset times, and the owner's own login on a node
// without Pyre accounts is listed as not managed.
func TestPrintAccountsPerNodeWithBars(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 46, 0, 0, time.UTC)
	reset5 := now.Add(2*time.Hour + 14*time.Minute)
	resetW := now.Add(3*24*time.Hour + 5*time.Hour)
	nodes := []c.Node{
		{NodeID: "node_synth-linux", Name: "synth-linux"},
		{NodeID: "node_synth-mac", Name: "synth-mac"},
		{NodeID: "node_synth-mini", Name: "synth-mini", Harnesses: []c.HarnessInfo{{Harness: c.HarnessClaude, Available: true}}},
	}
	av := c.AccountView{Account: c.Account{AccountID: "acct_synth-j", Provider: c.ProviderAnthropic, Label: "synthetic-j"},
		ByNode: []c.AccountObservation{
			{ReportingNode: "node_synth-linux", ObservedAt: now, Confidence: "high", Eligibility: c.EligibleYes,
				Windows: []c.UsageWindow{{Name: "five_hour", UsedPercent: 12, ResetAt: &reset5}, {Name: "seven_day", UsedPercent: 87, ResetAt: &resetW}}},
			{ReportingNode: "node_synth-mac", ObservedAt: now, Confidence: "low", Eligibility: c.EligibleUnknown, Stale: true,
				Detail: "access token expired; usage probe never refreshes", Windows: []c.UsageWindow{{Name: "seven_day", UsedPercent: 40}}},
		},
		Grants:      []c.DeviceGrant{{NodeID: "node_synth-linux", State: c.GrantValid}, {NodeID: "node_synth-mac", State: c.GrantValid}},
		Assignments: []c.AccountAssignment{{SessionID: "ses_synth-1", NodeID: "node_synth-linux"}},
	}
	av.Latest = &av.ByNode[1]
	var b bytes.Buffer
	printAccounts(&b, []c.AccountView{av}, nodes, now, func(id string) string { return "synthetic task" })
	out := b.String()
	for _, want := range []string{
		"ANTHROPIC · 1 ready\n", // the expired-at-rest login has room: available, calm
		"ACCOUNT", "5H", "WEEK", "RESETS",
		"synth-linux", "near limit", "█░░░░░░░░░  12%", "5:00 PM (in 2h 14m)", "█████████░  87%", "Thu 7:46 PM (in 3d 5h)", "in use by synthetic task on synth-linux",
		"synth-mac", "ready · refreshes on use", "(40%)",
		"your own login (not managed)", "synth-mini", "not managed ", "not observe your own login",
		"(n%) is the last known value",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "acct_synth") || strings.Contains(out, "node_synth") {
		t.Fatalf("IDs leak into the human table:\n%s", out)
	}
	b.Reset()
	printAccounts(&b, nil, nil, now, nil)
	if !strings.Contains(b.String(), "no accounts configured") {
		t.Fatalf("empty: %s", b.String())
	}
}
