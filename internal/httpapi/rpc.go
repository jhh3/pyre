package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jhh3/pyre/internal/contracts"
)

type rpcFunc func(ctx context.Context, body []byte) (any, error)

func unary[Req, Resp any](f func(context.Context, Req) (Resp, error)) rpcFunc {
	return func(ctx context.Context, body []byte) (any, error) {
		var req Req
		if err := DecodeStrict(body, &req); err != nil {
			return nil, err
		}
		return f(ctx, req)
	}
}

func nullary[Resp any](f func(context.Context) (Resp, error)) rpcFunc {
	return func(ctx context.Context, body []byte) (any, error) {
		var req struct{}
		if err := DecodeStrict(body, &req); err != nil {
			return nil, err
		}
		return f(ctx)
	}
}

// rpcTable maps every RPC op in contracts/routes.go to its API method.
func rpcTable(a contracts.API) map[string]rpcFunc {
	m := map[string]rpcFunc{
		contracts.OpHello:  unary(a.Hello),
		contracts.OpWhoAmI: nullary(a.WhoAmI),

		contracts.OpListNodes:        unary(a.ListNodes),
		contracts.OpGetNode:          unary(a.GetNode),
		contracts.OpCreateEnrollment: unary(a.CreateEnrollment),
		contracts.OpRevokeNode:       unary(a.RevokeNode),
		contracts.OpCreatePairing:    unary(a.CreatePairing),
		contracts.OpListClients:      unary(a.ListClients),
		contracts.OpRevokeClient:     unary(a.RevokeClient),

		contracts.OpListSessions:     unary(a.ListSessions),
		contracts.OpGetSession:       unary(a.GetSession),
		contracts.OpStartSession:     unary(a.StartSession),
		contracts.OpReadSession:      unary(a.ReadSession),
		contracts.OpAttachSession:    unary(a.AttachSession),
		contracts.OpInterruptSession: unary(a.InterruptSession),
		contracts.OpStopSession:      unary(a.StopSession),
		contracts.OpResumeSession:    unary(a.ResumeSession),
		contracts.OpAssociateSession: unary(a.AssociateSession),
		contracts.OpUpdateSession:    unary(a.UpdateSession),
		contracts.OpListRuns:         unary(a.ListRuns),

		contracts.OpAcquireLease:   unary(a.AcquireLease),
		contracts.OpReleaseLease:   unary(a.ReleaseLease),
		contracts.OpListApprovals:  unary(a.ListApprovals),
		contracts.OpGetApproval:    unary(a.GetApproval),
		contracts.OpAnswerApproval: unary(a.AnswerApproval),

		contracts.OpSendMessage:     unary(a.SendMessage),
		contracts.OpGetMessage:      unary(a.GetMessage),
		contracts.OpListMessages:    unary(a.ListMessages),
		contracts.OpAckMessage:      unary(a.AckMessage),
		contracts.OpReissueMessage:  unary(a.ReissueMessage),
		contracts.OpResolveDelivery: unary(a.ResolveDelivery),
		contracts.OpCancelMessage:   unary(a.CancelMessage),

		contracts.OpCreateProject: unary(a.CreateProject),
		contracts.OpGetProject:    unary(a.GetProject),
		contracts.OpListProjects:  unary(a.ListProjects),
		contracts.OpUpdateProject: unary(a.UpdateProject),

		contracts.OpCreateDocument:  unary(a.CreateDocument),
		contracts.OpGetDocument:     unary(a.GetDocument),
		contracts.OpListDocuments:   unary(a.ListDocuments),
		contracts.OpListRevisions:   unary(a.ListRevisions),
		contracts.OpProposeRevision: unary(a.ProposeRevision),
		contracts.OpListProposals:   unary(a.ListProposals),
		contracts.OpDiscardProposal: unary(a.DiscardProposal),

		contracts.OpBeginUpload:    unary(a.BeginUpload),
		contracts.OpFinalizeUpload: unary(a.FinalizeUpload),
		contracts.OpGetArtifact:    unary(a.GetArtifact),
		contracts.OpListArtifacts:  unary(a.ListArtifacts),
		contracts.OpListVersions:   unary(a.ListVersions),
		contracts.OpGetVersion:     unary(a.GetVersion),
		contracts.OpGetUpload:      unary(a.GetUpload),
		contracts.OpMintPreviewURL: unary(a.MintPreviewURL),
		contracts.OpUpdateArtifact: unary(a.UpdateArtifact),

		contracts.OpCreateComment:  unary(a.CreateComment),
		contracts.OpListComments:   unary(a.ListComments),
		contracts.OpGetComment:     unary(a.GetComment),
		contracts.OpResolveComment: unary(a.ResolveComment),

		contracts.OpCreateNote:       unary(a.CreateNote),
		contracts.OpGetNote:          unary(a.GetNote),
		contracts.OpListNotes:        unary(a.ListNotes),
		contracts.OpUpdateNoteSync:   unary(a.UpdateNoteSync),
		contracts.OpGetLaunchContext: unary(a.GetLaunchContext),
		contracts.OpPromoteNote:      unary(a.PromoteNote),

		contracts.OpListAccounts:        unary(a.ListAccounts),
		contracts.OpUpsertAccount:       unary(a.UpsertAccount),
		contracts.OpListFallbacks:       unary(a.ListFallbacks),
		contracts.OpAcquireAccountLease: unary(a.AcquireAccountLease),
		contracts.OpRenewAccountLease:   unary(a.RenewAccountLease),
		contracts.OpReleaseAccountLease: unary(a.ReleaseAccountLease),

		contracts.OpListAttention:        unary(a.ListAttention),
		contracts.OpGetAttention:         unary(a.GetAttention),
		contracts.OpUpdateAttention:      unary(a.UpdateAttention),
		contracts.OpFlagAttention:        unary(a.FlagAttention),
		contracts.OpGetNotificationPrefs: nullary(a.GetNotificationPrefs),
		contracts.OpSetNotificationPrefs: unary(a.SetNotificationPrefs),
		contracts.OpRegisterPush:         unary(a.RegisterPush),

		contracts.OpListEvents: unary(a.ListEvents),

		contracts.OpInvokeNodeAction:     unary(a.InvokeNodeAction),
		contracts.OpListConnections:      unary(a.ListConnections),
		contracts.OpCheckConnectionGrant: unary(a.CheckConnectionGrant),
		contracts.OpGetCommand:           unary(a.GetCommandResult),
	}
	addOptionalOps(m, a)
	return m
}

// DecodeStrict decodes one JSON value into v, rejecting unknown fields and
// trailing data. An empty body decodes as {}. Errors are invalid_argument.
func DecodeStrict(body []byte, v any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return contracts.Errorf(contracts.ErrInvalid, "invalid request body: %s", sanitizeJSONError(err))
	}
	if dec.More() {
		return contracts.Errorf(contracts.ErrInvalid, "invalid request body: trailing data after JSON value")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return contracts.Errorf(contracts.ErrInvalid, "invalid request body: trailing data after JSON value")
	}
	return nil
}

func sanitizeJSONError(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return "malformed JSON"
	case errors.As(err, &te):
		if te.Field != "" {
			return "wrong type for field " + te.Field
		}
		return "wrong JSON type"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "truncated JSON"
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

func (s *server) serveRPC(w http.ResponseWriter, r *http.Request, op string) {
	fn := s.ops[op]
	body, err := readLimited(r, s.opts.MaxBodyBytes)
	if err != nil {
		WriteError(w, err)
		return
	}
	resp, err := fn(r.Context(), body)
	if err != nil {
		noteInternal(r, err)
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func readLimited(r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, contracts.Errorf(contracts.ErrPayloadTooLarge, "request body exceeds %d bytes", limit)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, contracts.Errorf(contracts.ErrInvalid, "reading request body failed")
	}
	if int64(len(b)) > limit {
		return nil, contracts.Errorf(contracts.ErrPayloadTooLarge, "request body exceeds %d bytes", limit)
	}
	return b, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(contracts.ErrorBody{Error: &contracts.Error{Code: contracts.ErrInternal, Message: "internal error"}})
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
