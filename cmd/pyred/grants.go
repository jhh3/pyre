package main

import (
	"context"
	"time"

	"github.com/jhh3/pyre/internal/connections"
	"github.com/jhh3/pyre/internal/contracts"
)

// grantChecker is the hub operation the node asks on every connection use.
type grantChecker interface {
	CheckConnectionGrant(ctx context.Context, req contracts.CheckGrantRequest) (contracts.GrantDecision, error)
}

// hubGrants implements connections.GrantChecker by asking the hub
// (connections.check_grant, contract v1.2), which decides from its catalog
// and authoritative session records. Any hub error denies (fail closed).
// Without a hub client (standalone), or against a hub that predates the
// operation, it falls back to the pushed-grant mirror (deny by default).
type hubGrants struct {
	hub    func() grantChecker // nil result: no hub client
	pushed *pushedGrants
}

func (in *integrations) grantChecker() *hubGrants {
	return &hubGrants{pushed: in.grants, hub: func() grantChecker {
		if in.hub == nil {
			return nil
		}
		return in.hub
	}}
}

func (g *hubGrants) CheckGrant(ctx context.Context, connID, sessionID string) (connections.GrantDecision, error) {
	h := g.hub()
	if h == nil {
		return g.pushed.CheckGrant(ctx, connID, sessionID)
	}
	dec := connections.GrantDecision{ConnectionID: connID, SessionID: sessionID, CheckedAt: time.Now().UTC()}
	d, err := h.CheckConnectionGrant(ctx, contracts.CheckGrantRequest{ConnectionID: connID, SessionID: sessionID})
	switch {
	case contracts.IsCode(err, contracts.ErrUnsupported), contracts.IsCode(err, contracts.ErrNotFound):
		// Older hub without connections.check_grant.
		return g.pushed.CheckGrant(ctx, connID, sessionID)
	case err != nil:
		dec.Reason = "hub grant check failed; denying: " + contracts.AsError(err).Message
		return dec, err
	}
	dec.Allowed, dec.Generation, dec.Reason = d.Allowed, d.Generation, d.Reason
	return dec, nil
}
