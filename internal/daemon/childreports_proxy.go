package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.ChildReportReleaseAPI = (*LocalAPI)(nil)

// ReleaseChildReports forwards child_reports.release to the hub with the
// caller's identity (a session about to read its own inbox).
func (l *LocalAPI) ReleaseChildReports(ctx context.Context, req contracts.ReleaseChildReportsRequest) (contracts.ReleaseChildReportsResponse, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return contracts.ReleaseChildReportsResponse{}, err
	}
	u, ok := a.(contracts.ChildReportReleaseAPI)
	if !ok {
		return contracts.ReleaseChildReportsResponse{}, contracts.Errorf(contracts.ErrUnsupported, "child_reports.release is not available through this node's hub client")
	}
	return u.ReleaseChildReports(l.d.upstreamCtx(ctx), req)
}
