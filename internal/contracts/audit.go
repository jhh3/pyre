package contracts

import (
	"context"
	"time"
)

// Audit log (additive, W-audit). The hub keeps an owner-facing record of
// what Pyre did on its own and of changes to fleet state: which account a
// run got and why, automatic account switches, re-auth and cooldowns,
// tokens, session starts/stops/renames, node connections and upgrades, and
// dialogs a node answered by itself. Each entry says who/what/why/where/
// when and carries a plain-English Summary. Entries never contain secrets,
// message bodies or transcript text.
//
// Entries are derived inside the same transaction as the change they
// describe (from the event log and the hub's own mutations), kept for a
// bounded time (default 90 days / 100k entries) and listed newest first.
// Hubs that predate audit.list answer 404 (clients report unsupported); it
// is not part of API: callers type-assert AuditAPI.
const OpListAudit = "audit.list"

// AuditKind groups audit entries.
type AuditKind string

const (
	AuditAccount AuditKind = "account"
	AuditSession AuditKind = "session"
	AuditFleet   AuditKind = "fleet"
)

// Audit actions. Account:
const (
	AuditAccountChosen        = "account.chosen"         // a run got an account (Reason: the selection explanation)
	AuditAccountSwitched      = "account.switched"       // automatic switch after a limit/login failure (From -> To)
	AuditAccountWaiting       = "account.waiting"        // a limit hit and no other account was eligible
	AuditAccountReauthNeeded  = "account.reauth_needed"  // a machine's login expired or was rejected
	AuditAccountReauthed      = "account.reauthed"       // a machine's login is valid again
	AuditAccountCooldown      = "account.cooldown"       // limited until its reset
	AuditAccountCooldownEnded = "account.cooldown_ended" // usable again
	AuditAccountTokenAdded    = "account.token_added"    // owner stored a token
	AuditAccountTokenReplaced = "account.token_replaced" // owner replaced a token
	AuditAccountTokenRemoved  = "account.token_removed"  // owner removed a token
	AuditAccountTokenSynced   = "account.token_synced"   // a machine fetched the (new) token
	AuditAccountLoginAdded    = "account.login_added"    // a machine registered a login for the account
	AuditAccountAdded         = "account.added"          // first seen (owner registration or a node report)
	AuditAccountFolded        = "account.folded"         // merged into another account (From -> To)
	AuditAccountRenamed       = "account.renamed"        // label changed (From -> To)
	AuditAccountDisabled      = "account.disabled"       // excluded from new work
	AuditAccountEnabled       = "account.enabled"        // eligible for new work again
)

// Audit actions. Session:
const (
	AuditSessionStarted          = "session.started"
	AuditSessionStopped          = "session.stopped"
	AuditSessionInterrupted      = "session.interrupted"
	AuditSessionResumed          = "session.resumed" // To: "native resume" | "handoff"
	AuditSessionArchived         = "session.archived"
	AuditSessionUnarchived       = "session.unarchived"
	AuditSessionRenamed          = "session.renamed"     // From -> To
	AuditSessionAutoTitled       = "session.auto_titled" // From -> To
	AuditSessionLeaseTakenOver   = "session.lease_taken_over"
	AuditSessionReportedToParent = "session.reported_to_parent"
)

// Audit actions. Fleet:
const (
	AuditNodeEnrolled      = "node.enrolled"
	AuditNodeRevoked       = "node.revoked"
	AuditNodeConnected     = "node.connected"
	AuditNodeDisconnected  = "node.disconnected"
	AuditNodeUpgraded      = "node.upgraded"       // pyred version changed (From -> To)
	AuditNodeUpdateFailed  = "node.update_failed"  // an update failed, was refused or rolled back
	AuditHubUpgraded       = "hub.upgraded"        // pyre-hub version changed (From -> To)
	AuditNodeTrustAccepted = "node.trust_accepted" // a harness's folder-trust prompt answered for an owner-trusted dir
	AuditNodeExitConfirmed = "node.exit_confirmed" // Claude's "background work is running" exit dialog confirmed on stop
)

// AuditActor is who did it. Kind: "owner" | "session" | "node" | "pyre"
// (the hub or a node's own automation). Name is a display name resolved
// when listed (session title, node name).
type AuditActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// AuditEntry is one audit record. Display fields (NodeName, SessionTitle,
// AccountLabel, From/To labels and Summary) are resolved when listed, so a
// later rename or account fold shows current names.
type AuditEntry struct {
	AuditID   int64      `json:"audit_id"`
	At        time.Time  `json:"at"`
	Kind      AuditKind  `json:"kind"`
	Action    string     `json:"action"`
	Automatic bool       `json:"automatic"` // Pyre did it on its own (nobody asked)
	Actor     AuditActor `json:"actor"`

	NodeID       string `json:"node_id,omitempty"`
	NodeName     string `json:"node_name,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	SessionTitle string `json:"session_title,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`

	// From/To are the previous and new value (account labels for switches
	// and folds, titles for renames, versions for upgrades); FromAccountID/
	// ToAccountID are set for account switches and folds. FromNote/ToNote
	// carry context captured at the time (e.g. "weekly 96%").
	From          string `json:"from,omitempty"`
	To            string `json:"to,omitempty"`
	FromAccountID string `json:"from_account_id,omitempty"`
	ToAccountID   string `json:"to_account_id,omitempty"`
	FromNote      string `json:"from_note,omitempty"`
	ToNote        string `json:"to_note,omitempty"`
	Reason        string `json:"reason,omitempty"`

	// Summary is one plain-English line without time or machine, e.g.
	// "switched 'Credit perf' from code (weekly 96%) to me (weekly 0%) — limit reached".
	Summary string `json:"summary"`
}

// ListAuditRequest filters the audit log (newest first). Since/Until bound
// At; Kinds, SessionID, NodeID and AccountID narrow it. Limit is clamped
// (default 50, max 500).
type ListAuditRequest struct {
	PageRequest
	Since     *time.Time  `json:"since,omitempty"`
	Until     *time.Time  `json:"until,omitempty"`
	Kinds     []AuditKind `json:"kinds,omitempty"`
	SessionID string      `json:"session_id,omitempty"`
	NodeID    string      `json:"node_id,omitempty"`
	AccountID string      `json:"account_id,omitempty"`
}

// AuditAPI is implemented by hubs (and the mock backend, the client and
// the node-local proxy) that offer OpListAudit. Any authenticated
// principal may read it (it holds no secrets).
type AuditAPI interface {
	ListAudit(ctx context.Context, req ListAuditRequest) (Page[AuditEntry], error)
}

// EvNodeAutomation is a node-reported event (ReportEvent): the node's
// runtime answered a harness dialog by itself. Payload NodeAutomation. An
// older hub stores it as an ordinary event.
const EvNodeAutomation EventKind = "node.automation"

// Node automation actions.
const (
	AutomationTrustAccepted = "trust_accepted" // folder-trust prompt answered for an owner-trusted directory
	AutomationExitConfirmed = "exit_confirmed" // Claude's exit dialog confirmed ("Exit and stop tasks") on stop
)

// NodeAutomation is the payload of node.automation. Detail is a short
// nonsecret description (e.g. the trusted directory); never screen text.
type NodeAutomation struct {
	RunID     string  `json:"run_id"`
	SessionID string  `json:"session_id,omitempty"`
	Harness   Harness `json:"harness,omitempty"`
	Action    string  `json:"action"`
	Detail    string  `json:"detail,omitempty"`
}
