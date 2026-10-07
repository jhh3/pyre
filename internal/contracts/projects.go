package contracts

import "time"

// Project is an optional durable shared workspace. Only ID and Name are
// required; it needs no repository, board, or task state.
type Project struct {
	ProjectID       string            `json:"project_id"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	BriefDocumentID string            `json:"brief_document_id,omitempty"`
	Repos           []RepoBinding     `json:"repos"`
	Checkouts       []CheckoutBinding `json:"checkouts"`
	Links           []Link            `json:"links,omitempty"`
	EnvProfile      string            `json:"env_profile,omitempty"`
	Connections     []string          `json:"connections,omitempty"` // named connection references (never secrets)
	Archived        bool              `json:"archived"`
	Version         int64             `json:"version"` // compare-and-swap for metadata updates
	CreatedBy       Principal         `json:"created_by"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type RepoBinding struct {
	RepoID string `json:"repo_id"` // project-local stable key, e.g. "outfit"
	Name   string `json:"name"`
	URL    string `json:"url,omitempty"`
}

type Link struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type CreateProjectRequest struct {
	Mutation
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Brief       string        `json:"brief,omitempty"` // creates the brief document at revision 1
	Repos       []RepoBinding `json:"repos,omitempty"`
	Links       []Link        `json:"links,omitempty"`
}

// UpdateProjectRequest replaces mutable metadata if Version matches.
type UpdateProjectRequest struct {
	Mutation
	ProjectID   string             `json:"project_id"`
	Version     int64              `json:"version"`
	Name        *string            `json:"name,omitempty"`
	Description *string            `json:"description,omitempty"`
	Repos       *[]RepoBinding     `json:"repos,omitempty"`
	Checkouts   *[]CheckoutBinding `json:"checkouts,omitempty"`
	Links       *[]Link            `json:"links,omitempty"`
	EnvProfile  *string            `json:"env_profile,omitempty"`
	Connections *[]string          `json:"connections,omitempty"`
	Archived    *bool              `json:"archived,omitempty"`
}

type ListProjectsRequest struct {
	PageRequest
	IncludeArchived bool   `json:"include_archived,omitempty"`
	Query           string `json:"query,omitempty"`
}

// Document is a versioned text document, optionally in a project.
type Document struct {
	DocumentID     string    `json:"document_id"`
	ProjectID      *string   `json:"project_id"`
	Title          string    `json:"title"`
	Slug           string    `json:"slug"`       // unique within project, e.g. "brief", "decisions"
	MediaType      string    `json:"media_type"` // "text/markdown" default
	HeadRevisionID string    `json:"head_revision_id"`
	HeadSeq        int64     `json:"head_seq"`
	CreatedBy      Principal `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// DocumentRevision is immutable.
type DocumentRevision struct {
	RevisionID       string      `json:"revision_id"`
	DocumentID       string      `json:"document_id"`
	Seq              int64       `json:"seq"`
	ParentRevisionID string      `json:"parent_revision_id,omitempty"`
	Digest           Digest      `json:"digest"`
	Size             int64       `json:"size"`
	Author           Principal   `json:"author"`
	CreatedAt        time.Time   `json:"created_at"`
	Summary          string      `json:"summary,omitempty"`
	Content          string      `json:"content,omitempty"` // omitted in history listings
	Provenance       *Provenance `json:"provenance,omitempty"`
}

// DocumentProposal preserves a conflicting (or pending) edit so neither
// side is lost.
type DocumentProposal struct {
	ProposalID     string        `json:"proposal_id"`
	DocumentID     string        `json:"document_id"`
	BaseRevisionID string        `json:"base_revision_id"`
	Content        string        `json:"content"`
	Author         Principal     `json:"author"`
	CreatedAt      time.Time     `json:"created_at"`
	State          ProposalState `json:"state"`
	HeadAtConflict string        `json:"head_at_conflict,omitempty"`
}

type ProposalState string

const (
	ProposalConflicted ProposalState = "conflicted"
	ProposalApplied    ProposalState = "applied"
	ProposalDiscarded  ProposalState = "discarded"
)

type CreateDocumentRequest struct {
	Mutation
	ProjectID *string `json:"project_id,omitempty"`
	Title     string  `json:"title"`
	Slug      string  `json:"slug,omitempty"`
	MediaType string  `json:"media_type,omitempty"`
	Content   string  `json:"content"`
	Summary   string  `json:"summary,omitempty"`
}

type GetDocumentRequest struct {
	DocumentID string `json:"document_id,omitempty"`
	// Alternatively address by project + slug.
	ProjectID  string `json:"project_id,omitempty"`
	Slug       string `json:"slug,omitempty"`
	RevisionID string `json:"revision_id,omitempty"` // default head
}

type DocumentWithRevision struct {
	Document Document         `json:"document"`
	Revision DocumentRevision `json:"revision"`
}

// ProposeRevisionRequest commits only if the head equals BaseRevisionID.
// On mismatch the hub returns ErrConflict with details
// {"current_head": <rev id>, "proposal_id": <preserved proposal>}.
type ProposeRevisionRequest struct {
	Mutation
	DocumentID     string `json:"document_id"`
	BaseRevisionID string `json:"base_revision_id"`
	Content        string `json:"content"`
	Summary        string `json:"summary,omitempty"`
}

type ListDocumentsRequest struct {
	PageRequest
	ProjectID *string `json:"project_id,omitempty"`
}

type ListRevisionsRequest struct {
	PageRequest
	DocumentID string `json:"document_id"`
}

type ListProposalsRequest struct {
	PageRequest
	DocumentID string          `json:"document_id"`
	States     []ProposalState `json:"states,omitempty"`
}

// Provenance records where published material came from.
type Provenance struct {
	SessionID  string  `json:"session_id,omitempty"`
	RunID      string  `json:"run_id,omitempty"`
	NodeID     string  `json:"node_id,omitempty"`
	ProjectID  *string `json:"project_id,omitempty"`
	SourcePath string  `json:"source_path,omitempty"` // informational only
	Commit     string  `json:"commit,omitempty"`
}

// Artifact groups immutable published versions.
type Artifact struct {
	ArtifactID      string    `json:"artifact_id"`
	ProjectID       *string   `json:"project_id"`
	SessionID       string    `json:"session_id,omitempty"`
	Title           string    `json:"title"`
	Description     string    `json:"description,omitempty"`
	LatestVersionID string    `json:"latest_version_id,omitempty"`
	VersionCount    int       `json:"version_count"`
	CreatedBy       Principal `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ArtifactVersion is immutable; its bytes never change.
type ArtifactVersion struct {
	VersionID         string     `json:"version_id"`
	ArtifactID        string     `json:"artifact_id"`
	Seq               int64      `json:"seq"`
	PreviousVersionID string     `json:"previous_version_id,omitempty"`
	Digest            Digest     `json:"digest"`
	Size              int64      `json:"size"`
	MediaType         string     `json:"media_type"`
	Filename          string     `json:"filename"` // display only; never a storage path
	Author            Principal  `json:"author"`
	Provenance        Provenance `json:"provenance"`
	CreatedAt         time.Time  `json:"created_at"`
	Note              string     `json:"note,omitempty"`
	// ReviewRequested marks a version the producer asked the owner to review.
	ReviewRequested bool `json:"review_requested,omitempty"`
	// ContentURL is the hub-relative download path.
	ContentURL string `json:"content_url"`
	// PreviewURL is an isolated-origin preview URL for active content (HTML),
	// served without management credentials.
	PreviewURL string `json:"preview_url,omitempty"`
}

// BeginUploadRequest starts an upload. Either ArtifactID (new version) or
// Title (new artifact).
type BeginUploadRequest struct {
	Mutation
	ArtifactID  string  `json:"artifact_id,omitempty"`
	ProjectID   *string `json:"project_id,omitempty"`
	Title       string  `json:"title,omitempty"`
	Description string  `json:"description,omitempty"`
	Filename    string  `json:"filename"`
	MediaType   string  `json:"media_type,omitempty"`
	Size        int64   `json:"size"`
	Digest      Digest  `json:"digest"`
	Note        string  `json:"note,omitempty"`
	// ReviewRequested asks the owner to review this version (attention item).
	ReviewRequested bool       `json:"review_requested,omitempty"`
	Provenance      Provenance `json:"provenance"`
}

type Upload struct {
	UploadID   string    `json:"upload_id"`
	ArtifactID string    `json:"artifact_id,omitempty"`
	Size       int64     `json:"size"`
	Received   int64     `json:"received"` // resumable offset
	Digest     Digest    `json:"digest"`
	ExpiresAt  time.Time `json:"expires_at"`
	MaxBytes   int64     `json:"max_bytes"`
	State      string    `json:"state"` // "open" | "finalized" | "failed" | "expired"
}

// FinalizeUploadRequest verifies size+digest and atomically publishes.
// MintPreviewRequest asks for an isolated preview link for a version.
// TTLSeconds <= 0 selects the hub default (5 minutes); the hub caps it.
type MintPreviewRequest struct {
	VersionID  string `json:"version_id"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// PreviewURL is a minted preview link: absolute when the hub knows its
// public or preview origin, otherwise hub-relative ("/preview/...").
type PreviewURL struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type FinalizeUploadRequest struct {
	Mutation
	UploadID string `json:"upload_id"`
}

type PublishReceipt struct {
	Artifact Artifact        `json:"artifact"`
	Version  ArtifactVersion `json:"version"`
}

type ListArtifactsRequest struct {
	PageRequest
	ProjectID *string `json:"project_id,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
}

type ListVersionsRequest struct {
	PageRequest
	ArtifactID string `json:"artifact_id"`
}

// ReviewComment attaches to an exact document revision or artifact version.
type ReviewComment struct {
	CommentID  string     `json:"comment_id"`
	Target     ObjectRef  `json:"target"` // document_revision | artifact_version
	Anchor     *Anchor    `json:"anchor,omitempty"`
	Author     Principal  `json:"author"`
	Body       string     `json:"body"`
	InReplyTo  string     `json:"in_reply_to,omitempty"`
	Mentions   []string   `json:"mentions,omitempty"` // session IDs to notify, or "owner"
	Resolved   bool       `json:"resolved"`
	ResolvedBy *Principal `json:"resolved_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ProjectID  *string    `json:"project_id"`
}

type Anchor struct {
	Kind  string `json:"kind"`  // "line" | "range" | "quote" | "page" | "region"
	Value string `json:"value"` // e.g. "12-18", quoted text, "page=3"
}

type CreateCommentRequest struct {
	Mutation
	Target    ObjectRef `json:"target"`
	Anchor    *Anchor   `json:"anchor,omitempty"`
	Body      string    `json:"body"`
	InReplyTo string    `json:"in_reply_to,omitempty"`
	Mentions  []string  `json:"mentions,omitempty"`
}

type ListCommentsRequest struct {
	PageRequest
	Target     *ObjectRef `json:"target,omitempty"`
	ArtifactID string     `json:"artifact_id,omitempty"` // all versions of an artifact
	DocumentID string     `json:"document_id,omitempty"` // all revisions of a document
	ProjectID  *string    `json:"project_id,omitempty"`
	Unresolved bool       `json:"unresolved,omitempty"`
}

type ResolveCommentRequest struct {
	Mutation
	CommentID string `json:"comment_id"`
	Resolved  bool   `json:"resolved"`
}

// Note is an explicit memory item (note, handoff, decision) published
// promptly through the hub; Hive indexes it. Not an authoritative project
// document unless explicitly promoted.
type Note struct {
	NoteID     string     `json:"note_id"`
	Kind       NoteKind   `json:"kind"`
	ProjectID  *string    `json:"project_id"`
	SessionID  string     `json:"session_id,omitempty"`
	Title      string     `json:"title"`
	Body       string     `json:"body"`
	Author     Principal  `json:"author"`
	CreatedAt  time.Time  `json:"created_at"`
	Provenance Provenance `json:"provenance"`
	HiveRef    string     `json:"hive_ref,omitempty"`    // Hive item id once indexed
	SyncState  string     `json:"sync_state,omitempty"`  // "pending" | "indexed" | "failed"
	PromotedTo string     `json:"promoted_to,omitempty"` // document revision if promoted
}

type NoteKind string

const (
	NoteNote     NoteKind = "note"
	NoteHandoff  NoteKind = "handoff"
	NoteDecision NoteKind = "decision"
)

type CreateNoteRequest struct {
	Mutation
	Kind      NoteKind `json:"kind"`
	ProjectID *string  `json:"project_id,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
}

type ListNotesRequest struct {
	PageRequest
	ProjectID *string  `json:"project_id,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Kind      NoteKind `json:"kind,omitempty"`
	Query     string   `json:"query,omitempty"` // simple text match on hub; Hive provides ranked search
	// SyncStates filters by Hive sync_state ("pending" | "indexed" |
	// "failed"); empty = all. Notes with no recorded state count as pending.
	SyncStates []string `json:"sync_states,omitempty"`
}

// UpdateNoteSyncRequest is used by the Hive bridge to record indexing.
type UpdateNoteSyncRequest struct {
	Mutation
	NoteID    string `json:"note_id"`
	HiveRef   string `json:"hive_ref,omitempty"`
	SyncState string `json:"sync_state"`
}

// ContextRequest asks for a bounded launch brief.
type ContextRequest struct {
	ProjectID *string `json:"project_id,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	MaxBytes  int     `json:"max_bytes,omitempty"` // default 8 KiB
}

// LaunchContext is a bounded brief with retrieval pointers, recording what
// was included.
type LaunchContext struct {
	Text            string      `json:"text"`
	Bytes           int         `json:"bytes"`
	Truncated       bool        `json:"truncated"`
	BriefRevision   string      `json:"brief_revision_id,omitempty"`
	References      []ObjectRef `json:"references"`
	MemoryFreshness *Freshness  `json:"memory_freshness,omitempty"`
}

// DiscardProposalRequest marks a preserved proposal discarded (its content
// is retained for audit).
type DiscardProposalRequest struct {
	Mutation
	ProposalID string `json:"proposal_id"`
}

// UpdateArtifactRequest edits artifact metadata. ProjectID non-nil sets the
// project ("" detaches to standalone).
type UpdateArtifactRequest struct {
	Mutation
	ArtifactID  string  `json:"artifact_id"`
	ProjectID   *string `json:"project_id,omitempty"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
}

// PromoteNoteRequest promotes a note. Either DocumentID+BaseRevisionID
// (propose on an existing document; conflicts as ProposeRevision) or
// Title/Slug (+ProjectID) to create a new document.
type PromoteNoteRequest struct {
	Mutation
	NoteID         string  `json:"note_id"`
	DocumentID     string  `json:"document_id,omitempty"`
	BaseRevisionID string  `json:"base_revision_id,omitempty"`
	ProjectID      *string `json:"project_id,omitempty"`
	Title          string  `json:"title,omitempty"`
	Slug           string  `json:"slug,omitempty"`
}
