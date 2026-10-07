package main

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
)

// leaseAPI is the hub's accounts.lease.* surface (client.Client over the
// node credential).
type leaseAPI interface {
	AcquireAccountLease(ctx context.Context, req contracts.AcquireAccountLeaseRequest) (contracts.AccountLease, error)
	RenewAccountLease(ctx context.Context, req contracts.RenewAccountLeaseRequest) (contracts.AccountLease, error)
	ReleaseAccountLease(ctx context.Context, req contracts.ReleaseAccountLeaseRequest) (contracts.AccountLease, error)
}

// hubFleet implements accounts.FleetLeaser over the hub (contract v1.3).
// Each call is a new operation; the hub returns the existing lease to the
// same node and holder, so a retry after a lost response is safe.
type hubFleet struct{ hub leaseAPI }

func fleetLease(l contracts.AccountLease) accounts.FleetLease {
	return accounts.FleetLease{LeaseID: l.LeaseID, AccountID: l.AccountID, NodeID: l.NodeID, ExpiresAt: l.ExpiresAt}
}

func ttlSeconds(d time.Duration) int { return int(d / time.Second) }

func newOp() contracts.Mutation {
	return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
}

func (f hubFleet) AcquireAccountLease(ctx context.Context, req accounts.FleetLeaseRequest) (accounts.FleetLease, error) {
	l, err := f.hub.AcquireAccountLease(ctx, contracts.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: req.AccountID, Holder: req.Holder, TTLSeconds: ttlSeconds(req.TTL)})
	return fleetLease(l), err
}

func (f hubFleet) RenewAccountLease(ctx context.Context, leaseID string, ttl time.Duration) (accounts.FleetLease, error) {
	l, err := f.hub.RenewAccountLease(ctx, contracts.RenewAccountLeaseRequest{Mutation: newOp(), LeaseID: leaseID, TTLSeconds: ttlSeconds(ttl)})
	return fleetLease(l), err
}

func (f hubFleet) ReleaseAccountLease(ctx context.Context, leaseID string) error {
	_, err := f.hub.ReleaseAccountLease(ctx, contracts.ReleaseAccountLeaseRequest{Mutation: newOp(), LeaseID: leaseID})
	return err
}

// parseIndependent turns "acct_a, acct_b" into a set.
func parseIndependent(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

// rotaConfig builds accounts.RotaConfig from flags. Without a hub client
// the fleet lease is nil, so only per-machine independent accounts can be
// checked out (the accounts service denies the rest).
func rotaConfig(o runOpts, hub leaseAPI, busy accounts.HomeBusyChecker) accounts.RotaConfig {
	rc := accounts.RotaConfig{Enabled: o.rotaCheckout, Independent: parseIndependent(o.rotaIndependent), HomeBusy: busy}
	if o.rotaHome != "" {
		// rota's store and the live default logins live under another home
		// than this daemon's (an isolated-HOME node using the owner's rota).
		rc.ClaudeAccountsDir = filepath.Join(o.rotaHome, ".claude", "accounts")
		rc.CodexHome = filepath.Join(o.rotaHome, ".codex")
		rc.LiveClaudeJSON = filepath.Join(o.rotaHome, ".claude.json")
	}
	if hub != nil {
		rc.Fleet = hubFleet{hub: hub}
	}
	return rc
}

// recoverRota resolves rota checkouts left by a previous pyred after the
// daemon recovered its runs: checkouts of runs that are no longer live are
// checked in (with write-back); live runs keep theirs.
func recoverRota(ctx context.Context, svc *accounts.Service, d *daemon.Daemon) error {
	runs, err := d.ActiveRuns(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(runs))
	for _, r := range runs {
		live[r.RunID] = true
	}
	return svc.RecoverRotaCheckouts(ctx, func(runID string) bool { return live[runID] })
}
