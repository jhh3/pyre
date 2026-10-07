package hub

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

// GetCommandResult returns a node command's current outcome and, once
// reported, its final result. Readers: the owner, the command's actor, and
// the node the command was sent to.
func (h *Hub) GetCommandResult(ctx context.Context, req contracts.GetRequest) (contracts.NodeActionResult, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.NodeActionResult{}, err
	}
	r, err := h.getCommand(ctx, h.db, req.ID)
	if err != nil {
		return contracts.NodeActionResult{}, err
	}
	actor := r.Actor.Kind == p.Kind && r.Actor.ID == p.ID
	ownNode := p.Kind == contracts.PrincipalNode && p.OnBehalfOf == nil && p.ID == r.NodeID
	if !isOwner(p) && p.Kind != contracts.PrincipalSystem && !actor && !ownNode {
		return contracts.NodeActionResult{}, contracts.Errorf(contracts.ErrForbidden, "command %s belongs to another principal", r.CommandID).WithRef(contracts.KindCommand, r.CommandID)
	}
	res := contracts.NodeActionResult{CommandID: r.CommandID, Outcome: pendingOutcome(r)}
	if r.finished() {
		res.Result = r.Result
		res.Error = r.Error
		if r.State == cmdExpired || r.State == cmdCancelled {
			res.Error = contracts.AsError(commandError(r))
		}
	}
	return res, nil
}
