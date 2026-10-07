// Code generated from internal/contracts/api.go by a WP03 helper script; DO NOT EDIT by hand.

package conformance

import (
	"context"
	"io"

	c "github.com/jhh3/pyre/internal/contracts"
)

// ByPrincipal returns a contracts.API that forwards every call to the
// implementation chosen from the call's context, e.g. one HTTP client per
// principal token. It lets the suite drive targets whose identity comes
// from transport credentials rather than the context.
func ByPrincipal(pick func(ctx context.Context) c.API) c.API { return routed{pick} }

type routed struct {
	pick func(ctx context.Context) c.API
}

func (rt routed) Hello(ctx context.Context, req c.HelloRequest) (c.HelloResponse, error) {
	return rt.pick(ctx).Hello(ctx, req)
}

func (rt routed) WhoAmI(ctx context.Context) (c.Principal, error) {
	return rt.pick(ctx).WhoAmI(ctx)
}

func (rt routed) ListNodes(ctx context.Context, req c.ListNodesRequest) (c.Page[c.Node], error) {
	return rt.pick(ctx).ListNodes(ctx, req)
}

func (rt routed) GetNode(ctx context.Context, req c.GetRequest) (c.Node, error) {
	return rt.pick(ctx).GetNode(ctx, req)
}

func (rt routed) CreateEnrollment(ctx context.Context, req c.EnrollmentRequest) (c.EnrollmentTicket, error) {
	return rt.pick(ctx).CreateEnrollment(ctx, req)
}

func (rt routed) RevokeNode(ctx context.Context, req c.RevokeRequest) (c.Node, error) {
	return rt.pick(ctx).RevokeNode(ctx, req)
}

func (rt routed) CreatePairing(ctx context.Context, req c.PairingRequest) (c.PairingTicket, error) {
	return rt.pick(ctx).CreatePairing(ctx, req)
}

func (rt routed) ListClients(ctx context.Context, req c.PageRequest) (c.Page[c.ClientInfo], error) {
	return rt.pick(ctx).ListClients(ctx, req)
}

func (rt routed) RevokeClient(ctx context.Context, req c.RevokeRequest) (c.ClientInfo, error) {
	return rt.pick(ctx).RevokeClient(ctx, req)
}

func (rt routed) ListSessions(ctx context.Context, req c.ListSessionsRequest) (c.Page[c.SessionView], error) {
	return rt.pick(ctx).ListSessions(ctx, req)
}

func (rt routed) GetSession(ctx context.Context, req c.GetRequest) (c.SessionView, error) {
	return rt.pick(ctx).GetSession(ctx, req)
}

func (rt routed) StartSession(ctx context.Context, req c.StartSessionRequest) (c.StartSessionResponse, error) {
	return rt.pick(ctx).StartSession(ctx, req)
}

func (rt routed) ReadSession(ctx context.Context, req c.ReadSessionRequest) (c.ReadSessionResponse, error) {
	return rt.pick(ctx).ReadSession(ctx, req)
}

func (rt routed) AttachSession(ctx context.Context, req c.AttachRequest) (c.AttachDescriptor, error) {
	return rt.pick(ctx).AttachSession(ctx, req)
}

func (rt routed) InterruptSession(ctx context.Context, req c.SessionCommandRequest) (c.CommandReceipt, error) {
	return rt.pick(ctx).InterruptSession(ctx, req)
}

func (rt routed) StopSession(ctx context.Context, req c.SessionCommandRequest) (c.CommandReceipt, error) {
	return rt.pick(ctx).StopSession(ctx, req)
}

func (rt routed) ResumeSession(ctx context.Context, req c.ResumeRequest) (c.StartSessionResponse, error) {
	return rt.pick(ctx).ResumeSession(ctx, req)
}

func (rt routed) AssociateSession(ctx context.Context, req c.AssociateRequest) (c.SessionView, error) {
	return rt.pick(ctx).AssociateSession(ctx, req)
}

func (rt routed) UpdateSession(ctx context.Context, req c.UpdateSessionRequest) (c.SessionView, error) {
	return rt.pick(ctx).UpdateSession(ctx, req)
}

func (rt routed) ListRuns(ctx context.Context, req c.ListRunsRequest) (c.Page[c.Run], error) {
	return rt.pick(ctx).ListRuns(ctx, req)
}

func (rt routed) AcquireLease(ctx context.Context, req c.LeaseRequest) (c.InputLease, error) {
	return rt.pick(ctx).AcquireLease(ctx, req)
}

func (rt routed) ReleaseLease(ctx context.Context, req c.LeaseReleaseRequest) (c.InputLease, error) {
	return rt.pick(ctx).ReleaseLease(ctx, req)
}

func (rt routed) ListApprovals(ctx context.Context, req c.ListApprovalsRequest) (c.Page[c.Approval], error) {
	return rt.pick(ctx).ListApprovals(ctx, req)
}

func (rt routed) GetApproval(ctx context.Context, req c.GetRequest) (c.Approval, error) {
	return rt.pick(ctx).GetApproval(ctx, req)
}

func (rt routed) AnswerApproval(ctx context.Context, req c.AnswerApprovalRequest) (c.Approval, error) {
	return rt.pick(ctx).AnswerApproval(ctx, req)
}

func (rt routed) SendMessage(ctx context.Context, req c.SendMessageRequest) (c.MessageView, error) {
	return rt.pick(ctx).SendMessage(ctx, req)
}

func (rt routed) GetMessage(ctx context.Context, req c.GetRequest) (c.MessageView, error) {
	return rt.pick(ctx).GetMessage(ctx, req)
}

func (rt routed) ListMessages(ctx context.Context, req c.ListMessagesRequest) (c.Page[c.MessageView], error) {
	return rt.pick(ctx).ListMessages(ctx, req)
}

func (rt routed) AckMessage(ctx context.Context, req c.AckMessageRequest) (c.MessageView, error) {
	return rt.pick(ctx).AckMessage(ctx, req)
}

func (rt routed) ReissueMessage(ctx context.Context, req c.ReissueMessageRequest) (c.MessageView, error) {
	return rt.pick(ctx).ReissueMessage(ctx, req)
}

func (rt routed) ResolveDelivery(ctx context.Context, req c.ResolveDeliveryRequest) (c.MessageView, error) {
	return rt.pick(ctx).ResolveDelivery(ctx, req)
}

func (rt routed) CancelMessage(ctx context.Context, req c.CancelMessageRequest) (c.MessageView, error) {
	return rt.pick(ctx).CancelMessage(ctx, req)
}

func (rt routed) CreateProject(ctx context.Context, req c.CreateProjectRequest) (c.Project, error) {
	return rt.pick(ctx).CreateProject(ctx, req)
}

func (rt routed) GetProject(ctx context.Context, req c.GetRequest) (c.Project, error) {
	return rt.pick(ctx).GetProject(ctx, req)
}

func (rt routed) ListProjects(ctx context.Context, req c.ListProjectsRequest) (c.Page[c.Project], error) {
	return rt.pick(ctx).ListProjects(ctx, req)
}

func (rt routed) UpdateProject(ctx context.Context, req c.UpdateProjectRequest) (c.Project, error) {
	return rt.pick(ctx).UpdateProject(ctx, req)
}

func (rt routed) CreateDocument(ctx context.Context, req c.CreateDocumentRequest) (c.DocumentWithRevision, error) {
	return rt.pick(ctx).CreateDocument(ctx, req)
}

func (rt routed) GetDocument(ctx context.Context, req c.GetDocumentRequest) (c.DocumentWithRevision, error) {
	return rt.pick(ctx).GetDocument(ctx, req)
}

func (rt routed) ListDocuments(ctx context.Context, req c.ListDocumentsRequest) (c.Page[c.Document], error) {
	return rt.pick(ctx).ListDocuments(ctx, req)
}

func (rt routed) ListRevisions(ctx context.Context, req c.ListRevisionsRequest) (c.Page[c.DocumentRevision], error) {
	return rt.pick(ctx).ListRevisions(ctx, req)
}

func (rt routed) ProposeRevision(ctx context.Context, req c.ProposeRevisionRequest) (c.DocumentWithRevision, error) {
	return rt.pick(ctx).ProposeRevision(ctx, req)
}

func (rt routed) ListProposals(ctx context.Context, req c.ListProposalsRequest) (c.Page[c.DocumentProposal], error) {
	return rt.pick(ctx).ListProposals(ctx, req)
}

func (rt routed) DiscardProposal(ctx context.Context, req c.DiscardProposalRequest) (c.DocumentProposal, error) {
	return rt.pick(ctx).DiscardProposal(ctx, req)
}

func (rt routed) BeginUpload(ctx context.Context, req c.BeginUploadRequest) (c.Upload, error) {
	return rt.pick(ctx).BeginUpload(ctx, req)
}

func (rt routed) UploadChunk(ctx context.Context, uploadID string, offset int64, r io.Reader) (c.Upload, error) {
	return rt.pick(ctx).UploadChunk(ctx, uploadID, offset, r)
}

func (rt routed) FinalizeUpload(ctx context.Context, req c.FinalizeUploadRequest) (c.PublishReceipt, error) {
	return rt.pick(ctx).FinalizeUpload(ctx, req)
}

func (rt routed) GetUpload(ctx context.Context, req c.GetRequest) (c.Upload, error) {
	return rt.pick(ctx).GetUpload(ctx, req)
}

func (rt routed) UpdateArtifact(ctx context.Context, req c.UpdateArtifactRequest) (c.Artifact, error) {
	return rt.pick(ctx).UpdateArtifact(ctx, req)
}

func (rt routed) GetArtifact(ctx context.Context, req c.GetRequest) (c.Artifact, error) {
	return rt.pick(ctx).GetArtifact(ctx, req)
}

func (rt routed) ListArtifacts(ctx context.Context, req c.ListArtifactsRequest) (c.Page[c.Artifact], error) {
	return rt.pick(ctx).ListArtifacts(ctx, req)
}

func (rt routed) ListVersions(ctx context.Context, req c.ListVersionsRequest) (c.Page[c.ArtifactVersion], error) {
	return rt.pick(ctx).ListVersions(ctx, req)
}

func (rt routed) GetVersion(ctx context.Context, req c.GetRequest) (c.ArtifactVersion, error) {
	return rt.pick(ctx).GetVersion(ctx, req)
}

func (rt routed) OpenContent(ctx context.Context, versionID string) (io.ReadCloser, c.ArtifactVersion, error) {
	return rt.pick(ctx).OpenContent(ctx, versionID)
}

func (rt routed) CreateComment(ctx context.Context, req c.CreateCommentRequest) (c.ReviewComment, error) {
	return rt.pick(ctx).CreateComment(ctx, req)
}

func (rt routed) GetComment(ctx context.Context, req c.GetRequest) (c.ReviewComment, error) {
	return rt.pick(ctx).GetComment(ctx, req)
}

func (rt routed) ListComments(ctx context.Context, req c.ListCommentsRequest) (c.Page[c.ReviewComment], error) {
	return rt.pick(ctx).ListComments(ctx, req)
}

func (rt routed) ResolveComment(ctx context.Context, req c.ResolveCommentRequest) (c.ReviewComment, error) {
	return rt.pick(ctx).ResolveComment(ctx, req)
}

func (rt routed) CreateNote(ctx context.Context, req c.CreateNoteRequest) (c.Note, error) {
	return rt.pick(ctx).CreateNote(ctx, req)
}

func (rt routed) GetNote(ctx context.Context, req c.GetRequest) (c.Note, error) {
	return rt.pick(ctx).GetNote(ctx, req)
}

func (rt routed) ListNotes(ctx context.Context, req c.ListNotesRequest) (c.Page[c.Note], error) {
	return rt.pick(ctx).ListNotes(ctx, req)
}

func (rt routed) UpdateNoteSync(ctx context.Context, req c.UpdateNoteSyncRequest) (c.Note, error) {
	return rt.pick(ctx).UpdateNoteSync(ctx, req)
}

func (rt routed) PromoteNote(ctx context.Context, req c.PromoteNoteRequest) (c.DocumentWithRevision, error) {
	return rt.pick(ctx).PromoteNote(ctx, req)
}

func (rt routed) GetLaunchContext(ctx context.Context, req c.ContextRequest) (c.LaunchContext, error) {
	return rt.pick(ctx).GetLaunchContext(ctx, req)
}

func (rt routed) ListAccounts(ctx context.Context, req c.ListAccountsRequest) (c.Page[c.AccountView], error) {
	return rt.pick(ctx).ListAccounts(ctx, req)
}

func (rt routed) UpsertAccount(ctx context.Context, req c.UpsertAccountRequest) (c.Account, error) {
	return rt.pick(ctx).UpsertAccount(ctx, req)
}

func (rt routed) ListFallbacks(ctx context.Context, req c.ListFallbacksRequest) (c.Page[c.FallbackPlan], error) {
	return rt.pick(ctx).ListFallbacks(ctx, req)
}

func (rt routed) ListAttention(ctx context.Context, req c.ListAttentionRequest) (c.Page[c.AttentionItem], error) {
	return rt.pick(ctx).ListAttention(ctx, req)
}

func (rt routed) UpdateAttention(ctx context.Context, req c.UpdateAttentionRequest) (c.AttentionItem, error) {
	return rt.pick(ctx).UpdateAttention(ctx, req)
}

func (rt routed) FlagAttention(ctx context.Context, req c.FlagAttentionRequest) (c.AttentionItem, error) {
	return rt.pick(ctx).FlagAttention(ctx, req)
}

func (rt routed) GetNotificationPrefs(ctx context.Context) (c.NotificationPrefs, error) {
	return rt.pick(ctx).GetNotificationPrefs(ctx)
}

func (rt routed) SetNotificationPrefs(ctx context.Context, req c.SetNotificationPrefsRequest) (c.NotificationPrefs, error) {
	return rt.pick(ctx).SetNotificationPrefs(ctx, req)
}

func (rt routed) RegisterPush(ctx context.Context, req c.PushRegistration) (c.PushRegistrationResult, error) {
	return rt.pick(ctx).RegisterPush(ctx, req)
}

func (rt routed) ListEvents(ctx context.Context, req c.ListEventsRequest) (c.ListEventsResponse, error) {
	return rt.pick(ctx).ListEvents(ctx, req)
}

func (rt routed) InvokeNodeAction(ctx context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	return rt.pick(ctx).InvokeNodeAction(ctx, req)
}

func (rt routed) MintPreviewURL(ctx context.Context, req c.MintPreviewRequest) (c.PreviewURL, error) {
	return rt.pick(ctx).MintPreviewURL(ctx, req)
}

func (rt routed) GetAttention(ctx context.Context, req c.GetRequest) (c.AttentionItem, error) {
	return rt.pick(ctx).GetAttention(ctx, req)
}

func (rt routed) CheckConnectionGrant(ctx context.Context, req c.CheckGrantRequest) (c.GrantDecision, error) {
	return rt.pick(ctx).CheckConnectionGrant(ctx, req)
}

func (rt routed) AcquireAccountLease(ctx context.Context, req c.AcquireAccountLeaseRequest) (c.AccountLease, error) {
	return rt.pick(ctx).AcquireAccountLease(ctx, req)
}

func (rt routed) RenewAccountLease(ctx context.Context, req c.RenewAccountLeaseRequest) (c.AccountLease, error) {
	return rt.pick(ctx).RenewAccountLease(ctx, req)
}

func (rt routed) ReleaseAccountLease(ctx context.Context, req c.ReleaseAccountLeaseRequest) (c.AccountLease, error) {
	return rt.pick(ctx).ReleaseAccountLease(ctx, req)
}

func (rt routed) GetCommandResult(ctx context.Context, req c.GetRequest) (c.NodeActionResult, error) {
	return rt.pick(ctx).GetCommandResult(ctx, req)
}

func (rt routed) ListConnections(ctx context.Context, req c.ListConnectionsRequest) (c.Page[c.Connection], error) {
	return rt.pick(ctx).ListConnections(ctx, req)
}
