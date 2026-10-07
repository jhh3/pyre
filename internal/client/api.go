package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func longPoll(waitSeconds int) time.Duration {
	if waitSeconds <= 0 {
		return 0
	}
	if waitSeconds > 60 {
		waitSeconds = 60
	}
	return time.Duration(waitSeconds)*time.Second + 5*time.Second
}

// ---- system ----

func (cl *Client) Hello(ctx context.Context, req c.HelloRequest) (c.HelloResponse, error) {
	if req.Protocol == 0 {
		req.Protocol = c.ProtocolVersion
	}
	var out c.HelloResponse
	return out, cl.read(ctx, c.OpHello, req, &out)
}

func (cl *Client) WhoAmI(ctx context.Context) (c.Principal, error) {
	var out c.Principal
	return out, cl.read(ctx, c.OpWhoAmI, nil, &out)
}

// ---- fleet ----

func (cl *Client) ListNodes(ctx context.Context, req c.ListNodesRequest) (c.Page[c.Node], error) {
	var out c.Page[c.Node]
	return out, cl.read(ctx, c.OpListNodes, req, &out)
}

func (cl *Client) GetNode(ctx context.Context, req c.GetRequest) (c.Node, error) {
	var out c.Node
	return out, cl.read(ctx, c.OpGetNode, req, &out)
}

func (cl *Client) CreateEnrollment(ctx context.Context, req c.EnrollmentRequest) (c.EnrollmentTicket, error) {
	var out c.EnrollmentTicket
	return out, cl.mutate(ctx, c.OpCreateEnrollment, &req.Mutation, &req, &out)
}

func (cl *Client) RevokeNode(ctx context.Context, req c.RevokeRequest) (c.Node, error) {
	var out c.Node
	return out, cl.mutate(ctx, c.OpRevokeNode, &req.Mutation, &req, &out)
}

func (cl *Client) CreatePairing(ctx context.Context, req c.PairingRequest) (c.PairingTicket, error) {
	var out c.PairingTicket
	return out, cl.mutate(ctx, c.OpCreatePairing, &req.Mutation, &req, &out)
}

func (cl *Client) ListClients(ctx context.Context, req c.PageRequest) (c.Page[c.ClientInfo], error) {
	var out c.Page[c.ClientInfo]
	return out, cl.read(ctx, c.OpListClients, req, &out)
}

func (cl *Client) RevokeClient(ctx context.Context, req c.RevokeRequest) (c.ClientInfo, error) {
	var out c.ClientInfo
	return out, cl.mutate(ctx, c.OpRevokeClient, &req.Mutation, &req, &out)
}

// ---- sessions ----

func (cl *Client) ListSessions(ctx context.Context, req c.ListSessionsRequest) (c.Page[c.SessionView], error) {
	var out c.Page[c.SessionView]
	return out, cl.read(ctx, c.OpListSessions, req, &out)
}

func (cl *Client) GetSession(ctx context.Context, req c.GetRequest) (c.SessionView, error) {
	var out c.SessionView
	return out, cl.read(ctx, c.OpGetSession, req, &out)
}

func (cl *Client) StartSession(ctx context.Context, req c.StartSessionRequest) (c.StartSessionResponse, error) {
	var out c.StartSessionResponse
	return out, cl.mutate(ctx, c.OpStartSession, &req.Mutation, &req, &out)
}

func (cl *Client) ReadSession(ctx context.Context, req c.ReadSessionRequest) (c.ReadSessionResponse, error) {
	var out c.ReadSessionResponse
	return out, cl.read(ctx, c.OpReadSession, req, &out)
}

func (cl *Client) AttachSession(ctx context.Context, req c.AttachRequest) (c.AttachDescriptor, error) {
	var out c.AttachDescriptor
	return out, cl.mutate(ctx, c.OpAttachSession, &req.Mutation, &req, &out)
}

func (cl *Client) InterruptSession(ctx context.Context, req c.SessionCommandRequest) (c.CommandReceipt, error) {
	var out c.CommandReceipt
	return out, cl.mutate(ctx, c.OpInterruptSession, &req.Mutation, &req, &out)
}

func (cl *Client) StopSession(ctx context.Context, req c.SessionCommandRequest) (c.CommandReceipt, error) {
	var out c.CommandReceipt
	return out, cl.mutate(ctx, c.OpStopSession, &req.Mutation, &req, &out)
}

func (cl *Client) ResumeSession(ctx context.Context, req c.ResumeRequest) (c.StartSessionResponse, error) {
	var out c.StartSessionResponse
	return out, cl.mutate(ctx, c.OpResumeSession, &req.Mutation, &req, &out)
}

func (cl *Client) AssociateSession(ctx context.Context, req c.AssociateRequest) (c.SessionView, error) {
	var out c.SessionView
	return out, cl.mutate(ctx, c.OpAssociateSession, &req.Mutation, &req, &out)
}

func (cl *Client) UpdateSession(ctx context.Context, req c.UpdateSessionRequest) (c.SessionView, error) {
	var out c.SessionView
	return out, cl.mutate(ctx, c.OpUpdateSession, &req.Mutation, &req, &out)
}

func (cl *Client) ListRuns(ctx context.Context, req c.ListRunsRequest) (c.Page[c.Run], error) {
	var out c.Page[c.Run]
	return out, cl.read(ctx, c.OpListRuns, req, &out)
}

// ---- interaction ----

func (cl *Client) AcquireLease(ctx context.Context, req c.LeaseRequest) (c.InputLease, error) {
	var out c.InputLease
	return out, cl.mutate(ctx, c.OpAcquireLease, &req.Mutation, &req, &out)
}

func (cl *Client) ReleaseLease(ctx context.Context, req c.LeaseReleaseRequest) (c.InputLease, error) {
	var out c.InputLease
	return out, cl.mutate(ctx, c.OpReleaseLease, &req.Mutation, &req, &out)
}

func (cl *Client) ListApprovals(ctx context.Context, req c.ListApprovalsRequest) (c.Page[c.Approval], error) {
	var out c.Page[c.Approval]
	return out, cl.read(ctx, c.OpListApprovals, req, &out)
}

func (cl *Client) GetApproval(ctx context.Context, req c.GetRequest) (c.Approval, error) {
	var out c.Approval
	return out, cl.read(ctx, c.OpGetApproval, req, &out)
}

func (cl *Client) AnswerApproval(ctx context.Context, req c.AnswerApprovalRequest) (c.Approval, error) {
	var out c.Approval
	return out, cl.mutate(ctx, c.OpAnswerApproval, &req.Mutation, &req, &out)
}

// ---- messages ----

func (cl *Client) SendMessage(ctx context.Context, req c.SendMessageRequest) (c.MessageView, error) {
	var out c.MessageView
	return out, cl.mutate(ctx, c.OpSendMessage, &req.Mutation, &req, &out)
}

func (cl *Client) GetMessage(ctx context.Context, req c.GetRequest) (c.MessageView, error) {
	var out c.MessageView
	return out, cl.read(ctx, c.OpGetMessage, req, &out)
}

func (cl *Client) ListMessages(ctx context.Context, req c.ListMessagesRequest) (c.Page[c.MessageView], error) {
	var out c.Page[c.MessageView]
	return out, cl.call(ctx, c.OpListMessages, req, &out, true, longPoll(req.WaitSeconds))
}

func (cl *Client) AckMessage(ctx context.Context, req c.AckMessageRequest) (c.MessageView, error) {
	var out c.MessageView
	return out, cl.mutate(ctx, c.OpAckMessage, &req.Mutation, &req, &out)
}

func (cl *Client) ReissueMessage(ctx context.Context, req c.ReissueMessageRequest) (c.MessageView, error) {
	var out c.MessageView
	return out, cl.mutate(ctx, c.OpReissueMessage, &req.Mutation, &req, &out)
}

func (cl *Client) ResolveDelivery(ctx context.Context, req c.ResolveDeliveryRequest) (c.MessageView, error) {
	var out c.MessageView
	return out, cl.mutate(ctx, c.OpResolveDelivery, &req.Mutation, &req, &out)
}

func (cl *Client) CancelMessage(ctx context.Context, req c.CancelMessageRequest) (c.MessageView, error) {
	var out c.MessageView
	return out, cl.mutate(ctx, c.OpCancelMessage, &req.Mutation, &req, &out)
}

// ---- projects ----

func (cl *Client) CreateProject(ctx context.Context, req c.CreateProjectRequest) (c.Project, error) {
	var out c.Project
	return out, cl.mutate(ctx, c.OpCreateProject, &req.Mutation, &req, &out)
}

func (cl *Client) GetProject(ctx context.Context, req c.GetRequest) (c.Project, error) {
	var out c.Project
	return out, cl.read(ctx, c.OpGetProject, req, &out)
}

func (cl *Client) ListProjects(ctx context.Context, req c.ListProjectsRequest) (c.Page[c.Project], error) {
	var out c.Page[c.Project]
	return out, cl.read(ctx, c.OpListProjects, req, &out)
}

func (cl *Client) UpdateProject(ctx context.Context, req c.UpdateProjectRequest) (c.Project, error) {
	var out c.Project
	return out, cl.mutate(ctx, c.OpUpdateProject, &req.Mutation, &req, &out)
}

// ---- documents ----

func (cl *Client) CreateDocument(ctx context.Context, req c.CreateDocumentRequest) (c.DocumentWithRevision, error) {
	var out c.DocumentWithRevision
	return out, cl.mutate(ctx, c.OpCreateDocument, &req.Mutation, &req, &out)
}

func (cl *Client) GetDocument(ctx context.Context, req c.GetDocumentRequest) (c.DocumentWithRevision, error) {
	var out c.DocumentWithRevision
	return out, cl.read(ctx, c.OpGetDocument, req, &out)
}

func (cl *Client) ListDocuments(ctx context.Context, req c.ListDocumentsRequest) (c.Page[c.Document], error) {
	var out c.Page[c.Document]
	return out, cl.read(ctx, c.OpListDocuments, req, &out)
}

func (cl *Client) ListRevisions(ctx context.Context, req c.ListRevisionsRequest) (c.Page[c.DocumentRevision], error) {
	var out c.Page[c.DocumentRevision]
	return out, cl.read(ctx, c.OpListRevisions, req, &out)
}

func (cl *Client) ProposeRevision(ctx context.Context, req c.ProposeRevisionRequest) (c.DocumentWithRevision, error) {
	var out c.DocumentWithRevision
	return out, cl.mutate(ctx, c.OpProposeRevision, &req.Mutation, &req, &out)
}

func (cl *Client) ListProposals(ctx context.Context, req c.ListProposalsRequest) (c.Page[c.DocumentProposal], error) {
	var out c.Page[c.DocumentProposal]
	return out, cl.read(ctx, c.OpListProposals, req, &out)
}

func (cl *Client) DiscardProposal(ctx context.Context, req c.DiscardProposalRequest) (c.DocumentProposal, error) {
	var out c.DocumentProposal
	return out, cl.mutate(ctx, c.OpDiscardProposal, &req.Mutation, &req, &out)
}

// ---- artifacts ----

func (cl *Client) GetUpload(ctx context.Context, req c.GetRequest) (c.Upload, error) {
	var out c.Upload
	return out, cl.read(ctx, c.OpGetUpload, req, &out)
}

// MintPreviewURL is a read: minting a fresh one-time link on retry is
// harmless.
func (cl *Client) MintPreviewURL(ctx context.Context, req c.MintPreviewRequest) (c.PreviewURL, error) {
	var out c.PreviewURL
	return out, cl.read(ctx, c.OpMintPreviewURL, req, &out)
}

func (cl *Client) UpdateArtifact(ctx context.Context, req c.UpdateArtifactRequest) (c.Artifact, error) {
	var out c.Artifact
	return out, cl.mutate(ctx, c.OpUpdateArtifact, &req.Mutation, &req, &out)
}

func (cl *Client) BeginUpload(ctx context.Context, req c.BeginUploadRequest) (c.Upload, error) {
	var out c.Upload
	return out, cl.mutate(ctx, c.OpBeginUpload, &req.Mutation, &req, &out)
}

// UploadChunk buffers r (at most Options.MaxChunkBytes) so the PUT can be
// retried; chunks are idempotent for the same range.
func (cl *Client) UploadChunk(ctx context.Context, uploadID string, offset int64, r io.Reader) (c.Upload, error) {
	if !c.ValidID(uploadID, c.PrefixUpload) {
		return c.Upload{}, c.Errorf(c.ErrInvalid, "invalid upload id")
	}
	data, err := io.ReadAll(io.LimitReader(r, cl.opts.MaxChunkBytes+1))
	if err != nil {
		return c.Upload{}, c.Errorf(c.ErrInvalid, "client: read chunk: %v", err)
	}
	if int64(len(data)) > cl.opts.MaxChunkBytes {
		return c.Upload{}, c.Errorf(c.ErrPayloadTooLarge, "client: chunk exceeds %d bytes", cl.opts.MaxChunkBytes)
	}
	resp, err := cl.do(ctx, request{
		method:      http.MethodPut,
		path:        "/v1/uploads/" + url.PathEscape(uploadID),
		query:       url.Values{"offset": {strconv.FormatInt(offset, 10)}},
		body:        data,
		contentType: "application/octet-stream",
		idempotent:  true,
		readAll:     true,
	})
	if err != nil {
		return c.Upload{}, err
	}
	var out c.Upload
	return out, decodeBytes(resp.data, &out)
}

func (cl *Client) FinalizeUpload(ctx context.Context, req c.FinalizeUploadRequest) (c.PublishReceipt, error) {
	var out c.PublishReceipt
	return out, cl.mutate(ctx, c.OpFinalizeUpload, &req.Mutation, &req, &out)
}

func (cl *Client) GetArtifact(ctx context.Context, req c.GetRequest) (c.Artifact, error) {
	var out c.Artifact
	return out, cl.read(ctx, c.OpGetArtifact, req, &out)
}

func (cl *Client) ListArtifacts(ctx context.Context, req c.ListArtifactsRequest) (c.Page[c.Artifact], error) {
	var out c.Page[c.Artifact]
	return out, cl.read(ctx, c.OpListArtifacts, req, &out)
}

func (cl *Client) ListVersions(ctx context.Context, req c.ListVersionsRequest) (c.Page[c.ArtifactVersion], error) {
	var out c.Page[c.ArtifactVersion]
	return out, cl.read(ctx, c.OpListVersions, req, &out)
}

func (cl *Client) GetVersion(ctx context.Context, req c.GetRequest) (c.ArtifactVersion, error) {
	var out c.ArtifactVersion
	return out, cl.read(ctx, c.OpGetVersion, req, &out)
}

// OpenContent fetches the version manifest, then streams its bytes. The
// returned reader verifies size and sha256 digest: on mismatch, the final
// Read returns an error instead of io.EOF, so callers never mistake
// corrupted bytes for the published version.
func (cl *Client) OpenContent(ctx context.Context, versionID string) (io.ReadCloser, c.ArtifactVersion, error) {
	ver, err := cl.GetVersion(ctx, c.GetRequest{ID: versionID})
	if err != nil {
		return nil, c.ArtifactVersion{}, err
	}
	resp, err := cl.content(ctx, versionID, "")
	if err != nil {
		return nil, c.ArtifactVersion{}, err
	}
	return newVerifyingReader(resp, ver), ver, nil
}

// OpenContentRange streams length bytes of a version starting at offset
// (length <= 0 means to the end) using an HTTP Range request. Partial bytes
// cannot be digest-verified; callers resuming a download should verify the
// assembled whole against the version digest.
func (cl *Client) OpenContentRange(ctx context.Context, versionID string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 {
		return nil, c.Errorf(c.ErrInvalid, "offset must be >= 0")
	}
	rng := fmt.Sprintf("bytes=%d-", offset)
	if length > 0 {
		rng = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
	resp, err := cl.content(ctx, versionID, rng)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		resp.cancel()
		return nil, c.Errorf(c.ErrUnsupported, "hub ignored the Range request (HTTP %d)", resp.StatusCode)
	}
	return &bodyCloser{ReadCloser: resp.Body, cancel: resp.cancel}, nil
}

func (cl *Client) content(ctx context.Context, versionID, rng string) (*response, error) {
	if !c.ValidID(versionID, c.PrefixVersion) {
		return nil, c.Errorf(c.ErrInvalid, "invalid version id")
	}
	if cl.cfgErr != nil {
		return nil, cl.cfgErr
	}
	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := cl.onceWithHeader(ctx, versionID, rng)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !Retryable(err) || attempt+1 >= cl.opts.MaxAttempts {
			return nil, err
		}
		wait := max(cl.backoff(attempt), retryAfter)
		if err := cl.sleep(ctx, min(wait, cl.opts.RetryMax)); err != nil {
			return nil, err
		}
	}
}

func (cl *Client) onceWithHeader(ctx context.Context, versionID, rng string) (*response, time.Duration, error) {
	// Content downloads are streamed: bound the time to response headers,
	// not the whole transfer.
	actx, cancel := context.WithCancel(ctx)
	hr, err := cl.newHTTPRequest(actx, request{method: http.MethodGet, path: "/v1/content/" + url.PathEscape(versionID)})
	if err != nil {
		cancel()
		return nil, 0, err
	}
	hr.Header.Set("Accept", "*/*")
	if rng != "" {
		hr.Header.Set("Range", rng)
	}
	timer := time.AfterFunc(cl.opts.Timeout, cancel)
	resp, err := cl.hc.Do(hr)
	timer.Stop()
	if err != nil {
		cancel()
		return nil, 0, transportError(ctx, err)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
		return &response{Response: resp, cancel: cancel}, 0, nil
	}
	defer cancel()
	defer resp.Body.Close()
	return nil, cl.retryAfter(resp.Header.Get("Retry-After")), errorFromResponse(resp)
}

type bodyCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *bodyCloser) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

type verifyingReader struct {
	body   *response
	h      hash.Hash
	n      int64
	ver    c.ArtifactVersion
	err    error
	closed bool
}

func newVerifyingReader(resp *response, ver c.ArtifactVersion) *verifyingReader {
	return &verifyingReader{body: resp, h: sha256.New(), ver: ver}
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	n, err := v.body.Body.Read(p)
	v.h.Write(p[:n])
	v.n += int64(n)
	if v.n > v.ver.Size {
		v.err = c.Errorf(c.ErrConflict, "content of %s exceeds recorded size %d", v.ver.VersionID, v.ver.Size)
		return n, v.err
	}
	if err == io.EOF {
		got := "sha256:" + hex.EncodeToString(v.h.Sum(nil))
		switch {
		case v.n != v.ver.Size:
			v.err = c.Errorf(c.ErrConflict, "content of %s is %d bytes; recorded size %d", v.ver.VersionID, v.n, v.ver.Size)
		case !strings.EqualFold(got, string(v.ver.Digest)):
			v.err = c.Errorf(c.ErrConflict, "content of %s does not match its recorded digest", v.ver.VersionID)
		default:
			v.err = io.EOF
		}
		return n, v.err
	}
	if err != nil {
		v.err = transportError(context.Background(), err)
		return n, v.err
	}
	return n, nil
}

func (v *verifyingReader) Close() error {
	if v.closed {
		return nil
	}
	v.closed = true
	err := v.body.Body.Close()
	v.body.cancel()
	return err
}

// ---- reviews ----

func (cl *Client) CreateComment(ctx context.Context, req c.CreateCommentRequest) (c.ReviewComment, error) {
	var out c.ReviewComment
	return out, cl.mutate(ctx, c.OpCreateComment, &req.Mutation, &req, &out)
}

func (cl *Client) GetComment(ctx context.Context, req c.GetRequest) (c.ReviewComment, error) {
	var out c.ReviewComment
	return out, cl.read(ctx, c.OpGetComment, req, &out)
}

func (cl *Client) ListComments(ctx context.Context, req c.ListCommentsRequest) (c.Page[c.ReviewComment], error) {
	var out c.Page[c.ReviewComment]
	return out, cl.read(ctx, c.OpListComments, req, &out)
}

func (cl *Client) ResolveComment(ctx context.Context, req c.ResolveCommentRequest) (c.ReviewComment, error) {
	var out c.ReviewComment
	return out, cl.mutate(ctx, c.OpResolveComment, &req.Mutation, &req, &out)
}

// ---- notes ----

func (cl *Client) CreateNote(ctx context.Context, req c.CreateNoteRequest) (c.Note, error) {
	var out c.Note
	return out, cl.mutate(ctx, c.OpCreateNote, &req.Mutation, &req, &out)
}

func (cl *Client) GetNote(ctx context.Context, req c.GetRequest) (c.Note, error) {
	var out c.Note
	return out, cl.read(ctx, c.OpGetNote, req, &out)
}

func (cl *Client) ListNotes(ctx context.Context, req c.ListNotesRequest) (c.Page[c.Note], error) {
	var out c.Page[c.Note]
	return out, cl.read(ctx, c.OpListNotes, req, &out)
}

func (cl *Client) UpdateNoteSync(ctx context.Context, req c.UpdateNoteSyncRequest) (c.Note, error) {
	var out c.Note
	return out, cl.mutate(ctx, c.OpUpdateNoteSync, &req.Mutation, &req, &out)
}

func (cl *Client) PromoteNote(ctx context.Context, req c.PromoteNoteRequest) (c.DocumentWithRevision, error) {
	var out c.DocumentWithRevision
	return out, cl.mutate(ctx, c.OpPromoteNote, &req.Mutation, &req, &out)
}

func (cl *Client) GetLaunchContext(ctx context.Context, req c.ContextRequest) (c.LaunchContext, error) {
	var out c.LaunchContext
	return out, cl.read(ctx, c.OpGetLaunchContext, req, &out)
}

// ---- accounts ----

func (cl *Client) ListAccounts(ctx context.Context, req c.ListAccountsRequest) (c.Page[c.AccountView], error) {
	var out c.Page[c.AccountView]
	return out, cl.read(ctx, c.OpListAccounts, req, &out)
}

func (cl *Client) UpsertAccount(ctx context.Context, req c.UpsertAccountRequest) (c.Account, error) {
	var out c.Account
	return out, cl.mutate(ctx, c.OpUpsertAccount, &req.Mutation, &req, &out)
}

func (cl *Client) ListFallbacks(ctx context.Context, req c.ListFallbacksRequest) (c.Page[c.FallbackPlan], error) {
	var out c.Page[c.FallbackPlan]
	return out, cl.read(ctx, c.OpListFallbacks, req, &out)
}

func (cl *Client) AcquireAccountLease(ctx context.Context, req c.AcquireAccountLeaseRequest) (c.AccountLease, error) {
	var out c.AccountLease
	return out, cl.mutate(ctx, c.OpAcquireAccountLease, &req.Mutation, &req, &out)
}

func (cl *Client) RenewAccountLease(ctx context.Context, req c.RenewAccountLeaseRequest) (c.AccountLease, error) {
	var out c.AccountLease
	return out, cl.mutate(ctx, c.OpRenewAccountLease, &req.Mutation, &req, &out)
}

func (cl *Client) ReleaseAccountLease(ctx context.Context, req c.ReleaseAccountLeaseRequest) (c.AccountLease, error) {
	var out c.AccountLease
	return out, cl.mutate(ctx, c.OpReleaseAccountLease, &req.Mutation, &req, &out)
}

// ---- attention ----

func (cl *Client) ListAttention(ctx context.Context, req c.ListAttentionRequest) (c.Page[c.AttentionItem], error) {
	var out c.Page[c.AttentionItem]
	return out, cl.read(ctx, c.OpListAttention, req, &out)
}

func (cl *Client) GetAttention(ctx context.Context, req c.GetRequest) (c.AttentionItem, error) {
	var out c.AttentionItem
	return out, cl.read(ctx, c.OpGetAttention, req, &out)
}

func (cl *Client) UpdateAttention(ctx context.Context, req c.UpdateAttentionRequest) (c.AttentionItem, error) {
	var out c.AttentionItem
	return out, cl.mutate(ctx, c.OpUpdateAttention, &req.Mutation, &req, &out)
}

func (cl *Client) FlagAttention(ctx context.Context, req c.FlagAttentionRequest) (c.AttentionItem, error) {
	var out c.AttentionItem
	return out, cl.mutate(ctx, c.OpFlagAttention, &req.Mutation, &req, &out)
}

func (cl *Client) GetNotificationPrefs(ctx context.Context) (c.NotificationPrefs, error) {
	var out c.NotificationPrefs
	return out, cl.read(ctx, c.OpGetNotificationPrefs, nil, &out)
}

func (cl *Client) SetNotificationPrefs(ctx context.Context, req c.SetNotificationPrefsRequest) (c.NotificationPrefs, error) {
	var out c.NotificationPrefs
	return out, cl.mutate(ctx, c.OpSetNotificationPrefs, &req.Mutation, &req, &out)
}

func (cl *Client) RegisterPush(ctx context.Context, req c.PushRegistration) (c.PushRegistrationResult, error) {
	var out c.PushRegistrationResult
	return out, cl.mutate(ctx, c.OpRegisterPush, &req.Mutation, &req, &out)
}

// ---- events ----

func (cl *Client) ListEvents(ctx context.Context, req c.ListEventsRequest) (c.ListEventsResponse, error) {
	var out c.ListEventsResponse
	return out, cl.call(ctx, c.OpListEvents, req, &out, true, longPoll(req.WaitSeconds))
}

// ---- node actions ----

func (cl *Client) InvokeNodeAction(ctx context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	var out c.NodeActionResult
	if err := cl.mutateWait(ctx, c.OpInvokeNodeAction, &req.Mutation, &req, &out, time.Duration(max(req.TimeoutSeconds, 0))*time.Second); err != nil {
		return out, err
	}
	return out, nil
}

func (cl *Client) ListConnections(ctx context.Context, req c.ListConnectionsRequest) (c.Page[c.Connection], error) {
	var out c.Page[c.Connection]
	return out, cl.read(ctx, c.OpListConnections, req, &out)
}

func (cl *Client) GetCommandResult(ctx context.Context, req c.GetRequest) (c.NodeActionResult, error) {
	var out c.NodeActionResult
	return out, cl.read(ctx, c.OpGetCommand, req, &out)
}

func (cl *Client) CheckConnectionGrant(ctx context.Context, req c.CheckGrantRequest) (c.GrantDecision, error) {
	var out c.GrantDecision
	return out, cl.read(ctx, c.OpCheckConnectionGrant, req, &out)
}

func (cl *Client) mutateWait(ctx context.Context, op string, m *c.Mutation, in, out any, wait time.Duration) error {
	if m.OperationID == "" {
		m.OperationID = c.NewID(c.PrefixOperation)
	}
	return cl.call(ctx, op, in, out, true, wait)
}
