package mock

import (
	"context"
	"sort"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/search"
)

var _ c.SearchAPI = (*Backend)(nil)

// Search mirrors the hub's fan-out over the mock fleet: transcripts of runs
// on connected nodes match by substring (every term), disconnected nodes
// are reported not searched (offline), archived sessions are dropped unless
// asked, documents and artifacts match by title. Mock only.
func (b *Backend) Search(ctx context.Context, req c.SearchRequest) (c.SearchResponse, error) {
	return query(b, ctx, func(p c.Principal) (c.SearchResponse, error) {
		if p.Kind == c.PrincipalNode {
			return c.SearchResponse{}, c.Errorf(c.ErrForbidden, "node principals may not search")
		}
		if err := search.NormalizeRequest(&req); err != nil {
			return c.SearchResponse{}, err
		}
		terms := search.Terms(req.Query)
		resp := c.SearchResponse{Query: req.Query, Hits: []c.SearchHit{}, Nodes: []c.SearchNodeStatus{}, SnapshotAt: b.now()}
		var hits []c.SearchHit
		nodeIDs := make([]string, 0, len(b.nodes))
		for id := range b.nodes {
			nodeIDs = append(nodeIDs, id)
		}
		sort.Strings(nodeIDs)
		for _, id := range nodeIDs {
			n := b.nodes[id]
			if req.NodeID != "" && id != req.NodeID {
				continue
			}
			st := c.SearchNodeStatus{NodeID: id, Name: n.Name, State: c.SearchNodeSearched}
			if n.Connection.State != c.ConnConnected {
				st.State, st.Detail = c.SearchNodeOffline, "not searched (offline)"
				resp.Nodes = append(resp.Nodes, st)
				continue
			}
			if !search.WantSource(req, c.SearchSourceTranscript) {
				st.State = c.SearchNodeSkipped
				resp.Nodes = append(resp.Nodes, st)
				continue
			}
			for runID, entries := range b.transcripts {
				r, ok := b.runs[runID]
				if !ok || r.NodeID != id {
					continue
				}
				s, ok := b.sessions[r.SessionID]
				if !ok || (s.Archived && !req.IncludeArchived) {
					continue
				}
				for _, e := range entries {
					if e.Kind == "screen" || e.Kind == "system" || (req.Since != nil && e.At.Before(*req.Since)) || !search.Matches(e.Text, terms) {
						continue
					}
					st.Hits++
					kind := e.Kind
					if kind == "pyre_message" {
						kind = "message"
					}
					hits = append(hits, c.SearchHit{Source: c.SearchSourceTranscript, NodeID: id, SessionID: s.SessionID, RunID: runID,
						Title: s.Title, Kind: kind, Seq: e.Seq, At: e.At, Snippet: search.Snippet(e.Text, terms, 160),
						Score: float64(len(terms)), Archived: s.Archived, ProjectID: deref(s.ProjectID), Provenance: string(s.Harness) + " transcript"})
				}
			}
			resp.Nodes = append(resp.Nodes, st)
		}
		if search.WantSource(req, c.SearchSourceDocument) {
			for _, d := range b.documents {
				if search.Matches(d.Title+" "+d.Slug, terms) {
					hits = append(hits, c.SearchHit{Source: c.SearchSourceDocument, Ref: d.DocumentID, ProjectID: deref(d.ProjectID),
						Title: d.Title, At: d.UpdatedAt, Snippet: search.Snippet(d.Title, terms, 160), Provenance: "document " + d.Slug})
				}
			}
		}
		resp.Hits, resp.Truncated = search.Rank(hits, b.now(), req.Limit)
		search.SortNodes(resp.Nodes)
		return resp, nil
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
