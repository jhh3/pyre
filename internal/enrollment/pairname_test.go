package enrollment

import (
	"context"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// `pyre pair` without --name sends no client name: the client keeps the
// name the owner gave `pyre pair-client --name` (rc4 smoke finding).
func TestRedeemPairingKeepsPairingName(t *testing.T) {
	f := newFixture(t, RateLimitConfig{Disabled: true})
	ctx := context.Background()
	p := f.pairing(t, "cli")
	cred, err := f.store.RedeemPairing(ctx, contracts.PairingExchange{PairingID: p.PairingID, Code: p.Code, Kind: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.store.GetClient(ctx, cred.ClientID)
	if err != nil || c.Name != "pixel" || c.Kind != "cli" {
		t.Fatalf("client: %+v %v", c, err)
	}
}
