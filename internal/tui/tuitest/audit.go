package tuitest

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

// The test wrappers pass audit.list through to the wrapped API when it
// offers it (contracts.AuditAPI is not part of contracts.API).

func auditVia(api contracts.API, ctx context.Context, req contracts.ListAuditRequest) (contracts.Page[contracts.AuditEntry], error) {
	if a, ok := api.(contracts.AuditAPI); ok {
		return a.ListAudit(ctx, req)
	}
	return contracts.Page[contracts.AuditEntry]{}, contracts.Errorf(contracts.ErrUnsupported, "audit log not offered")
}

func (e *Embeddable) ListAudit(ctx context.Context, req contracts.ListAuditRequest) (contracts.Page[contracts.AuditEntry], error) {
	return auditVia(e.API, ctx, req)
}

func (r *Recorder) ListAudit(ctx context.Context, req contracts.ListAuditRequest) (contracts.Page[contracts.AuditEntry], error) {
	return auditVia(r.API, ctx, req)
}

func (s *Slow) ListAudit(ctx context.Context, req contracts.ListAuditRequest) (contracts.Page[contracts.AuditEntry], error) {
	if err := s.wait(ctx, "ListAudit"); err != nil {
		return contracts.Page[contracts.AuditEntry]{}, err
	}
	return auditVia(s.API, ctx, req)
}
