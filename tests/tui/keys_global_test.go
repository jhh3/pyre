package tui_test

import "testing"

func TestMAndAOpenTabsFromSessions(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	h.Key("M")
	h.Settle()
	if h.M.ActiveView() != "Machines" {
		t.Fatalf("M: view %s", h.M.ActiveView())
	}
	h.Key("1", "A")
	h.Settle()
	if h.M.ActiveView() != "Accounts" {
		t.Fatalf("A: view %s", h.M.ActiveView())
	}
}
