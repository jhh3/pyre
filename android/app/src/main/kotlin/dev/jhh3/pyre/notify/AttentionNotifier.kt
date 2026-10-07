package dev.jhh3.pyre.notify

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.ApprovalAnswerer
import dev.jhh3.pyre.model.ApprovalOption
import dev.jhh3.pyre.model.ApprovalState
import dev.jhh3.pyre.model.AttentionItem
import dev.jhh3.pyre.model.AttentionKind
import dev.jhh3.pyre.model.AttentionState
import dev.jhh3.pyre.model.Event
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.model.EventKinds
import dev.jhh3.pyre.model.ListAttentionRequest
import dev.jhh3.pyre.model.PushKeys
import dev.jhh3.pyre.net.PyreApiException

data class QuickAction(val approvalId: String, val runId: String, val option: ApprovalOption)

data class NotificationSpec(
    val attentionId: String,
    val title: String,
    val text: String,
    val deepLink: String,
    val urgent: Boolean,
    val actions: List<QuickAction>,
)

interface NotificationSink {
    fun post(spec: NotificationSpec)
    fun cancel(attentionId: String)
}

/**
 * Turns the hub's durable attention items into phone notifications.
 *
 * Presentation policy (quiet hours, mutes, min severity, dedupe) is applied
 * by the hub: in hub-stream mode we alert only for items whose
 * `notification.state == "sent"`; FCM pushes are already the hub's decision.
 * Push/stream events are wake-ups: before showing controls we fetch the
 * canonical item and the live approval. Any item that leaves open/read — e.g.
 * an approval resolved in the terminal — has its notification withdrawn.
 */
class AttentionNotifier(
    private val repo: Repository,
    private val sink: NotificationSink,
) {
    private val posted = HashMap<String, String>() // attentionId -> updatedAt|count

    /** Snapshot reconcile (on connect / reset_required). */
    suspend fun refresh(): Result<Int> = try {
        val items = repo.call {
            listAttention(ListAttentionRequest(states = listOf(AttentionState.OPEN, AttentionState.READ, AttentionState.HANDLED, AttentionState.WITHDRAWN, AttentionState.SNOOZED), limit = 200)).items
        }
        Result.success(reconcile(items))
    } catch (e: PyreApiException) {
        Result.failure(e)
    }

    suspend fun reconcile(items: List<AttentionItem>): Int {
        var n = 0
        val seen = HashSet<String>()
        for (item in items) {
            seen += item.attentionId
            if (apply(item, requireSent = true)) n++
        }
        posted.keys.filter { it !in seen }.forEach { cancel(it) }
        return n
    }

    /** One hub-stream event. Returns true if a notification was posted. */
    suspend fun onEvent(e: Event): Boolean {
        if (e.kind != EventKinds.ATTENTION_CHANGED) return false
        val item = e.payload?.let { runCatching { PyreJson.wire.decodeFromJsonElement(AttentionItem.serializer(), it) }.getOrNull() }
            ?: fetch(e.`object`.id).getOrNull()
            ?: return false
        return apply(item, requireSent = true)
    }

    /**
     * FCM data-only message (contracts.PushDataKeys). `dismiss` withdraws the
     * alert; `notify` fetches the canonical item first and falls back to the
     * generic title + deep link (no controls) if the hub is unreachable.
     */
    suspend fun onPush(data: Map<String, String>) {
        val id = data[PushKeys.ATTENTION_ID] ?: return
        if (data[PushKeys.ACTION] == PushKeys.ACTION_DISMISS) {
            cancel(id)
            return
        }
        val fetched = fetch(id)
        val item = fetched.getOrNull()
        val err = fetched.exceptionOrNull() as? PyreApiException
        when {
            item != null -> apply(item, requireSent = false)
            err?.code == ErrorCodes.NOT_FOUND -> cancel(id)
            else -> sink.post(
                NotificationSpec(
                    attentionId = id,
                    title = data[PushKeys.TITLE].orEmpty().ifEmpty { "Pyre" },
                    text = kindLabel(data[PushKeys.KIND].orEmpty()) + " — open Pyre to see details (hub not reachable right now).",
                    deepLink = data[PushKeys.DEEP_LINK].orEmpty(),
                    urgent = data[PushKeys.SEVERITY] == "urgent",
                    actions = emptyList(),
                ),
            )
        }
    }

    /** attention.get (v1.2): one canonical item in any state. */
    private suspend fun fetch(id: String): Result<AttentionItem> = try {
        Result.success(repo.call { getAttention(id) })
    } catch (e: PyreApiException) {
        Result.failure(e)
    }

    private suspend fun apply(item: AttentionItem, requireSent: Boolean): Boolean {
        if (item.state != AttentionState.OPEN && item.state != AttentionState.READ) {
            cancel(item.attentionId)
            return false
        }
        if (requireSent && item.notification?.state != "sent") return false
        // A read item is not re-alerted; an already shown one stays until it leaves open/read.
        if (item.state == AttentionState.READ && item.attentionId !in posted) return false
        val version = "${item.updatedAt}|${item.count}"
        if (posted[item.attentionId] == version) return false
        val spec = specFor(item)
        if (spec == null) {
            cancel(item.attentionId)
            return false
        }
        sink.post(spec)
        posted[item.attentionId] = version
        return true
    }

    private fun cancel(id: String) {
        posted.remove(id)
        sink.cancel(id)
    }

    private suspend fun specFor(item: AttentionItem): NotificationSpec? {
        var actions = emptyList<QuickAction>()
        var text = item.summary.ifEmpty { kindLabel(item.kind) }
        if (item.`object`.kind == "approval") {
            val approval = runCatching { repo.call { getApproval(item.`object`.id) } }.getOrNull()
            if (approval != null) {
                if (approval.state != ApprovalState.PENDING) return null // stale: never show controls for a resolved request
                actions = ApprovalAnswerer.quickActions(approval).map { QuickAction(approval.approvalId, approval.runId, it) }
                if (approval.body.isNotEmpty()) text = approval.body
            }
        }
        return NotificationSpec(
            attentionId = item.attentionId,
            title = item.title + if (item.count > 1) " (${item.count})" else "",
            text = text,
            // A tap opens the session (its approval/message is answerable there); items without one keep their own link.
            deepLink = if (item.sessionId.isNotEmpty()) "pyre://session/${item.sessionId}" else item.deepLink,
            urgent = item.severity == "urgent",
            actions = actions,
        )
    }

    companion object {
        fun kindLabel(kind: String) = when (kind) {
            AttentionKind.APPROVAL -> "Approval requested"
            AttentionKind.QUESTION -> "Question waiting"
            AttentionKind.FAILURE -> "Run failed"
            AttentionKind.ACCOUNT_EXHAUSTED -> "No eligible account"
            AttentionKind.REVIEW_READY -> "Ready for review"
            AttentionKind.MESSAGE -> "New message"
            AttentionKind.INDETERMINATE -> "Delivery outcome unknown"
            AttentionKind.CONFLICT -> "Document conflict"
            AttentionKind.COMMENT -> "New comment"
            AttentionKind.UPDATE -> "Update result"
            AttentionKind.NODE -> "Machine status"
            else -> "Needs attention"
        }
    }
}
