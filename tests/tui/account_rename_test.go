package tui_test

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// R on an Accounts card opens the label editor prefilled; enter saves
// through accounts.update; an empty label is refused with a hint.
func TestRenameAccountWithR(t *testing.T) {
	h, rec, _ := startMock(t, 160, 34, nil, nil)
	h.Key(":")
	h.Type("accounts")
	h.Key("enter")
	h.Settle()
	h.Key("R")
	if !h.Contains("Rename account") {
		t.Fatalf("R did not open the account editor:\n%s", h.View())
	}
	h.Key("ctrl+u")
	h.Key("enter")
	if !h.Contains("An account needs a name") || len(rec.Calls("UpdateAccount")) != 0 {
		t.Fatalf("empty label not refused:\n%s", h.View())
	}
	h.Type("synth-renamed")
	h.Key("enter")
	h.WaitFor("rename sent", 3*sec, func() bool { return len(rec.Calls("UpdateAccount")) == 1 })
	req := rec.Calls("UpdateAccount")[0].Request.(contracts.UpdateAccountRequest)
	if req.AccountID == "" || req.Label == nil || *req.Label != "synth-renamed" || req.OperationID == "" || req.Disabled != nil {
		t.Fatalf("rename request %+v", req)
	}
	h.WaitFor("new label on the card", 3*sec, func() bool { return h.Contains("synth-renamed") })
}
