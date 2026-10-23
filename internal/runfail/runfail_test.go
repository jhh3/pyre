package runfail

import (
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

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

// Capacity singles out "every account is limited; wait for the reset"
// (grouped per provider in the inbox), never a rejected login.
func TestCapacity(t *testing.T) {
	cases := []struct {
		reason string
		prov   contracts.Provider
		ok     bool
	}{
		{"launch failed: no eligible openai subscription on this node: acct_01SYNTHX: cooldown until 2026-10-12T20:00:00Z", contracts.ProviderOpenAI, true},
		{"launch failed: no eligible anthropic subscription on this node: acct_01SYNTHX: usage limit reached", contracts.ProviderAnthropic, true},
		{"launch failed: no eligible subscription: all exhausted", "", true},
		{"launch failed: no eligible openai subscription on this node: acct_01SYNTHX: 401 Unauthorized (cooldown)", "", false},
		{"launch failed: checkout missing", "", false},
		{"claude exited with an API error", "", false},
	}
	for _, c := range cases {
		if prov, ok := Capacity(c.reason); prov != c.prov || ok != c.ok {
			t.Errorf("Capacity(%q) = %q %v, want %q %v", c.reason, prov, ok, c.prov, c.ok)
		}
	}
}

// Runs that failed the same way have one cause, whatever their IDs, paths,
// numbers and times (shapes from the live hub, 2026-10-03..17); different
// failures do not.
func TestCauseOf(t *testing.T) {
	one := 1
	same := [][]string{
		{`launch failed: working directory "/home/synth/wt/app-desktop" does not exist on this node`,
			`launch failed: working directory "/tmp/claude-1000/synth/scratchpad/feedback-monitor" does not exist on this node`,
			`launch failed: working directory "/home/synth/opening-scale 2793" does not exist on this node`},
		{"launch unsupported: native resume unsupported for run run_01SYNTHA and handoff not allowed: unsupported: no native thread to resume on this account",
			"launch unsupported: native resume unsupported for run run_01SYNTHB and handoff not allowed: unsupported: no native thread to resume on this account"},
		{"launch failed: command expired at 2026-10-15T04:04:33Z (hub clock) before the node could start it; not executed",
			"launch failed: command expired at 2026-10-15T05:54:52Z (hub clock) before the node could start it; not executed"},
		{`launch failed: permissions bypass: the owner never accepted Claude's bypass-permissions warning on this node (no skipDangerousModePermissionPrompt in C:\Users\synth\.claude.json)`,
			`launch failed: permissions bypass: the owner never accepted Claude's bypass-permissions warning on this node (no skipDangerousModePermissionPrompt in C:\Users\other\.claude.json)`},
		{"launch failed: no eligible openai subscription on this node: acct_01SYNTHX: verified at checkout: availability unknown: wham/usage failed: 401 Unauthorized",
			"launch failed: no eligible openai subscription on this node: acct_01SYNTHY: 401 Unauthorized"},
		{"launch failed: tmux exploded at 12:01 in /tmp/a", "launch failed: tmux exploded at 13:44 in /var/b"},
		{"SYNTH crash after 120 s in ses_01SYNTHA", "SYNTH crash after 7 s in ses_01SYNTHB"},
	}
	seen := map[string]bool{}
	for _, group := range same {
		key := CauseOf(group[0], nil).Key
		for _, r := range group[1:] {
			if k := CauseOf(r, nil).Key; k != key {
				t.Errorf("CauseOf(%q) = %q, want %q (as %q)", r, k, key, group[0])
			}
		}
		if seen[key] {
			t.Errorf("cause %q shared by different failures", key)
		}
		seen[key] = true
	}
	cases := []struct {
		reason string
		exit   *int
		key    string
		label  string
	}{
		{`launch failed: working directory "/x" does not exist on this node`, nil, "start.cwd-missing", "working folder missing"},
		{"launch failed: working directory is required", nil, "start.cwd-required", "no working folder given"},
		{"launch failed: no eligible openai subscription on this node: acct_01SYNTHX: exhausted until 2026-10-16T15:29:23Z", nil, "start.capacity-codex", "no Codex account with capacity"},
		{"launch failed: no eligible anthropic subscription on this node: ", nil, "start.no-account-claude", "no usable Claude account"},
		{"launch interrupted (pyred restarted before the harness started); nothing ran — relaunch to retry", nil, "start.interrupted", "pyred restarted before it started"},
		{"launch failed: thread start/resume outcome unknown: codex app-server connection lost (before send)", nil, "start.connection-lost", "lost the harness while starting"},
		{"claude exited with an API error", &one, "run.exit-1", "exit code 1"},
	}
	for _, c := range cases {
		got := CauseOf(c.reason, c.exit)
		if got.Key != c.key || CauseLabel(got.Key) != c.label || got.Launch != CauseLaunch(c.key) {
			t.Errorf("CauseOf(%q) = %+v %q, want %q %q", c.reason, got, CauseLabel(got.Key), c.key, c.label)
		}
	}
	if k := CauseOf("launch failed: SYNTH-SECRET-BODY-TEXT", nil).Key; !strings.HasPrefix(k, "start.other-") || CauseLabel(k) != "" || strings.Contains(strings.ToLower(k), "secret") {
		t.Errorf("unrecognised: %q", k)
	}
}

func TestAgyHarnessNamed(t *testing.T) {
	if got := harnessCode(`launch failed: harness "agy" unavailable on this node: agy not installed`); got != "agy" {
		t.Fatalf("harness %q", got)
	}
	if got := harnessCode("a legacy strategy failed"); got != "any" {
		t.Fatalf("substring matched: %q", got)
	}
}
