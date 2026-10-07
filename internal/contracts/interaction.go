package contracts

import "time"

// InputLease governs conflicting RAW terminal writes to one run. Transcript
// viewers and structured peer mail do not need a lease. Losing a transport
// does not free a lease; expiry or explicit takeover does.
type InputLease struct {
	LeaseID    string     `json:"lease_id"`
	RunID      string     `json:"run_id"`
	Holder     Principal  `json:"holder"`
	AcquiredAt time.Time  `json:"acquired_at"`
	RenewedAt  time.Time  `json:"renewed_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	TakeoverOf string     `json:"takeover_of,omitempty"` // previous lease ID if taken over
	ReleasedAt *time.Time `json:"released_at,omitempty"`
}

// Active reports whether the lease is held at time now.
func (l InputLease) Active(now time.Time) bool {
	return l.ReleasedAt == nil && now.Before(l.ExpiresAt)
}

type LeaseRequest struct {
	Mutation
	RunID      string `json:"run_id"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"` // default 120
	// Takeover forcibly replaces an active lease held by someone else. The
	// takeover is recorded with the actor.
	Takeover bool `json:"takeover,omitempty"`
}

type LeaseReleaseRequest struct {
	Mutation
	LeaseID string `json:"lease_id"`
}

// ApprovalKind classifies a pending request from a runtime.
type ApprovalKind string

const (
	ApprovalPermission ApprovalKind = "permission" // tool/command/file permission
	ApprovalQuestion   ApprovalKind = "question"   // structured question with options or free text
	ApprovalPlan       ApprovalKind = "plan"       // plan approval
)

type ApprovalState string

const (
	ApprovalPending   ApprovalState = "pending"
	ApprovalResolved  ApprovalState = "resolved"
	ApprovalExpired   ApprovalState = "expired"
	ApprovalWithdrawn ApprovalState = "withdrawn" // runtime moved on / run replaced
)

// Approval is bound to its originating run and runtime request ID so an
// answer cannot land in an unrelated prompt. Only one resolution commits.
type Approval struct {
	ApprovalID string              `json:"approval_id"`
	SessionID  string              `json:"session_id"`
	RunID      string              `json:"run_id"`
	RequestID  string              `json:"request_id"` // adapter-native request identity
	Kind       ApprovalKind        `json:"kind"`
	Title      string              `json:"title"`
	Body       string              `json:"body,omitempty"` // explanation; redacted
	Options    []ApprovalOption    `json:"options,omitempty"`
	AllowText  bool                `json:"allow_text"`
	CreatedAt  time.Time           `json:"created_at"`
	ExpiresAt  *time.Time          `json:"expires_at,omitempty"`
	State      ApprovalState       `json:"state"`
	Resolution *ApprovalResolution `json:"resolution,omitempty"`
}

type ApprovalOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind,omitempty"` // "allow" | "deny" | "allow_always" | "choice"
}

type ApprovalResolution struct {
	OptionID   string    `json:"option_id,omitempty"`
	Text       string    `json:"text,omitempty"`
	Actor      Principal `json:"actor"`
	ResolvedAt time.Time `json:"resolved_at"`
	// Source is "pyre" when answered through Pyre, "native" when the runtime
	// reports it was answered in the terminal (actor may then be unknown).
	Source string `json:"source"`
	// Effect distinguishes "submitted" (Pyre sent the answer) from
	// "effective" (runtime evidence shows it took effect) and
	// "superseded" (the request was resolved elsewhere first).
	Effect string `json:"effect,omitempty"`
	// Reason distinguishes native outcomes, e.g. "answered_in_terminal",
	// "interrupted", "run_replaced", "timeout".
	Reason string `json:"reason,omitempty"`
}

type AnswerApprovalRequest struct {
	Mutation
	ApprovalID string `json:"approval_id"`
	RunID      string `json:"run_id"` // must equal the approval's run
	OptionID   string `json:"option_id,omitempty"`
	Text       string `json:"text,omitempty"`
}

type ListApprovalsRequest struct {
	PageRequest
	SessionID string          `json:"session_id,omitempty"`
	States    []ApprovalState `json:"states,omitempty"` // default pending
}
