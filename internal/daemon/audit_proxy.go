package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.AuditAPI = (*LocalAPI)(nil)

// ListAudit forwards audit.list to the hub with the caller's identity.
func (l *LocalAPI) ListAudit(ctx context.Context, req contracts.ListAuditRequest) (contracts.Page[contracts.AuditEntry], error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return contracts.Page[contracts.AuditEntry]{}, err
	}
	u, ok := a.(contracts.AuditAPI)
	if !ok {
		return contracts.Page[contracts.AuditEntry]{}, contracts.Errorf(contracts.ErrUnsupported, "audit.list is not available through this node's hub client")
	}
	return u.ListAudit(l.d.upstreamCtx(ctx), req)
}
