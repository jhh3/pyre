package daemon

import (
	"context"
	"io"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
)

type bearerKey struct{}

// WithBearer returns ctx carrying the raw bearer token of a local request,
// so the proxy forwards it to the hub with the caller's own identity.
func WithBearer(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, bearerKey{}, tok)
}

func bearerFrom(ctx context.Context) string {
	s, _ := ctx.Value(bearerKey{}).(string)
	return s
}

// upstream returns the hub client for bearer, or ErrUnavailable when no
// hub is configured.
func (d *Daemon) upstream(bearer string) (c.API, error) {
	if d.cfg.Upstream == nil {
		return nil, c.Errorf(c.ErrUnavailable, "no hub configured on this node")
	}
	key := hashToken(bearer)
	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.upstreams[key]; ok {
		return a, nil
	}
	if len(d.upstreams) >= 64 {
		for k := range d.upstreams {
			delete(d.upstreams, k)
			break
		}
	}
	a := d.cfg.Upstream(bearer)
	d.upstreams[key] = a
	return a, nil
}

// proxy forwards every contracts.API call to the hub with the request's
// bearer. LocalAPI overrides the calls that have node-local fallbacks.
type proxy struct{ d *Daemon }

func fwd[Req, Resp any](p proxy, ctx context.Context, req Req, f func(c.API, context.Context, Req) (Resp, error)) (Resp, error) {
	a, err := p.d.upstream(bearerFrom(ctx))
	if err != nil {
		var z Resp
		return z, err
	}
	return f(a, p.d.upstreamCtx(ctx), req)
}

func fwd0[Resp any](p proxy, ctx context.Context, f func(c.API, context.Context) (Resp, error)) (Resp, error) {
	a, err := p.d.upstream(bearerFrom(ctx))
	if err != nil {
		var z Resp
		return z, err
	}
	return f(a, p.d.upstreamCtx(ctx))
}

// SetHubLink records whether the node's hub link is connected (pyred calls
// it on every link state change).
func (d *Daemon) SetHubLink(connected bool) { d.hubLinkDown.Store(!connected) }

// hubUnreachable reports whether proxied calls cannot reach a hub right
// now: no hub is configured, or the hub link is down.
func (d *Daemon) hubUnreachable() bool {
	return d.cfg.Upstream == nil || d.hubLinkDown.Load()
}

// upstreamCtx makes proxied calls single-attempt while the hub link is
// down: the hub is known unreachable, so local fallbacks (read cache,
// node-local sessions, outbox) answer at once instead of after the client's
// retry backoff. With the link up, the client's normal retries apply.
func (d *Daemon) upstreamCtx(ctx context.Context) context.Context {
	if d.hubLinkDown.Load() {
		return client.WithMaxAttempts(ctx, 1)
	}
	return ctx
}

var _ c.API = proxy{}

func (p proxy) Hello(ctx context.Context, r c.HelloRequest) (c.HelloResponse, error) {
	return fwd(p, ctx, r, c.API.Hello)
}
func (p proxy) WhoAmI(ctx context.Context) (c.Principal, error) { return fwd0(p, ctx, c.API.WhoAmI) }
func (p proxy) ListNodes(ctx context.Context, r c.ListNodesRequest) (c.Page[c.Node], error) {
	return fwd(p, ctx, r, c.API.ListNodes)
}
func (p proxy) GetNode(ctx context.Context, r c.GetRequest) (c.Node, error) {
	return fwd(p, ctx, r, c.API.GetNode)
}
func (p proxy) CreateEnrollment(ctx context.Context, r c.EnrollmentRequest) (c.EnrollmentTicket, error) {
	return fwd(p, ctx, r, c.API.CreateEnrollment)
}
func (p proxy) RevokeNode(ctx context.Context, r c.RevokeRequest) (c.Node, error) {
	return fwd(p, ctx, r, c.API.RevokeNode)
}
func (p proxy) CreatePairing(ctx context.Context, r c.PairingRequest) (c.PairingTicket, error) {
	return fwd(p, ctx, r, c.API.CreatePairing)
}
func (p proxy) ListClients(ctx context.Context, r c.PageRequest) (c.Page[c.ClientInfo], error) {
	return fwd(p, ctx, r, c.API.ListClients)
}
func (p proxy) RevokeClient(ctx context.Context, r c.RevokeRequest) (c.ClientInfo, error) {
	return fwd(p, ctx, r, c.API.RevokeClient)
}
func (p proxy) ListSessions(ctx context.Context, r c.ListSessionsRequest) (c.Page[c.SessionView], error) {
	return fwd(p, ctx, r, c.API.ListSessions)
}
func (p proxy) GetSession(ctx context.Context, r c.GetRequest) (c.SessionView, error) {
	return fwd(p, ctx, r, c.API.GetSession)
}
func (p proxy) StartSession(ctx context.Context, r c.StartSessionRequest) (c.StartSessionResponse, error) {
	return fwd(p, ctx, r, c.API.StartSession)
}
func (p proxy) ReadSession(ctx context.Context, r c.ReadSessionRequest) (c.ReadSessionResponse, error) {
	return fwd(p, ctx, r, c.API.ReadSession)
}
func (p proxy) AttachSession(ctx context.Context, r c.AttachRequest) (c.AttachDescriptor, error) {
	return fwd(p, ctx, r, c.API.AttachSession)
}
func (p proxy) InterruptSession(ctx context.Context, r c.SessionCommandRequest) (c.CommandReceipt, error) {
	return fwd(p, ctx, r, c.API.InterruptSession)
}
func (p proxy) StopSession(ctx context.Context, r c.SessionCommandRequest) (c.CommandReceipt, error) {
	return fwd(p, ctx, r, c.API.StopSession)
}
func (p proxy) ResumeSession(ctx context.Context, r c.ResumeRequest) (c.StartSessionResponse, error) {
	return fwd(p, ctx, r, c.API.ResumeSession)
}
func (p proxy) AssociateSession(ctx context.Context, r c.AssociateRequest) (c.SessionView, error) {
	return fwd(p, ctx, r, c.API.AssociateSession)
}
func (p proxy) UpdateSession(ctx context.Context, r c.UpdateSessionRequest) (c.SessionView, error) {
	return fwd(p, ctx, r, c.API.UpdateSession)
}
func (p proxy) ListRuns(ctx context.Context, r c.ListRunsRequest) (c.Page[c.Run], error) {
	return fwd(p, ctx, r, c.API.ListRuns)
}
func (p proxy) AcquireLease(ctx context.Context, r c.LeaseRequest) (c.InputLease, error) {
	return fwd(p, ctx, r, c.API.AcquireLease)
}
func (p proxy) ReleaseLease(ctx context.Context, r c.LeaseReleaseRequest) (c.InputLease, error) {
	return fwd(p, ctx, r, c.API.ReleaseLease)
}
func (p proxy) ListApprovals(ctx context.Context, r c.ListApprovalsRequest) (c.Page[c.Approval], error) {
	return fwd(p, ctx, r, c.API.ListApprovals)
}
func (p proxy) GetApproval(ctx context.Context, r c.GetRequest) (c.Approval, error) {
	return fwd(p, ctx, r, c.API.GetApproval)
}
func (p proxy) AnswerApproval(ctx context.Context, r c.AnswerApprovalRequest) (c.Approval, error) {
	return fwd(p, ctx, r, c.API.AnswerApproval)
}
func (p proxy) SendMessage(ctx context.Context, r c.SendMessageRequest) (c.MessageView, error) {
	return fwd(p, ctx, r, c.API.SendMessage)
}
func (p proxy) GetMessage(ctx context.Context, r c.GetRequest) (c.MessageView, error) {
	return fwd(p, ctx, r, c.API.GetMessage)
}
func (p proxy) ListMessages(ctx context.Context, r c.ListMessagesRequest) (c.Page[c.MessageView], error) {
	return fwd(p, ctx, r, c.API.ListMessages)
}
func (p proxy) AckMessage(ctx context.Context, r c.AckMessageRequest) (c.MessageView, error) {
	return fwd(p, ctx, r, c.API.AckMessage)
}
func (p proxy) ReissueMessage(ctx context.Context, r c.ReissueMessageRequest) (c.MessageView, error) {
	return fwd(p, ctx, r, c.API.ReissueMessage)
}
func (p proxy) ResolveDelivery(ctx context.Context, r c.ResolveDeliveryRequest) (c.MessageView, error) {
	return fwd(p, ctx, r, c.API.ResolveDelivery)
}
func (p proxy) CancelMessage(ctx context.Context, r c.CancelMessageRequest) (c.MessageView, error) {
	return fwd(p, ctx, r, c.API.CancelMessage)
}
func (p proxy) CreateProject(ctx context.Context, r c.CreateProjectRequest) (c.Project, error) {
	return fwd(p, ctx, r, c.API.CreateProject)
}
func (p proxy) GetProject(ctx context.Context, r c.GetRequest) (c.Project, error) {
	return fwd(p, ctx, r, c.API.GetProject)
}
func (p proxy) ListProjects(ctx context.Context, r c.ListProjectsRequest) (c.Page[c.Project], error) {
	return fwd(p, ctx, r, c.API.ListProjects)
}
func (p proxy) UpdateProject(ctx context.Context, r c.UpdateProjectRequest) (c.Project, error) {
	return fwd(p, ctx, r, c.API.UpdateProject)
}
func (p proxy) CreateDocument(ctx context.Context, r c.CreateDocumentRequest) (c.DocumentWithRevision, error) {
	return fwd(p, ctx, r, c.API.CreateDocument)
}
func (p proxy) GetDocument(ctx context.Context, r c.GetDocumentRequest) (c.DocumentWithRevision, error) {
	return fwd(p, ctx, r, c.API.GetDocument)
}
func (p proxy) ListDocuments(ctx context.Context, r c.ListDocumentsRequest) (c.Page[c.Document], error) {
	return fwd(p, ctx, r, c.API.ListDocuments)
}
func (p proxy) ListRevisions(ctx context.Context, r c.ListRevisionsRequest) (c.Page[c.DocumentRevision], error) {
	return fwd(p, ctx, r, c.API.ListRevisions)
}
func (p proxy) ProposeRevision(ctx context.Context, r c.ProposeRevisionRequest) (c.DocumentWithRevision, error) {
	return fwd(p, ctx, r, c.API.ProposeRevision)
}
func (p proxy) ListProposals(ctx context.Context, r c.ListProposalsRequest) (c.Page[c.DocumentProposal], error) {
	return fwd(p, ctx, r, c.API.ListProposals)
}
func (p proxy) BeginUpload(ctx context.Context, r c.BeginUploadRequest) (c.Upload, error) {
	return fwd(p, ctx, r, c.API.BeginUpload)
}
func (p proxy) UploadChunk(ctx context.Context, uploadID string, offset int64, r io.Reader) (c.Upload, error) {
	a, err := p.d.upstream(bearerFrom(ctx))
	if err != nil {
		return c.Upload{}, err
	}
	return a.UploadChunk(p.d.upstreamCtx(ctx), uploadID, offset, r)
}
func (p proxy) FinalizeUpload(ctx context.Context, r c.FinalizeUploadRequest) (c.PublishReceipt, error) {
	return fwd(p, ctx, r, c.API.FinalizeUpload)
}
func (p proxy) GetArtifact(ctx context.Context, r c.GetRequest) (c.Artifact, error) {
	return fwd(p, ctx, r, c.API.GetArtifact)
}
func (p proxy) ListArtifacts(ctx context.Context, r c.ListArtifactsRequest) (c.Page[c.Artifact], error) {
	return fwd(p, ctx, r, c.API.ListArtifacts)
}
func (p proxy) ListVersions(ctx context.Context, r c.ListVersionsRequest) (c.Page[c.ArtifactVersion], error) {
	return fwd(p, ctx, r, c.API.ListVersions)
}
func (p proxy) GetVersion(ctx context.Context, r c.GetRequest) (c.ArtifactVersion, error) {
	return fwd(p, ctx, r, c.API.GetVersion)
}
func (p proxy) OpenContent(ctx context.Context, versionID string) (io.ReadCloser, c.ArtifactVersion, error) {
	a, err := p.d.upstream(bearerFrom(ctx))
	if err != nil {
		return nil, c.ArtifactVersion{}, err
	}
	return a.OpenContent(p.d.upstreamCtx(ctx), versionID)
}
func (p proxy) CreateComment(ctx context.Context, r c.CreateCommentRequest) (c.ReviewComment, error) {
	return fwd(p, ctx, r, c.API.CreateComment)
}
func (p proxy) ListComments(ctx context.Context, r c.ListCommentsRequest) (c.Page[c.ReviewComment], error) {
	return fwd(p, ctx, r, c.API.ListComments)
}
func (p proxy) ResolveComment(ctx context.Context, r c.ResolveCommentRequest) (c.ReviewComment, error) {
	return fwd(p, ctx, r, c.API.ResolveComment)
}
func (p proxy) CreateNote(ctx context.Context, r c.CreateNoteRequest) (c.Note, error) {
	return fwd(p, ctx, r, c.API.CreateNote)
}
func (p proxy) GetNote(ctx context.Context, r c.GetRequest) (c.Note, error) {
	return fwd(p, ctx, r, c.API.GetNote)
}
func (p proxy) ListNotes(ctx context.Context, r c.ListNotesRequest) (c.Page[c.Note], error) {
	return fwd(p, ctx, r, c.API.ListNotes)
}
func (p proxy) UpdateNoteSync(ctx context.Context, r c.UpdateNoteSyncRequest) (c.Note, error) {
	return fwd(p, ctx, r, c.API.UpdateNoteSync)
}
func (p proxy) GetLaunchContext(ctx context.Context, r c.ContextRequest) (c.LaunchContext, error) {
	return fwd(p, ctx, r, c.API.GetLaunchContext)
}
func (p proxy) ListAccounts(ctx context.Context, r c.ListAccountsRequest) (c.Page[c.AccountView], error) {
	return fwd(p, ctx, r, c.API.ListAccounts)
}
func (p proxy) UpsertAccount(ctx context.Context, r c.UpsertAccountRequest) (c.Account, error) {
	return fwd(p, ctx, r, c.API.UpsertAccount)
}
func (p proxy) ListFallbacks(ctx context.Context, r c.ListFallbacksRequest) (c.Page[c.FallbackPlan], error) {
	return fwd(p, ctx, r, c.API.ListFallbacks)
}
func (p proxy) ListAttention(ctx context.Context, r c.ListAttentionRequest) (c.Page[c.AttentionItem], error) {
	return fwd(p, ctx, r, c.API.ListAttention)
}
func (p proxy) UpdateAttention(ctx context.Context, r c.UpdateAttentionRequest) (c.AttentionItem, error) {
	return fwd(p, ctx, r, c.API.UpdateAttention)
}
func (p proxy) FlagAttention(ctx context.Context, r c.FlagAttentionRequest) (c.AttentionItem, error) {
	return fwd(p, ctx, r, c.API.FlagAttention)
}
func (p proxy) GetNotificationPrefs(ctx context.Context) (c.NotificationPrefs, error) {
	return fwd0(p, ctx, c.API.GetNotificationPrefs)
}
func (p proxy) SetNotificationPrefs(ctx context.Context, r c.SetNotificationPrefsRequest) (c.NotificationPrefs, error) {
	return fwd(p, ctx, r, c.API.SetNotificationPrefs)
}
func (p proxy) RegisterPush(ctx context.Context, r c.PushRegistration) (c.PushRegistrationResult, error) {
	return fwd(p, ctx, r, c.API.RegisterPush)
}
func (p proxy) ListEvents(ctx context.Context, r c.ListEventsRequest) (c.ListEventsResponse, error) {
	return fwd(p, ctx, r, c.API.ListEvents)
}
func (p proxy) InvokeNodeAction(ctx context.Context, r c.NodeActionRequest) (c.NodeActionResult, error) {
	return fwd(p, ctx, r, c.API.InvokeNodeAction)
}
func (p proxy) ListConnections(ctx context.Context, r c.ListConnectionsRequest) (c.Page[c.Connection], error) {
	return fwd(p, ctx, r, c.API.ListConnections)
}
func (p proxy) DiscardProposal(ctx context.Context, r c.DiscardProposalRequest) (c.DocumentProposal, error) {
	return fwd(p, ctx, r, c.API.DiscardProposal)
}
func (p proxy) GetUpload(ctx context.Context, r c.GetRequest) (c.Upload, error) {
	return fwd(p, ctx, r, c.API.GetUpload)
}
func (p proxy) UpdateArtifact(ctx context.Context, r c.UpdateArtifactRequest) (c.Artifact, error) {
	return fwd(p, ctx, r, c.API.UpdateArtifact)
}
func (p proxy) GetComment(ctx context.Context, r c.GetRequest) (c.ReviewComment, error) {
	return fwd(p, ctx, r, c.API.GetComment)
}
func (p proxy) PromoteNote(ctx context.Context, r c.PromoteNoteRequest) (c.DocumentWithRevision, error) {
	return fwd(p, ctx, r, c.API.PromoteNote)
}
