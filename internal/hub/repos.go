package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/repoindex"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Repo index and session worktrees (contracts/repos.go). Nodes report a
// replaceable repo snapshot and per-session worktree snapshots; the hub
// stores the latest of each and answers repos.list (fuzzy, recent-first,
// across nodes, enriched with where sessions last ran) and
// sessions.worktrees.

var _ contracts.ReposAPI = (*Hub)(nil)

// repoMigrations live in their own namespace (additive).
var repoMigrations = []sqlitex.Migration{{Namespace: "hub_repos", Version: 1, Name: "repo_index", SQL: `
CREATE TABLE node_repos (
	node_id TEXT PRIMARY KEY,
	idx TEXT NOT NULL,
	reported_at TEXT NOT NULL
);
CREATE TABLE session_worktrees (
	session_id TEXT PRIMARY KEY,
	node_id TEXT NOT NULL,
	payload TEXT NOT NULL,
	reported_at TEXT NOT NULL
);
`}}

func init() {
	// Agents may create fresh worktrees for the sessions they launch (MCP
	// start_session repo/fresh) and read a session's worktrees; removal
	// stays owner-only.
	sessionActions[contracts.ActionWorktreeCreate] = true
	sessionActions[contracts.ActionWorktreeScan] = true
}

const (
	maxReposPerNode = 5000
	maxRepoBytes    = 4 << 20
)

// applyRepoReportTx ingests the repo-index report kinds; ok is false for
// any other kind.
func (h *Hub) applyRepoReportTx(ctx context.Context, tx *sql.Tx, nodeID string, rep contracts.NodeReport) (bool, error) {
	switch rep.Kind {
	case contracts.ReportRepos:
		if len(rep.Payload) > maxRepoBytes {
			return true, contracts.Errorf(contracts.ErrInvalid, "repo index too large (%d bytes)", len(rep.Payload))
		}
		idx, err := decode[contracts.NodeRepoIndex](rep.Payload)
		if err != nil {
			return true, err
		}
		if idx.NodeID != "" && idx.NodeID != nodeID {
			return true, contracts.Errorf(contracts.ErrForbidden, "node %s cannot report repos for %s", nodeID, idx.NodeID)
		}
		if len(idx.Repos) > maxReposPerNode {
			idx.Repos, idx.Truncated = idx.Repos[:maxReposPerNode], true
		}
		idx.NodeID = nodeID
		_, err = tx.ExecContext(ctx, `INSERT INTO node_repos(node_id,idx,reported_at) VALUES(?,?,?) ON CONFLICT(node_id) DO UPDATE SET idx=excluded.idx, reported_at=excluded.reported_at`,
			nodeID, persistence.JSON(idx), persistence.Time(h.now()))
		return true, err
	case contracts.ReportSessionWorktrees:
		sw, err := decode[contracts.SessionWorktrees](rep.Payload)
		if err != nil {
			return true, err
		}
		if sw.SessionID == "" {
			return true, contracts.Errorf(contracts.ErrInvalid, "session worktrees need session_id")
		}
		if sw.RunID != "" {
			if run, err := h.getRun(ctx, tx, sw.RunID); err == nil && run.NodeID != nodeID {
				return true, contracts.Errorf(contracts.ErrForbidden, "node %s cannot report worktrees for run %s", nodeID, sw.RunID)
			}
		}
		sw.NodeID = nodeID
		if sw.ReportedAt.IsZero() {
			sw.ReportedAt = h.now()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO session_worktrees(session_id,node_id,payload,reported_at) VALUES(?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET node_id=excluded.node_id, payload=excluded.payload, reported_at=excluded.reported_at`,
			sw.SessionID, nodeID, persistence.JSON(sw), persistence.Time(h.now()))
		return true, err
	}
	return false, nil
}

// ListRepos searches every node's repo index.
func (h *Hub) ListRepos(ctx context.Context, req contracts.ListReposRequest) (contracts.RepoList, error) {
	if _, err := principal(ctx); err != nil {
		return contracts.RepoList{}, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	out := contracts.RepoList{Items: []contracts.RepoEntry{}, Nodes: map[string]time.Time{}}
	q := `SELECT node_id, idx, reported_at FROM node_repos`
	args := []any{}
	if req.NodeID != "" {
		q += ` WHERE node_id=?`
		args = append(args, req.NodeID)
	}
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return out, err
	}
	type nodeIdx struct {
		id  string
		idx contracts.NodeRepoIndex
	}
	var idxs []nodeIdx
	for rows.Next() {
		var id, raw, at string
		if err := rows.Scan(&id, &raw, &at); err != nil {
			rows.Close()
			return out, err
		}
		var idx contracts.NodeRepoIndex
		if json.Unmarshal([]byte(raw), &idx) != nil {
			continue
		}
		idxs = append(idxs, nodeIdx{id, idx})
		out.Nodes[id] = idx.IndexedAt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	used, err := h.lastUsedDirs(ctx)
	if err != nil {
		return out, err
	}
	var all []contracts.RepoEntry
	for _, ni := range idxs {
		info, err := h.nodeInfo(ctx, ni.id)
		if err != nil || info.Revoked {
			delete(out.Nodes, ni.id)
			continue
		}
		name := info.Name
		if name == "" {
			name = ni.id
		}
		for _, r := range ni.idx.Repos {
			e := contracts.RepoEntry{Repo: r, NodeID: ni.id, NodeName: name}
			if t := repoLastUsed(used[ni.id], r.Path); !t.IsZero() {
				e.LastUsedAt = &t
			}
			all = append(all, e)
		}
	}
	ranked := repoindex.Rank(req.Query, all)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out.Items = append(out.Items, ranked...)
	return out, nil
}

// lastUsedDirs maps node -> run working directory -> latest run activity.
func (h *Hub) lastUsedDirs(ctx context.Context) (map[string]map[string]time.Time, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT node_id, json_extract(run,'$.cwd'), MAX(updated_at) FROM runs GROUP BY node_id, json_extract(run,'$.cwd')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]time.Time{}
	for rows.Next() {
		var node string
		var cwd, at sql.NullString
		if err := rows.Scan(&node, &cwd, &at); err != nil {
			return nil, err
		}
		if !cwd.Valid || cwd.String == "" || !at.Valid {
			continue
		}
		t := persistence.ParseTime(at.String)
		if t.IsZero() {
			continue
		}
		if out[node] == nil {
			out[node] = map[string]time.Time{}
		}
		out[node][filepath.Clean(cwd.String)] = t
	}
	return out, rows.Err()
}

// repoLastUsed: the latest run in the repo, a directory below it (its
// .worktrees, .claude/worktrees) or its <repo>-worktrees sibling.
func repoLastUsed(dirs map[string]time.Time, repo string) time.Time {
	var best time.Time
	sib := repo + "-worktrees" + string(filepath.Separator)
	for d, t := range dirs {
		if (d == repo || strings.HasPrefix(d, repo+string(filepath.Separator)) || strings.HasPrefix(d, sib)) && t.After(best) {
			best = t
		}
	}
	return best
}

// SessionWorktrees returns the session's last reported worktrees.
func (h *Hub) SessionWorktrees(ctx context.Context, req contracts.SessionWorktreesRequest) (contracts.SessionWorktrees, error) {
	if _, err := principal(ctx); err != nil {
		return contracts.SessionWorktrees{}, err
	}
	if req.SessionID == "" {
		return contracts.SessionWorktrees{}, contracts.Errorf(contracts.ErrInvalid, "session_id is required")
	}
	var raw string
	err := h.db.QueryRowContext(ctx, `SELECT payload FROM session_worktrees WHERE session_id=?`, req.SessionID).Scan(&raw)
	if err == sql.ErrNoRows {
		if _, gerr := h.getSession(ctx, h.db, req.SessionID); gerr != nil {
			return contracts.SessionWorktrees{}, gerr
		}
		return contracts.SessionWorktrees{SessionID: req.SessionID, Repos: []contracts.RepoWorktrees{}}, nil
	}
	if err != nil {
		return contracts.SessionWorktrees{}, err
	}
	var sw contracts.SessionWorktrees
	if err := json.Unmarshal([]byte(raw), &sw); err != nil {
		return contracts.SessionWorktrees{}, err
	}
	return sw, nil
}
