package contracts

import "time"

// EventKind names a durable or observational event.
type EventKind string

const (
	EvNodeEnrolled       EventKind = "node.enrolled"
	EvNodeStatus         EventKind = "node.status" // replaceable observation
	EvNodeConnection     EventKind = "node.connection"
	EvNodeRevoked        EventKind = "node.revoked"
	EvSessionCreated     EventKind = "session.created"
	EvSessionUpdated     EventKind = "session.updated"
	EvRunStarted         EventKind = "run.started"
	EvRunObserved        EventKind = "run.observed"
	EvRunEnded           EventKind = "run.ended"
	EvMessageStored      EventKind = "message.stored"
	EvMessageDelivery    EventKind = "message.delivery"
	EvMessageAck         EventKind = "message.ack"
	EvApprovalRequested  EventKind = "approval.requested"
	EvApprovalResolved   EventKind = "approval.resolved"
	EvApprovalWithdrawn  EventKind = "approval.withdrawn"
	EvLeaseChanged       EventKind = "lease.changed"
	EvProjectChanged     EventKind = "project.changed"
	EvDocumentRevised    EventKind = "document.revised"
	EvDocumentConflict   EventKind = "document.conflict"
	EvArtifactPublished  EventKind = "artifact.published"
	EvCommentCreated     EventKind = "comment.created"
	EvCommentResolved    EventKind = "comment.resolved"
	EvNoteCreated        EventKind = "note.created"
	EvAccountObserved    EventKind = "account.observed"
	EvAccountAssigned    EventKind = "account.assigned"
	EvGrantChanged       EventKind = "account.grant" // DeviceGrant; state dead/expired needs owner action
	EvAccountExhausted   EventKind = "account.exhausted"
	EvFallbackPlanned    EventKind = "account.fallback"
	EvAttentionChanged   EventKind = "attention.changed"
	EvEnvironmentApplied EventKind = "environment.applied"
	EvEnvironmentDrift   EventKind = "environment.drift"
	EvUpdateStaged       EventKind = "update.staged"
	EvUpdateResult       EventKind = "update.result"
	EvCommandResult      EventKind = "command.result"
	EvConnectionChanged  EventKind = "connection.changed"
	EvArtifactChanged    EventKind = "artifact.changed"
	EvAccountChanged     EventKind = "account.changed"
	EvAccountLease       EventKind = "account.lease" // AccountLease: acquired or released
)

// Event payloads (PayloadVersion 1). Emitters MUST use these shapes:
//
//	node.enrolled, node.status, node.revoked  Node
//	node.connection                           NodeConnection
//	session.created, session.updated          Session
//	run.started, run.observed, run.ended      Run
//	message.stored, message.delivery,
//	message.ack                               MessageView
//	approval.requested/resolved/withdrawn     Approval
//	lease.changed                             InputLease
//	project.changed                           Project
//	document.revised                          DocumentRevision (Content omitted)
//	document.conflict                         DocumentProposal (Content omitted)
//	artifact.published                        PublishReceipt
//	comment.created, comment.resolved         ReviewComment
//	note.created                              Note (Body omitted)
//	account.observed                          AccountObservation
//	account.assigned                          AccountAssignment
//	account.grant                             DeviceGrant
//	account.fallback                          FallbackPlan
//	account.exhausted                         the node's payload (provider, session_id, from_run_id,
//	                                          from_account_id, outcome, earliest_reset, reasons), or a
//	                                          FallbackPlan when the hub derived it; one per session +
//	                                          from_run_id
//	attention.changed                         AttentionItem
//	environment.applied                       EnvironmentGeneration
//	environment.drift                         []DriftItem
//	update.staged, update.result              UpdateResult
//	command.result                            CommandResult
//	connection.changed                        Connection
//	artifact.changed                          Artifact
//	account.changed                           Account
//	account.lease                             AccountLease
//	node.automation                           NodeAutomation (audit.go)
//
// Event is a durable event. The hub assigns Cursor on ingest; the cursor
// orders the shared view by ingestion, not causality across hosts.
type Event struct {
	EventID        string    `json:"event_id"`
	Cursor         int64     `json:"cursor"`
	Origin         string    `json:"origin"` // "hub" | "node:<id>" | "client:<id>"
	OriginSeq      uint64    `json:"origin_seq,omitempty"`
	Kind           EventKind `json:"kind"`
	Object         ObjectRef `json:"object"`
	ProjectID      *string   `json:"project_id"`
	SessionID      string    `json:"session_id,omitempty"`
	PayloadVersion int       `json:"payload_version"`
	Payload        RawJSON   `json:"payload,omitempty"`
	OccurredAt     time.Time `json:"occurred_at"`
	IngestedAt     time.Time `json:"ingested_at"`
}

type ListEventsRequest struct {
	AfterCursor int64       `json:"after_cursor"`
	Limit       int         `json:"limit,omitempty"`
	Kinds       []EventKind `json:"kinds,omitempty"`
	SessionID   string      `json:"session_id,omitempty"`
	ProjectID   *string     `json:"project_id,omitempty"`
	WaitSeconds int         `json:"wait_seconds,omitempty"` // long-poll up to 60s
}

// ListEventsResponse returns events after the cursor. If AfterCursor is older
// than retention the hub returns ErrResetRequired with details
// {"oldest_cursor": n, "snapshot_cursor": m}; clients re-snapshot via list
// calls and resume from snapshot_cursor.
type ListEventsResponse struct {
	Events     []Event `json:"events"`
	NextCursor int64   `json:"next_cursor"`
	HeadCursor int64   `json:"head_cursor"`
}

// AttentionKind classifies user-directed attention.
type AttentionKind string

const (
	AttnApproval      AttentionKind = "approval"
	AttnQuestion      AttentionKind = "question"
	AttnFailure       AttentionKind = "failure"
	AttnAccountPool   AttentionKind = "account_exhausted"
	AttnReviewReady   AttentionKind = "review_ready"
	AttnMessage       AttentionKind = "message"
	AttnIndeterminate AttentionKind = "indeterminate_delivery"
	AttnConflict      AttentionKind = "document_conflict"
	AttnComment       AttentionKind = "comment"
	AttnUpdate        AttentionKind = "update"
	AttnNode          AttentionKind = "node"
	// AttnFlag is raised by a session itself ("I need you"; v1.3,
	// FlagAttention). It clears when the session clears it, the owner
	// engages with the session, or the run ends.
	AttnFlag AttentionKind = "flag"
)

type AttentionState string

const (
	AttnOpen      AttentionState = "open"
	AttnRead      AttentionState = "read"
	AttnHandled   AttentionState = "handled"
	AttnWithdrawn AttentionState = "withdrawn" // underlying request resolved elsewhere
	AttnSnoozed   AttentionState = "snoozed"
)

// AttentionItem is the durable Inbox entry. Repeated events collapse into
// one item by GroupKey with Count and UpdatedAt.
type AttentionItem struct {
	AttentionID  string         `json:"attention_id"`
	Kind         AttentionKind  `json:"kind"`
	Severity     string         `json:"severity"` // "info" | "normal" | "urgent"
	GroupKey     string         `json:"group_key"`
	Object       ObjectRef      `json:"object"`
	SessionID    string         `json:"session_id,omitempty"`
	ProjectID    *string        `json:"project_id"`
	Title        string         `json:"title"`
	Summary      string         `json:"summary,omitempty"` // no secrets/transcripts
	State        AttentionState `json:"state"`
	Count        int            `json:"count"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	SnoozedUntil *time.Time     `json:"snoozed_until,omitempty"`
	// DeepLink is a stable pyre:// link to the object.
	DeepLink string `json:"deep_link"`
	// Notification tracks push delivery separately from the human acting.
	Notification *NotificationStatus `json:"notification,omitempty"`
}

type NotificationStatus struct {
	Provider string     `json:"provider"` // "fcm" | "hub-stream" | "terminal" | "none"
	State    string     `json:"state"`    // "pending" | "sent" | "failed" | "suppressed_quiet_hours" | "disabled"
	SentAt   *time.Time `json:"sent_at,omitempty"`
	Detail   string     `json:"detail,omitempty"`
}

type ListAttentionRequest struct {
	PageRequest
	States    []AttentionState `json:"states,omitempty"` // default open+read
	Kinds     []AttentionKind  `json:"kinds,omitempty"`
	ProjectID *string          `json:"project_id,omitempty"`
	SessionID string           `json:"session_id,omitempty"`
}

// FlagAttentionRequest lets a session say it needs the owner (or stop
// saying so). Session principals may flag only themselves; the owner may
// clear any session's flag. Text is the agent's own one-line reason (never
// transcript), shown as the flag's text. (v1.3)
type FlagAttentionRequest struct {
	Mutation
	SessionID string `json:"session_id,omitempty"` // default: the calling session
	Text      string `json:"text,omitempty"`
	Clear     bool   `json:"clear,omitempty"`
}

// Session attention levels (SessionFlag.Level).
const (
	FlagNeedsYou = "needs_you" // blocking: approval, question, asked, failed, reauth, blocked
	FlagMessage  = "message"   // informational: a message (or comment/review) for the owner
)

// SessionFlag summarises why a session wants the owner (v1.3). Reason is
// one of approval, question, asked (the session flagged itself), failed,
// reauth, blocked, delivery, conflict for FlagNeedsYou; message, comment,
// review for FlagMessage. Text is a short display line of the top item
// with session IDs already resolved to titles.
type SessionFlag struct {
	Level       string    `json:"level"`
	Reason      string    `json:"reason"`
	Text        string    `json:"text,omitempty"`
	AttentionID string    `json:"attention_id,omitempty"`
	Kind        string    `json:"kind,omitempty"` // the top item's AttentionKind
	Count       int       `json:"count"`          // open items behind the flag
	Messages    int       `json:"messages"`       // of which informational
	Since       time.Time `json:"since"`
}

type UpdateAttentionRequest struct {
	Mutation
	AttentionID  string         `json:"attention_id"`
	State        AttentionState `json:"state"`
	SnoozedUntil *time.Time     `json:"snoozed_until,omitempty"`
}

// UpdateResult is the payload of update.staged / update.result events.
type UpdateResult struct {
	ToolVersion
	NodeID     string `json:"node_id"`
	Outcome    string `json:"outcome"` // "staged" | "activated" | "failed" | "rolled_back"
	RolledBack bool   `json:"rolled_back"`
	Detail     string `json:"detail,omitempty"`
}

// Push data keys (FCM data-only messages; values are strings). Clients
// fetch canonical state before acting:
//
//	attention_id, kind, severity, deep_link, group_key,
//	action ("notify" | "dismiss" — dismiss withdraws a shown alert),
//	title (generic, e.g. "Pyre: approval needed"; never message bodies)
const PushDataKeys = "attention_id,kind,severity,deep_link,group_key,action,title"

// NotificationPrefs controls presentation only; durable records are unaffected.
type NotificationPrefs struct {
	QuietHoursStart string                 `json:"quiet_hours_start,omitempty"` // "22:00" local to TZ
	QuietHoursEnd   string                 `json:"quiet_hours_end,omitempty"`
	TZ              string                 `json:"tz,omitempty"`
	MutedProjects   []string               `json:"muted_projects,omitempty"`
	MinSeverity     string                 `json:"min_severity,omitempty"`
	Kinds           map[AttentionKind]bool `json:"kinds,omitempty"` // false disables push for a kind
}

// PushRegistration registers a device push endpoint (token is a provider
// push token, not a Pyre credential).
type PushRegistration struct {
	Mutation
	Provider    string `json:"provider"` // "fcm" | "hub-stream"
	DeviceToken string `json:"device_token,omitempty"`
	ClientID    string `json:"client_id,omitempty"`
}
