package daemon

import (
	"context"
	"os"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/search"
)

var _ contracts.SearchAPI = (*LocalAPI)(nil)

// Search forwards search.query to the hub with the caller's identity (the
// hub fans out to every node). When the hub is unreachable it answers from
// this node's own transcript index, marking every other node not searched.
func (l *LocalAPI) Search(ctx context.Context, req contracts.SearchRequest) (contracts.SearchResponse, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err == nil {
		s, ok := a.(contracts.SearchAPI)
		if !ok {
			return contracts.SearchResponse{}, contracts.Errorf(contracts.ErrUnsupported, "search is not available through this node's hub client")
		}
		resp, err := s.Search(l.d.upstreamCtx(ctx), req)
		if err == nil || !hubDown(err) {
			return resp, err
		}
	} else if !hubDown(err) {
		return contracts.SearchResponse{}, err
	}
	return l.d.searchOffline(ctx, req)
}

// searchOffline answers a fan-out request from this node alone.
func (d *Daemon) searchOffline(ctx context.Context, req contracts.SearchRequest) (contracts.SearchResponse, error) {
	if err := search.NormalizeRequest(&req); err != nil {
		return contracts.SearchResponse{}, err
	}
	name := d.cfg.NodeName
	if name == "" {
		name, _ = os.Hostname()
	}
	resp := contracts.SearchResponse{Query: req.Query, Hits: []contracts.SearchHit{}, SnapshotAt: d.now()}
	st := contracts.SearchNodeStatus{NodeID: d.nodeID, Name: name}
	if req.NodeID != "" && req.NodeID != d.nodeID {
		st.State, st.Detail = contracts.SearchNodeSkipped, "hub unreachable; only this node can be searched"
		resp.Nodes = []contracts.SearchNodeStatus{st}
		return resp, nil
	}
	if !search.WantSource(req, contracts.SearchSourceTranscript) {
		st.State = contracts.SearchNodeSkipped
		resp.Nodes = []contracts.SearchNodeStatus{st}
		return resp, nil
	}
	res, err := d.searchLocal(ctx, contracts.TranscriptSearchParams{Query: req.Query, Since: req.Since, Limit: req.Limit, BudgetMS: req.BudgetMS})
	if err != nil {
		return resp, err
	}
	hits := make([]contracts.SearchHit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, search.FromTranscript(h))
	}
	resp.Hits, resp.Truncated = search.Rank(hits, d.now(), req.Limit)
	resp.Truncated = resp.Truncated || res.Truncated
	st.State, st.Hits, st.TookMS, st.Partial = contracts.SearchNodeSearched, len(res.Hits), res.TookMS, res.Partial
	st.Detail = "hub unreachable: searched this node only (archived sessions are not filtered); other nodes not searched"
	resp.Nodes = []contracts.SearchNodeStatus{st}
	resp.TookMS = res.TookMS
	return resp, nil
}
