package hub

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Node avoid flags (contracts/node_avoid.go): the owner or a session asks
// that new work stay off a machine until a time. The hub stores one flag
// per node and scope, attaches the active ones to Node.Avoid (nodes.list,
// nodes.get), refuses sessions.start on a hard-avoided node without the
// override (checkAvoid), skips avoided nodes in NodeSelector placement,
// and lists them in launch context and get_context (avoidContext). A flag
// is read against its expiry, so it lifts by itself; writes prune expired
// rows.

var nodeAvoidMigrations = []sqlitex.Migration{{Namespace: "hub_node_avoid", Version: 1, Name: "node_avoid", SQL: `
CREATE TABLE node_avoid (
	node_id TEXT NOT NULL,
	project_id TEXT NOT NULL DEFAULT '', -- '' = every placement
	hard INTEGER NOT NULL DEFAULT 0,
	reason TEXT NOT NULL DEFAULT '',
	set_by TEXT NOT NULL, -- principal JSON
	owner_set INTEGER NOT NULL DEFAULT 0,
	updated_by TEXT NOT NULL DEFAULT '', -- principal JSON of a session that tightened the owner's flag
	set_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	PRIMARY KEY (node_id, project_id)
);
CREATE INDEX node_avoid_expires ON node_avoid(expires_at);
`}}

var _ contracts.NodeAvoidAPI = (*Hub)(nil)

const avoidCols = `node_id,project_id,hard,reason,set_by,owner_set,updated_by,set_at,expires_at`

func scanAvoid(sc interface{ Scan(...any) error }) (contracts.NodeAvoid, error) {
	var a contracts.NodeAvoid
	var project, by, updated, setAt, expires string
	var hard, ownerSet int
	if err := sc.Scan(&a.NodeID, &project, &hard, &a.Reason, &by, &ownerSet, &updated, &setAt, &expires); err != nil {
		return a, err
	}
	if project != "" {
		a.ProjectID = &project
	}
	a.Hard, a.OwnerSet = hard != 0, ownerSet != 0
	_ = persistence.FromJSON(by, &a.SetBy)
	if updated != "" {
		var u contracts.Principal
		if persistence.FromJSON(updated, &u) == nil {
			a.UpdatedBy = &u
		}
	}
	a.SetAt = persistence.ParseTime(setAt)
	a.ExpiresAt = persistence.ParseTime(expires)
	return a, nil
}

// activeAvoids returns the flags in force at now, per node ("" nodeID:
// every node).
func (h *Hub) activeAvoids(ctx context.Context, q queryer, nodeID string, now time.Time) (map[string][]contracts.NodeAvoid, error) {
	where, args := `expires_at>?`, []any{persistence.Time(now)}
	if nodeID != "" {
		where += ` AND node_id=?`
		args = append(args, nodeID)
	}
	rows, err := q.QueryContext(ctx, `SELECT `+avoidCols+` FROM node_avoid WHERE `+where+` ORDER BY node_id, project_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]contracts.NodeAvoid{}
	for rows.Next() {
		a, err := scanAvoid(rows)
		if err != nil {
			return nil, err
		}
		if a.Active(now) { // the stored text compares like the time; this is the authority
			out[a.NodeID] = append(out[a.NodeID], a)
		}
	}
	return out, rows.Err()
}

// withAvoids attaches active flags to listed nodes. A read failure leaves
// them off (placement then ignores soft flags; sessions.start still
// enforces hard ones).
func (h *Hub) withAvoids(ctx context.Context, ns []contracts.Node) {
	if len(ns) == 0 {
		return
	}
	nodeID := ""
	if len(ns) == 1 {
		nodeID = ns[0].NodeID
	}
	m, err := h.activeAvoids(ctx, h.db, nodeID, h.now())
	if err != nil {
		return
	}
	for i := range ns {
		ns[i].Avoid = m[ns[i].NodeID]
	}
}

// avoidActor is who a flag is attributed to: the delegating principal when
// there is one.
func avoidActor(p contracts.Principal) contracts.Principal {
	if p.OnBehalfOf != nil {
		return *p.OnBehalfOf
	}
	return p
}

// AvoidNode implements nodes.avoid (contracts.NodeAvoidAPI).
func (h *Hub) AvoidNode(ctx context.Context, req contracts.AvoidNodeRequest) (contracts.AvoidNodeResponse, error) {
	var out contracts.AvoidNodeResponse
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	actor, byOwner := avoidActor(p), isOwner(p)
	if !byOwner && actor.Kind != contracts.PrincipalSession {
		return out, contracts.Errorf(contracts.ErrForbidden, "only the owner and sessions set node avoid flags")
	}
	nodeID := strings.TrimSpace(req.NodeID)
	if nodeID == "" {
		return out, contracts.Errorf(contracts.ErrInvalid, "node_id is required")
	}
	ni, err := h.nodeInfo(ctx, nodeID)
	if err != nil {
		return out, err
	}
	if !ni.Enrolled {
		return out, contracts.Errorf(contracts.ErrNotFound, "node is not enrolled").WithRef(contracts.KindNode, nodeID)
	}
	project := ""
	if req.ProjectID != nil {
		project = strings.TrimSpace(*req.ProjectID)
	}
	if project != "" && h.cfg.Projects != nil {
		if _, err := h.cfg.Projects.GetProject(h.systemCtx(ctx), contracts.GetRequest{ID: project}); err != nil {
			return out, err
		}
	}
	reason := strings.Join(strings.Fields(req.Reason), " ")
	now := h.now()
	var expires time.Time
	if !req.Clear {
		if req.ExpiresAt == nil || req.ExpiresAt.IsZero() {
			return out, contracts.Errorf(contracts.ErrInvalid, "expires_at is required: an avoid flag always lifts by itself (at most %s ahead)", shortDur(contracts.NodeAvoidMaxTTL))
		}
		expires = req.ExpiresAt.UTC()
		switch {
		case !expires.After(now):
			return out, contracts.Errorf(contracts.ErrInvalid, "expires_at %s is not in the future", expires.Format(time.RFC3339))
		case expires.After(now.Add(contracts.NodeAvoidMaxTTL + time.Minute)): // a minute of clock skew
			return out, contracts.Errorf(contracts.ErrInvalid, "an avoid flag lasts at most %s (expires_at %s)", shortDur(contracts.NodeAvoidMaxTTL), expires.Format(time.RFC3339))
		case len(reason) > contracts.NodeAvoidMaxReason:
			return out, contracts.Errorf(contracts.ErrInvalid, "reason is at most %d bytes", contracts.NodeAvoidMaxReason)
		}
	}
	err = h.mutate(ctx, req.OperationID, contracts.OpNodesAvoid, p, req, &out, func(tx *sql.Tx) (any, error) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM node_avoid WHERE expires_at<=?`, persistence.Time(now)); err != nil {
			return nil, err
		}
		cur, err := h.activeAvoids(ctx, tx, nodeID, now)
		if err != nil {
			return nil, err
		}
		res := contracts.AvoidNodeResponse{NodeID: nodeID, NodeName: ni.Name}
		if req.Clear {
			var targets []contracts.NodeAvoid
			for _, a := range cur[nodeID] {
				if req.ProjectID == nil || avoidProject(a) == project {
					targets = append(targets, a)
				}
			}
			for _, a := range targets {
				if a.OwnerSet && !byOwner {
					return nil, contracts.Errorf(contracts.ErrForbidden, "the owner set the avoid flag on %s (until %s); only the owner clears it", nodeLabel(ni), askClock(a.ExpiresAt)).WithRef(contracts.KindNode, nodeID)
				}
			}
			for _, a := range targets {
				if _, err := tx.ExecContext(ctx, `DELETE FROM node_avoid WHERE node_id=? AND project_id=?`, nodeID, avoidProject(a)); err != nil {
					return nil, err
				}
				h.auditTx(ctx, tx, auditRec{Action: contracts.AuditNodeAvoidCleared, Actor: actorOf(p), NodeID: nodeID, FromNote: avoidProject(a), At: now})
			}
			res.Cleared = len(targets)
		} else {
			next := contracts.NodeAvoid{NodeID: nodeID, Hard: req.Hard, Reason: reason, SetBy: actor, OwnerSet: byOwner, SetAt: now, ExpiresAt: expires}
			if project != "" {
				next.ProjectID = &project
			}
			for _, a := range cur[nodeID] {
				if avoidProject(a) != project || !a.OwnerSet || byOwner {
					continue
				}
				// The owner's flag: a session only tightens or extends it.
				merged := a
				merged.Hard = a.Hard || req.Hard
				if expires.After(a.ExpiresAt) {
					merged.ExpiresAt = expires
				}
				if merged.Reason == "" {
					merged.Reason = reason
				}
				merged.UpdatedBy = &actor
				if a.Hard && !req.Hard || expires.Before(a.ExpiresAt) {
					res.Note = fmt.Sprintf("the owner's flag on %s (%s until %s) is kept: a session only tightens or extends it", nodeLabel(ni), avoidStrength(a), askClock(a.ExpiresAt))
				}
				next = merged
			}
			updated := ""
			if next.UpdatedBy != nil {
				updated = persistence.JSON(*next.UpdatedBy)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO node_avoid(`+avoidCols+`) VALUES(?,?,?,?,?,?,?,?,?)
				ON CONFLICT(node_id, project_id) DO UPDATE SET hard=excluded.hard, reason=excluded.reason, set_by=excluded.set_by,
				owner_set=excluded.owner_set, updated_by=excluded.updated_by, set_at=excluded.set_at, expires_at=excluded.expires_at`,
				nodeID, project, boolInt(next.Hard), next.Reason, persistence.JSON(next.SetBy), boolInt(next.OwnerSet), updated,
				persistence.Time(next.SetAt), persistence.Time(next.ExpiresAt)); err != nil {
				return nil, err
			}
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditNodeAvoided, Actor: actorOf(p), NodeID: nodeID, To: avoidStrength(next),
				ToNote: askClock(next.ExpiresAt), FromNote: project, Reason: reason, At: now})
		}
		after, err := h.activeAvoids(ctx, tx, nodeID, now)
		if err != nil {
			return nil, err
		}
		res.Avoid = after[nodeID]
		if res.Avoid == nil {
			res.Avoid = []contracts.NodeAvoid{}
		}
		if err := h.emitPayload(ctx, tx, contracts.EvNodeAvoidChanged, contracts.ObjectRef{Kind: contracts.KindNode, ID: nodeID}, "", nil, "hub", 0, res); err != nil {
			return nil, err
		}
		return res, nil
	})
	return out, err
}

func avoidProject(a contracts.NodeAvoid) string {
	if a.ProjectID == nil {
		return ""
	}
	return *a.ProjectID
}

func avoidStrength(a contracts.NodeAvoid) string {
	if a.Hard {
		return "hard"
	}
	return "soft"
}

// shortDur renders whole days or hours ("7 days", "2h").
func shortDur(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	}
	return d.String()
}

// avoidExempt reports a start that places no agent work by choice: a
// headless `pyre run` record or a monitor.
func avoidExempt(req contracts.StartSessionRequest) bool {
	for _, a := range req.Args {
		if a == contracts.ArgPyreExec || strings.HasPrefix(a, contracts.ArgPyreExec+"=") {
			return true
		}
	}
	for _, t := range req.Tags {
		if t == contracts.TagMonitor {
			return true
		}
	}
	return false
}

// checkAvoid refuses a start on a node with a hard flag that covers the
// launch's project, unless the request overrides it and the caller may
// (the owner always; others only over a flag the owner did not set). Soft
// flags never refuse: the client warns.
func (h *Hub) checkAvoid(ctx context.Context, p contracts.Principal, ni nodeInfo, projectID *string, override bool) error {
	m, err := h.activeAvoids(ctx, h.db, ni.ID, h.now())
	if err != nil {
		return err
	}
	pid := ""
	if projectID != nil {
		pid = *projectID
	}
	a, ok := contracts.EffectiveAvoid(m[ni.ID], pid, h.now())
	if !ok || !a.Hard {
		return nil
	}
	if override && a.Overridable(isOwner(p)) {
		return nil
	}
	msg := fmt.Sprintf("node %s is avoided (hard) until %s", nodeLabel(ni), askClock(a.ExpiresAt))
	if a.Reason != "" {
		msg += ": " + a.Reason
	}
	if override {
		msg += "; the owner set it, so only the owner's credentials override it"
	} else {
		msg += "; pick another node, or pass --ignore-avoid (MCP ignore_avoid) to start there anyway"
	}
	return contracts.Errorf(contracts.ErrForbidden, "%s", msg).WithRef(contracts.KindNode, ni.ID).
		WithDetail("avoid_until", a.ExpiresAt.Format(time.RFC3339))
}

// avoidContextMax bounds the avoided machines listed in a context.
const avoidContextMax = 10

// avoidContext lists the machines new work should stay off for a session
// in projectID ("" when none): for the launch context and get_context.
func (h *Hub) avoidContext(ctx context.Context, projectID *string) string {
	now := h.now()
	m, err := h.activeAvoids(ctx, h.db, "", now)
	if err != nil || len(m) == 0 {
		return ""
	}
	pid := ""
	if projectID != nil {
		pid = *projectID
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var lines []string
	for _, id := range ids {
		a, ok := contracts.EffectiveAvoid(m[id], pid, now)
		if !ok {
			continue
		}
		if len(lines) == avoidContextMax {
			lines = append(lines, "- … more: `pyre nodes`")
			break
		}
		name := id
		if ni, err := h.nodeInfo(ctx, id); err == nil {
			name = nodeLabel(ni)
		}
		line := fmt.Sprintf("- %s: %s, until %s", name, avoidStrength(a), askClock(a.ExpiresAt))
		if a.Reason != "" {
			line += " — " + oneLineText(a.Reason, 120)
		}
		line += " (" + avoidSetter(a) + ")"
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "## Machines to keep new work off (temporary)\n\n" +
		"Don't start new sessions or workers on these until the time shown; sessions already there keep running. " +
		"`pyre start` without --node skips them; a hard one refuses even --node. `pyre nodes` shows the current list.\n" +
		strings.Join(lines, "\n") + "\n"
}

func avoidSetter(a contracts.NodeAvoid) string {
	s := "set by a session " + a.SetBy.ID
	if a.OwnerSet {
		s = "set by the owner"
	}
	if a.UpdatedBy != nil {
		s += ", extended by session " + a.UpdatedBy.ID
	}
	return s
}
