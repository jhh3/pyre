package dev.jhh3.pyre.ui.session

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.core.Clock
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.data.AddressKinds
import dev.jhh3.pyre.data.IntentIds
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.OutboxItem
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.data.valueOrNull
import dev.jhh3.pyre.domain.Flags
import dev.jhh3.pyre.domain.Pins
import dev.jhh3.pyre.model.AckMessageRequest
import dev.jhh3.pyre.model.Address
import dev.jhh3.pyre.model.Approval
import dev.jhh3.pyre.model.Caps
import dev.jhh3.pyre.model.CancelMessageRequest
import dev.jhh3.pyre.model.DeliveryState
import dev.jhh3.pyre.model.DeliveryStatus
import dev.jhh3.pyre.model.MessageBody
import dev.jhh3.pyre.model.MessageEnvelope
import dev.jhh3.pyre.model.MessageMode
import dev.jhh3.pyre.model.MessageView
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.ReadSessionResponse
import dev.jhh3.pyre.model.ReissueMessageRequest
import dev.jhh3.pyre.model.ResolveDeliveryRequest
import dev.jhh3.pyre.model.SendMessageRequest
import dev.jhh3.pyre.model.SessionCommandRequest
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.model.StorageState
import dev.jhh3.pyre.model.TranscriptEntry
import dev.jhh3.pyre.model.UpdateSessionRequest
import dev.jhh3.pyre.net.PyreApiException
import java.time.Instant
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

/** A message row: hub-stored, or still only in the local outbox. */
data class MessageRow(
    val view: MessageView,
    val localOnly: Boolean = false,
    val localError: String = "",
    val localFailed: Boolean = false,
    val attempts: Int = 0,
)

/** Reconciliation dialog for an indeterminate (or rejected) delivery. */
data class ReconcileUi(val messageId: String, val acceptRisk: Boolean = false, val busy: Boolean = false)

data class SessionDetailUi(
    val session: Load<SessionView> = Load.Loading,
    val node: Node? = null,
    val accountLabel: String? = null,
    val transcript: List<TranscriptEntry> = emptyList(),
    /** The transcript as chat items (grouped, classified), recomputed only when it changes. */
    val chat: List<ChatItem> = emptyList(),
    /** Whether an older page may exist before the first loaded entry. */
    val canLoadOlder: Boolean = false,
    val loadingOlder: Boolean = false,
    val transcriptLoaded: Boolean = false,
    val transcriptCursor: String = "",
    val transcriptGap: Boolean = false,
    val transcriptCachedAt: Instant? = null,
    val screen: String = "",
    val messages: List<MessageRow> = emptyList(),
    val approvals: List<Approval> = emptyList(),
    val draft: String = "",
    val sending: Boolean = false,
    val notice: String? = null,
    val reconcile: ReconcileUi? = null,
    /** session id -> title, for naming message peers. */
    val titles: Map<String, String> = emptyMap(),
    /** The newest unacknowledged message this session sent you, while its flag says so. */
    val messageForYou: MessageView? = null,
    /** Set by Reply: the next send is threaded as a reply to this message. */
    val replyTo: MessageView? = null,
    val now: Instant = Instant.EPOCH,
) {
    val caps: List<String> get() = session.valueOrNull()?.run?.capabilities.orEmpty()
    val canQueue: Boolean get() = Caps.INPUT_QUEUE in caps
    val canSteer: Boolean get() = Caps.INPUT_STEER in caps
    val canInterrupt: Boolean get() = Caps.INTERRUPT in caps
    val canAttach: Boolean get() = caps.any { it.startsWith("attach.") }
}

class SessionDetailViewModel(
    private val repo: Repository,
    val sessionId: String,
    private val clock: Clock = repo.clock,
) : ViewModel() {
    private val _ui = MutableStateFlow(SessionDetailUi(now = clock.now()))
    val ui: StateFlow<SessionDetailUi> = _ui.asStateFlow()
    private var hubMessages: List<MessageView> = emptyList()

    /** How many of the latest entries are read; grows as the user scrolls up (hub cap 500). */
    private var tailLimit = PAGE

    init {
        refresh()
        viewModelScope.launch { repo.outbox.items.collect { mergeMessages() } }
    }

    fun refresh() {
        viewModelScope.launch {
            val s = repo.session(sessionId)
            _ui.value = _ui.value.copy(session = s, now = clock.now())
            val run = s.valueOrNull()?.run
            if (run != null) {
                val node = repo.nodes().valueOrNull()?.firstOrNull { it.nodeId == run.nodeId }
                val label = run.accountId.takeIf { it.isNotEmpty() }?.let { id ->
                    repo.accounts().valueOrNull()?.firstOrNull { it.account.accountId == id }?.account?.label ?: id
                }
                _ui.value = _ui.value.copy(node = node, accountLabel = label)
            }
            loadMessageForYou()
            loadTranscript(reset = true)
            repo.sessions().valueOrNull()?.let { list ->
                val titles = list.associate { it.session.sessionId to it.session.title }
                _ui.value = _ui.value.copy(titles = titles, chat = Chat.items(_ui.value.transcript, titles))
            }
            repo.approvalsFor(sessionId).valueOrNull()?.let { _ui.value = _ui.value.copy(approvals = it) }
            repo.sessionMessages(sessionId).valueOrNull()?.let { hubMessages = it }
            repo.flushOutbox()
            mergeMessages()
        }
    }

    /**
     * Periodic update while the screen is visible: run state, pending
     * requests, delivery facts and newer transcript entries (appended, so a
     * page the user is reading is not replaced).
     */
    fun poll() {
        if (_ui.value.session !is Load.Ready) return refresh()
        viewModelScope.launch {
            val s = repo.session(sessionId)
            if (s is Load.Ready) _ui.value = _ui.value.copy(session = s, now = clock.now())
            loadMessageForYou()
            loadTranscript(reset = false)
            repo.approvalsFor(sessionId).valueOrNull()?.let { _ui.value = _ui.value.copy(approvals = it) }
            // Replay queued sends here too: without the hub stream nothing else
            // retries while the screen stays open after the hub comes back.
            if (repo.outbox.items.value.any { it.state == OutboxItem.PENDING } && repo.flushOutbox() &&
                _ui.value.notice == PENDING_NOTICE
            ) {
                _ui.value = _ui.value.copy(notice = null)
            }
            repo.sessionMessages(sessionId).valueOrNull()?.let { hubMessages = it }
            mergeMessages()
        }
    }

    /** Reads the owner's inbox only when the session's flag says it has a message for you. */
    private suspend fun loadMessageForYou() {
        val v = _ui.value.session.valueOrNull() ?: return
        val m = if (Flags.messages(v) == 0) null else repo.ownerInbox().valueOrNull()?.let { Flags.messageFor(v, it) }
        _ui.value = _ui.value.copy(messageForYou = m)
    }

    /** Reply to the session's message for you: the next send is threaded to it. */
    fun startReply() {
        _ui.value.messageForYou?.let { _ui.value = _ui.value.copy(replyTo = it) }
    }

    fun cancelReply() { _ui.value = _ui.value.copy(replyTo = null) }

    /** Acknowledges the message for you without a reply (messages.ack), which clears the "✉ message" flag. */
    fun dismissMessage() {
        val m = _ui.value.messageForYou ?: return
        val id = m.envelope.messageId
        if (!inFlight.add("ack:$id")) return
        _ui.value = _ui.value.copy(messageForYou = null, replyTo = _ui.value.replyTo?.takeIf { it.envelope.messageId != id })
        viewModelScope.launch {
            try {
                repo.call { ackMessage(AckMessageRequest(operationId = repo.intents.forIntent("ack:$id"), messageId = id)) }
                repo.intents.complete("ack:$id")
                repo.session(sessionId).let { if (it is Load.Ready) _ui.value = _ui.value.copy(session = it) }
            } catch (e: PyreApiException) {
                _ui.value = _ui.value.copy(messageForYou = m)
                notice("Dismiss: ${e.describe()}")
            } finally {
                inFlight.remove("ack:$id")
            }
        }
    }

    private suspend fun loadTranscript(reset: Boolean) {
        val cursor = if (reset) "" else _ui.value.transcriptCursor
        when (val t = repo.transcript(sessionId, cursor, if (reset) tailLimit else PAGE)) {
            is Load.Ready -> apply(t.value, reset, t.cachedAt)
            is Load.Failed -> if (reset) _ui.value = _ui.value.copy(notice = "Transcript: ${t.error.describe()}")
            Load.Loading -> Unit
        }
    }

    private fun apply(r: ReadSessionResponse, reset: Boolean, cachedAt: Instant?) {
        val cur = _ui.value.transcript
        if (!reset && r.entries.isEmpty() && _ui.value.transcriptLoaded) {
            // Nothing new: keep the same list instance so nothing re-renders.
            if (r.screen != _ui.value.screen) _ui.value = _ui.value.copy(screen = r.screen)
            return
        }
        val merged = if (reset) r.entries else (cur + r.entries).distinctBy { it.runId to it.seq }
        _ui.value = _ui.value.copy(
            transcript = merged,
            chat = if (merged == cur && _ui.value.transcriptLoaded) _ui.value.chat else Chat.items(merged, _ui.value.titles),
            canLoadOlder = if (reset) r.entries.size >= tailLimit && tailLimit < MAX_TAIL else _ui.value.canLoadOlder,
            transcriptLoaded = true,
            transcriptCursor = r.nextCursor.ifEmpty { _ui.value.transcriptCursor },
            transcriptGap = _ui.value.transcriptGap && !reset || r.gap,
            transcriptCachedAt = cachedAt,
            screen = r.screen,
        )
    }

    fun loadNewer() = viewModelScope.launch { loadTranscript(reset = false) }

    /**
     * Loads earlier activity when the user scrolls to the top: re-reads a
     * longer tail (sessions.read has no backwards cursor) and merges it, so
     * entries already shown keep their keys and the list does not jump.
     */
    fun loadOlder() {
        val u = _ui.value
        if (!u.canLoadOlder || u.loadingOlder) return
        _ui.value = u.copy(loadingOlder = true)
        viewModelScope.launch {
            val want = (tailLimit + PAGE * 2).coerceAtMost(MAX_TAIL)
            when (val t = repo.transcript(sessionId, "", want)) {
                is Load.Ready -> {
                    tailLimit = want
                    val merged = (t.value.entries + _ui.value.transcript).distinctBy { it.runId to it.seq }.sortedBy { it.seq }
                    _ui.value = _ui.value.copy(
                        transcript = merged,
                        chat = Chat.items(merged, _ui.value.titles),
                        canLoadOlder = t.value.entries.size >= want && want < MAX_TAIL,
                        transcriptGap = _ui.value.transcriptGap || t.value.gap,
                        loadingOlder = false,
                    )
                }
                else -> _ui.value = _ui.value.copy(loadingOlder = false)
            }
        }
    }

    private fun mergeMessages() {
        val local = repo.outbox.items.value.filter {
            it.kind == OutboxItem.KIND_SEND && it.sendMessage?.recipient?.id == sessionId && it.state != OutboxItem.DONE
        }
        val hubIds = hubMessages.map { it.envelope.messageId }.toSet()
        val doneStored = repo.outbox.items.value.mapNotNull { o -> o.stored?.takeIf { it.envelope.recipient.id == sessionId } }
        val hub = (hubMessages + doneStored.filter { it.envelope.messageId !in hubIds }).map { MessageRow(it) }
        val pending = local.filter { it.sendMessage!!.messageId !in hubIds }.map { o ->
            val req = o.sendMessage!!
            MessageRow(
                view = MessageView(
                    envelope = MessageEnvelope(messageId = req.messageId, operationId = req.operationId, recipient = req.recipient, mode = req.mode.ifEmpty { MessageMode.QUEUE }, body = req.body),
                    status = DeliveryStatus(storage = if (o.state == OutboxItem.FAILED) StorageState.REJECTED else StorageState.PENDING_LOCAL, runtime = DeliveryState.QUEUED),
                ),
                localOnly = true,
                localError = o.lastError,
                localFailed = o.state == OutboxItem.FAILED,
                attempts = o.attempts,
            )
        }
        // Inbox cursors are per mailbox, so order both directions by creation time.
        _ui.value = _ui.value.copy(messages = (hub.sortedWith(compareBy({ it.view.envelope.createdAt }, { it.view.cursor })) + pending), now = clock.now())
    }

    fun setDraft(s: String) { _ui.value = _ui.value.copy(draft = s) }

    fun dismissNotice() { _ui.value = _ui.value.copy(notice = null) }

    /**
     * Queues (or steers) a message. The request — with a fresh operation_id
     * and message_id fixed now — goes through the durable outbox, so an
     * offline send stays visible as pending and a later replay cannot
     * duplicate it.
     */
    fun send(steer: Boolean = false) {
        val text = _ui.value.draft.trim()
        if (text.isEmpty() || _ui.value.sending) return
        if (steer && !_ui.value.canSteer) { notice("This session's adapter does not support steering (input.steer)."); return }
        if (!steer && !_ui.value.canQueue) { notice("This session's adapter does not advertise queued input (input.queue)."); return }
        val req = SendMessageRequest(
            operationId = Ids.operation(),
            messageId = Ids.message(),
            recipient = Address(AddressKinds.SESSION, sessionId),
            projectId = _ui.value.session.valueOrNull()?.session?.projectId,
            mode = if (steer) MessageMode.STEER else MessageMode.QUEUE,
            body = MessageBody(text),
            threadId = _ui.value.replyTo?.envelope?.threadId.orEmpty(),
            inReplyTo = _ui.value.replyTo?.envelope?.messageId.orEmpty(),
        )
        _ui.value = _ui.value.copy(draft = "", sending = true, replyTo = null)
        viewModelScope.launch {
            repo.outbox.enqueueSend(req)
            val drained = repo.flushOutbox()
            if (drained) repo.sessionMessages(sessionId).valueOrNull()?.let { hubMessages = it }
            mergeMessages()
            _ui.value = _ui.value.copy(sending = false, notice = if (drained) null else PENDING_NOTICE)
            if (drained && req.inReplyTo.isNotEmpty()) {
                repo.session(sessionId).let { if (it is Load.Ready) _ui.value = _ui.value.copy(session = it) }
                loadMessageForYou()
            }
        }
    }

    fun discardLocal(operationId: String) = viewModelScope.launch { repo.outbox.discard(operationId) }

    fun interrupt() {
        val run = _ui.value.session.valueOrNull()?.run ?: return
        val key = "interrupt:${run.runId}"
        viewModelScope.launch {
            try {
                val r = repo.call { interrupt(SessionCommandRequest(operationId = repo.intents.forIntent(key), sessionId = sessionId, runId = run.runId, reason = "interrupt from Android")) }
                repo.intents.complete(key)
                notice("Interrupt ${if (r.accepted) "accepted" else "not accepted"} — outcome: ${r.outcome}${r.detail.takeIf { it.isNotEmpty() }?.let { " ($it)" }.orEmpty()}")
            } catch (e: PyreApiException) {
                notice("Interrupt: ${e.describe()}")
            }
        }
    }

    /**
     * Renames the session (sessions.update title). A person's rename always
     * wins: the hub stops automatic naming for this session. Blank is refused.
     * Returns false (with a notice) when nothing was sent.
     */
    fun rename(title: String): Boolean {
        val t = title.trim()
        if (t.isEmpty()) {
            notice("A session name can't be blank.")
            return false
        }
        val cur = _ui.value.session.valueOrNull() ?: return false
        if (t == cur.session.title) return true
        viewModelScope.launch {
            try {
                val v = repo.call { updateSession(UpdateSessionRequest(operationId = Ids.operation(), sessionId = sessionId, title = t)) }
                _ui.value = _ui.value.copy(session = Load.Ready(v), titles = _ui.value.titles + (sessionId to v.session.title))
                refresh()
            } catch (e: PyreApiException) {
                notice("Rename: ${e.describe()}")
            }
        }
        return true
    }

    /**
     * Pins or unpins the session (reserved "pinned" / "pinned_at:" tags via
     * sessions.update, read-modify-write of the current tags; shared with
     * the TUI). The header updates at once; a failure puts it back.
     */
    fun togglePin() {
        val cur = _ui.value.session.valueOrNull() ?: return
        val tags = Pins.toggle(cur.session.tags, clock.now())
        val verb = if (Pins.isPinned(tags)) "Pin" else "Unpin"
        _ui.value = _ui.value.copy(session = Load.Ready(cur.copy(session = cur.session.copy(tags = tags))))
        viewModelScope.launch {
            try {
                val v = repo.call { updateSession(UpdateSessionRequest(operationId = Ids.operation(), sessionId = sessionId, tags = tags)) }
                _ui.value = _ui.value.copy(session = Load.Ready(v))
                notice(if (Pins.isPinned(v.session.tags)) "Pinned — it shows first in Sessions." else "Unpinned.")
            } catch (e: PyreApiException) {
                _ui.value = _ui.value.copy(session = Load.Ready(cur))
                notice("$verb: ${e.describe()}")
            }
        }
    }

    // ---- delivery reconciliation ----

    fun openReconcile(messageId: String) { _ui.value = _ui.value.copy(reconcile = ReconcileUi(messageId)) }
    fun closeReconcile() { _ui.value = _ui.value.copy(reconcile = null) }
    fun setAcceptRisk(v: Boolean) { _ui.value.reconcile?.let { _ui.value = _ui.value.copy(reconcile = it.copy(acceptRisk = v)) } }

    /** Records the human decision about an indeterminate delivery (accepted | rejected). */
    fun resolve(messageId: String, resolution: String) = mutateMessage(IntentIds.resolve(messageId, resolution)) { op ->
        resolveDelivery(ResolveDeliveryRequest(operationId = op, messageId = messageId, resolution = resolution, note = "resolved from Android after inspecting evidence"))
    }

    /** Creates a NEW message referencing the original. Requires explicit acceptance of duplication risk when the outcome is unknown. */
    fun reissue(messageId: String) {
        val row = _ui.value.messages.firstOrNull { it.view.envelope.messageId == messageId } ?: return
        val risky = row.view.status.runtime == DeliveryState.INDETERMINATE
        if (risky && _ui.value.reconcile?.acceptRisk != true) {
            notice("Confirm that you accept the duplication risk before reissuing.")
            return
        }
        mutateMessage(IntentIds.reissue(messageId)) { op ->
            reissueMessage(ReissueMessageRequest(operationId = op, messageId = messageId, acceptDuplicateRisk = true))
        }
    }

    fun cancel(messageId: String) = mutateMessage("cancel:$messageId") { op ->
        cancelMessage(CancelMessageRequest(operationId = op, messageId = messageId))
    }

    private val inFlight = HashSet<String>()

    /**
     * Runs a message mutation under a stable per-intent operation_id. The
     * intent is deliberately NOT completed: a later repeat (double tap,
     * reopened dialog) replays the same operation and gets the hub's stored
     * result instead of creating a second reissue.
     */
    private fun mutateMessage(intent: String, block: suspend dev.jhh3.pyre.net.PyreApi.(String) -> MessageView) {
        if (!inFlight.add(intent)) return
        _ui.value.reconcile?.let { _ui.value = _ui.value.copy(reconcile = it.copy(busy = true)) }
        viewModelScope.launch {
            try {
                val op = repo.intents.forIntent(intent)
                val result = repo.call { block(op) }
                hubMessages = (hubMessages.filterNot { it.envelope.messageId == result.envelope.messageId } + result)
                repo.sessionMessages(sessionId).valueOrNull()?.let { hubMessages = it }
                mergeMessages()
                _ui.value = _ui.value.copy(reconcile = null)
            } catch (e: PyreApiException) {
                _ui.value = _ui.value.copy(reconcile = _ui.value.reconcile?.copy(busy = false), notice = e.describe())
            } finally {
                inFlight.remove(intent)
            }
        }
    }

    private fun notice(s: String) { _ui.value = _ui.value.copy(notice = s) }

    private companion object {
        const val PAGE = 100
        const val MAX_TAIL = 500
        const val PENDING_NOTICE = "Hub unreachable — message is pending locally and will be sent on reconnect."
    }
}
