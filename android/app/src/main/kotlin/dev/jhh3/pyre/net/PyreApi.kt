package dev.jhh3.pyre.net

import dev.jhh3.pyre.model.*

/**
 * The subset of the hub API the Android companion uses. One interface so
 * ViewModels/tests can use a fake; [HttpPyreApi] is the real binding.
 */
interface PyreApi {
    suspend fun hello(): HelloResponse
    suspend fun listNodes(req: ListNodesRequest = ListNodesRequest()): Page<Node>
    suspend fun getNode(id: String): Node
    suspend fun listSessions(req: ListSessionsRequest = ListSessionsRequest()): Page<SessionView>
    suspend fun getSession(id: String): SessionView
    suspend fun startSession(req: StartSessionRequest): StartSessionResponse
    suspend fun readSession(req: ReadSessionRequest): ReadSessionResponse
    suspend fun attach(req: AttachRequest): AttachDescriptor
    suspend fun interrupt(req: SessionCommandRequest): CommandReceipt
    suspend fun stop(req: SessionCommandRequest): CommandReceipt
    suspend fun updateSession(req: UpdateSessionRequest): SessionView
    suspend fun acquireLease(req: LeaseRequest): InputLease
    suspend fun listApprovals(req: ListApprovalsRequest): Page<Approval>
    suspend fun getApproval(id: String): Approval
    suspend fun answerApproval(req: AnswerApprovalRequest): Approval
    suspend fun sendMessage(req: SendMessageRequest): MessageView
    suspend fun getMessage(id: String): MessageView
    suspend fun listMessages(req: ListMessagesRequest): Page<MessageView>
    suspend fun reissueMessage(req: ReissueMessageRequest): MessageView
    suspend fun resolveDelivery(req: ResolveDeliveryRequest): MessageView
    suspend fun cancelMessage(req: CancelMessageRequest): MessageView
    /** messages.ack: acknowledge a message without replying (clears a session's "message" flag). */
    suspend fun ackMessage(req: AckMessageRequest): MessageView =
        throw PyreApiException(ErrorCodes.UNSUPPORTED, "messages.ack is not available")
    suspend fun listProjects(req: ListProjectsRequest = ListProjectsRequest()): Page<Project>
    suspend fun getProject(id: String): Project
    suspend fun getDocument(req: GetDocumentRequest): DocumentWithRevision
    suspend fun listDocuments(req: ListDocumentsRequest): Page<Document>
    suspend fun listRevisions(req: ListRevisionsRequest): Page<DocumentRevision>
    suspend fun listArtifacts(req: ListArtifactsRequest): Page<Artifact>
    suspend fun getArtifact(id: String): Artifact
    suspend fun listVersions(req: ListVersionsRequest): Page<ArtifactVersion>
    suspend fun getVersion(id: String): ArtifactVersion
    suspend fun previewUrl(req: MintPreviewRequest): PreviewUrl
    suspend fun createComment(req: CreateCommentRequest): ReviewComment
    suspend fun listComments(req: ListCommentsRequest): Page<ReviewComment>
    suspend fun getComment(id: String): ReviewComment
    suspend fun listAccounts(req: ListAccountsRequest = ListAccountsRequest()): Page<AccountView>
    suspend fun listAttention(req: ListAttentionRequest = ListAttentionRequest()): Page<AttentionItem>
    suspend fun getAttention(id: String): AttentionItem
    suspend fun updateAttention(req: UpdateAttentionRequest): AttentionItem
    suspend fun getNotificationPrefs(): NotificationPrefs
    suspend fun setNotificationPrefs(req: SetNotificationPrefsRequest): NotificationPrefs
    suspend fun registerPush(req: PushRegistration): PushRegistrationResult
    suspend fun listEvents(req: ListEventsRequest): ListEventsResponse
    suspend fun nodeAction(req: NodeActionRequest): NodeActionResult
    /** Fleet repo index (`repos.list`, additive); hubs that predate it answer unsupported. */
    suspend fun listRepos(req: ListReposRequest): RepoList =
        throw PyreApiException(ErrorCodes.UNSUPPORTED, "repos.list is not available")
    /** Audit log (`audit.list`, additive, read-only); hubs that predate it answer HTTP 404. */
    suspend fun listAudit(req: ListAuditRequest): Page<AuditEntry> =
        throw PyreApiException(ErrorCodes.UNSUPPORTED, "audit.list is not available")
    /** Asks (`asks.*`, additive; Hello feature "asks"); hubs without them answer HTTP 404. */
    suspend fun listAsks(req: ListAsksRequest): Page<Ask> =
        throw PyreApiException(ErrorCodes.UNSUPPORTED, "asks.list is not available")
    suspend fun getAsk(id: String): Ask =
        throw PyreApiException(ErrorCodes.UNSUPPORTED, "asks.get is not available")
    /** Batch: one call answers several asks; each item commits independently. */
    suspend fun answerAsks(req: AnswerAsksRequest): AnswerAsksResponse =
        throw PyreApiException(ErrorCodes.UNSUPPORTED, "asks.answer is not available")
    suspend fun withdrawAsk(req: WithdrawAskRequest): Ask =
        throw PyreApiException(ErrorCodes.UNSUPPORTED, "asks.withdraw is not available")
}

class HttpPyreApi(val client: HubClient) : PyreApi {
    private suspend inline fun <reified Req, reified Res> call(op: String, req: Req, mutation: Boolean = false): Res =
        client.rpc(op, req, kotlinx.serialization.serializer<Req>(), kotlinx.serialization.serializer<Res>(), mutation)

    override suspend fun hello(): HelloResponse =
        call(Ops.HELLO, HelloRequest(Protocol.VERSION, "pyre-android/0.1.0"))

    override suspend fun listNodes(req: ListNodesRequest): Page<Node> = call(Ops.LIST_NODES, req)
    override suspend fun getNode(id: String): Node = call(Ops.GET_NODE, GetRequest(id))
    override suspend fun listSessions(req: ListSessionsRequest): Page<SessionView> = call(Ops.LIST_SESSIONS, req)
    override suspend fun getSession(id: String): SessionView = call(Ops.GET_SESSION, GetRequest(id))
    override suspend fun startSession(req: StartSessionRequest): StartSessionResponse = call(Ops.START_SESSION, req, true)
    override suspend fun readSession(req: ReadSessionRequest): ReadSessionResponse = call(Ops.READ_SESSION, req)
    override suspend fun attach(req: AttachRequest): AttachDescriptor = call(Ops.ATTACH_SESSION, req, true)
    override suspend fun interrupt(req: SessionCommandRequest): CommandReceipt = call(Ops.INTERRUPT_SESSION, req, true)
    override suspend fun stop(req: SessionCommandRequest): CommandReceipt = call(Ops.STOP_SESSION, req, true)
    override suspend fun updateSession(req: UpdateSessionRequest): SessionView = call(Ops.UPDATE_SESSION, req, true)
    override suspend fun acquireLease(req: LeaseRequest): InputLease = call(Ops.ACQUIRE_LEASE, req, true)
    override suspend fun listApprovals(req: ListApprovalsRequest): Page<Approval> = call(Ops.LIST_APPROVALS, req)
    override suspend fun getApproval(id: String): Approval = call(Ops.GET_APPROVAL, GetRequest(id))
    override suspend fun answerApproval(req: AnswerApprovalRequest): Approval = call(Ops.ANSWER_APPROVAL, req, true)
    override suspend fun sendMessage(req: SendMessageRequest): MessageView = call(Ops.SEND_MESSAGE, req, true)
    override suspend fun getMessage(id: String): MessageView = call(Ops.GET_MESSAGE, GetRequest(id))
    override suspend fun listMessages(req: ListMessagesRequest): Page<MessageView> = call(Ops.LIST_MESSAGES, req)
    override suspend fun reissueMessage(req: ReissueMessageRequest): MessageView = call(Ops.REISSUE_MESSAGE, req, true)
    override suspend fun resolveDelivery(req: ResolveDeliveryRequest): MessageView = call(Ops.RESOLVE_DELIVERY, req, true)
    override suspend fun cancelMessage(req: CancelMessageRequest): MessageView = call(Ops.CANCEL_MESSAGE, req, true)
    override suspend fun ackMessage(req: AckMessageRequest): MessageView = call(Ops.ACK_MESSAGE, req, true)
    override suspend fun listProjects(req: ListProjectsRequest): Page<Project> = call(Ops.LIST_PROJECTS, req)
    override suspend fun getProject(id: String): Project = call(Ops.GET_PROJECT, GetRequest(id))
    override suspend fun getDocument(req: GetDocumentRequest): DocumentWithRevision = call(Ops.GET_DOCUMENT, req)
    override suspend fun listDocuments(req: ListDocumentsRequest): Page<Document> = call(Ops.LIST_DOCUMENTS, req)
    override suspend fun listRevisions(req: ListRevisionsRequest): Page<DocumentRevision> = call(Ops.LIST_REVISIONS, req)
    override suspend fun listArtifacts(req: ListArtifactsRequest): Page<Artifact> = call(Ops.LIST_ARTIFACTS, req)
    override suspend fun getArtifact(id: String): Artifact = call(Ops.GET_ARTIFACT, GetRequest(id))
    override suspend fun listVersions(req: ListVersionsRequest): Page<ArtifactVersion> = call(Ops.LIST_VERSIONS, req)
    override suspend fun previewUrl(req: MintPreviewRequest): PreviewUrl = call(Ops.PREVIEW_URL, req)
    override suspend fun getVersion(id: String): ArtifactVersion = call(Ops.GET_VERSION, GetRequest(id))
    override suspend fun createComment(req: CreateCommentRequest): ReviewComment = call(Ops.CREATE_COMMENT, req, true)
    override suspend fun getComment(id: String): ReviewComment = call(Ops.GET_COMMENT, GetRequest(id))
    override suspend fun listComments(req: ListCommentsRequest): Page<ReviewComment> = call(Ops.LIST_COMMENTS, req)
    override suspend fun listAccounts(req: ListAccountsRequest): Page<AccountView> = call(Ops.LIST_ACCOUNTS, req)
    override suspend fun listAttention(req: ListAttentionRequest): Page<AttentionItem> = call(Ops.LIST_ATTENTION, req)
    override suspend fun getAttention(id: String): AttentionItem = call(Ops.GET_ATTENTION, GetRequest(id))
    override suspend fun updateAttention(req: UpdateAttentionRequest): AttentionItem = call(Ops.UPDATE_ATTENTION, req, true)
    override suspend fun getNotificationPrefs(): NotificationPrefs = call(Ops.GET_NOTIFICATION_PREFS, EmptyRequest())
    override suspend fun setNotificationPrefs(req: SetNotificationPrefsRequest): NotificationPrefs = call(Ops.SET_NOTIFICATION_PREFS, req, true)
    override suspend fun registerPush(req: PushRegistration): PushRegistrationResult = call(Ops.REGISTER_PUSH, req, true)
    override suspend fun listEvents(req: ListEventsRequest): ListEventsResponse = call(Ops.LIST_EVENTS, req)
    override suspend fun nodeAction(req: NodeActionRequest): NodeActionResult = call(Ops.INVOKE_NODE_ACTION, req, true)
    override suspend fun listRepos(req: ListReposRequest): RepoList = call(RepoOps.LIST_REPOS, req)
    override suspend fun listAudit(req: ListAuditRequest): Page<AuditEntry> = call(AuditOps.LIST_AUDIT, req)
    override suspend fun listAsks(req: ListAsksRequest): Page<Ask> = call(AsksOps.LIST, req)
    override suspend fun getAsk(id: String): Ask = call(AsksOps.GET, GetRequest(id))
    override suspend fun answerAsks(req: AnswerAsksRequest): AnswerAsksResponse = call(AsksOps.ANSWER, req, true)
    override suspend fun withdrawAsk(req: WithdrawAskRequest): Ask = call(AsksOps.WITHDRAW, req, true)
}
