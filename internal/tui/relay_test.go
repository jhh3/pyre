package tui

import (
	"reflect"
	"testing"
)

// What tmux.AttachArgv builds: a set-option prefix before the attach.
func TestGridAttachWithCommandPrefix(t *testing.T) {
	d := []string{"/usr/bin/tmux", "-u", "-S", "/run/pyre/tmux.sock", "set-option", "-g", "mouse", "on", ";", "attach-session", "-t", "=pyre_run_x:", "-r"}
	tc, ok := parseTmuxAttach(d)
	if !ok {
		t.Fatal("prefixed tmux attach not recognised")
	}
	want := []string{"/usr/bin/tmux", "-u", "-S", "/run/pyre/tmux.sock", "attach-session", "-t", "=pyre_run_x:", "-r", "-f", "ignore-size"}
	if got := tc.attachCommand(true); !reflect.DeepEqual(got, want) {
		t.Fatalf("attach argv:\n got %q\nwant %q", got, want)
	}
	if p := tc.probeCommand(); p[4] != "display-message" {
		t.Fatalf("probe argv: %q", p)
	}
}

func TestRelayOpenBudget(t *testing.T) {
	m := &Model{}
	for i := 0; i < relayOpenBurst; i++ {
		if !m.relayOpenOK("node_a") {
			t.Fatalf("open %d refused within the burst", i)
		}
	}
	if m.relayOpenOK("node_a") {
		t.Fatal("open beyond the burst allowed")
	}
	if !m.relayOpenOK("node_b") {
		t.Fatal("the budget is per node")
	}
}
