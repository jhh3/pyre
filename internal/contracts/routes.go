package contracts

// HTTP binding.
//
// RPC operations: `POST /v1/<op>` with a JSON request body, JSON response
// body, `Authorization: Bearer <token>`, and `X-Pyre-Protocol: <n>`.
// Errors: non-2xx status from ErrorCode.HTTPStatus() with body ErrorBody.
// Operations with no request fields accept `{}`.
//
// Non-RPC endpoints are listed separately below.
const (
	OpHello  = "system.hello"
	OpWhoAmI = "system.whoami"

	OpListNodes        = "nodes.list"
	OpGetNode          = "nodes.get"
	OpCreateEnrollment = "nodes.enroll"
	OpRevokeNode       = "nodes.revoke"
	OpCreatePairing    = "clients.pair"
	OpListClients      = "clients.list"
	OpRevokeClient     = "clients.revoke"

	OpListSessions     = "sessions.list"
	OpGetSession       = "sessions.get"
	OpStartSession     = "sessions.start"
	OpReadSession      = "sessions.read"
	OpAttachSession    = "sessions.attach"
	OpInterruptSession = "sessions.interrupt"
	OpStopSession      = "sessions.stop"
	OpResumeSession    = "sessions.resume"
	OpAssociateSession = "sessions.associate"
	OpUpdateSession    = "sessions.update"
	OpListRuns         = "runs.list"

	OpAcquireLease   = "leases.acquire"
	OpReleaseLease   = "leases.release"
	OpListApprovals  = "approvals.list"
	OpGetApproval    = "approvals.get"
	OpAnswerApproval = "approvals.answer"

	OpSendMessage     = "messages.send"
	OpGetMessage      = "messages.get"
	OpListMessages    = "messages.list"
	OpAckMessage      = "messages.ack"
	OpReissueMessage  = "messages.reissue"
	OpResolveDelivery = "messages.resolve"
	OpCancelMessage   = "messages.cancel"

	OpCreateProject = "projects.create"
	OpGetProject    = "projects.get"
	OpListProjects  = "projects.list"
	OpUpdateProject = "projects.update"

	OpCreateDocument  = "documents.create"
	OpGetDocument     = "documents.get"
	OpListDocuments   = "documents.list"
	OpListRevisions   = "documents.revisions"
	OpProposeRevision = "documents.propose"
	OpListProposals   = "documents.proposals"
	OpDiscardProposal = "documents.discard_proposal"

	OpBeginUpload    = "artifacts.begin_upload"
	OpFinalizeUpload = "artifacts.finalize"
	OpGetArtifact    = "artifacts.get"
	OpListArtifacts  = "artifacts.list"
	OpListVersions   = "artifacts.versions"
	OpGetVersion     = "artifacts.get_version"
	OpGetUpload      = "artifacts.get_upload"
	OpUpdateArtifact = "artifacts.update"
	OpMintPreviewURL = "artifacts.preview_url"

	OpCreateComment  = "comments.create"
	OpListComments   = "comments.list"
	OpGetComment     = "comments.get"
	OpResolveComment = "comments.resolve"

	OpCreateNote       = "notes.create"
	OpGetNote          = "notes.get"
	OpListNotes        = "notes.list"
	OpUpdateNoteSync   = "notes.sync"
	OpGetLaunchContext = "context.get"
	OpPromoteNote      = "notes.promote"

	OpListAccounts  = "accounts.list"
	OpUpsertAccount = "accounts.upsert"
	OpListFallbacks = "accounts.fallbacks"
	// Account leases: node principals only.
	OpAcquireAccountLease = "accounts.lease.acquire"
	OpRenewAccountLease   = "accounts.lease.renew"
	OpReleaseAccountLease = "accounts.lease.release"

	OpListAttention        = "attention.list"
	OpUpdateAttention      = "attention.update"
	OpGetAttention         = "attention.get"
	OpFlagAttention        = "attention.flag" // sessions (themselves) and the owner
	OpGetNotificationPrefs = "notifications.get_prefs"
	OpSetNotificationPrefs = "notifications.set_prefs"
	OpRegisterPush         = "notifications.register"

	OpListEvents = "events.list"

	OpInvokeNodeAction = "nodes.action"
	OpListConnections  = "connections.list"
	// OpCheckConnectionGrant is callable by node principals (for sessions
	// on that node), sessions (for themselves) and the owner.
	OpCheckConnectionGrant = "connections.check_grant"
	OpGetCommand           = "commands.get"
)

// RPCPath returns the HTTP path for an RPC op.
func RPCPath(op string) string { return "/v1/" + op }

// Non-RPC endpoints.
const (
	// PUT body = raw bytes; query offset=<n>. Response: Upload JSON.
	PathUploadChunk = "/v1/uploads/{upload_id}"
	// GET verified immutable bytes; supports Range; Content-Type = media type;
	// Content-Disposition attachment with sanitized filename. Auth required.
	PathContent = "/v1/content/{version_id}"
	// GET isolated preview for active content. Served with a restrictive CSP
	// sandbox, no cookies, and a short-lived signed token instead of the
	// management bearer token, so HTML cannot call the management API.
	PathPreview = "/preview/{version_id}/{token}"
	// WebSocket: server pushes Event JSON frames after ?after_cursor=<n>.
	// A stale cursor yields a single {"error":{code:"reset_required"}} frame,
	// then close code 4410; any other error frame is followed by close 4500.
	PathEventStream = "/v1/events/stream"
	// WebSocket: node control channel (see nodeproto.go). Node bearer token.
	PathNodeConnect = "/v1/node/connect"
	// WebSocket: authenticated web terminal for one run, using a one-time
	// ticket from AttachDescriptor (ticket in the first frame, NOT the URL).
	// Frames: first client text frame {"type":"auth","ticket":"...","cols":N,"rows":N};
	// then binary frames carry raw terminal bytes in both directions; client
	// text frames {"type":"resize","cols":N,"rows":N}; server text frame
	// {"type":"error","error":{...}} before close; close code is 4000 + the
	// error's HTTP status (e.g. 4401, 4409, 4410, 4503), 1000 for a normal
	// close. Unknown client text frames are ignored. Other WebSockets
	// (events, node) authenticate with the bearer token in the upgrade
	// request's Authorization header.
	PathTerminal = "/v1/terminal"
	// WebSocket: node side of a web-terminal stream (node bearer token;
	// ?stream=<stream_id> from TerminalOpenCommand). Binary frames = PTY bytes;
	// text frames {"type":"resize",...} relayed from the client.
	PathNodeTerminal = "/v1/node/terminal"
	// POST unauthenticated one-time exchanges.
	PathEnrollExchange = "/v1/enroll/exchange"
	PathPairExchange   = "/v1/pair/exchange"
	// GET liveness (unauthenticated, no data).
	PathHealth = "/healthz"
)

// DeepLink formats a stable pyre:// link for an object.
func DeepLink(ref ObjectRef) string { return "pyre://" + string(ref.Kind) + "/" + ref.ID }
