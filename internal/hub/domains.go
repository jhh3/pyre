package hub

import (
	"context"
	"io"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/store"
)

// Delegation to pluggable domains. A nil domain returns ErrUnsupported.

// ---- FleetAPI (WP04) ----

func (h *Hub) ListNodes(ctx context.Context, req contracts.ListNodesRequest) (contracts.Page[contracts.Node], error) {
	if h.cfg.Fleet == nil {
		return contracts.Page[contracts.Node]{}, notConfigured("fleet")
	}
	return h.cfg.Fleet.ListNodes(ctx, req)
}

func (h *Hub) GetNode(ctx context.Context, req contracts.GetRequest) (contracts.Node, error) {
	if h.cfg.Fleet == nil {
		return contracts.Node{}, notConfigured("fleet")
	}
	return h.cfg.Fleet.GetNode(ctx, req)
}

// CreateEnrollment and RevokeNode are owner-only; a session's request is
// filed for the owner's approval (owneractions.go).
func (h *Hub) CreateEnrollment(ctx context.Context, req contracts.EnrollmentRequest) (contracts.EnrollmentTicket, error) {
	if h.cfg.Fleet == nil {
		return contracts.EnrollmentTicket{}, notConfigured("fleet")
	}
	if p, ok := contracts.PrincipalFrom(ctx); ok && p.Kind == contracts.PrincipalSession && !isOwner(p) {
		return h.enrollApproval(ctx, p, req)
	}
	return h.cfg.Fleet.CreateEnrollment(ctx, req)
}

func (h *Hub) RevokeNode(ctx context.Context, req contracts.RevokeRequest) (contracts.Node, error) {
	if h.cfg.Fleet == nil {
		return contracts.Node{}, notConfigured("fleet")
	}
	if p, ok := contracts.PrincipalFrom(ctx); ok && p.Kind == contracts.PrincipalSession && !isOwner(p) {
		return contracts.Node{}, h.revokeApproval(ctx, p, req)
	}
	return h.cfg.Fleet.RevokeNode(ctx, req)
}

func (h *Hub) CreatePairing(ctx context.Context, req contracts.PairingRequest) (contracts.PairingTicket, error) {
	if h.cfg.Fleet == nil {
		return contracts.PairingTicket{}, notConfigured("fleet")
	}
	return h.cfg.Fleet.CreatePairing(ctx, req)
}

func (h *Hub) ListClients(ctx context.Context, req contracts.PageRequest) (contracts.Page[contracts.ClientInfo], error) {
	if h.cfg.Fleet == nil {
		return contracts.Page[contracts.ClientInfo]{}, notConfigured("fleet")
	}
	return h.cfg.Fleet.ListClients(ctx, req)
}

func (h *Hub) RevokeClient(ctx context.Context, req contracts.RevokeRequest) (contracts.ClientInfo, error) {
	if h.cfg.Fleet == nil {
		return contracts.ClientInfo{}, notConfigured("fleet")
	}
	return h.cfg.Fleet.RevokeClient(ctx, req)
}

// ---- ProjectAPI (WP11) ----

func (h *Hub) CreateProject(ctx context.Context, req contracts.CreateProjectRequest) (contracts.Project, error) {
	if h.cfg.Projects == nil {
		return contracts.Project{}, notConfigured("projects")
	}
	return h.cfg.Projects.CreateProject(ctx, req)
}

func (h *Hub) GetProject(ctx context.Context, req contracts.GetRequest) (contracts.Project, error) {
	if h.cfg.Projects == nil {
		return contracts.Project{}, notConfigured("projects")
	}
	return h.cfg.Projects.GetProject(ctx, req)
}

func (h *Hub) ListProjects(ctx context.Context, req contracts.ListProjectsRequest) (contracts.Page[contracts.Project], error) {
	if h.cfg.Projects == nil {
		return contracts.Page[contracts.Project]{}, notConfigured("projects")
	}
	return h.cfg.Projects.ListProjects(ctx, req)
}

func (h *Hub) UpdateProject(ctx context.Context, req contracts.UpdateProjectRequest) (contracts.Project, error) {
	if h.cfg.Projects == nil {
		return contracts.Project{}, notConfigured("projects")
	}
	return h.cfg.Projects.UpdateProject(ctx, req)
}

// ---- DocumentAPI (WP11) ----

func (h *Hub) CreateDocument(ctx context.Context, req contracts.CreateDocumentRequest) (contracts.DocumentWithRevision, error) {
	if h.cfg.Documents == nil {
		return contracts.DocumentWithRevision{}, notConfigured("documents")
	}
	return h.cfg.Documents.CreateDocument(ctx, req)
}

func (h *Hub) GetDocument(ctx context.Context, req contracts.GetDocumentRequest) (contracts.DocumentWithRevision, error) {
	if h.cfg.Documents == nil {
		return contracts.DocumentWithRevision{}, notConfigured("documents")
	}
	return h.cfg.Documents.GetDocument(ctx, req)
}

func (h *Hub) ListDocuments(ctx context.Context, req contracts.ListDocumentsRequest) (contracts.Page[contracts.Document], error) {
	if h.cfg.Documents == nil {
		return contracts.Page[contracts.Document]{}, notConfigured("documents")
	}
	return h.cfg.Documents.ListDocuments(ctx, req)
}

func (h *Hub) ListRevisions(ctx context.Context, req contracts.ListRevisionsRequest) (contracts.Page[contracts.DocumentRevision], error) {
	if h.cfg.Documents == nil {
		return contracts.Page[contracts.DocumentRevision]{}, notConfigured("documents")
	}
	return h.cfg.Documents.ListRevisions(ctx, req)
}

func (h *Hub) ProposeRevision(ctx context.Context, req contracts.ProposeRevisionRequest) (contracts.DocumentWithRevision, error) {
	if h.cfg.Documents == nil {
		return contracts.DocumentWithRevision{}, notConfigured("documents")
	}
	return h.cfg.Documents.ProposeRevision(ctx, req)
}

// Fact sheet writes are limited to the owner and the hub applying an
// approved fact (contracts.FactSheetWriter), enforced by the projects store
// on the resolved slug of every write path.

func (h *Hub) ListProposals(ctx context.Context, req contracts.ListProposalsRequest) (contracts.Page[contracts.DocumentProposal], error) {
	if h.cfg.Documents == nil {
		return contracts.Page[contracts.DocumentProposal]{}, notConfigured("documents")
	}
	return h.cfg.Documents.ListProposals(ctx, req)
}

// ---- ArtifactAPI (WP11) ----

func (h *Hub) BeginUpload(ctx context.Context, req contracts.BeginUploadRequest) (contracts.Upload, error) {
	if h.cfg.Artifacts == nil {
		return contracts.Upload{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.BeginUpload(ctx, req)
}

func (h *Hub) UploadChunk(ctx context.Context, uploadID string, offset int64, r io.Reader) (contracts.Upload, error) {
	if h.cfg.Artifacts == nil {
		return contracts.Upload{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.UploadChunk(ctx, uploadID, offset, r)
}

func (h *Hub) FinalizeUpload(ctx context.Context, req contracts.FinalizeUploadRequest) (contracts.PublishReceipt, error) {
	if h.cfg.Artifacts == nil {
		return contracts.PublishReceipt{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.FinalizeUpload(ctx, req)
}

func (h *Hub) GetArtifact(ctx context.Context, req contracts.GetRequest) (contracts.Artifact, error) {
	if h.cfg.Artifacts == nil {
		return contracts.Artifact{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.GetArtifact(ctx, req)
}

func (h *Hub) ListArtifacts(ctx context.Context, req contracts.ListArtifactsRequest) (contracts.Page[contracts.Artifact], error) {
	if h.cfg.Artifacts == nil {
		return contracts.Page[contracts.Artifact]{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.ListArtifacts(ctx, req)
}

func (h *Hub) ListVersions(ctx context.Context, req contracts.ListVersionsRequest) (contracts.Page[contracts.ArtifactVersion], error) {
	if h.cfg.Artifacts == nil {
		return contracts.Page[contracts.ArtifactVersion]{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.ListVersions(ctx, req)
}

// MintPreviewURL delegates to the artifact store, which mints a one-time
// ticket under the hub's preview base (separate preview origin if
// configured, else the hub URL).
func (h *Hub) MintPreviewURL(ctx context.Context, req contracts.MintPreviewRequest) (contracts.PreviewURL, error) {
	if h.cfg.Artifacts == nil {
		return contracts.PreviewURL{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.MintPreviewURL(ctx, req)
}

func (h *Hub) GetVersion(ctx context.Context, req contracts.GetRequest) (contracts.ArtifactVersion, error) {
	if h.cfg.Artifacts == nil {
		return contracts.ArtifactVersion{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.GetVersion(ctx, req)
}

func (h *Hub) OpenContent(ctx context.Context, versionID string) (io.ReadCloser, contracts.ArtifactVersion, error) {
	if h.cfg.Artifacts == nil {
		return nil, contracts.ArtifactVersion{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.OpenContent(ctx, versionID)
}

// ---- ReviewAPI (WP11) ----

func (h *Hub) CreateComment(ctx context.Context, req contracts.CreateCommentRequest) (contracts.ReviewComment, error) {
	if h.cfg.Reviews == nil {
		return contracts.ReviewComment{}, notConfigured("reviews")
	}
	return h.cfg.Reviews.CreateComment(ctx, req)
}

func (h *Hub) ListComments(ctx context.Context, req contracts.ListCommentsRequest) (contracts.Page[contracts.ReviewComment], error) {
	if h.cfg.Reviews == nil {
		return contracts.Page[contracts.ReviewComment]{}, notConfigured("reviews")
	}
	return h.cfg.Reviews.ListComments(ctx, req)
}

func (h *Hub) ResolveComment(ctx context.Context, req contracts.ResolveCommentRequest) (contracts.ReviewComment, error) {
	if h.cfg.Reviews == nil {
		return contracts.ReviewComment{}, notConfigured("reviews")
	}
	return h.cfg.Reviews.ResolveComment(ctx, req)
}

// ---- NoteAPI (WP11) ----

func (h *Hub) CreateNote(ctx context.Context, req contracts.CreateNoteRequest) (contracts.Note, error) {
	if h.cfg.Notes == nil {
		return contracts.Note{}, notConfigured("notes")
	}
	return h.cfg.Notes.CreateNote(ctx, req)
}

func (h *Hub) GetNote(ctx context.Context, req contracts.GetRequest) (contracts.Note, error) {
	if h.cfg.Notes == nil {
		return contracts.Note{}, notConfigured("notes")
	}
	return h.cfg.Notes.GetNote(ctx, req)
}

func (h *Hub) ListNotes(ctx context.Context, req contracts.ListNotesRequest) (contracts.Page[contracts.Note], error) {
	if h.cfg.Notes == nil {
		return contracts.Page[contracts.Note]{}, notConfigured("notes")
	}
	return h.cfg.Notes.ListNotes(ctx, req)
}

func (h *Hub) UpdateNoteSync(ctx context.Context, req contracts.UpdateNoteSyncRequest) (contracts.Note, error) {
	if h.cfg.Notes == nil {
		return contracts.Note{}, notConfigured("notes")
	}
	return h.cfg.Notes.UpdateNoteSync(ctx, req)
}

func (h *Hub) GetLaunchContext(ctx context.Context, req contracts.ContextRequest) (contracts.LaunchContext, error) {
	if h.cfg.Notes == nil {
		return contracts.LaunchContext{}, notConfigured("notes")
	}
	return h.cfg.Notes.GetLaunchContext(ctx, req)
}

// ---- AttentionAPI (WP17) ----

func (h *Hub) GetAttention(ctx context.Context, req contracts.GetRequest) (contracts.AttentionItem, error) {
	if h.cfg.Attention == nil {
		return contracts.AttentionItem{}, notConfigured("attention")
	}
	it, err := h.cfg.Attention.GetAttention(ctx, req)
	if err == nil {
		one := []contracts.AttentionItem{it}
		h.resolveAttentionTitles(ctx, one)
		it = one[0]
	}
	return it, err
}

func (h *Hub) ListAttention(ctx context.Context, req contracts.ListAttentionRequest) (contracts.Page[contracts.AttentionItem], error) {
	if h.cfg.Attention == nil {
		return contracts.Page[contracts.AttentionItem]{}, notConfigured("attention")
	}
	page, err := h.cfg.Attention.ListAttention(ctx, req)
	if err == nil {
		h.resolveAttentionTitles(ctx, page.Items)
	}
	return page, err
}

func (h *Hub) UpdateAttention(ctx context.Context, req contracts.UpdateAttentionRequest) (contracts.AttentionItem, error) {
	if h.cfg.Attention == nil {
		return contracts.AttentionItem{}, notConfigured("attention")
	}
	// What the item stood for when he marked it: mail it collected up to
	// its last update (a message that arrives later is not acknowledged).
	var before contracts.AttentionItem
	if req.State == contracts.AttnHandled {
		before, _ = h.cfg.Attention.GetAttention(ctx, contracts.GetRequest{ID: req.AttentionID})
	}
	it, err := h.cfg.Attention.UpdateAttention(ctx, req)
	if p, ok := contracts.PrincipalFrom(ctx); err == nil && ok && isOwner(p) && it.Kind == contracts.AttnMessage && it.State == contracts.AttnHandled &&
		before.AttentionID != "" && before.State != contracts.AttnHandled {
		h.ackMessageItem(ctx, before)
	}
	return it, err
}

// ackMessageItem: the owner marking a message item done (the phone's Done,
// the web's read) acknowledges the mail it stands for, so no surface still
// shows it waiting on him. Best effort.
func (h *Hub) ackMessageItem(ctx context.Context, it contracts.AttentionItem) {
	var sel store.OwnerMail
	switch {
	case it.Object.Kind == contracts.KindThread && !contracts.ValidID(it.Object.ID, contracts.PrefixAsk):
		sel.ThreadID = it.Object.ID
	case it.Object.Kind == contracts.KindMessage && it.SessionID != "":
		sel.FromSession, sel.NoThread = it.SessionID, true // a sender's unthreaded mail is one item
	case it.Object.Kind == contracts.KindMessage:
		sel.MessageID = it.Object.ID
	default:
		return
	}
	sel.Through = it.UpdatedAt
	_, _ = h.msgs.AckOwnerMail(ctx, sel, contracts.AckRefEngaged)
}

func (h *Hub) FlagAttention(ctx context.Context, req contracts.FlagAttentionRequest) (contracts.AttentionItem, error) {
	if h.cfg.Attention == nil {
		return contracts.AttentionItem{}, notConfigured("attention")
	}
	return h.cfg.Attention.FlagAttention(ctx, req)
}

func (h *Hub) GetNotificationPrefs(ctx context.Context) (contracts.NotificationPrefs, error) {
	if h.cfg.Attention == nil {
		return contracts.NotificationPrefs{}, notConfigured("attention")
	}
	return h.cfg.Attention.GetNotificationPrefs(ctx)
}

func (h *Hub) SetNotificationPrefs(ctx context.Context, req contracts.SetNotificationPrefsRequest) (contracts.NotificationPrefs, error) {
	if h.cfg.Attention == nil {
		return contracts.NotificationPrefs{}, notConfigured("attention")
	}
	return h.cfg.Attention.SetNotificationPrefs(ctx, req)
}

func (h *Hub) RegisterPush(ctx context.Context, req contracts.PushRegistration) (contracts.PushRegistrationResult, error) {
	if h.cfg.Attention == nil {
		return contracts.PushRegistrationResult{}, notConfigured("attention")
	}
	return h.cfg.Attention.RegisterPush(ctx, req)
}

// ---- contracts v1.1 additions ----

func (h *Hub) DiscardProposal(ctx context.Context, req contracts.DiscardProposalRequest) (contracts.DocumentProposal, error) {
	if h.cfg.Documents == nil {
		return contracts.DocumentProposal{}, notConfigured("documents")
	}
	return h.cfg.Documents.DiscardProposal(ctx, req)
}

func (h *Hub) GetUpload(ctx context.Context, req contracts.GetRequest) (contracts.Upload, error) {
	if h.cfg.Artifacts == nil {
		return contracts.Upload{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.GetUpload(ctx, req)
}

func (h *Hub) UpdateArtifact(ctx context.Context, req contracts.UpdateArtifactRequest) (contracts.Artifact, error) {
	if h.cfg.Artifacts == nil {
		return contracts.Artifact{}, notConfigured("artifacts")
	}
	return h.cfg.Artifacts.UpdateArtifact(ctx, req)
}

func (h *Hub) GetComment(ctx context.Context, req contracts.GetRequest) (contracts.ReviewComment, error) {
	if h.cfg.Reviews == nil {
		return contracts.ReviewComment{}, notConfigured("reviews")
	}
	return h.cfg.Reviews.GetComment(ctx, req)
}

func (h *Hub) PromoteNote(ctx context.Context, req contracts.PromoteNoteRequest) (contracts.DocumentWithRevision, error) {
	if h.cfg.Notes == nil {
		return contracts.DocumentWithRevision{}, notConfigured("notes")
	}
	return h.cfg.Notes.PromoteNote(ctx, req)
}
