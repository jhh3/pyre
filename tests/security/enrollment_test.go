package security

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/transport"
)

func (f *fixture) ownerAPICtx() context.Context {
	return contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: f.owner.ClientID})
}

// TestEnrollmentOneTimeAndBruteForceLockout: through the real HTTP exchange
// endpoints, a node secret works once; wrong secrets get a uniform
// unauthorized answer indistinguishable from an unknown node; after
// MaxFailedAttempts wrong guesses the ticket is dead even for the correct
// secret; expired tickets fail; pairing codes behave the same way.
func TestEnrollmentOneTimeAndBruteForceLockout(t *testing.T) {
	f := newFixture(t, fixtureOpts{rate: enrollment.RateLimitConfig{Disabled: true}})
	ctx := f.ownerAPICtx()

	tk, err := f.fleet.CreateEnrollment(ctx, contracts.EnrollmentRequest{Mutation: op(), Name: "one-time"})
	if err != nil {
		t.Fatal(err)
	}
	f.remember(tk.Secret)
	ok := f.post(contracts.PathEnrollExchange, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "h"})
	if ok.status != 200 {
		t.Fatalf("redeem: %d %s", ok.status, ok.body)
	}
	var cred contracts.NodeCredential
	mustJSON(t, ok.body, &cred)
	f.remember(cred.Token)
	again := f.post(contracts.PathEnrollExchange, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret})
	if again.code() != contracts.ErrConflict {
		t.Fatalf("second redeem: %d %s", again.status, again.body)
	}

	// Uniform failure: unknown node vs wrong secret.
	tk2, err := f.fleet.CreateEnrollment(ctx, contracts.EnrollmentRequest{Mutation: op(), Name: "brute"})
	if err != nil {
		t.Fatal(err)
	}
	f.remember(tk2.Secret)
	guess := func(nodeID string) rpcResult {
		s, _ := transport.NewSecret(transport.PrefixEnrollSecret)
		f.remember(s)
		return f.post(contracts.PathEnrollExchange, contracts.NodeEnrollExchange{NodeID: nodeID, Secret: s})
	}
	unknown := guess(contracts.NewID(contracts.PrefixNode))
	wrong := guess(tk2.NodeID)
	if unknown.status != wrong.status || unknown.err == nil || wrong.err == nil || unknown.err.Message != wrong.err.Message || unknown.err.Code != contracts.ErrUnauthorized {
		t.Fatalf("unknown node %d %s vs wrong secret %d %s: responses must be identical", unknown.status, unknown.body, wrong.status, wrong.body)
	}
	for i := 1; i < enrollment.MaxFailedAttempts; i++ {
		guess(tk2.NodeID)
	}
	late := f.post(contracts.PathEnrollExchange, contracts.NodeEnrollExchange{NodeID: tk2.NodeID, Secret: tk2.Secret})
	if late.status == 200 || late.code() != contracts.ErrExpired {
		t.Fatalf("correct secret after %d failures: %d %s (want expired/invalidated)", enrollment.MaxFailedAttempts, late.status, late.body)
	}

	// Expiry.
	tk3, err := f.fleet.CreateEnrollment(ctx, contracts.EnrollmentRequest{Mutation: op(), Name: "expiring", TTL: 60})
	if err != nil {
		t.Fatal(err)
	}
	f.remember(tk3.Secret)
	f.clock.Advance(61 * time.Second)
	if r := f.post(contracts.PathEnrollExchange, contracts.NodeEnrollExchange{NodeID: tk3.NodeID, Secret: tk3.Secret}); r.code() != contracts.ErrExpired {
		t.Fatalf("expired ticket: %d %s", r.status, r.body)
	}

	// Pairing: one-time and lockout.
	pt, err := f.fleet.CreatePairing(ctx, contracts.PairingRequest{Mutation: op(), ClientName: "phone", Kind: "android"})
	if err != nil {
		t.Fatal(err)
	}
	f.remember(pt.Code)
	for i := 0; i < enrollment.MaxFailedAttempts; i++ {
		c, _ := transport.NewSecret(transport.PrefixPairingCode)
		f.remember(c)
		if r := f.post(contracts.PathPairExchange, contracts.PairingExchange{PairingID: pt.PairingID, Code: c, Kind: "android"}); r.code() != contracts.ErrUnauthorized {
			t.Fatalf("wrong pairing code: %d %s", r.status, r.body)
		}
	}
	if r := f.post(contracts.PathPairExchange, contracts.PairingExchange{PairingID: pt.PairingID, Code: pt.Code, Kind: "android"}); r.status == 200 {
		t.Fatal("pairing code accepted after brute-force lockout")
	}
	pt2, err := f.fleet.CreatePairing(ctx, contracts.PairingRequest{Mutation: op(), ClientName: "cli2", Kind: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	f.remember(pt2.Code)
	first := f.post(contracts.PathPairExchange, contracts.PairingExchange{PairingID: pt2.PairingID, Code: pt2.Code, Kind: "cli"})
	if first.status != 200 {
		t.Fatalf("pair: %d %s", first.status, first.body)
	}
	var cc contracts.ClientCredential
	mustJSON(t, first.body, &cc)
	f.remember(cc.Token)
	if r := f.post(contracts.PathPairExchange, contracts.PairingExchange{PairingID: pt2.PairingID, Code: pt2.Code, Kind: "cli"}); r.code() != contracts.ErrConflict {
		t.Fatalf("pairing code reuse: %d %s", r.status, r.body)
	}
	f.assertNoLeaks(nil)
}

// TestExchangeRateLimited: unauthenticated exchange endpoints are rate
// limited per remote address (429 + Retry-After), so online guessing is
// throttled before ticket lockout even matters.
func TestExchangeRateLimited(t *testing.T) {
	f := newFixture(t, fixtureOpts{rate: enrollment.RateLimitConfig{PerIPBurst: 5, PerIPEvery: time.Hour}})
	var limited, total int
	for i := 0; i < 12; i++ {
		s, _ := transport.NewSecret(transport.PrefixEnrollSecret)
		f.remember(s)
		r := f.post(contracts.PathEnrollExchange, contracts.NodeEnrollExchange{NodeID: contracts.NewID(contracts.PrefixNode), Secret: s})
		total++
		if r.status == http.StatusTooManyRequests {
			limited++
			if r.header.Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
		}
	}
	if limited != total-5 {
		t.Fatalf("%d of %d attempts limited, want %d", limited, total, total-5)
	}
	// Exchange endpoints never echo the presented secret.
	f.assertNoLeaks(nil)
}

// TestEnrollmentRequiresOwner: neither sessions nor anonymous callers can
// mint enrollment or pairing tickets.
func TestEnrollmentRequiresOwner(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	_, sessTok := f.startSession("would-enroll")
	for _, tok := range []string{sessTok, ""} {
		for _, opName := range []string{contracts.OpCreateEnrollment, contracts.OpCreatePairing} {
			body := any(contracts.EnrollmentRequest{Mutation: op(), Name: "x"})
			if opName == contracts.OpCreatePairing {
				body = contracts.PairingRequest{Mutation: op(), Kind: "cli"}
			}
			r := f.rpc(tok, opName, body)
			if r.status != http.StatusForbidden && r.status != http.StatusUnauthorized {
				t.Errorf("%s with token %q: %d %s", opName, strings.Repeat("*", min(len(tok), 4)), r.status, r.body)
			}
		}
	}
}
