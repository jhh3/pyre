package tui

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestSessionAttentionMapping(t *testing.T) {
	flag := func(level, reason string) *contracts.SessionFlag {
		return &contracts.SessionFlag{Level: level, Reason: reason, Count: 1}
	}
	for _, c := range []struct {
		name          string
		sv            contracts.SessionView
		level, reason string
	}{
		{"nothing", contracts.SessionView{}, "", ""},
		{"old hub: open items, no flag", contracts.SessionView{Attention: 2}, contracts.FlagNeedsYou, "attention"},
		{"approval", contracts.SessionView{Attention: 1, Flag: flag(contracts.FlagNeedsYou, "approval")}, contracts.FlagNeedsYou, "approval"},
		{"asked", contracts.SessionView{Attention: 1, Flag: flag(contracts.FlagNeedsYou, "asked")}, contracts.FlagNeedsYou, "asked"},
		{"message only", contracts.SessionView{Attention: 1, Flag: flag(contracts.FlagMessage, "message")}, contracts.FlagMessage, "message"},
		{"flag wins over count", contracts.SessionView{Attention: 3, Flag: flag(contracts.FlagMessage, "comment")}, contracts.FlagMessage, "comment"},
	} {
		level, reason := sessionAttention(c.sv)
		if level != c.level || reason != c.reason {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, level, reason, c.level, c.reason)
		}
		if got := sessionNeedsYou(c.sv); got != (c.level == contracts.FlagNeedsYou) {
			t.Errorf("%s: sessionNeedsYou = %v", c.name, got)
		}
	}
}

func TestListStatusFlagLabels(t *testing.T) {
	run := func(st contracts.RunState) *contracts.Run { return &contracts.Run{RunID: "run_synth", State: st} }
	msg := &contracts.SessionFlag{Level: contracts.FlagMessage, Reason: "message", Count: 1, Messages: 1}
	asked := &contracts.SessionFlag{Level: contracts.FlagNeedsYou, Reason: "asked", Count: 1}
	online := &contracts.Node{Connection: contracts.NodeConnection{State: contracts.ConnConnected}}
	offline := &contracts.Node{Connection: contracts.NodeConnection{State: contracts.ConnDisconnected}}
	for _, c := range []struct {
		name string
		sv   contracts.SessionView
		node *contracts.Node
		want string
	}{
		{"idle with a message reads message, not needs you", contracts.SessionView{Run: run(contracts.RunWaitingInput), Attention: 1, Flag: msg}, online, "✉ message"},
		{"working with a message", contracts.SessionView{Run: run(contracts.RunRunning), Attention: 1, Flag: msg}, online, "✉ message"},
		{"asked for you", contracts.SessionView{Run: run(contracts.RunWaitingInput), Attention: 1, Flag: asked}, online, "! needs you"},
		{"approval keeps its own word", contracts.SessionView{Run: run(contracts.RunWaitingApproval), Attention: 1, Flag: msg}, online, "! approval"},
		{"failed keeps its own word", contracts.SessionView{Run: run(contracts.RunFailed), Attention: 1, Flag: msg}, online, "✗ failed"},
		{"offline wins", contracts.SessionView{Run: run(contracts.RunRunning), Attention: 1, Flag: asked}, offline, "◌ offline"},
		{"nothing", contracts.SessionView{Run: run(contracts.RunWaitingInput)}, online, "○ idle"},
	} {
		st := listStatus(c.sv, c.node)
		if got := st.sym + " " + st.label; got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if c.want == "✉ message" && (st.tone == toneWarn || st.tone == toneBad) {
			t.Errorf("%s: a message must not be amber/red", c.name)
		}
		if c.want == "✉ message" && gridNeedsYou(c.sv, c.node) {
			t.Errorf("%s: a message alone must not count as needing you", c.name)
		}
	}
}

func TestFlagCounts(t *testing.T) {
	ss := []contracts.SessionView{
		{Flag: &contracts.SessionFlag{Level: contracts.FlagNeedsYou, Reason: "approval", Count: 2, Messages: 1}},
		{Flag: &contracts.SessionFlag{Level: contracts.FlagMessage, Reason: "message", Count: 2, Messages: 2}},
		{Session: contracts.Session{Archived: true}, Flag: &contracts.SessionFlag{Level: contracts.FlagNeedsYou, Reason: "failed", Count: 1}},
		{},
	}
	needs, msgs := flagCounts(ss)
	if needs != 1 || msgs != 3 {
		t.Fatalf("needs=%d messages=%d, want 1 and 3", needs, msgs)
	}
}
