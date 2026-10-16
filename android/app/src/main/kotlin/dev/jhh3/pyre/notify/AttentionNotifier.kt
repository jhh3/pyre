package dev.jhh3.pyre.notify

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.ApprovalAnswerer
import dev.jhh3.pyre.domain.Asks
import dev.jhh3.pyre.model.Ask
import dev.jhh3.pyre.model.AsksOps
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

/** A notification button: an approval option, or (when [askId] is set) an ask option answered through asks.answer. */
data class QuickAction(val approvalId: String, val runId: String, val option: ApprovalOption, val askId: String = "")

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
 * canonical item and the live approval or ask. Any item that leaves
 * open/read — e.g. an approval resolved in the terminal, an ask answered
 * elsewhere or defaulted at its deadline — has its notification withdrawn.
 */
class AttentionNotifier(
    private val repo: Repository,
    private val sink: NotificationSink,
) {
    private val posted = HashMap<String, String>() // attentionId -> updatedAt|count
    private val askItems = HashMap<String, String>() // askId -> attentionId (shown with ask actions)

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
        if (e.kind == EventKinds.ASK_CHANGED) {
            onAskChanged(e)
            return false
        }
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

    /**
     * ask.changed: a decided ask (answered anywhere, defaulted, expired,
     * withdrawn, superseded) withdraws its notification at once, before the
     * attention item's own change arrives.
     */
    private fun onAskChanged(e: Event) {
        // Without a payload the attention.changed that follows decides.
        val ask = e.payload?.let { runCatching { PyreJson.wire.decodeFromJsonElement(Ask.serializer(), it) }.getOrNull() } ?: return
        if (!ask.decided) return
        val attentionId = ask.attentionId.ifEmpty { askItems[ask.askId].orEmpty() }
        if (attentionId.isNotEmpty() && attentionId in posted) cancel(attentionId)
        askItems.remove(ask.askId)
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
        askItems.values.removeAll { it == id }
        sink.cancel(id)
    }

    private suspend fun specFor(item: AttentionItem): NotificationSpec? {
        var actions = emptyList<QuickAction>()
        var text = item.summary.ifEmpty { kindLabel(item.kind) }
        var deepLink = if (item.sessionId.isNotEmpty()) "pyre://session/${item.sessionId}" else item.deepLink
        val isAsk = item.kind == AttentionKind.ASK || item.`object`.kind == AsksOps.KIND
        if (isAsk) {
            val askId = item.`object`.id
            // Tapping opens the ask's sheet in the inbox (options, context, the chat link).
            deepLink = "pyre://ask/$askId"
            val ask = runCatching { repo.call { getAsk(askId) } }.getOrNull()
            if (ask != null) {
                if (ask.decided) return null // stale: never offer choices for a decided ask
                askItems[askId] = item.attentionId
                actions = askActions(ask)
                text = askText(ask, repo.clock.now())
            }
        } else if (item.`object`.kind == "approval") {
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
            // A tap opens the session (its approval/message is answerable there) or the ask; items without one keep their own link.
            deepLink = deepLink,
            // An ask has a deadline after which its default applies: alert like an urgent item.
            urgent = item.severity == "urgent" || isAsk,
            actions = actions,
        )
    }

    companion object {
        /**
         * Up to three option buttons (Android's limit), in the ask's order;
         * with four options the default is kept and the last other one is
         * left to the app.
         */
        fun askActions(a: Ask): List<QuickAction> {
            val max = Asks.MAX_NOTIFICATION_ACTIONS
            val chosen = if (a.options.size <= max) a.options else {
                val def = a.options.firstOrNull { it.id == a.defaultOption }
                val others = a.options.filter { it.id != a.defaultOption }.take(if (def != null) max - 1 else max)
                a.options.filter { it == def || it in others }
            }
            return chosen.map { o ->
                val label = o.label.ifEmpty { o.id } + if (o.id == a.defaultOption) " (default)" else ""
                QuickAction(approvalId = "", runId = "", option = ApprovalOption(id = o.id, label = label, kind = "choice"), askId = a.askId)
            }
        }

        /** The question, its default and deadline, then the context. */
        fun askText(a: Ask, now: java.time.Instant): String {
            val deadline = Asks.deadline(a, now).replaceFirstChar { it.uppercase() }
            val more = if (a.options.size > Asks.MAX_NOTIFICATION_ACTIONS) "Open Pyre for all ${a.options.size} options." else ""
            return listOf(a.title, listOf(deadline, more).filter { it.isNotEmpty() }.joinToString(" · "), a.context)
                .filter { it.isNotEmpty() }
                .joinToString("\n")
        }

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
            AttentionKind.ASK -> "Question for you"
            else -> "Needs attention"
        }
    }
}
