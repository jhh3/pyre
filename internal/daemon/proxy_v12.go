package daemon

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Contract v1.2 operations, forwarded to the hub like every other hub-only
// call (ErrUnavailable while the hub is unreachable).

func (p proxy) GetAttention(ctx context.Context, r c.GetRequest) (c.AttentionItem, error) {
	return fwd(p, ctx, r, c.API.GetAttention)
}

func (p proxy) MintPreviewURL(ctx context.Context, r c.MintPreviewRequest) (c.PreviewURL, error) {
	return fwd(p, ctx, r, c.API.MintPreviewURL)
}

func (p proxy) CheckConnectionGrant(ctx context.Context, r c.CheckGrantRequest) (c.GrantDecision, error) {
	return fwd(p, ctx, r, c.API.CheckConnectionGrant)
}

func (p proxy) AcquireAccountLease(ctx context.Context, r c.AcquireAccountLeaseRequest) (c.AccountLease, error) {
	return fwd(p, ctx, r, c.API.AcquireAccountLease)
}

func (p proxy) RenewAccountLease(ctx context.Context, r c.RenewAccountLeaseRequest) (c.AccountLease, error) {
	return fwd(p, ctx, r, c.API.RenewAccountLease)
}

func (p proxy) ReleaseAccountLease(ctx context.Context, r c.ReleaseAccountLeaseRequest) (c.AccountLease, error) {
	return fwd(p, ctx, r, c.API.ReleaseAccountLease)
}

func (p proxy) GetCommandResult(ctx context.Context, r c.GetRequest) (c.NodeActionResult, error) {
	return fwd(p, ctx, r, c.API.GetCommandResult)
}
