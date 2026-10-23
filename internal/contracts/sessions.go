package contracts

import (
	"fmt"
	"strings"
	"time"
)

// Session is a durable conversation/work identity. It persists across runs.
type Session struct {
	SessionID    string    `json:"session_id"`
	Harness      Harness   `json:"harness"`
	Title        string    `json:"title"`
	ProjectID    *string   `json:"project_id"` // optional; null for standalone
	CreatedBy    Principal `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	CurrentRunID string    `json:"current_run_id,omitempty"`
	Lineage      Lineage   `json:"lineage"`
	Tags         []string  `json:"tags,omitempty"`
	Archived     bool      `json:"archived"`
	// Icon (W-archive2, additive) is an optional single emoji shown before
	// the title (one grapheme, at most two cells). Automatic titling never
	// changes it.
	Icon string `json:"icon,omitempty"`
	// OwnerActiveAt (additive) is when the owner last sent this session a
	// message or typed into it through Pyre (terminal relay, input, input
	// lease). The hub records it at most once a minute per session; nil
	// when the owner never did. Sessions acting on the owner's behalf do not
	// count.
	OwnerActiveAt *time.Time `json:"owner_active_at,omitempty"`
}

// StartedBy reports whether session sessionID started s (it created s, or
// is its lineage parent). Additive (v1.7): a session principal may archive
// itself and the sessions it started.
func (s Session) StartedBy(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	return (s.CreatedBy.Kind == PrincipalSession && s.CreatedBy.ID == sessionID) || s.Lineage.ParentSessionID == sessionID
}

// Parent is the session s sits under and reports to ("" at the top level):
// its lineage parent, else (never reparented) the session principal that
// created it. Additive (W-reparent).
func (s Session) Parent() string {
	if p := s.Lineage.ParentSessionID; p != "" && p != s.SessionID {
		return p
	}
	if s.Lineage.ReparentedAt == nil && s.CreatedBy.Kind == PrincipalSession && s.CreatedBy.ID != "" && s.CreatedBy.ID != s.SessionID {
		return s.CreatedBy.ID
	}
	return ""
}

// Lineage links a session to where it came from.
type Lineage struct {
	ParentSessionID string `json:"parent_session_id,omitempty"` // session it sits under and reports to (its launcher unless reparented)
	ForkedFrom      string `json:"forked_from,omitempty"`       // explicit fork/successor source session
	HandoffNoteID   string `json:"handoff_note_id,omitempty"`   // reconstructed-continuation handoff
	// ReparentedFrom/ReparentedAt (W-reparent, additive) record the last
	// explicit reparent: the previous parent ("" when it was at the top
	// level) and when. A non-nil ReparentedAt means the parent was chosen
	// explicitly, so CreatedBy no longer implies one (Session.Parent).
	// CreatedBy stays the historical creator.
	ReparentedFrom string     `json:"reparented_from,omitempty"`
	ReparentedAt   *time.Time `json:"reparented_at,omitempty"`
}

// RunState is a narrow observation of an execution. It is never a statement
// that a user goal is complete.
type RunState string

const (
	RunStarting        RunState = "starting"
	RunRunning         RunState = "running"          // agent working
	RunWaitingInput    RunState = "waiting_input"    // idle at prompt
	RunWaitingApproval RunState = "waiting_approval" // pending approval/question
	RunExiting         RunState = "exiting"
	RunExited          RunState = "exited"
	RunFailed          RunState = "failed"
	RunUnknown         RunState = "unknown" // observer lost contact; NOT failure
	// RunWaitingBackground: idle at the prompt but background tasks/subagents
	// may start a new turn on their own (e.g. Claude task notifications).
	RunWaitingBackground RunState = "waiting_on_background"
)

// Terminal reports whether the state ends the run.
func (s RunState) Terminal() bool { return s == RunExited || s == RunFailed }

// Run is one execution of a session on one node.
type Run struct {
	RunID              string                `json:"run_id"`
	SessionID          string                `json:"session_id"`
	NodeID             string                `json:"node_id"`
	Adapter            string                `json:"adapter"` // e.g. "claude-tmux", "codex-appserver", "agy-tmux", "shell-tmux", "mock"
	Harness            Harness               `json:"harness"`
	Cwd                string                `json:"cwd"`
	Checkout           *CheckoutBinding      `json:"checkout,omitempty"`
	Runtime            RuntimeVersion        `json:"runtime"`
	EnvGenerationID    string                `json:"env_generation_id,omitempty"`
	AccountID          string                `json:"account_id,omitempty"`
	AssignmentID       string                `json:"assignment_id,omitempty"`
	State              RunState              `json:"state"`
	StateReason        string                `json:"state_reason,omitempty"`
	Freshness          Freshness             `json:"freshness"`
	StartedAt          time.Time             `json:"started_at"`
	EndedAt            *time.Time            `json:"ended_at,omitempty"`
	ExitCode           *int                  `json:"exit_code,omitempty"`
	NativeRef          *NativeRef            `json:"native_ref,omitempty"`
	Capabilities       []string              `json:"capabilities"`
	PreviousRunID      string                `json:"previous_run_id,omitempty"` // lineage across resume/fallback
	ContinuationKind   ContinuationKind      `json:"continuation_kind,omitempty"`
	Subagents          []SubagentObservation `json:"subagents,omitempty"`
	Lease              *InputLease           `json:"lease,omitempty"`
	PendingApprovalIDs []string              `json:"pending_approval_ids,omitempty"`
	AdapterData        map[string]string     `json:"adapter_data,omitempty"` // namespaced, nonsecret
	// Usage is the run's latest process-tree resource sample, filled by the
	// hub at presentation from its node's status (v1.8); nodes do not set
	// it in run reports.
	Usage *RunUsage `json:"usage,omitempty"`
	// Activity is what a running run is doing right now ("running tests",
	// "editing main.go"), derived by the node from the harness's in-flight
	// tool or item (v1.9, additive). Nil when the run is not working or the
	// adapter cannot tell. Only the latest value is kept.
	Activity *RunActivity `json:"activity,omitempty"`
}

// RunActivity is a short, nonsecret phrase (at most 40 characters; commands
// truncated, paths reduced to their base name) and when it began.
type RunActivity struct {
	Text  string    `json:"text"`
	Since time.Time `json:"since"`
}

// RuntimeVersion pins the executable/config a run was started with.
type RuntimeVersion struct {
	Harness    Harness `json:"harness"`
	Version    string  `json:"version"`
	Executable string  `json:"executable,omitempty"`
	Digest     Digest  `json:"digest,omitempty"`
}

// NativeRef is the harness's own resumable identity (Claude session id,
// Codex thread id). It may be replaced on resume; Pyre IDs are stable.
type NativeRef struct {
	Kind string `json:"kind"` // "claude_session" | "codex_thread" | "agy_conversation" | "tmux_session"
	ID   string `json:"id"`
}

// ContinuationKind describes how a run continues a previous one.
type ContinuationKind string

const (
	ContinuationNone         ContinuationKind = ""
	ContinuationNativeResume ContinuationKind = "native_resume" // harness resumed the same thread
	ContinuationHandoff      ContinuationKind = "handoff"       // reconstructed context; not the same thread
	ContinuationFresh        ContinuationKind = "fresh"         // new run, no prior context
)

// SubagentObservation is surfaced only where the harness exposes it. Native
// subagents are never independently attachable fleet sessions.
type SubagentObservation struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind,omitempty"` // harness-specific agent type
	State     string     `json:"state"`          // "running" | "stopped" | "unknown"
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Summary   string     `json:"summary,omitempty"`
}

// CheckoutBinding records where work happens on a node.
type CheckoutBinding struct {
	NodeID string       `json:"node_id"`
	Path   string       `json:"path"`
	RepoID string       `json:"repo_id,omitempty"`
	Mode   CheckoutMode `json:"mode"`
	Branch string       `json:"branch,omitempty"`
}

type CheckoutMode string

const (
	CheckoutShared   CheckoutMode = "shared"   // several sessions use one directory
	CheckoutWorktree CheckoutMode = "worktree" // isolated git worktree
	CheckoutPlain    CheckoutMode = "plain"    // a directory, not necessarily git
)

// SessionView is what list/inspect return: the session, its current run,
// and the freshness of the combined observation.
type SessionView struct {
	Session   Session `json:"session"`
	Run       *Run    `json:"run,omitempty"`
	NodeName  string  `json:"node_name,omitempty"`
	Attention int     `json:"attention"` // open attention items for this session
	// Flag is why the session wants the owner, nil when nothing does
	// (v1.3). Blocking reasons outrank informational messages.
	Flag      *SessionFlag `json:"flag,omitempty"`
	Freshness Freshness    `json:"freshness"`
	// Monitor (additive) marks a monitor session (Session.IsMonitor).
	Monitor bool `json:"monitor,omitempty"`
	// Monitors (additive) are the live monitors (run not ended) that wake
	// this session, oldest first, in sessions.list, sessions.get and
	// sessions.get_many views.
	Monitors []MonitorRef `json:"monitors,omitempty"`
	// ArchiveCascade (additive) is set on the answer to an archive by the
	// owner or a session (sessions.update archived=true) when sessions sit
	// under the archived one: the descendants Pyre archives with it and
	// those it leaves because the owner is using them.
	ArchiveCascade *ArchiveCascade `json:"archive_cascade,omitempty"`
}

// ArchiveCascade is what archiving a session does to the sessions under it
// (its lineage descendants). Archived lists those archived with it: at once
// when idle or ended, when their current turn ends otherwise. Left lists
// those the owner started or attached to, messaged or typed into since
// they were created; they stay as they are.
type ArchiveCascade struct {
	Archived []CascadeSession `json:"archived,omitempty"`
	Left     []CascadeSession `json:"left,omitempty"`
}

// CascadeSession names one session of an ArchiveCascade.
type CascadeSession struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
}

// Line says what the cascade did in one line ("" when it did nothing):
// `also archived 3 sessions it started; left 1 the owner is using: "t" (ses_…)`.
func (c *ArchiveCascade) Line() string {
	if c == nil || len(c.Archived)+len(c.Left) == 0 {
		return ""
	}
	var parts []string
	if n := len(c.Archived); n > 0 {
		noun := "sessions"
		if n == 1 {
			noun = "session"
		}
		parts = append(parts, fmt.Sprintf("also archived %d %s it started", n, noun))
	}
	if n := len(c.Left); n > 0 {
		names := make([]string, 0, n)
		for _, s := range c.Left {
			names = append(names, fmt.Sprintf("%q (%s)", s.Title, s.SessionID))
		}
		parts = append(parts, fmt.Sprintf("left %d the owner is using: %s", n, strings.Join(names, ", ")))
	}
	return strings.Join(parts, "; ")
}

// AccountPolicy selects the subscription for a launch. Cross-provider
// fallback is not representable.
type AccountPolicy struct {
	Mode      AccountPolicyMode `json:"mode"`                 // default "eligible"
	AccountID string            `json:"account_id,omitempty"` // for "explicit"
	Prefer    []string          `json:"prefer,omitempty"`     // account IDs in preference order
	// Strict keeps an explicit account even when it is at its limit or
	// ineligible (the launch waits/fails). Default false: such an account
	// falls back to the best eligible same-provider account, and the
	// assignment reason says so.
	Strict bool `json:"strict,omitempty"`
}

type AccountPolicyMode string

const (
	AccountEligible AccountPolicyMode = "eligible" // any eligible configured subscription of the harness's provider
	AccountExplicit AccountPolicyMode = "explicit" // this account; when ineligible, the best eligible one (Strict: fail)
	AccountAmbient  AccountPolicyMode = "ambient"  // the node's existing default login (no Pyre assignment); reported as such
)

// StartSessionRequest creates a session and its first run.
type StartSessionRequest struct {
	Mutation
	NodeID        string           `json:"node_id,omitempty"` // explicit node; or use NodeSelector
	NodeSelector  *NodeSelector    `json:"node_selector,omitempty"`
	Harness       Harness          `json:"harness"`
	Title         string           `json:"title,omitempty"`
	Cwd           string           `json:"cwd,omitempty"`
	Checkout      *CheckoutRequest `json:"checkout,omitempty"`
	ProjectID     *string          `json:"project_id,omitempty"`
	EnvProfile    string           `json:"env_profile,omitempty"`
	AccountPolicy AccountPolicy    `json:"account_policy"`
	InitialPrompt string           `json:"initial_prompt,omitempty"`
	Args          []string         `json:"args,omitempty"` // extra harness args (validated by adapter allowlist)
	Tags          []string         `json:"tags,omitempty"`
	// IncludeContext asks for a bounded project brief/memory context at launch.
	IncludeContext bool `json:"include_context,omitempty"`
	// ParentSessionID is set automatically when an agent session launches a peer.
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// ReportToParent (v1.7, additive) asks the hub to message the parent
	// session automatically when this session finishes a turn, needs input,
	// or exits. Nil means the default: on when the creator is a session
	// (ParentSessionID set from its principal), off otherwise. The hub
	// records the choice as the reserved tag TagReportParent or
	// TagReportNone on the session.
	ReportToParent *bool `json:"report_to_parent,omitempty"`
}

// Reserved session tags for automatic reports to the parent session
// (v1.7, additive). A session with a parent and TagReportParent reports to
// it; TagReportNone records an explicit opt-out. Editing the tags toggles
// the behaviour.
const (
	TagReportParent = "report:parent"
	TagReportNone   = "report:none"
	// TagPyreRun marks a headless session recorded by `pyre run` (the
	// caller ran the command itself): never reports to a parent and never
	// raises owner attention.
	TagPyreRun = "pyre run"
)

// TagArchiveWhenDone (W-archive2, additive) is the reserved session tag
// the hub sets for UpdateSessionRequest.WhenDone: the session is archived
// once its current turn ends and its run has been stopped.
const TagArchiveWhenDone = "archive:when-done"

// Headless-run markers in StartSessionRequest.Args (pyre run, W-pyrerun).
// ArgPyreExec=<hex SHA-256 of a claim secret> makes the node record the
// run without starting anything (its harness's exec adapter): the caller
// claims the account binding over the node's local socket and runs the
// command itself. ArgPyreExecResume=<id> names the conversation the
// caller's command resumes. These are the only arguments a node credential
// may start a session with, and only on itself (hub nodestart.go).
const (
	ArgPyreExec       = "--pyre-exec"
	ArgPyreExecResume = "--pyre-exec-resume"
)

// NodeSelector is a caller-supplied rule for choosing a node.
type NodeSelector struct {
	Platform    string            `json:"platform,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	LeastLoaded bool              `json:"least_loaded,omitempty"`
}

// CheckoutRequest asks for a project checkout binding at launch.
type CheckoutRequest struct {
	RepoID string       `json:"repo_id,omitempty"`
	Mode   CheckoutMode `json:"mode"`
	Branch string       `json:"branch,omitempty"`
	Path   string       `json:"path,omitempty"`
}

type StartSessionResponse struct {
	Session SessionView `json:"session"`
	// Command is the node command that will perform the launch; observe the
	// run state to learn the launch outcome.
	CommandID string `json:"command_id,omitempty"`
	// Warnings: launcher notes, e.g. a fresh worktree branched from local
	// HEAD because its repository has no origin remote (set client-side).
	Warnings []string `json:"warnings,omitempty"`
}

type ListSessionsRequest struct {
	PageRequest
	NodeID          string     `json:"node_id,omitempty"`
	ProjectID       *string    `json:"project_id,omitempty"` // "" with non-nil pointer = standalone only
	Harness         Harness    `json:"harness,omitempty"`
	States          []RunState `json:"states,omitempty"`
	Query           string     `json:"query,omitempty"`
	IncludeArchived bool       `json:"include_archived,omitempty"`
	// IncludeMonitors (additive) also lists monitor sessions; by default
	// they are left out (contracts/monitors.go).
	IncludeMonitors bool `json:"include_monitors,omitempty"`
}

type GetRequest struct {
	ID string `json:"id"`
}

// ReadSessionRequest reads bounded transcript/activity.
type ReadSessionRequest struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"` // default current run
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Tail      bool   `json:"tail,omitempty"` // return the latest entries
}

// TranscriptEntry is one bounded, redacted activity item.
type TranscriptEntry struct {
	Seq       int64     `json:"seq"`
	RunID     string    `json:"run_id"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"` // "user" | "assistant" | "tool" | "system" | "screen" | "pyre_message"
	Text      string    `json:"text"`
	MessageID string    `json:"message_id,omitempty"` // when the entry is a delivered Pyre message
	Truncated bool      `json:"truncated,omitempty"`
}

type ReadSessionResponse struct {
	Entries    []TranscriptEntry `json:"entries"`
	NextCursor string            `json:"next_cursor,omitempty"`
	Gap        bool              `json:"gap"` // true if entries were lost/expired before cursor
	Freshness  Freshness         `json:"freshness"`
	// Screen is an optional current terminal snapshot (plain text) for previews.
	Screen string `json:"screen,omitempty"`
}

// AttachMechanism states exactly how a client reaches a run's terminal.
type AttachMechanism string

const (
	AttachLocalTmux   AttachMechanism = "local_tmux"   // run `command` locally
	AttachSSHTmux     AttachMechanism = "ssh_tmux"     // run `command` (ssh -t ... tmux attach)
	AttachWebTerminal AttachMechanism = "web_terminal" // open `url` with one-time ticket
	AttachReadOnly    AttachMechanism = "read_only"    // transcript/screen only
)

type AttachRequest struct {
	Mutation
	SessionID    string            `json:"session_id"`
	ClientNodeID string            `json:"client_node_id,omitempty"` // the attaching client's node, if any
	Preferred    []AttachMechanism `json:"preferred,omitempty"`
	ReadOnly     bool              `json:"read_only,omitempty"`
}

// AttachDescriptor is short-lived and authorized. Never log Ticket.
type AttachDescriptor struct {
	RunID     string          `json:"run_id"`
	NodeID    string          `json:"node_id"`
	Mechanism AttachMechanism `json:"mechanism"`
	Command   []string        `json:"command,omitempty"`
	URL       string          `json:"url,omitempty"`
	Ticket    string          `json:"ticket,omitempty"` // one-time, expires quickly
	ExpiresAt time.Time       `json:"expires_at"`
	ReadOnly  bool            `json:"read_only"`
	Notes     string          `json:"notes,omitempty"` // e.g. "detach with Ctrl-b d"
	Lease     *InputLease     `json:"lease,omitempty"`
}

// SessionCommandRequest drives interrupt/stop.
type SessionCommandRequest struct {
	Mutation
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"` // must match current run if set
	Reason    string `json:"reason,omitempty"`
}

// CommandReceipt reports command acceptance, separate from observed outcome.
type CommandReceipt struct {
	CommandID  string         `json:"command_id"`
	Accepted   bool           `json:"accepted"`
	Outcome    CommandOutcome `json:"outcome"`
	Detail     string         `json:"detail,omitempty"`
	ObservedAt time.Time      `json:"observed_at"`
}

type CommandOutcome string

const (
	OutcomePending       CommandOutcome = "pending"   // queued for the node
	OutcomeDelivered     CommandOutcome = "delivered" // node accepted the command
	OutcomeSucceeded     CommandOutcome = "succeeded" // effect observed
	OutcomeFailed        CommandOutcome = "failed"
	OutcomeUnsupported   CommandOutcome = "unsupported"
	OutcomeIndeterminate CommandOutcome = "indeterminate"
)

// ResumeRequest creates a new run from a continuation reference.
type ResumeRequest struct {
	Mutation
	SessionID     string        `json:"session_id"`
	NodeID        string        `json:"node_id,omitempty"` // default: previous run's node
	AccountPolicy AccountPolicy `json:"account_policy"`
	// AllowHandoff permits a reconstructed (non-native) continuation when
	// native resume is unsupported. Default false => ErrUnsupported.
	AllowHandoff bool `json:"allow_handoff,omitempty"`
	// EnvFile (additive) replaces the session's environment file for this
	// and later runs (an absolute path on the node), or "none" drops it;
	// "" keeps the previous run's. Needs harness capability launch.env_file.
	EnvFile string `json:"env_file,omitempty"`
}

// AssociateRequest attaches/detaches a session to a project after the fact.
type AssociateRequest struct {
	Mutation
	SessionID string  `json:"session_id"`
	ProjectID *string `json:"project_id"` // null detaches
}

// UpdateSessionRequest edits mutable session metadata.
type UpdateSessionRequest struct {
	Mutation
	SessionID string    `json:"session_id"`
	Title     *string   `json:"title,omitempty"`
	Tags      *[]string `json:"tags,omitempty"`
	Archived  *bool     `json:"archived,omitempty"`
	// WhenDone (W-archive2, additive): with Archived=true, a session whose
	// run is still live is not archived at once. The hub records
	// TagArchiveWhenDone, stops the run gracefully once its current turn
	// ends (Claude: /exit; Codex: end the thread; shell: exit) and then
	// archives the session. An ended session is archived at once.
	// Archived=false cancels a pending archive.
	WhenDone bool `json:"when_done,omitempty"`
	// Icon (W-archive2, additive) sets the session's emoji ("" clears).
	// Any principal may set any session's icon, like a rename.
	Icon *string `json:"icon,omitempty"`
	// Parent (W-reparent, additive) moves the session under another
	// session: its lineage parent, which receives its automatic reports
	// (pending reports retarget). "" (or "none") detaches it to the top
	// level. The parent must exist, not be archived, and not be the session
	// or one of its descendants. The owner may reparent any session; a
	// session principal only itself, the sessions it currently parents, and
	// sessions in its own project. Both sides get a Pyre notice message.
	Parent *string `json:"parent,omitempty"`
}
