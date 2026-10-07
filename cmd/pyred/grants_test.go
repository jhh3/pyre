package main

import (
	"context"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

type fakeGrantHub struct {
	d   contracts.GrantDecision
	err error
	got contracts.CheckGrantRequest
}

func (f *fakeGrantHub) CheckConnectionGrant(_ context.Context, req contracts.CheckGrantRequest) (contracts.GrantDecision, error) {
	f.got = req
	return f.d, f.err
}

func TestHubGrantsAsksHubAndFailsClosed(t *testing.T) {
	pushed := &pushedGrants{active: map[grantKey]bool{}}
	pushed.record(contracts.ConnectionGrantParams{ConnectionID: "conn_synth", SessionID: "ses_synth"}, true)
	fh := &fakeGrantHub{d: contracts.GrantDecision{Allowed: false, Generation: 7, Reason: "no active grant"}}
	g := &hubGrants{pushed: pushed, hub: func() grantChecker { return fh }}
	ctx := context.Background()

	// The hub is authoritative: a pushed grant the hub no longer has is denied.
	d, err := g.CheckGrant(ctx, "conn_synth", "ses_synth")
	if err != nil || d.Allowed || d.Generation != 7 || fh.got.ConnectionID != "conn_synth" || fh.got.SessionID != "ses_synth" {
		t.Fatalf("hub deny: %+v %v (req %+v)", d, err, fh.got)
	}
	fh.d = contracts.GrantDecision{Allowed: true, Generation: 8, Reason: "session grant"}
	if d, err = g.CheckGrant(ctx, "conn_synth", "ses_synth"); err != nil || !d.Allowed || d.Generation != 8 {
		t.Fatalf("hub allow: %+v %v", d, err)
	}
	// Hub unreachable: deny with the error.
	fh.err = contracts.Errorf(contracts.ErrUnavailable, "hub down")
	if d, err = g.CheckGrant(ctx, "conn_synth", "ses_synth"); err == nil || d.Allowed {
		t.Fatalf("hub down: %+v %v", d, err)
	}
	// Hub without the operation: pushed mirror.
	fh.err = contracts.Errorf(contracts.ErrNotFound, "unknown op")
	if d, err = g.CheckGrant(ctx, "conn_synth", "ses_synth"); err != nil || !d.Allowed {
		t.Fatalf("old hub fallback: %+v %v", d, err)
	}
	// Standalone: pushed mirror (deny by default).
	g.hub = func() grantChecker { return nil }
	if d, _ = g.CheckGrant(ctx, "conn_other", "ses_synth"); d.Allowed {
		t.Fatalf("standalone default: %+v", d)
	}
}
