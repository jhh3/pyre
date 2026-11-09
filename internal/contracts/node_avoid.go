package contracts

import (
	"context"
	"time"
)

// Node avoid flags (additive, optional): the owner, or a session, asks
// that new work stay off a machine for a while (presenting, a timed
// benchmark, heavy local work). The flag lifts by itself at ExpiresAt:
// every reader evaluates it against the time, so no cleanup job is needed.
// It only affects new placement; sessions already on the node keep
// running, and resumes of existing sessions are not refused.
//
//   - soft (default): automatic placement (`pyre start`/`pyre new`/MCP
//     start_session without a node, the hub's NodeSelector) skips the node
//     unless nothing else fits; an explicit node still starts there (the
//     CLI warns). Agents are told through `pyre nodes`, get_context and the
//     launch context not to place manual workers there.
//   - hard: sessions.start refuses the node (forbidden) unless the request
//     sets StartSessionRequest.IgnoreAvoid. A hard flag the owner set
//     (OwnerSet) is overridden only by the owner's credentials; one a
//     session set by any caller that passes the override.
//
// Who sets it: the owner and sessions (nodes.avoid; a node acting for
// itself is refused). One flag per node and scope (ProjectID nil = every
// placement, else only placements for that project). Precedence: the
// owner's flag (OwnerSet) cannot be cleared or weakened by a session: a
// session's request on it only tightens (hard wins) and extends (the later
// expiry wins), and the owner's reason and attribution are kept
// (UpdatedBy names the session). The owner replaces or clears any flag.
// A session's flag may be replaced or cleared by any session or the owner.
//
// Headless records (`pyre run`, ArgPyreExec), monitors (TagMonitor) and
// starts by a node principal on itself are never refused: they place no
// new agent work by choice.
const OpNodesAvoid = "nodes.avoid"

// FeatureNodeAvoid is the Hello feature advertising nodes.avoid and
// Node.Avoid.
const FeatureNodeAvoid = "node_avoid"

// Limits.
const (
	NodeAvoidDefaultTTL = 2 * time.Hour
	NodeAvoidMaxTTL     = 7 * 24 * time.Hour
	NodeAvoidMaxReason  = 200
)

// EvNodeAvoidChanged is emitted when a node's avoid flags change (payload:
// AvoidNodeResponse).
const EvNodeAvoidChanged EventKind = "node.avoid_changed"

// NodeAvoid is one active avoid flag on a node.
type NodeAvoid struct {
	NodeID string `json:"node_id"`
	// ProjectID limits the flag to placements for that project (nil: every
	// placement).
	ProjectID *string `json:"project_id,omitempty"`
	Hard      bool    `json:"hard"`
	Reason    string  `json:"reason,omitempty"`
	// SetBy is who set the flag (the owner, or a session); OwnerSet says
	// the owner did, so sessions cannot clear or weaken it.
	SetBy    Principal `json:"set_by"`
	OwnerSet bool      `json:"owner_set"`
	// UpdatedBy is a session that tightened or extended the owner's flag
	// (nil otherwise).
	UpdatedBy *Principal `json:"updated_by,omitempty"`
	SetAt     time.Time  `json:"set_at"`
	ExpiresAt time.Time  `json:"expires_at"`
}

// Active reports whether the flag still holds at now.
func (a NodeAvoid) Active(now time.Time) bool { return now.Before(a.ExpiresAt) }

// AppliesTo reports whether the flag covers a placement for projectID
// ("" = a placement outside any project).
func (a NodeAvoid) AppliesTo(projectID string) bool {
	return a.ProjectID == nil || *a.ProjectID == "" || *a.ProjectID == projectID
}

// Overridable reports whether a caller may start on a node despite this
// hard flag (with the override): always the owner; anyone else only when
// the owner did not set it.
func (a NodeAvoid) Overridable(byOwner bool) bool { return byOwner || !a.OwnerSet }

// EffectiveAvoid is the flag that governs a placement for projectID at now
// among flags: an active hard flag first (the latest expiry), else the
// active soft flag that lasts longest; ok false when none applies.
func EffectiveAvoid(flags []NodeAvoid, projectID string, now time.Time) (NodeAvoid, bool) {
	var best NodeAvoid
	ok := false
	for _, a := range flags {
		if !a.Active(now) || !a.AppliesTo(projectID) {
			continue
		}
		if !ok || a.Hard && !best.Hard || a.Hard == best.Hard && a.ExpiresAt.After(best.ExpiresAt) {
			best, ok = a, true
		}
	}
	return best, ok
}

// AvoidNodeRequest sets (or with Clear removes) a node's avoid flag.
type AvoidNodeRequest struct {
	Mutation
	NodeID string `json:"node_id"`
	// ProjectID scopes the flag to one project's placements (nil: every
	// placement). With Clear, nil clears every flag on the node.
	ProjectID *string `json:"project_id,omitempty"`
	Hard      bool    `json:"hard,omitempty"`
	Reason    string  `json:"reason,omitempty"` // at most NodeAvoidMaxReason bytes
	// ExpiresAt is required to set a flag: after now and at most
	// NodeAvoidMaxTTL ahead (an unbounded flag is refused).
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Clear     bool       `json:"clear,omitempty"`
}

// AvoidNodeResponse is the node's active flags after the change.
type AvoidNodeResponse struct {
	NodeID   string      `json:"node_id"`
	NodeName string      `json:"node_name,omitempty"`
	Avoid    []NodeAvoid `json:"avoid"`
	Cleared  int         `json:"cleared,omitempty"` // flags removed by a clear
	// Note explains a request that was merged into the owner's flag (a
	// session cannot weaken it).
	Note string `json:"note,omitempty"`
}

// NodeAvoidAPI is implemented by hubs (and the client and the node-local
// proxy) that offer nodes.avoid. Flags are read on Node.Avoid
// (nodes.list, nodes.get).
type NodeAvoidAPI interface {
	AvoidNode(ctx context.Context, req AvoidNodeRequest) (AvoidNodeResponse, error)
}
