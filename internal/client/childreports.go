package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.ChildReportReleaseAPI = (*Client)(nil)

// ReleaseChildReports calls child_reports.release. A hub that predates it
// answers 404, reported as unsupported.
func (cl *Client) ReleaseChildReports(ctx context.Context, req c.ReleaseChildReportsRequest) (c.ReleaseChildReportsResponse, error) {
	var out c.ReleaseChildReportsResponse
	err := cl.call(ctx, c.OpReleaseChildReports, req, &out, true, 0)
	return out, reposUnsupported(c.OpReleaseChildReports, err)
}
