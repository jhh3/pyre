package dev.jhh3.pyre.data

import dev.jhh3.pyre.core.Clock
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.PyreApi
import dev.jhh3.pyre.net.PyreApiException
import java.time.Instant
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.serialization.KSerializer
import kotlinx.serialization.builtins.ListSerializer

/** Result of a read that may be served from the offline cache. */
sealed interface Load<out T> {
    data object Loading : Load<Nothing>

    /** [cachedAt] non-null means the value came from the cache (hub unreachable or erroring). */
    data class Ready<T>(val value: T, val cachedAt: Instant? = null, val error: PyreApiException? = null) : Load<T>

    data class Failed(val error: PyreApiException) : Load<Nothing>
}

fun <T> Load<T>.valueOrNull(): T? = (this as? Load.Ready<T>)?.value

/** App-wide connectivity/authorization facts derived from the last call. */
data class HubHealth(
    val reachable: Boolean = true,
    val unauthorized: Boolean = false,
    val protocolMismatch: String? = null,
    val lastError: String? = null,
    val lastOkAt: Instant? = null,
)

class HubHealthTracker(private val clock: Clock = Clock.System) {
    private val _state = MutableStateFlow(HubHealth())
    val state: StateFlow<HubHealth> = _state.asStateFlow()

    fun ok() {
        _state.value = HubHealth(lastOkAt = clock.now())
    }

    fun failed(e: PyreApiException) {
        val cur = _state.value
        _state.value = when {
            e.transport -> cur.copy(reachable = false, lastError = e.describe())
            e.isUnauthorized -> cur.copy(reachable = true, unauthorized = true, lastError = e.describe())
            e.isProtocolMismatch -> cur.copy(reachable = true, protocolMismatch = e.message, lastError = e.describe())
            else -> cur.copy(reachable = true, lastError = e.describe())
        }
    }
}

/**
 * Repository over the hub API: cached reads ("cached Xm ago" when offline),
 * the durable outbox for offline-capable mutations, and stable intent ids.
 */
class Repository(
    val api: PyreApi,
    val cache: CacheStore,
    val outbox: Outbox,
    val intents: IntentIds,
    val health: HubHealthTracker = HubHealthTracker(),
    val clock: Clock = Clock.System,
) {
    /** Runs a hub call, recording reachability/auth state. */
    suspend fun <T> call(block: suspend PyreApi.() -> T): T = try {
        api.block().also { health.ok() }
    } catch (e: PyreApiException) {
        health.failed(e)
        throw e
    }

    suspend fun <T> cachedFetch(key: String, ser: KSerializer<T>, fetch: suspend PyreApi.() -> T): Load<T> = try {
        val v = call(fetch)
        cache.write(key, ser, v)
        Load.Ready(v)
    } catch (e: PyreApiException) {
        val c = cache.read(key, ser)
        if (c != null) Load.Ready(c.value, c.savedAt, e) else Load.Failed(e)
    }

    /** Reads up to [maxPages] pages of a cursor-paginated list. */
    suspend fun <T> allPages(maxPages: Int = 5, page: suspend PyreApi.(cursor: String) -> Page<T>): List<T> {
        val out = ArrayList<T>()
        var cursor = ""
        repeat(maxPages) {
            val p = api.page(cursor)
            out += p.items
            if (p.nextCursor.isEmpty()) return out
            cursor = p.nextCursor
        }
        return out
    }

    /**
     * Replays the outbox. A replay that delivered something proves the hub
     * is reachable again, so the "Hub unreachable" banner clears with it
     * instead of waiting for the next screen read.
     */
    suspend fun flushOutbox(): Boolean {
        val hadPending = outbox.items.value.any { it.state == OutboxItem.PENDING }
        val ok = outbox.flush(api)
        if (ok && hadPending) health.ok()
        return ok
    }

    // ---- cached reads ----

    suspend fun nodes(): Load<List<Node>> =
        cachedFetch("nodes", ListSerializer(Node.serializer())) { allPages { listNodes(ListNodesRequest(cursor = it, limit = 200)) } }

    suspend fun sessions(): Load<List<SessionView>> =
        cachedFetch("sessions", ListSerializer(SessionView.serializer())) { allPages { listSessions(ListSessionsRequest(cursor = it, limit = 200)) } }

    suspend fun session(id: String): Load<SessionView> =
        cachedFetch("session_$id", SessionView.serializer()) { getSession(id) }

    /** The latest [limit] entries (empty cursor), or the entries after [cursor]. */
    suspend fun transcript(sessionId: String, cursor: String, limit: Int = 100): Load<ReadSessionResponse> =
        cachedFetch("transcript_${sessionId}_${cursor}_$limit", ReadSessionResponse.serializer()) {
            readSession(ReadSessionRequest(sessionId = sessionId, cursor = cursor, limit = limit, tail = cursor.isEmpty()))
        }

    suspend fun sessionMessages(sessionId: String): Load<List<MessageView>> =
        cachedFetch("messages_$sessionId", ListSerializer(MessageView.serializer())) {
            // Both directions, so a conversation with a peer reads as one thread.
            listMessages(ListMessagesRequest(box = "all", address = Address(AddressKinds.SESSION, sessionId), limit = 50)).items
        }

    /** The owner's unacknowledged inbox (listed, never read one by one: messages.get by the owner counts as viewing). */
    suspend fun ownerInbox(): Load<List<MessageView>> =
        cachedFetch("messages_owner", ListSerializer(MessageView.serializer())) {
            listMessages(ListMessagesRequest(box = "inbox", unacked = true, address = Address(AddressKinds.OWNER, "owner"), limit = 100)).items
        }

    suspend fun approvalsFor(sessionId: String): Load<List<Approval>> =
        cachedFetch("approvals_$sessionId", ListSerializer(Approval.serializer())) {
            listApprovals(ListApprovalsRequest(sessionId = sessionId, states = listOf(ApprovalState.PENDING))).items
        }

    suspend fun pendingApprovals(): Load<List<Approval>> =
        cachedFetch("approvals_pending", ListSerializer(Approval.serializer())) {
            listApprovals(ListApprovalsRequest(states = listOf(ApprovalState.PENDING))).items
        }

    suspend fun attention(): Load<List<AttentionItem>> =
        cachedFetch("attention", ListSerializer(AttentionItem.serializer())) {
            allPages { listAttention(ListAttentionRequest(cursor = it, limit = 200)) }
        }

    suspend fun projects(): Load<List<Project>> =
        cachedFetch("projects", ListSerializer(Project.serializer())) { allPages { listProjects(ListProjectsRequest(cursor = it, limit = 200)) } }

    suspend fun project(id: String): Load<Project> = cachedFetch("project_$id", Project.serializer()) { getProject(id) }

    suspend fun documents(projectId: String): Load<List<Document>> =
        cachedFetch("docs_$projectId", ListSerializer(Document.serializer())) { listDocuments(ListDocumentsRequest(projectId = projectId, limit = 200)).items }

    suspend fun document(documentId: String, revisionId: String = ""): Load<DocumentWithRevision> =
        cachedFetch("doc_${documentId}_$revisionId", DocumentWithRevision.serializer()) {
            getDocument(GetDocumentRequest(documentId = documentId, revisionId = revisionId))
        }

    suspend fun brief(projectId: String): Load<DocumentWithRevision> =
        cachedFetch("brief_$projectId", DocumentWithRevision.serializer()) { getDocument(GetDocumentRequest(projectId = projectId, slug = "brief")) }

    suspend fun revisions(documentId: String): Load<List<DocumentRevision>> =
        cachedFetch("revs_$documentId", ListSerializer(DocumentRevision.serializer())) { listRevisions(ListRevisionsRequest(documentId = documentId, limit = 50)).items }

    suspend fun artifacts(projectId: String?): Load<List<Artifact>> =
        cachedFetch("artifacts_${projectId ?: "all"}", ListSerializer(Artifact.serializer())) {
            listArtifacts(ListArtifactsRequest(projectId = projectId, limit = 200)).items
        }

    suspend fun artifact(id: String): Load<Artifact> = cachedFetch("artifact_$id", Artifact.serializer()) { getArtifact(id) }

    suspend fun versions(artifactId: String): Load<List<ArtifactVersion>> =
        cachedFetch("versions_$artifactId", ListSerializer(ArtifactVersion.serializer())) { listVersions(ListVersionsRequest(artifactId = artifactId, limit = 100)).items }

    suspend fun comments(target: ObjectRef): Load<List<ReviewComment>> =
        cachedFetch("comments_${target.kind}_${target.id}", ListSerializer(ReviewComment.serializer())) {
            listComments(ListCommentsRequest(target = target, limit = 200)).items
        }

    suspend fun accounts(): Load<List<AccountView>> =
        cachedFetch("accounts", ListSerializer(AccountView.serializer())) { allPages { listAccounts(ListAccountsRequest(cursor = it, limit = 200)) } }

    @Volatile private var features: Set<String>? = null

    /**
     * The hub's Hello features, fetched once per pairing (null when the hub
     * could not be reached; asked again next time).
     */
    suspend fun hubFeatures(): Set<String>? {
        features?.let { return it }
        return try {
            call { hello() }.features.toSet().also { features = it }
        } catch (e: PyreApiException) {
            null
        }
    }

    /** Whether the hub offers asks (Hello feature "asks"); false when unknown. */
    suspend fun asksSupported(): Boolean = hubFeatures()?.contains(AsksOps.FEATURE) == true

    /** Owner inbox items in [states] (attention.list), all kinds. */
    suspend fun inboxItems(key: String, states: List<String>, maxPages: Int = 5): Load<List<AttentionItem>> =
        cachedFetch("inbox_$key", ListSerializer(AttentionItem.serializer())) {
            allPages(maxPages) { listAttention(ListAttentionRequest(cursor = it, limit = 200, states = states)) }
        }

    /** Asks in [states] (asks.list); [since] limits decided ones (RFC 3339). */
    suspend fun asks(key: String, states: List<String>, since: String? = null): Load<List<Ask>> =
        cachedFetch("asks_$key", ListSerializer(Ask.serializer())) {
            allPages(2) { listAsks(ListAsksRequest(cursor = it, limit = 200, states = states, since = since)) }
        }

    fun clearLocal() {
        cache.clear()
        outbox.clear()
    }
}

object AddressKinds {
    const val SESSION = "session"
    const val OWNER = "owner"
}
