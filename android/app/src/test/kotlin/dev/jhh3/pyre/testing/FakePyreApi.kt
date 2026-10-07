package dev.jhh3.pyre.testing

import dev.jhh3.pyre.core.Clock
import dev.jhh3.pyre.data.InMemoryBlobStore
import dev.jhh3.pyre.data.InMemoryCacheStore
import dev.jhh3.pyre.data.IntentIds
import dev.jhh3.pyre.data.Outbox
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.PyreApi
import dev.jhh3.pyre.net.PyreApiException
import java.io.IOException
import java.time.Instant

/** Base fake: every call fails as "not stubbed" unless a test overrides it. */
open class FakePyreApi : PyreApi {
    val calls = mutableListOf<Pair<String, Any?>>()
    protected fun <T> unstubbed(op: String): T = throw PyreApiException(ErrorCodes.UNSUPPORTED, "fake: $op not stubbed")
    protected fun record(op: String, req: Any?) { calls += op to req }
    fun count(op: String) = calls.count { it.first == op }

    override suspend fun hello(): HelloResponse = unstubbed(Ops.HELLO)
    override suspend fun listNodes(req: ListNodesRequest): Page<Node> = Page(emptyList())
    override suspend fun getNode(id: String): Node = unstubbed(Ops.GET_NODE)
    override suspend fun listSessions(req: ListSessionsRequest): Page<SessionView> = Page(emptyList())
    override suspend fun getSession(id: String): SessionView = unstubbed(Ops.GET_SESSION)
    override suspend fun startSession(req: StartSessionRequest): StartSessionResponse = unstubbed(Ops.START_SESSION)
    override suspend fun readSession(req: ReadSessionRequest): ReadSessionResponse = ReadSessionResponse()
    override suspend fun attach(req: AttachRequest): AttachDescriptor = unstubbed(Ops.ATTACH_SESSION)
    override suspend fun interrupt(req: SessionCommandRequest): CommandReceipt = unstubbed(Ops.INTERRUPT_SESSION)
    override suspend fun updateSession(req: UpdateSessionRequest): SessionView = unstubbed(Ops.UPDATE_SESSION)
    override suspend fun stop(req: SessionCommandRequest): CommandReceipt = unstubbed(Ops.STOP_SESSION)
    override suspend fun acquireLease(req: LeaseRequest): InputLease = unstubbed(Ops.ACQUIRE_LEASE)
    override suspend fun listApprovals(req: ListApprovalsRequest): Page<Approval> = Page(emptyList())
    override suspend fun getApproval(id: String): Approval = unstubbed(Ops.GET_APPROVAL)
    override suspend fun answerApproval(req: AnswerApprovalRequest): Approval = unstubbed(Ops.ANSWER_APPROVAL)
    override suspend fun sendMessage(req: SendMessageRequest): MessageView = unstubbed(Ops.SEND_MESSAGE)
    override suspend fun getMessage(id: String): MessageView = unstubbed(Ops.GET_MESSAGE)
    override suspend fun listMessages(req: ListMessagesRequest): Page<MessageView> = Page(emptyList())
    override suspend fun reissueMessage(req: ReissueMessageRequest): MessageView = unstubbed(Ops.REISSUE_MESSAGE)
    override suspend fun resolveDelivery(req: ResolveDeliveryRequest): MessageView = unstubbed(Ops.RESOLVE_DELIVERY)
    override suspend fun cancelMessage(req: CancelMessageRequest): MessageView = unstubbed(Ops.CANCEL_MESSAGE)
    override suspend fun listProjects(req: ListProjectsRequest): Page<Project> = Page(emptyList())
    override suspend fun getProject(id: String): Project = unstubbed(Ops.GET_PROJECT)
    override suspend fun getDocument(req: GetDocumentRequest): DocumentWithRevision = unstubbed(Ops.GET_DOCUMENT)
    override suspend fun listDocuments(req: ListDocumentsRequest): Page<Document> = Page(emptyList())
    override suspend fun listRevisions(req: ListRevisionsRequest): Page<DocumentRevision> = Page(emptyList())
    override suspend fun listArtifacts(req: ListArtifactsRequest): Page<Artifact> = Page(emptyList())
    override suspend fun getArtifact(id: String): Artifact = unstubbed(Ops.GET_ARTIFACT)
    override suspend fun listVersions(req: ListVersionsRequest): Page<ArtifactVersion> = Page(emptyList())
    override suspend fun getVersion(id: String): ArtifactVersion = unstubbed(Ops.GET_VERSION)
    override suspend fun previewUrl(req: MintPreviewRequest): PreviewUrl = unstubbed(Ops.PREVIEW_URL)
    override suspend fun createComment(req: CreateCommentRequest): ReviewComment = unstubbed(Ops.CREATE_COMMENT)
    override suspend fun getComment(id: String): ReviewComment = unstubbed(Ops.GET_COMMENT)
    override suspend fun listComments(req: ListCommentsRequest): Page<ReviewComment> = Page(emptyList())
    override suspend fun listAccounts(req: ListAccountsRequest): Page<AccountView> = Page(emptyList())
    override suspend fun listAttention(req: ListAttentionRequest): Page<AttentionItem> = Page(emptyList())
    override suspend fun getAttention(id: String): AttentionItem = unstubbed(Ops.GET_ATTENTION)
    override suspend fun updateAttention(req: UpdateAttentionRequest): AttentionItem = unstubbed(Ops.UPDATE_ATTENTION)
    override suspend fun getNotificationPrefs(): NotificationPrefs = NotificationPrefs()
    override suspend fun setNotificationPrefs(req: SetNotificationPrefsRequest): NotificationPrefs = req.prefs
    override suspend fun registerPush(req: PushRegistration): PushRegistrationResult = unstubbed(Ops.REGISTER_PUSH)
    override suspend fun listEvents(req: ListEventsRequest): ListEventsResponse = ListEventsResponse()
    override suspend fun nodeAction(req: NodeActionRequest): NodeActionResult = unstubbed(Ops.INVOKE_NODE_ACTION)
}

val FIXED_NOW: Instant = Instant.parse("2026-10-04T12:00:30Z")
val fixedClock = Clock { FIXED_NOW }

fun offline(): PyreApiException = PyreApiException.transport(IOException("synthetic: network down"))

fun testRepo(api: PyreApi, blobs: InMemoryBlobStore = InMemoryBlobStore()) =
    Repository(api, InMemoryCacheStore(fixedClock), Outbox(blobs, fixedClock), IntentIds(blobs), clock = fixedClock)
