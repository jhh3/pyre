package contracts

import (
	"context"
	"time"
)

// Standing grants (W-grants, additive, optional): a lasting go-ahead from
// the owner ("merge PRs once reviewed and green") that answers matching
// asks without asking him again.
//
// Who creates one: only the owner (grants.create refuses every other
// principal). A session PROPOSES one (CreateAskRequest.Proposal, MCP
// propose_grant): that is an ordinary owner ask with the options approve /
// reject and no default; when the owner answers approve, the hub itself
// writes the grant with his answer text (if he typed one) as Quote and the
// ask as AskID. Nothing a session writes is ever stored as the owner's
// words.
//
// Scope:
//   - "project": every session in ProjectID holds it;
//   - "session": SessionID and every session it starts, transitively.
//
// Matching (asks.create, sessions only): an ask is answered by a held grant
// only when all of these hold — it names Action explicitly (no topic-key
// guess); it offers the grant's Option (by option id or label,
// case-insensitive), which is the option chosen; it has a DefaultOption
// and is not Irreversible; it is not a proposal, a reopen, or on a topic
// that is open or was decided in the last 7 days (those checks run first);
// neither its action, topic, title nor the chosen option is on the hub's
// deny-list (production deploys, deleting or dropping data, spending money,
// force-push or history rewrites, public releases or publishing, account or
// credential changes); and the grant has not expired or been revoked. The
// asker's own session tree and project are used; a project_id in the
// request does not widen what is matched (an ask for another project is
// never grant-answered). Deny-listed actions are never grantable either.
//
// Condition is an informational note shown with the grant; the hub never
// evaluates it. Grants expire (ExpiresAt; default 7 days, at most 30). Each
// create and each ask a grant answers raises an informational inbox item
// (AttnGrantFYI, one item per grant collecting its uses).
//
// grants.list: the owner lists anything; a session lists only grants of
// its own project or session tree (default: those holding for itself).
// grants.revoke: the owner, or a session the grant holds for (revoking only
// narrows).
const (
	OpGrantsCreate = "grants.create"
	OpGrantsList   = "grants.list"
	OpGrantsRevoke = "grants.revoke"
)

// FeatureGrants is the Hello feature advertising the grants ops.
const FeatureGrants = "standing_grants"

// PrefixStandingGrant is the standing grant ID prefix (PrefixGrant is the
// device grant's).
const PrefixStandingGrant = "sgr"

// KindStandingGrant is the ObjectRef kind of a standing grant.
const KindStandingGrant ObjectKind = "standing_grant"

// EvStandingGrantChanged is emitted when a standing grant is created or revoked
// (payload: StandingGrant).
const EvStandingGrantChanged EventKind = "standing_grant.changed"

// AttnGrantFYI is the informational inbox item for a standing grant (group
// "grant:<grant_id>"): raised when it is created and raised again (Count)
// each time it answers an ask; handled when it is revoked. It never pushes
// unless the owner enables the kind in NotificationPrefs.Kinds.
const AttnGrantFYI AttentionKind = "grant_fyi"

type GrantScope string

const (
	GrantScopeProject GrantScope = "project"
	GrantScopeSession GrantScope = "session"
)

// Limits.
const (
	GrantMaxAction    = 40
	GrantMaxCondition = 500
	GrantMaxQuote     = 2000
	GrantDefaultTTL   = 7 * 24 * time.Hour
	GrantMaxTTL       = 30 * 24 * time.Hour
	FactMaxText       = 500
)

type StandingGrant struct {
	GrantID   string     `json:"grant_id"`
	Scope     GrantScope `json:"scope"`
	ProjectID *string    `json:"project_id"`           // scope project (also set for session grants in a project)
	SessionID string     `json:"session_id,omitempty"` // scope session: the root of the tree
	// Action is a normalized slug ("merge"); asks must name it (Ask.Action).
	Action string `json:"action"`
	// Option is the ask option the grant chooses (an option id or label);
	// an ask without it is not answered.
	Option string `json:"option"`
	// Condition is an informational note ("once reviewed and CI green").
	// The hub never evaluates it.
	Condition string `json:"condition,omitempty"`
	// Quote is the owner's own words: what he typed when creating it or
	// with his approval. "" when he approved a proposal without typing.
	Quote string `json:"quote,omitempty"`
	// AskID is the proposal ask the owner approved ("" when he created the
	// grant directly).
	AskID string `json:"ask_id,omitempty"`
	// ProposedBy is the session that proposed it (nil when the owner
	// created it directly).
	ProposedBy   *Principal `json:"proposed_by,omitempty"`
	RecordedBy   Principal  `json:"recorded_by"` // always the owner
	ExpiresAt    time.Time  `json:"expires_at"`
	UsedCount    int        `json:"used_count"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokeReason string     `json:"revoke_reason,omitempty"`
}

// Active reports whether the grant still answers asks at now.
func (g StandingGrant) Active(now time.Time) bool {
	return g.RevokedAt == nil && now.Before(g.ExpiresAt)
}

// CreateGrantRequest is the owner's (only the owner's) direct grant.
type CreateGrantRequest struct {
	Mutation
	Scope     GrantScope `json:"scope"`                // default: project when ProjectID is set, else session
	ProjectID *string    `json:"project_id,omitempty"` // scope project
	SessionID string     `json:"session_id,omitempty"` // scope session: the root of the tree
	Action    string     `json:"action"`
	Option    string     `json:"option"`              // required: the ask option it chooses
	Condition string     `json:"condition,omitempty"` // informational note
	Quote     string     `json:"quote,omitempty"`     // optional: the owner's words
	// ExpiresAt defaults to GrantDefaultTTL from now; at most GrantMaxTTL.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ListGrantsRequest lists active grants (and revoked or expired ones on
// request). With SessionID it lists the grants that hold for that session:
// its own tree's (itself and its ancestors) and its project's.
type ListGrantsRequest struct {
	ProjectID      *string `json:"project_id,omitempty"`
	SessionID      string  `json:"session_id,omitempty"`
	Action         string  `json:"action,omitempty"`
	IncludeRevoked bool    `json:"include_revoked,omitempty"` // revoked and expired too
}

type ListGrantsResponse struct {
	Grants []StandingGrant `json:"grants"`
}

type RevokeGrantRequest struct {
	Mutation
	GrantID string `json:"grant_id"`
	Reason  string `json:"reason,omitempty"`
}

// StandingGrantsAPI is implemented by hubs (and the client and the
// node-local proxy) that offer the grants ops.
type StandingGrantsAPI interface {
	CreateGrant(ctx context.Context, req CreateGrantRequest) (StandingGrant, error)
	ListGrants(ctx context.Context, req ListGrantsRequest) (ListGrantsResponse, error)
	RevokeGrant(ctx context.Context, req RevokeGrantRequest) (StandingGrant, error)
}

// ---- proposals ----

// A proposal (CreateAskRequest.Proposal) is an owner ask asking him to
// approve a standing grant or a project fact. The hub writes the ask's
// title, structured context and options (ProposalApprove /
// ProposalReject); the session's Context is kept as its reason, labeled
// as the session's words. A proposal has no default (a DefaultOption other
// than reject is refused), is never answered by a grant, and follows the
// ask rules otherwise (a child's proposal goes to its parent). When the
// owner answers approve, the hub writes the grant (Proposal.ResultID =
// grant ID) or appends the fact to the project's fact sheet
// (FactSheetSlug; ResultID = revision ID), quoting his answer text when he
// typed one and citing the ask ID. Reject, no answer or a withdrawal
// writes nothing.
type ProposalKind string

const (
	ProposalGrant ProposalKind = "grant"
	ProposalFact  ProposalKind = "fact"
)

// Proposal options.
const (
	ProposalApprove = "approve"
	ProposalReject  = "reject"
)

// AskProposalState is what the hub did with a proposal.
type AskProposalState string

const (
	AskProposalPending  AskProposalState = "pending"  // not answered (or expired/withdrawn: see the ask)
	AskProposalApproved AskProposalState = "approved" // approved; the fact is being written
	AskProposalApplied  AskProposalState = "applied"  // written (ResultID)
	AskProposalRejected AskProposalState = "rejected" // answered with anything but approve
	AskProposalFailed   AskProposalState = "failed"   // approved but could not be written (Error)
)

type AskProposal struct {
	Kind  ProposalKind   `json:"kind"`
	Grant *GrantProposal `json:"grant,omitempty"`
	// Fact is the proposed fact sheet line (kind fact).
	Fact string `json:"fact,omitempty"`
	// Set by the hub.
	State    AskProposalState `json:"state,omitempty"`
	ResultID string           `json:"result_id,omitempty"`
	Error    string           `json:"error,omitempty"`
}

// GrantProposal is a proposed standing grant (defaults as in
// CreateGrantRequest; scope session roots at the proposer or a session it
// started).
type GrantProposal struct {
	Scope     GrantScope `json:"scope,omitempty"`
	SessionID string     `json:"session_id,omitempty"`
	Action    string     `json:"action"`
	Option    string     `json:"option"`
	Condition string     `json:"condition,omitempty"`
	// ExpiresInDays defaults to 7, at most 30.
	ExpiresInDays int `json:"expires_in_days,omitempty"`
}
