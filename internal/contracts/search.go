package contracts

import (
	"context"
	"time"
)

// Global search (W-search, additive). One query over every Pyre session
// transcript on every node, plus memory (Hive, where present on a node) and
// the hub's notes, documents and artifacts. Proposed for the next contract
// minor; nothing existing changes.
//
//   - Node action ActionTranscriptSearch: each node answers for its own
//     runs from an incremental local full-text index (SQLite FTS5 in the
//     node database), within a time budget and a hit limit.
//   - Hub operation OpSearch (POST /v1/search.query): the hub fans the query
//     out to every connected node in parallel, merges and ranks by relevance
//     and recency, and reports each node's outcome (offline nodes: "not
//     searched (offline)"). Hubs that do not implement it answer 404, so
//     clients fall back or report it.
//
// Visibility follows the existing reads: owner clients and session
// principals (which already read any session transcript through
// sessions.read) may search; node principals may not.

// ActionTranscriptSearch: TranscriptSearchParams -> TranscriptSearchResult.
const ActionTranscriptSearch = "transcript.search"

// OpSearch is the hub's fan-out search: SearchRequest -> SearchResponse.
const OpSearch = "search.query"

// Snippet match markers: snippets carry the matched terms between these.
const (
	SearchMarkOpen  = "«" // «
	SearchMarkClose = "»" // »
)

// Search sources.
const (
	SearchSourceTranscript = "transcript"
	SearchSourceMemory     = "memory"
	SearchSourceNote       = "note"
	SearchSourceDocument   = "document"
	SearchSourceArtifact   = "artifact"
)

// Per-node search states in SearchResponse.Nodes.
const (
	SearchNodeSearched    = "searched"
	SearchNodeOffline     = "offline"     // not searched (offline)
	SearchNodeTimeout     = "timeout"     // asked, no answer within the budget
	SearchNodeUnsupported = "unsupported" // node's pyred predates transcript.search
	SearchNodeError       = "error"
	SearchNodeSkipped     = "skipped" // filtered out by node_id
)

// TranscriptSearchParams is the node action payload.
type TranscriptSearchParams struct {
	Query string     `json:"query"`
	Since *time.Time `json:"since,omitempty"`
	// SessionIDs restricts the search to these sessions (empty: all).
	SessionIDs []string `json:"session_ids,omitempty"`
	Limit      int      `json:"limit,omitempty"`     // default 50, max 200
	BudgetMS   int      `json:"budget_ms,omitempty"` // default 1500, max 10000
}

// TranscriptHit is one matching transcript entry on a node. Seq is the
// entry's sequence number as sessions.read numbers it for that run, so a
// reader can open the run at Cursor = Seq-1 (exact for Claude; Codex
// numbering follows the rollout and readers locate the match by text).
type TranscriptHit struct {
	SessionID string    `json:"session_id"`
	RunID     string    `json:"run_id"`
	NodeID    string    `json:"node_id"`
	Harness   Harness   `json:"harness"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"` // user | assistant | tool
	Seq       int64     `json:"seq"`
	At        time.Time `json:"at"`
	Snippet   string    `json:"snippet"` // matches between SearchMarkOpen/Close
	Score     float64   `json:"score"`   // relevance, higher is better
}

type TranscriptSearchResult struct {
	Hits      []TranscriptHit `json:"hits"`
	Truncated bool            `json:"truncated"`         // more hits than Limit
	Partial   bool            `json:"partial,omitempty"` // budget ran out (index catch-up or query)
	Runs      int             `json:"runs"`              // runs in the node's index
	Pending   int             `json:"pending"`           // runs with unindexed transcript bytes
	TookMS    int64           `json:"took_ms"`
	Freshness Freshness       `json:"freshness"`
}

// SearchRequest is the hub fan-out query.
type SearchRequest struct {
	Query  string     `json:"query"`
	NodeID string     `json:"node_id,omitempty"` // one node only
	Since  *time.Time `json:"since,omitempty"`
	Limit  int        `json:"limit,omitempty"` // default 40, max 200
	// IncludeArchived includes archived sessions' transcripts.
	IncludeArchived bool `json:"include_archived,omitempty"`
	// Sources limits what is searched (default: all).
	Sources  []string `json:"sources,omitempty"`
	BudgetMS int      `json:"budget_ms,omitempty"` // default 2500, max 15000
}

// SearchHit is one merged result. Transcript hits carry the session, run
// and Seq to open; other sources carry a Ref (memory id, note id, document
// id, artifact id) and their provenance.
type SearchHit struct {
	Source     string    `json:"source"`
	NodeID     string    `json:"node_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	RunID      string    `json:"run_id,omitempty"`
	ProjectID  string    `json:"project_id,omitempty"`
	Ref        string    `json:"ref,omitempty"`
	Title      string    `json:"title"`
	Kind       string    `json:"kind,omitempty"`
	Seq        int64     `json:"seq,omitempty"`
	At         time.Time `json:"at"`
	Snippet    string    `json:"snippet"`
	Score      float64   `json:"score"` // merged relevance x recency
	Archived   bool      `json:"archived,omitempty"`
	Provenance string    `json:"provenance,omitempty"`
}

// SearchNodeStatus says what happened on each node.
type SearchNodeStatus struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	State  string `json:"state"` // SearchNode*
	Detail string `json:"detail,omitempty"`
	Hits   int    `json:"hits"`
	TookMS int64  `json:"took_ms"`
	// Partial: the node's budget ran out (index catch-up or query).
	Partial bool `json:"partial,omitempty"`
}

type SearchResponse struct {
	Query      string             `json:"query"`
	Hits       []SearchHit        `json:"hits"`
	Nodes      []SearchNodeStatus `json:"nodes"`
	Truncated  bool               `json:"truncated"`
	TookMS     int64              `json:"took_ms"`
	SnapshotAt time.Time          `json:"snapshot_at"`
}

// SearchAPI is implemented by hubs (and the mock backend, the client and
// the node-local proxy) that offer OpSearch. It is deliberately not part
// of API: callers type-assert.
type SearchAPI interface {
	Search(ctx context.Context, req SearchRequest) (SearchResponse, error)
}
