package main

import (
	"context"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// asNode injects the node principal the hub would derive from the node
// credential.
type asNode struct {
	b  *mock.Backend
	id string
}

func (a asNode) ctx(ctx context.Context) context.Context {
	return contracts.WithPrincipal(ctx, contracts.Principal{Kind: contracts.PrincipalNode, ID: a.id})
}
func (a asNode) AcquireAccountLease(ctx context.Context, r contracts.AcquireAccountLeaseRequest) (contracts.AccountLease, error) {
	return a.b.AcquireAccountLease(a.ctx(ctx), r)
}
func (a asNode) RenewAccountLease(ctx context.Context, r contracts.RenewAccountLeaseRequest) (contracts.AccountLease, error) {
	return a.b.RenewAccountLease(a.ctx(ctx), r)
}
func (a asNode) ReleaseAccountLease(ctx context.Context, r contracts.ReleaseAccountLeaseRequest) (contracts.AccountLease, error) {
	return a.b.ReleaseAccountLease(a.ctx(ctx), r)
}

func TestHubFleetLeaser(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewFakeClock(mock.FixtureEpoch), Empty: true})
	owner := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth"})
	acct, err := b.UpsertAccount(owner, contracts.UpsertAccountRequest{Mutation: newOp(), Provider: contracts.ProviderAnthropic, Label: "synth"})
	if err != nil {
		t.Fatal(err)
	}
	var mac, linux accounts.FleetLeaser = hubFleet{hub: asNode{b, "node_synth-mac"}}, hubFleet{hub: asNode{b, "node_synth-linux"}}
	ctx := context.Background()
	l, err := mac.AcquireAccountLease(ctx, accounts.FleetLeaseRequest{AccountID: acct.AccountID, Holder: "rota-checkout:grant_1", TTL: 10 * time.Minute})
	if err != nil || l.LeaseID == "" || l.NodeID != "node_synth-mac" {
		t.Fatalf("acquire: %+v %v", l, err)
	}
	// A retry (lost response) returns the same lease.
	if again, err := mac.AcquireAccountLease(ctx, accounts.FleetLeaseRequest{AccountID: acct.AccountID, Holder: "rota-checkout:grant_1", TTL: 10 * time.Minute}); err != nil || again.LeaseID != l.LeaseID {
		t.Fatalf("retry: %+v %v", again, err)
	}
	if _, err := linux.AcquireAccountLease(ctx, accounts.FleetLeaseRequest{AccountID: acct.AccountID, Holder: "rota-checkout:grant_2"}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("other node: %v", err)
	}
	if r, err := mac.RenewAccountLease(ctx, l.LeaseID, 20*time.Minute); err != nil || !r.ExpiresAt.After(l.ExpiresAt) {
		t.Fatalf("renew: %+v %v", r, err)
	}
	if err := mac.ReleaseAccountLease(ctx, l.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := linux.AcquireAccountLease(ctx, accounts.FleetLeaseRequest{AccountID: acct.AccountID, Holder: "rota-checkout:grant_2"}); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestRotaConfigFromFlags(t *testing.T) {
	rc := rotaConfig(runOpts{rotaCheckout: true, rotaIndependent: " acct_a,acct_b ,, "}, nil, nil)
	if !rc.Enabled || len(rc.Independent) != 2 || !rc.Independent["acct_a"] || !rc.Independent["acct_b"] || rc.Fleet != nil {
		t.Fatalf("%+v", rc)
	}
	if rc := rotaConfig(runOpts{}, asNode{}, nil); rc.Enabled || rc.Fleet == nil || rc.ClaudeAccountsDir != "" || rc.LiveClaudeJSON != "" {
		t.Fatalf("default: %+v", rc)
	}
	// An isolated-HOME node uses the owner's rota store and live default
	// logins from --rota-home, never its own (empty) HOME.
	rc = rotaConfig(runOpts{rotaCheckout: true, rotaHome: "/home/owner"}, nil, nil)
	if rc.ClaudeAccountsDir != "/home/owner/.claude/accounts" || rc.CodexHome != "/home/owner/.codex" || rc.LiveClaudeJSON != "/home/owner/.claude.json" {
		t.Fatalf("rota home: %+v", rc)
	}
}
