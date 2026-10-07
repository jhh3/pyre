package hub

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestAccountLeaseExpiryClampAndEvents(t *testing.T) {
	x := newHarness(t)
	a, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: "op_acct"}, Provider: contracts.ProviderAnthropic, Label: "synth-lease"})
	if err != nil {
		t.Fatal(err)
	}
	mac := as(contracts.Principal{Kind: contracts.PrincipalNode, ID: nodeMac})
	linux := as(contracts.Principal{Kind: contracts.PrincipalNode, ID: "node_synth-linux"})
	op := func() contracts.Mutation {
		return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
	}

	l, err := x.h.AcquireAccountLease(mac, contracts.AcquireAccountLeaseRequest{Mutation: op(), AccountID: a.AccountID, Holder: "rota-checkout:grant_1", TTLSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.ExpiresAt.Sub(l.AcquiredAt); got != time.Minute {
		t.Fatalf("ttl not clamped to 60s: %v", got)
	}
	if def, _ := x.h.AcquireAccountLease(mac, contracts.AcquireAccountLeaseRequest{Mutation: op(), AccountID: a.AccountID, Holder: "rota-checkout:grant_1"}); def.ExpiresAt.Sub(def.AcquiredAt) != 30*time.Minute {
		t.Fatalf("default ttl: %+v", def)
	}
	conflictOp := op()
	if _, err := x.h.AcquireAccountLease(linux, contracts.AcquireAccountLeaseRequest{Mutation: conflictOp, AccountID: a.AccountID, Holder: "rota-checkout:grant_2"}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	// The lease expires on its own; the conflict was not stored, so the
	// same operation retried later succeeds.
	x.clock.Add(31 * time.Minute)
	l2, err := x.h.AcquireAccountLease(linux, contracts.AcquireAccountLeaseRequest{Mutation: conflictOp, AccountID: a.AccountID, Holder: "rota-checkout:grant_2"})
	if err != nil || l2.NodeID != "node_synth-linux" {
		t.Fatalf("after expiry: %+v %v", l2, err)
	}
	// The expired lease cannot be revived while another node holds it.
	if _, err := x.h.RenewAccountLease(mac, contracts.RenewAccountLeaseRequest{Mutation: op(), LeaseID: l.LeaseID}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("revive expired lease under another holder: %v", err)
	}
	if _, err := x.h.ReleaseAccountLease(linux, contracts.ReleaseAccountLeaseRequest{Mutation: op(), LeaseID: l2.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if n := x.countEvents(contracts.EvAccountLease); n != 3 { // two acquisitions + one release
		t.Fatalf("account.lease events = %d", n)
	}
}

func TestUpsertAccountProviderRef(t *testing.T) {
	x := newHarness(t)
	op := func() contracts.Mutation {
		return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
	}
	a, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: op(), Provider: contracts.ProviderAnthropic, Label: "synth", RotaRef: "synth", ProviderRef: "synthetic-uuid-1"})
	if err != nil || a.ProviderRef != "synthetic-uuid-1" {
		t.Fatalf("create: %+v %v", a, err)
	}
	// Omitting provider_ref keeps it; a different one is refused.
	if a2, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: op(), AccountID: a.AccountID, Provider: contracts.ProviderAnthropic, Label: "renamed"}); err != nil || a2.ProviderRef != "synthetic-uuid-1" || a2.Label != "renamed" {
		t.Fatalf("update keeps ref: %+v %v", a2, err)
	}
	if _, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: op(), AccountID: a.AccountID, Provider: contracts.ProviderAnthropic, Label: "x", ProviderRef: "synthetic-uuid-2"}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("rebind: %v", err)
	}
}
