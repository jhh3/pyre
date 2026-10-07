package runfail

import "testing"

func TestPlain(t *testing.T) {
	names := Names{Account: func(id string) string {
		if id == "acct_01SYNTHX" {
			return "synth-x"
		}
		return ""
	}, Node: "node-a"}
	cases := []struct {
		reason  string
		started bool
		want    string
	}{
		{"launch failed: no eligible openai subscription on this node: acct_01SYNTHX: verified at checkout: availability unknown: account/rateLimits/read failed: wham/usage failed: 401 Unauthorized", false,
			"Couldn't start: synth-x's Codex login on node-a needs re-auth (401). Fix: pyre accounts login --node node-a"},
		{"no eligible openai subscription on this node: acct_01SYNTHX: cooldown until 2026-10-06T10:00:00Z (usage)", false,
			"Couldn't start: no Codex account with capacity right now; it can resume after the reset"},
		{"no eligible anthropic subscription on this node: grant changed", false,
			"Couldn't start: no usable Claude account on node-a (grant changed)"},
		{"process exited with code 2", true, "Failed: process exited with code 2"},
		{"launch failed: tmux: no server", false, "Couldn't start: tmux: no server"},
		{"", false, ""},
	}
	for _, c := range cases {
		if got := Plain(c.reason, c.started, names); got != c.want {
			t.Errorf("Plain(%q)\n got %q\nwant %q", c.reason, got, c.want)
		}
	}
}

func TestSummaryNeverCopiesUnknownText(t *testing.T) {
	if got := Summary("SYNTH-SECRET-BODY-TEXT", true, Names{}); got != "" {
		t.Fatalf("%q", got)
	}
	if got := Summary("launch failed: SYNTH-SECRET-BODY-TEXT", false, Names{}); got != "Couldn't start; open the session for the reason." {
		t.Fatalf("%q", got)
	}
	if got := Summary("launch failed: no eligible openai subscription on this node: acct_01SYNTHX: wham/usage failed: 401 Unauthorized", false, Names{Node: "node-a"}); got !=
		"Couldn't start: acct_01SYNTHX's Codex login on node-a needs re-auth (401). Fix: pyre accounts login --node node-a" {
		t.Fatalf("%q", got)
	}
}
