package contracts

import (
	"context"
	"io"
	"time"
)

// API is the hub's public operation set, used identically by the CLI, TUI,
// Android (over HTTP), agent tools, the node daemon's local proxy, and the
// deterministic mock. The authenticated Principal is carried in ctx (see
// WithPrincipal); request bodies never claim identity.
//
// Implementations: internal/hub (SQLite, real), internal/runtime/mock
// (in-memory, labelled mock), internal/client (HTTP), internal/daemon
// (local proxy with outbox/cached fallbacks).
type API interface {
	SystemAPI
	FleetAPI
	SessionAPI
	InteractionAPI
	MessageAPI
	ProjectAPI
	DocumentAPI
	ArtifactAPI
	ReviewAPI
	NoteAPI
	AccountAPI
	AttentionAPI
	EventAPI
	NodeActionAPI
}

type SystemAPI interface {
	Hello(ctx context.Context, req HelloRequest) (HelloResponse, error)
	WhoAmI(ctx context.Context) (Principal, error)
}

type FleetAPI interface {
	ListNodes(ctx context.Context, req ListNodesRequest) (Page[Node], error)
	GetNode(ctx context.Context, req GetRequest) (Node, error)
	CreateEnrollment(ctx context.Context, req EnrollmentRequest) (EnrollmentTicket, error)
	RevokeNode(ctx context.Context, req RevokeRequest) (Node, error)
	CreatePairing(ctx context.Context, req PairingRequest) (PairingTicket, error)
	ListClients(ctx context.Context, req PageRequest) (Page[ClientInfo], error)
	RevokeClient(ctx context.Context, req RevokeRequest) (ClientInfo, error)
}

type SessionAPI interface {
	ListSessions(ctx context.Context, req ListSessionsRequest) (Page[SessionView], error)
	GetSession(ctx context.Context, req GetRequest) (SessionView, error)
	StartSession(ctx context.Context, req StartSessionRequest) (StartSessionResponse, error)
	ReadSession(ctx context.Context, req ReadSessionRequest) (ReadSessionResponse, error)
	AttachSession(ctx context.Context, req AttachRequest) (AttachDescriptor, error)
	InterruptSession(ctx context.Context, req SessionCommandRequest) (CommandReceipt, error)
	StopSession(ctx context.Context, req SessionCommandRequest) (CommandReceipt, error)
	ResumeSession(ctx context.Context, req ResumeRequest) (StartSessionResponse, error)
	AssociateSession(ctx context.Context, req AssociateRequest) (SessionView, error)
	UpdateSession(ctx context.Context, req UpdateSessionRequest) (SessionView, error)
	ListRuns(ctx context.Context, req ListRunsRequest) (Page[Run], error)
}

type InteractionAPI interface {
	AcquireLease(ctx context.Context, req LeaseRequest) (InputLease, error)
	ReleaseLease(ctx context.Context, req LeaseReleaseRequest) (InputLease, error)
	ListApprovals(ctx context.Context, req ListApprovalsRequest) (Page[Approval], error)
	GetApproval(ctx context.Context, req GetRequest) (Approval, error)
	AnswerApproval(ctx context.Context, req AnswerApprovalRequest) (Approval, error)
}

type MessageAPI interface {
	SendMessage(ctx context.Context, req SendMessageRequest) (MessageView, error)
	GetMessage(ctx context.Context, req GetRequest) (MessageView, error)
	ListMessages(ctx context.Context, req ListMessagesRequest) (Page[MessageView], error)
	AckMessage(ctx context.Context, req AckMessageRequest) (MessageView, error)
	ReissueMessage(ctx context.Context, req ReissueMessageRequest) (MessageView, error)
	ResolveDelivery(ctx context.Context, req ResolveDeliveryRequest) (MessageView, error)
	CancelMessage(ctx context.Context, req CancelMessageRequest) (MessageView, error)
}

type ProjectAPI interface {
	CreateProject(ctx context.Context, req CreateProjectRequest) (Project, error)
	GetProject(ctx context.Context, req GetRequest) (Project, error)
	ListProjects(ctx context.Context, req ListProjectsRequest) (Page[Project], error)
	UpdateProject(ctx context.Context, req UpdateProjectRequest) (Project, error)
}

type DocumentAPI interface {
	CreateDocument(ctx context.Context, req CreateDocumentRequest) (DocumentWithRevision, error)
	GetDocument(ctx context.Context, req GetDocumentRequest) (DocumentWithRevision, error)
	ListDocuments(ctx context.Context, req ListDocumentsRequest) (Page[Document], error)
	ListRevisions(ctx context.Context, req ListRevisionsRequest) (Page[DocumentRevision], error)
	ProposeRevision(ctx context.Context, req ProposeRevisionRequest) (DocumentWithRevision, error)
	ListProposals(ctx context.Context, req ListProposalsRequest) (Page[DocumentProposal], error)
	DiscardProposal(ctx context.Context, req DiscardProposalRequest) (DocumentProposal, error)
}

type ArtifactAPI interface {
	BeginUpload(ctx context.Context, req BeginUploadRequest) (Upload, error)
	// UploadChunk writes bytes at offset; idempotent for the same range.
	UploadChunk(ctx context.Context, uploadID string, offset int64, r io.Reader) (Upload, error)
	FinalizeUpload(ctx context.Context, req FinalizeUploadRequest) (PublishReceipt, error)
	// GetUpload returns an upload's state (resume offset).
	GetUpload(ctx context.Context, req GetRequest) (Upload, error)
	// UpdateArtifact edits metadata or associates a standalone artifact
	// with a project later. Versions are never modified.
	UpdateArtifact(ctx context.Context, req UpdateArtifactRequest) (Artifact, error)
	GetArtifact(ctx context.Context, req GetRequest) (Artifact, error)
	ListArtifacts(ctx context.Context, req ListArtifactsRequest) (Page[Artifact], error)
	ListVersions(ctx context.Context, req ListVersionsRequest) (Page[ArtifactVersion], error)
	GetVersion(ctx context.Context, req GetRequest) (ArtifactVersion, error)
	// OpenContent returns verified immutable bytes; caller closes.
	OpenContent(ctx context.Context, versionID string) (io.ReadCloser, ArtifactVersion, error)
	// MintPreviewURL issues a short-lived, one-time isolated preview link
	// for a version (v1.2). The link carries its own ticket; it is not a
	// management credential.
	MintPreviewURL(ctx context.Context, req MintPreviewRequest) (PreviewURL, error)
}

type ReviewAPI interface {
	CreateComment(ctx context.Context, req CreateCommentRequest) (ReviewComment, error)
	GetComment(ctx context.Context, req GetRequest) (ReviewComment, error)
	ListComments(ctx context.Context, req ListCommentsRequest) (Page[ReviewComment], error)
	ResolveComment(ctx context.Context, req ResolveCommentRequest) (ReviewComment, error)
}

type NoteAPI interface {
	CreateNote(ctx context.Context, req CreateNoteRequest) (Note, error)
	GetNote(ctx context.Context, req GetRequest) (Note, error)
	ListNotes(ctx context.Context, req ListNotesRequest) (Page[Note], error)
	UpdateNoteSync(ctx context.Context, req UpdateNoteSyncRequest) (Note, error)
	// PromoteNote explicitly turns a note into an authoritative document
	// revision (new document, or a CAS proposal on an existing one) and
	// records Note.PromotedTo. Nothing is promoted implicitly.
	PromoteNote(ctx context.Context, req PromoteNoteRequest) (DocumentWithRevision, error)
	GetLaunchContext(ctx context.Context, req ContextRequest) (LaunchContext, error)
}

type AccountAPI interface {
	ListAccounts(ctx context.Context, req ListAccountsRequest) (Page[AccountView], error)
	UpsertAccount(ctx context.Context, req UpsertAccountRequest) (Account, error)
	ListFallbacks(ctx context.Context, req ListFallbacksRequest) (Page[FallbackPlan], error)
	// Fleet refresh-family leases (node principals; contract v1.3).
	AcquireAccountLease(ctx context.Context, req AcquireAccountLeaseRequest) (AccountLease, error)
	RenewAccountLease(ctx context.Context, req RenewAccountLeaseRequest) (AccountLease, error)
	ReleaseAccountLease(ctx context.Context, req ReleaseAccountLeaseRequest) (AccountLease, error)
}

type AttentionAPI interface {
	ListAttention(ctx context.Context, req ListAttentionRequest) (Page[AttentionItem], error)
	// GetAttention returns one item in any state (v1.2; deep links and
	// notification taps).
	GetAttention(ctx context.Context, req GetRequest) (AttentionItem, error)
	UpdateAttention(ctx context.Context, req UpdateAttentionRequest) (AttentionItem, error)
	// FlagAttention raises or clears a session's own "needs you" flag
	// (v1.3). Clearing with nothing flagged returns a zero item.
	FlagAttention(ctx context.Context, req FlagAttentionRequest) (AttentionItem, error)
	GetNotificationPrefs(ctx context.Context) (NotificationPrefs, error)
	SetNotificationPrefs(ctx context.Context, req SetNotificationPrefsRequest) (NotificationPrefs, error)
	RegisterPush(ctx context.Context, req PushRegistration) (PushRegistrationResult, error)
}

type EventAPI interface {
	ListEvents(ctx context.Context, req ListEventsRequest) (ListEventsResponse, error)
}

// NodeActionAPI routes typed integration actions (inventory, environment
// plan/apply, updates, connection checks/invocations, account operations)
// to a node over its control channel. Action names and payloads are defined
// in actions.go; adding an action does not change the core API.
type NodeActionAPI interface {
	InvokeNodeAction(ctx context.Context, req NodeActionRequest) (NodeActionResult, error)
	ListConnections(ctx context.Context, req ListConnectionsRequest) (Page[Connection], error)
	// CheckConnectionGrant answers, from current hub state, whether a
	// session may use a connection right now (v1.2). Nodes call it on every
	// connection use; any error must be treated as denial.
	CheckConnectionGrant(ctx context.Context, req CheckGrantRequest) (GrantDecision, error)
	// GetCommandResult returns a node command's current outcome and, once
	// reported, its final result (v1.4). The command's actor, the owner and
	// the command's node may read it.
	GetCommandResult(ctx context.Context, req GetRequest) (NodeActionResult, error)
}

// ---- small request/response types used above ----

type HelloRequest struct {
	Protocol int    `json:"protocol"`
	Client   string `json:"client"` // "pyre-cli/0.1.0", "pyre-android/0.1.0"
}

type HelloResponse struct {
	Protocol    int       `json:"protocol"`
	MinProtocol int       `json:"min_protocol"`
	HubID       string    `json:"hub_id"`
	HubVersion  string    `json:"hub_version"`
	ServerTime  time.Time `json:"server_time"`
	Principal   Principal `json:"principal"`
	Mock        bool      `json:"mock"` // true when served by the deterministic mock
	Features    []string  `json:"features"`
}

type ListNodesRequest struct {
	PageRequest
	IncludeRevoked bool `json:"include_revoked,omitempty"`
}

type RevokeRequest struct {
	Mutation
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

type ListRunsRequest struct {
	PageRequest
	SessionID string `json:"session_id"`
}

type UpsertAccountRequest struct {
	Mutation
	AccountID string   `json:"account_id,omitempty"` // empty = create
	Provider  Provider `json:"provider"`
	Label     string   `json:"label"`
	Disabled  bool     `json:"disabled,omitempty"`
	RotaRef   string   `json:"rota_ref,omitempty"`
	// PerMachineIndependent sets Account.PerMachineIndependent.
	PerMachineIndependent bool `json:"per_machine_independent,omitempty"`
	// ProviderRef sets Account.ProviderRef (the provider's nonsecret account
	// id used to verify identity); empty keeps the stored value.
	ProviderRef string `json:"provider_ref,omitempty"`
}

type ListFallbacksRequest struct {
	PageRequest
	SessionID string `json:"session_id,omitempty"`
}

type SetNotificationPrefsRequest struct {
	Mutation
	Prefs NotificationPrefs `json:"prefs"`
}

type PushRegistrationResult struct {
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
	Detail   string `json:"detail,omitempty"` // e.g. "FCM not configured on hub"
}

type ListConnectionsRequest struct {
	PageRequest
	NodeID    string `json:"node_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

// CheckGrantRequest asks whether SessionID may use ConnectionID now.
type CheckGrantRequest struct {
	ConnectionID string `json:"connection_id"`
	SessionID    string `json:"session_id"`
}

// GrantDecision is a use-time grant check result. Generation is the
// connection's catalog generation the decision was made against.
type GrantDecision struct {
	Allowed    bool   `json:"allowed"`
	Generation int64  `json:"generation"`
	Reason     string `json:"reason"`
}

// NodeActionRequest invokes a typed action on a node.
type NodeActionRequest struct {
	Mutation
	NodeID string  `json:"node_id"`
	Action string  `json:"action"` // see actions.go
	Params RawJSON `json:"params,omitempty"`
	// TimeoutSeconds bounds a synchronous wait; on timeout the result is
	// returned with Outcome pending/indeterminate and a CommandID to observe.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

type NodeActionResult struct {
	CommandID string         `json:"command_id"`
	Outcome   CommandOutcome `json:"outcome"`
	Result    RawJSON        `json:"result,omitempty"`
	Error     *Error         `json:"error,omitempty"`
}

// ---- principal context ----

type principalKey struct{}

// WithPrincipal returns ctx carrying the authenticated principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
