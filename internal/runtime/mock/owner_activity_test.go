package mock

import (
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// The mock marks owner activity like the hub, so TUI tests can show it.
func TestOwnerActiveAt(t *testing.T) {
	b, clk, node := newEmpty(t)
	sv := start(t, b, node)
	sid := sv.Session.SessionID
	get := func() *time.Time {
		t.Helper()
		v, err := b.GetSession(as(owner), c.GetRequest{ID: sid})
		if err != nil {
			t.Fatal(err)
		}
		return v.Session.OwnerActiveAt
	}
	send := func(p c.Principal) {
		t.Helper()
		if _, err := b.SendMessage(as(p), c.SendMessageRequest{Mutation: op(), Recipient: c.Address{Kind: c.AddrSession, ID: sid}, Body: c.MessageBody{Text: "synthetic"}}); err != nil {
			t.Fatal(err)
		}
	}
	send(c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-peer"})
	if got := get(); got != nil {
		t.Fatalf("session message marked owner activity: %v", got)
	}
	t1 := clk.Now()
	send(owner)
	if got := get(); got == nil || !got.Equal(t1) {
		t.Fatalf("owner message: %v, want %v", got, t1)
	}
	clk.Advance(30 * time.Second)
	send(owner)
	if got := get(); !got.Equal(t1) {
		t.Fatalf("within a minute: %v, want %v", got, t1)
	}
	clk.Advance(31 * time.Second)
	t2 := clk.Now()
	if _, err := b.AcquireLease(as(owner), c.LeaseRequest{Mutation: op(), RunID: sv.Run.RunID}); err != nil {
		t.Fatal(err)
	}
	if got := get(); !got.Equal(t2) {
		t.Fatalf("lease: %v, want %v", got, t2)
	}
}
