package dev.jhh3.pyre.data

import dev.jhh3.pyre.core.Clock
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.CreateCommentRequest
import dev.jhh3.pyre.model.MessageView
import dev.jhh3.pyre.model.SendMessageRequest
import dev.jhh3.pyre.model.UpdateAttentionRequest
import dev.jhh3.pyre.net.PyreApi
import dev.jhh3.pyre.net.PyreApiException
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.serialization.Serializable
import kotlinx.serialization.builtins.ListSerializer

/**
 * Durable origin outbox for actions taken while the hub may be unreachable.
 *
 * Each item is created ONCE with its operation_id (and message_id for
 * sends) fixed, then replayed verbatim until the hub answers. Because the hub
 * deduplicates by operation_id/message_id, replay after reconnect can never
 * create a second message/comment. Items stay visible as "pending (not yet
 * stored on hub)" until a hub response arrives; a non-retryable hub error
 * marks the item failed with the hub's explanation (never silently dropped).
 *
 * Approval answers are deliberately NOT queued offline: an approval must be
 * re-validated against the live request when the user acts.
 */
@Serializable
data class OutboxItem(
    val operationId: String,
    val kind: String,
    val sendMessage: SendMessageRequest? = null,
    val createComment: CreateCommentRequest? = null,
    val updateAttention: UpdateAttentionRequest? = null,
    val createdAtMillis: Long,
    val state: String = PENDING,
    val attempts: Int = 0,
    val lastError: String = "",
    val stored: MessageView? = null,
) {
    companion object {
        const val PENDING = "pending"
        const val DONE = "done"
        const val FAILED = "failed"

        const val KIND_SEND = "messages.send"
        const val KIND_COMMENT = "comments.create"
        const val KIND_ATTENTION = "attention.update"
    }
}

class Outbox(private val store: BlobStore, private val clock: Clock = Clock.System) {
    private val ser = ListSerializer(OutboxItem.serializer())
    private val mutex = Mutex()
    private val _items = MutableStateFlow(load())
    val items: StateFlow<List<OutboxItem>> = _items.asStateFlow()

    private fun load(): List<OutboxItem> =
        store.get(FILE)?.let { runCatching { PyreJson.local.decodeFromString(ser, it) }.getOrNull() } ?: emptyList()

    private fun save(list: List<OutboxItem>) {
        // Keep completed items briefly for UI reconciliation; cap history.
        val trimmed = list.filter { it.state != OutboxItem.DONE } + list.filter { it.state == OutboxItem.DONE }.takeLast(50)
        _items.value = trimmed.sortedBy { it.createdAtMillis }
        store.put(FILE, PyreJson.local.encodeToString(ser, _items.value))
    }

    suspend fun enqueueSend(req: SendMessageRequest) = enqueue(OutboxItem(req.operationId, OutboxItem.KIND_SEND, sendMessage = req, createdAtMillis = clock.now().toEpochMilli()))
    suspend fun enqueueComment(req: CreateCommentRequest) = enqueue(OutboxItem(req.operationId, OutboxItem.KIND_COMMENT, createComment = req, createdAtMillis = clock.now().toEpochMilli()))
    suspend fun enqueueAttention(req: UpdateAttentionRequest) = enqueue(OutboxItem(req.operationId, OutboxItem.KIND_ATTENTION, updateAttention = req, createdAtMillis = clock.now().toEpochMilli()))

    private suspend fun enqueue(item: OutboxItem) = mutex.withLock {
        if (_items.value.none { it.operationId == item.operationId }) save(_items.value + item)
    }

    /** Removes a failed/done item. Pending items cannot be discarded: a prior
     *  attempt may already have reached the hub, so they must reconcile. */
    suspend fun discard(operationId: String) = mutex.withLock {
        save(_items.value.filterNot { it.operationId == operationId && it.state != OutboxItem.PENDING })
    }

    /**
     * Replays pending items in creation order. Stops at the first transport
     * failure (still offline) so order is preserved. Returns true if the
     * outbox is drained of pending items.
     */
    suspend fun flush(api: PyreApi): Boolean = mutex.withLock {
        var list = _items.value
        for (item in list.filter { it.state == OutboxItem.PENDING }) {
            val updated = try {
                val stored = execute(api, item)
                item.copy(state = OutboxItem.DONE, attempts = item.attempts + 1, lastError = "", stored = stored)
            } catch (e: PyreApiException) {
                // protocol_mismatch: the hub refused the request before running
                // it (incompatible version); keep it pending until a hub that
                // speaks our protocol answers, like an offline hub.
                if (e.transport || e.retryable || e.code == "outcome_indeterminate" || e.isProtocolMismatch) {
                    list = list.map { if (it.operationId == item.operationId) item.copy(attempts = item.attempts + 1, lastError = e.describe()) else it }
                    save(list)
                    return@withLock false
                }
                item.copy(state = OutboxItem.FAILED, attempts = item.attempts + 1, lastError = e.describe())
            }
            list = list.map { if (it.operationId == item.operationId) updated else it }
            save(list)
        }
        true
    }

    private suspend fun execute(api: PyreApi, item: OutboxItem): MessageView? = when (item.kind) {
        OutboxItem.KIND_SEND -> api.sendMessage(requireNotNull(item.sendMessage))
        OutboxItem.KIND_COMMENT -> { api.createComment(requireNotNull(item.createComment)); null }
        OutboxItem.KIND_ATTENTION -> { api.updateAttention(requireNotNull(item.updateAttention)); null }
        else -> throw PyreApiException("unsupported", "unknown outbox item ${item.kind}")
    }

    fun clear() {
        _items.value = emptyList()
        store.put(FILE, null)
    }

    private companion object {
        const val FILE = "outbox.json"
    }
}
