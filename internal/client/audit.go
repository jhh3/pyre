package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.AuditAPI = (*Client)(nil)

// ListAudit calls audit.list. A hub that predates it answers 404, reported
// as unsupported.
func (cl *Client) ListAudit(ctx context.Context, req c.ListAuditRequest) (c.Page[c.AuditEntry], error) {
	var out c.Page[c.AuditEntry]
	err := cl.read(ctx, c.OpListAudit, req, &out)
	return out, reposUnsupported(c.OpListAudit, err)
}
