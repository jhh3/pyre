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

// A failure or exhausted-account flag counts while the session is live;
// once it has ended (or is archived) it is history: shown, not counted.
func TestNeedsYouIgnoresEndedFailures(t *testing.T) {
	run := func(st contracts.RunState) *contracts.Run { return &contracts.Run{RunID: "run_synth", State: st} }
	flag := func(reason string) *contracts.SessionFlag {
		return &contracts.SessionFlag{Level: contracts.FlagNeedsYou, Reason: reason, Count: 1}
	}
	online := &contracts.Node{Connection: contracts.NodeConnection{State: contracts.ConnConnected}}
	for _, c := range []struct {
		name string
		sv   contracts.SessionView
		want bool
		row  string
	}{
		{"ended launch failure", contracts.SessionView{Run: run(contracts.RunExited), Flag: flag("failed")}, false, "✗ failed"},
		{"failed run, failure item", contracts.SessionView{Run: run(contracts.RunFailed), Flag: flag("failed")}, false, "✗ failed"},
		{"failed run, nothing open", contracts.SessionView{Run: run(contracts.RunFailed)}, false, "✗ failed"},
		{"never ran, failure item", contracts.SessionView{Flag: flag("failed")}, false, "✗ failed"},
		{"ended, account exhausted", contracts.SessionView{Run: run(contracts.RunExited), Flag: flag("reauth")}, false, "⊘ re-auth"},
		{"archived with an ask", contracts.SessionView{Session: contracts.Session{Archived: true}, Run: run(contracts.RunExited), Flag: flag("ask")}, false, ""},
		{"live, failure from an earlier run", contracts.SessionView{Run: run(contracts.RunWaitingInput), Flag: flag("failed")}, true, "! needs you"},
		{"live, account exhausted", contracts.SessionView{Run: run(contracts.RunRunning), Flag: flag("reauth")}, true, "! needs you"},
		{"ended, still asking you", contracts.SessionView{Run: run(contracts.RunExited), Flag: flag("asked")}, true, "! needs you"},
		{"waiting on approval", contracts.SessionView{Run: run(contracts.RunWaitingApproval)}, true, "! approval"},
	} {
		if got := needsYou(c.sv, online); got != c.want {
			t.Errorf("%s: needsYou = %v, want %v", c.name, got, c.want)
		}
		if c.row == "" {
			continue
		}
		if st := listStatus(c.sv, online); st.sym+" "+st.label != c.row {
			t.Errorf("%s: row reads %q, want %q", c.name, st.sym+" "+st.label, c.row)
		}
	}
}
