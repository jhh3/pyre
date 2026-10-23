package contracts

import (
	"context"
	"time"
)

// Issues (additive, optional): sessions file what went wrong with Pyre
// itself, with a machine, or with Pyre's own guidance, so a triage session
// can check them periodically instead of the owner hearing about each one.
//
// issues.file: the owner or any session. The hub fills the reporter from
// the principal (session, its current run unless RunID names another of
// its runs, that run's node, harness and version, the session's project,
// the node's pyred version) and adds an automatic context block (the run's
// state and reason, cwd); Context from the caller (e.g. pyred log lines
// about the run) is appended. Title, body and context are redacted with
// the credential patterns before they are stored.
//
// Dedupe: Fingerprint = kind + normalized title (lowercase; IDs, numbers,
// hex, paths, URLs and timestamps removed) + the node for kind machine.
// The newest issue with the same fingerprint absorbs the report as an
// occurrence (Occurrences+1, LastSeenAt) instead of a new issue: open and
// triaged stay as they are; fixed goes back to open with a note "reported
// again after fix"; wontfix stays wontfix; duplicate forwards to the issue
// it duplicates (DuplicateOf) when set.
//
// Triage: the hub setting issues.triage_session names one session. A new
// issue sends it one message ("[pyre issue] <kind>: "<title>" — iss_… from
// <session title> on <node> (pyre vX)", the title quoted and labeled as
// the reporter's words; no body); a repeat sends nothing except when the
// count reaches IssueRecurringThresholds (a "recurring" message) or a fixed
// issue reopens. Past IssueMaxNewPerHour new issues per session per hour,
// issues are stored without a message and the triage session is told once.
// Messages go through an outbox (retried by hub maintenance). Nothing goes
// to the owner's inbox. Without a triage session (or when it is archived or
// gone) issues are only stored.
//
// Redaction: title, body, context, related IDs, notes and links pass the
// shared credential patterns (credentials.RedactPatterns); related IDs and
// links have per-item limits (IssueMaxRef, IssueMaxLink). A machine named
// by NodeID must be a known node.
//
// issues.list / issues.get: the owner and any session. issues.update
// (status, note, links, duplicate_of): the owner or the triage session.
// issues.triage.set: the owner, or the current triage session (to hand the
// role on or turn it off with SessionID ""). issues.triage.get: anyone.
const (
	OpIssuesFile      = "issues.file"
	OpIssuesList      = "issues.list"
	OpIssuesGet       = "issues.get"
	OpIssuesUpdate    = "issues.update"
	OpIssuesTriageGet = "issues.triage.get"
	OpIssuesTriageSet = "issues.triage.set"
)

// FeatureIssues is the Hello feature advertising the issues ops.
const FeatureIssues = "issues"

// PrefixIssue is the issue ID prefix.
const PrefixIssue = "iss"

// KindIssue is the ObjectRef kind of an issue.
const KindIssue ObjectKind = "issue"

type IssueKind string

const (
	IssuePyreBug  IssueKind = "pyre_bug" // Pyre itself misbehaved
	IssueMachine  IssueKind = "machine"  // a machine is broken (missing tool, login expired, disk full, wrong config)
	IssueGuidance IssueKind = "guidance" // Pyre's instructions were wrong or confusing
	IssueOther    IssueKind = "other"
)

// IssueKinds lists the valid kinds.
var IssueKinds = []IssueKind{IssuePyreBug, IssueMachine, IssueGuidance, IssueOther}

type IssueStatus string

const (
	IssueOpen      IssueStatus = "open"
	IssueTriaged   IssueStatus = "triaged"
	IssueFixed     IssueStatus = "fixed"
	IssueWontfix   IssueStatus = "wontfix"
	IssueDuplicate IssueStatus = "duplicate"
)

// IssueStatuses lists the valid statuses.
var IssueStatuses = []IssueStatus{IssueOpen, IssueTriaged, IssueFixed, IssueWontfix, IssueDuplicate}

// Resolved reports whether s closes an issue (fixed, wontfix, duplicate).
func (s IssueStatus) Resolved() bool {
	return s == IssueFixed || s == IssueWontfix || s == IssueDuplicate
}

// IssueRecurringThresholds are the occurrence counts at which a repeat of
// an open or triaged issue messages the triage session again.
var IssueRecurringThresholds = []int{3, 10, 50}

// Limits.
const (
	IssueMaxTitle   = 200
	IssueMaxBody    = 20000
	IssueMaxContext = 8000
	IssueMaxNote    = 4000
	IssueMaxLinks   = 20
	IssueMaxRelated = 20
	IssueMaxRef     = 120 // one related ID
	IssueMaxLink    = 500 // one link
	// IssueMaxNewPerHour new issues per session per hour get a triage
	// message; past it they are stored silently and the triage session is
	// told once that the session is filing a lot.
	IssueMaxNewPerHour = 10
	// IssueMaxStoredOccurrences: past it a repeat only counts (no new
	// evidence row), except a report that reopens a fixed issue.
	IssueMaxStoredOccurrences = 50
)

type Issue struct {
	IssueID     string      `json:"issue_id"`
	Kind        IssueKind   `json:"kind"`
	Title       string      `json:"title"`
	Body        string      `json:"body,omitempty"` // markdown, from the first report
	Status      IssueStatus `json:"status"`
	Fingerprint string      `json:"fingerprint"`
	Occurrences int         `json:"occurrences"` // reports absorbed, the first included
	FirstSeenAt time.Time   `json:"first_seen_at"`
	LastSeenAt  time.Time   `json:"last_seen_at"`
	// The first reporter.
	ReportedBy        Principal `json:"reported_by"`
	ReporterSessionID string    `json:"reporter_session_id,omitempty"`
	ReporterRunID     string    `json:"reporter_run_id,omitempty"`
	ReporterTitle     string    `json:"reporter_title,omitempty"` // the session's title when filed
	NodeID            string    `json:"node_id,omitempty"`
	NodeName          string    `json:"node_name,omitempty"`
	Harness           Harness   `json:"harness,omitempty"`
	HarnessVersion    string    `json:"harness_version,omitempty"`
	PyreVersion       string    `json:"pyre_version,omitempty"` // the node's pyred (else the filing client's)
	ProjectID         *string   `json:"project_id,omitempty"`
	// Context is the automatic context block of the first report.
	Context     string     `json:"context,omitempty"`
	Related     []string   `json:"related,omitempty"` // session/run IDs the reporter named
	Links       []string   `json:"links,omitempty"`   // PR URLs and the like, added by triage
	DuplicateOf string     `json:"duplicate_of,omitempty"`
	Resolution  string     `json:"resolution,omitempty"` // the note given with the last status change
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy  *Principal `json:"resolved_by,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// IssueOccurrence is one report absorbed by an issue (the first included).
type IssueOccurrence struct {
	Seq            int       `json:"seq"` // 1 = the first report
	IssueID        string    `json:"issue_id"`
	ReportedBy     Principal `json:"reported_by"`
	SessionID      string    `json:"session_id,omitempty"`
	RunID          string    `json:"run_id,omitempty"`
	NodeID         string    `json:"node_id,omitempty"`
	NodeName       string    `json:"node_name,omitempty"`
	Harness        Harness   `json:"harness,omitempty"`
	HarnessVersion string    `json:"harness_version,omitempty"`
	PyreVersion    string    `json:"pyre_version,omitempty"`
	Title          string    `json:"title"` // as reported (titles vary within a fingerprint)
	Body           string    `json:"body,omitempty"`
	Context        string    `json:"context,omitempty"`
	Related        []string  `json:"related,omitempty"`
	At             time.Time `json:"at"`
}

// IssueNote is a triage note or a status change.
type IssueNote struct {
	IssueID    string      `json:"issue_id"`
	By         Principal   `json:"by"`
	Text       string      `json:"text,omitempty"`
	FromStatus IssueStatus `json:"from_status,omitempty"` // set on a status change
	ToStatus   IssueStatus `json:"to_status,omitempty"`
	Links      []string    `json:"links,omitempty"`
	At         time.Time   `json:"at"`
}

type FileIssueRequest struct {
	Mutation
	Kind  IssueKind `json:"kind"`
	Title string    `json:"title"`
	Body  string    `json:"body,omitempty"` // what happened, evidence (markdown)
	// Context is extra automatic context from the caller (pyred log lines
	// about the run); the hub adds its own block.
	Context string   `json:"context,omitempty"`
	Related []string `json:"related,omitempty"` // session/run IDs involved
	// NodeID names the machine an issue is about (default: the reporter's
	// run's node). It is part of a machine issue's fingerprint.
	NodeID string `json:"node_id,omitempty"`
	// RunID is the reporter's run (default: its session's current run);
	// a run of another session is ignored.
	RunID string `json:"run_id,omitempty"`
	// PyreVersion is the filing client's version, used when the node's
	// pyred version is unknown.
	PyreVersion string `json:"pyre_version,omitempty"`
}

type IssueFileOutcome string

const (
	IssueCreated  IssueFileOutcome = "created"  // a new issue
	IssueRecurred IssueFileOutcome = "recurred" // added to an existing issue
	IssueReopened IssueFileOutcome = "reopened" // added to a fixed issue, which is open again
)

type FileIssueResponse struct {
	Issue   Issue            `json:"issue"`
	Outcome IssueFileOutcome `json:"outcome"`
	// TriageSessionID is the session told about it ("" when none was
	// messaged: no triage session, or a repeat below a threshold).
	TriageSessionID string `json:"triage_session_id,omitempty"`
	// Throttled: the session filed more than IssueMaxNewPerHour new issues
	// in the last hour; this one is stored without a triage message.
	Throttled bool `json:"throttled,omitempty"`
}

// ListIssuesRequest lists issues, most recently seen first. Statuses
// defaults to open and triaged; All lists every status.
type ListIssuesRequest struct {
	Statuses  []IssueStatus `json:"statuses,omitempty"`
	All       bool          `json:"all,omitempty"`
	Kind      IssueKind     `json:"kind,omitempty"`
	NodeID    string        `json:"node_id,omitempty"`
	ProjectID *string       `json:"project_id,omitempty"`
	Limit     int           `json:"limit,omitempty"` // default 100, at most 500
}

type ListIssuesResponse struct {
	Issues []Issue `json:"issues"`
}

type GetIssueRequest struct {
	IssueID string `json:"issue_id"`
}

// IssueDetail is an issue with its stored occurrences (oldest first, at
// most the first 5 and the last 45; past IssueMaxStoredOccurrences repeats
// only count) and notes.
type IssueDetail struct {
	Issue       Issue             `json:"issue"`
	Occurrences []IssueOccurrence `json:"occurrences"`
	Notes       []IssueNote       `json:"notes"`
}

// UpdateIssueRequest changes an issue (owner or triage session). At least
// one of Status, Note, Links is required. Status duplicate takes
// DuplicateOf (another issue's ID; later reports go there).
type UpdateIssueRequest struct {
	Mutation
	IssueID     string      `json:"issue_id"`
	Status      IssueStatus `json:"status,omitempty"`
	Note        string      `json:"note,omitempty"`
	Links       []string    `json:"links,omitempty"` // added
	DuplicateOf string      `json:"duplicate_of,omitempty"`
}

// IssueTriage is the hub's triage setting.
type IssueTriage struct {
	SessionID    string     `json:"session_id,omitempty"` // "" = off
	SessionTitle string     `json:"session_title,omitempty"`
	Archived     bool       `json:"archived,omitempty"` // set but archived: nothing is sent
	SetBy        *Principal `json:"set_by,omitempty"`
	SetAt        *time.Time `json:"set_at,omitempty"`
}

// SetIssueTriageRequest names the triage session ("" turns triage off).
type SetIssueTriageRequest struct {
	Mutation
	SessionID string `json:"session_id"`
}

// IssuesAPI is implemented by hubs (and the client and the node-local
// proxy) that offer the issues ops.
type IssuesAPI interface {
	FileIssue(ctx context.Context, req FileIssueRequest) (FileIssueResponse, error)
	ListIssues(ctx context.Context, req ListIssuesRequest) (ListIssuesResponse, error)
	GetIssue(ctx context.Context, req GetIssueRequest) (IssueDetail, error)
	UpdateIssue(ctx context.Context, req UpdateIssueRequest) (Issue, error)
	GetIssueTriage(ctx context.Context, req struct{}) (IssueTriage, error)
	SetIssueTriage(ctx context.Context, req SetIssueTriageRequest) (IssueTriage, error)
}
