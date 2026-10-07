package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/search"
)

var _ contracts.SearchAPI = (*Hub)(nil)

// Search fans a query out to every connected node in parallel
// (transcript.search, and memory.search where Hive answers), adds the hub's
// own notes, documents and artifacts, filters archived sessions unless
// asked, and merges by relevance and recency. Offline nodes are reported
// "not searched (offline)", never silently skipped. Owner clients and
// session principals may search (session principals already read any
// transcript through sessions.read); node principals may not.
func (h *Hub) Search(ctx context.Context, req contracts.SearchRequest) (contracts.SearchResponse, error) {
	start := time.Now()
	p, err := principal(ctx)
	if err != nil {
		return contracts.SearchResponse{}, err
	}
	if !isOwner(p) && p.Kind != contracts.PrincipalSession && p.Kind != contracts.PrincipalSystem {
		return contracts.SearchResponse{}, contracts.Errorf(contracts.ErrForbidden, "%s principals may not search", p.Kind)
	}
	if err := search.NormalizeRequest(&req); err != nil {
		return contracts.SearchResponse{}, err
	}
	budget := time.Duration(req.BudgetMS) * time.Millisecond
	nodes, err := h.searchNodes(ctx)
	if err != nil {
		return contracts.SearchResponse{}, err
	}
	terms := search.Terms(req.Query)
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		hits     []contracts.SearchHit
		statuses []contracts.SearchNodeStatus
		trunc    bool
	)
	add := func(hs ...contracts.SearchHit) {
		mu.Lock()
		hits = append(hits, hs...)
		mu.Unlock()
	}
	nodeLimit := min(req.Limit*2, 200)
	for _, n := range nodes {
		if req.NodeID != "" && n.NodeID != req.NodeID {
			continue
		}
		st := contracts.SearchNodeStatus{NodeID: n.NodeID, Name: n.Name}
		if c, _ := h.nodeConnected(ctx, h.db, n.NodeID); !c {
			st.State, st.Detail = contracts.SearchNodeOffline, "not searched (offline)"
			statuses = append(statuses, st)
			continue
		}
		if search.WantSource(req, contracts.SearchSourceTranscript) {
			wg.Add(1)
			go func(st contracts.SearchNodeStatus) {
				defer wg.Done()
				t0 := time.Now()
				var res contracts.TranscriptSearchResult
				params := contracts.TranscriptSearchParams{Query: req.Query, Since: req.Since, Limit: nodeLimit, BudgetMS: int(budget.Milliseconds() * 8 / 10)}
				state, detail := h.searchRoundTrip(ctx, p, st.NodeID, contracts.ActionTranscriptSearch, params, budget, &res)
				st.State, st.Detail, st.TookMS = state, detail, time.Since(t0).Milliseconds()
				if state == contracts.SearchNodeSearched {
					st.Hits, st.Partial = len(res.Hits), res.Partial
					if res.Partial {
						st.Detail = "partial: the node's time budget ran out"
					}
					hs := make([]contracts.SearchHit, 0, len(res.Hits))
					for _, th := range res.Hits {
						th.NodeID = st.NodeID
						hs = append(hs, search.FromTranscript(th))
					}
					add(hs...)
					if res.Truncated {
						mu.Lock()
						trunc = true
						mu.Unlock()
					}
				}
				mu.Lock()
				statuses = append(statuses, st)
				mu.Unlock()
			}(st)
		} else {
			st.State = contracts.SearchNodeSkipped
			statuses = append(statuses, st)
		}
		if search.WantSource(req, contracts.SearchSourceMemory) {
			wg.Add(1)
			go func(nodeID, name string) {
				defer wg.Done()
				var res contracts.MemorySearchResult
				// Memory is best effort: a node without Hive answers
				// unsupported and contributes nothing.
				if state, _ := h.searchRoundTrip(ctx, p, nodeID, contracts.ActionMemorySearch, contracts.MemorySearchParams{Query: req.Query, Limit: 20}, budget, &res); state != contracts.SearchNodeSearched {
					return
				}
				hs := make([]contracts.SearchHit, 0, len(res.Hits))
				for _, m := range res.Hits {
					if req.Since != nil && !m.At.IsZero() && m.At.Before(*req.Since) {
						continue
					}
					hs = append(hs, contracts.SearchHit{Source: contracts.SearchSourceMemory, NodeID: nodeID, SessionID: m.SessionRef,
						ProjectID: m.ProjectID, Ref: m.ID, Title: m.Title, Kind: m.Kind, At: m.At, Snippet: markSnippet(m.Snippet, terms),
						Provenance: "hive on " + name + ": " + m.Source})
				}
				add(hs...)
			}(n.NodeID, n.Name)
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		add(h.searchHubObjects(ctx, req, terms)...)
	}()
	wg.Wait()
	hits = h.applySessionFacts(ctx, hits, req.IncludeArchived)
	ranked, cut := search.Rank(hits, h.now(), req.Limit)
	search.SortNodes(statuses)
	if statuses == nil {
		statuses = []contracts.SearchNodeStatus{}
	}
	return contracts.SearchResponse{Query: req.Query, Hits: ranked, Nodes: statuses, Truncated: cut || trunc,
		TookMS: time.Since(start).Milliseconds(), SnapshotAt: h.now()}, nil
}

// searchNodes lists enrolled, non-revoked nodes.
func (h *Hub) searchNodes(ctx context.Context) ([]contracts.Node, error) {
	var out []contracts.Node
	if h.cfg.Fleet != nil {
		cur := ""
		for i := 0; i < 20; i++ {
			page, err := h.cfg.Fleet.ListNodes(h.systemCtx(ctx), contracts.ListNodesRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
			if err != nil {
				return nil, err
			}
			for _, n := range page.Items {
				if n.Connection.State != contracts.ConnRevoked && n.Labels[contracts.NodeLabelEnrollment] != "pending" {
					out = append(out, n)
				}
			}
			if cur = page.NextCursor; cur == "" {
				break
			}
		}
		return out, nil
	}
	rows, err := h.db.QueryContext(ctx, `SELECT node_id FROM node_state ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, contracts.Node{NodeID: id, Name: id})
	}
	return out, rows.Err()
}

// searchRoundTrip queues one node action as the caller and waits up to
// wait for its result. The command expires shortly after the wait, so an
// unanswered search never lingers in the node's queue.
func (h *Hub) searchRoundTrip(ctx context.Context, p contracts.Principal, nodeID, action string, params any, wait time.Duration, out any) (string, string) {
	raw, err := json.Marshal(params)
	if err != nil {
		return contracts.SearchNodeError, err.Error()
	}
	var cmd commandRec
	if err := h.tx(ctx, func(tx *sql.Tx) error {
		var err error
		cmd, err = h.enqueueTx(ctx, tx, nodeID, contracts.CmdAction, p,
			contracts.NodeActionRequest{NodeID: nodeID, Action: action, Params: raw}, wait+5*time.Second,
			contracts.ObjectRef{Kind: contracts.KindNode, ID: nodeID})
		return err
	}); err != nil {
		return contracts.SearchNodeError, contracts.AsError(err).Message
	}
	rec, err := h.waitCommand(ctx, cmd.CommandID, wait)
	if err != nil {
		return contracts.SearchNodeError, contracts.AsError(err).Message
	}
	if !rec.finished() {
		return contracts.SearchNodeTimeout, "no answer within " + wait.String()
	}
	if rec.Outcome != contracts.OutcomeSucceeded {
		e := contracts.AsError(commandError(rec))
		if e.Code == contracts.ErrUnsupported {
			return contracts.SearchNodeUnsupported, "this node's pyred has no " + action + " (upgrade it)"
		}
		return contracts.SearchNodeError, e.Message
	}
	if err := json.Unmarshal(rec.Result, out); err != nil {
		return contracts.SearchNodeError, "malformed result: " + err.Error()
	}
	return contracts.SearchNodeSearched, ""
}

// searchHubObjects matches the hub's notes, documents and artifacts
// (through the domain APIs with the caller's identity, so their own
// visibility rules apply).
func (h *Hub) searchHubObjects(ctx context.Context, req contracts.SearchRequest, terms []string) []contracts.SearchHit {
	var out []contracts.SearchHit
	since := func(t time.Time) bool { return req.Since == nil || !t.Before(*req.Since) }
	if h.cfg.Notes != nil && search.WantSource(req, contracts.SearchSourceNote) {
		page, err := h.cfg.Notes.ListNotes(ctx, contracts.ListNotesRequest{PageRequest: contracts.PageRequest{Limit: 200}})
		if err == nil {
			for _, n := range page.Items {
				if !since(n.CreatedAt) || !search.Matches(n.Title+" "+n.Body, terms) {
					continue
				}
				out = append(out, contracts.SearchHit{Source: contracts.SearchSourceNote, Ref: n.NoteID, SessionID: n.SessionID,
					ProjectID: deref(n.ProjectID), Title: n.Title, Kind: string(n.Kind), At: n.CreatedAt,
					Snippet: search.Snippet(n.Title+" — "+n.Body, terms, 160), Provenance: "note"})
			}
		}
	}
	if h.cfg.Documents != nil && search.WantSource(req, contracts.SearchSourceDocument) {
		page, err := h.cfg.Documents.ListDocuments(ctx, contracts.ListDocumentsRequest{PageRequest: contracts.PageRequest{Limit: 200}})
		if err == nil {
			for _, d := range page.Items {
				if !since(d.UpdatedAt) || !search.Matches(d.Title+" "+d.Slug, terms) {
					continue
				}
				out = append(out, contracts.SearchHit{Source: contracts.SearchSourceDocument, Ref: d.DocumentID, ProjectID: deref(d.ProjectID),
					Title: d.Title, At: d.UpdatedAt, Snippet: search.Snippet(d.Title, terms, 160), Provenance: "document " + d.Slug})
			}
		}
	}
	if h.cfg.Artifacts != nil && search.WantSource(req, contracts.SearchSourceArtifact) {
		page, err := h.cfg.Artifacts.ListArtifacts(ctx, contracts.ListArtifactsRequest{PageRequest: contracts.PageRequest{Limit: 200}})
		if err == nil {
			for _, a := range page.Items {
				if !since(a.UpdatedAt) || !search.Matches(a.Title+" "+a.Description, terms) {
					continue
				}
				out = append(out, contracts.SearchHit{Source: contracts.SearchSourceArtifact, Ref: a.ArtifactID, ProjectID: deref(a.ProjectID),
					Title: a.Title, At: a.UpdatedAt, Snippet: search.Snippet(a.Title+" — "+a.Description, terms, 160), Provenance: "artifact"})
			}
		}
	}
	return out
}

// applySessionFacts drops archived sessions' transcript hits (unless
// asked) and takes titles and projects from the hub's session records.
func (h *Hub) applySessionFacts(ctx context.Context, hits []contracts.SearchHit, includeArchived bool) []contracts.SearchHit {
	sessions := map[string]*contracts.Session{}
	out := hits[:0]
	for _, ht := range hits {
		if ht.SessionID != "" {
			s, ok := sessions[ht.SessionID]
			if !ok {
				if got, err := h.getSession(ctx, h.db, ht.SessionID); err == nil {
					s = &got
				}
				sessions[ht.SessionID] = s
			}
			if s != nil {
				if s.Archived && !includeArchived {
					continue
				}
				ht.Archived = s.Archived
				if ht.Source == contracts.SearchSourceTranscript {
					ht.Title = s.Title
					if s.ProjectID != nil {
						ht.ProjectID = *s.ProjectID
					}
				}
			}
		}
		out = append(out, ht)
	}
	return out
}

func markSnippet(s string, terms []string) string {
	if s == "" {
		return s
	}
	return search.Snippet(s, terms, 200)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
