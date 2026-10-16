package dev.jhh3.pyre.ui.inbox

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.OutboxItem
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.data.valueOrNull
import dev.jhh3.pyre.domain.AnswerWindow
import dev.jhh3.pyre.domain.AskAnswerer
import dev.jhh3.pyre.domain.Asks
import dev.jhh3.pyre.domain.AsksAnswerResult
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.AnswerAskItem
import dev.jhh3.pyre.model.Ask
import dev.jhh3.pyre.model.AskState
import dev.jhh3.pyre.model.AsksOps
import dev.jhh3.pyre.model.AttentionItem
import dev.jhh3.pyre.model.AttentionKind
import dev.jhh3.pyre.model.AttentionState
import dev.jhh3.pyre.model.MessageView
import dev.jhh3.pyre.model.UpdateAttentionRequest
import dev.jhh3.pyre.net.PyreApiException
import java.time.Duration
import java.time.Instant
import java.time.LocalTime
import java.time.ZoneId
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

// The owner inbox: attention items (messages, asks, approvals, ...) with
// read / done / snooze state kept by the hub (attention.update), and asks
// (asks.*, when the hub offers them). Tapping an option answers at once with
// a 5 s Undo; every answer given in that window goes out in one asks.answer
// call (AnswerWindow).

enum class InboxTab(val label: String) { ASKS("Asks"), MESSAGES("Messages"), ALL("All"), DONE("Done") }

/** [projectId]: null = any project, [Inbox.NO_PROJECT] = items without one. */
data class InboxFilters(val unreadOnly: Boolean = false, val projectId: String? = null, val snoozed: Boolean = false)

/** One inbox line: an attention item, joined with its ask when it is one (or an open ask the hub has no item for). */
data class InboxRow(
    val key: String,
    /** "" when an open ask has no attention item (nothing to mark). */
    val attentionId: String,
    val kind: String,
    val state: String,
    val snoozedUntil: Instant?,
    val sessionId: String,
    val projectId: String?,
    val title: String,
    val summary: String,
    val at: Instant?,
    val count: Int,
    val deepLink: String,
    val ask: Ask? = null,
) {
    val unread: Boolean get() = state == AttentionState.OPEN
}

/** Pure inbox logic (joining, tabs, filters, wording); tested without Android. */
object Inbox {
    const val NO_PROJECT = "@none"
    const val SNOOZE_HOUR_MINUTES = 60L
    /** Decided asks shown under Done. */
    val DONE_WINDOW: Duration = Duration.ofDays(7)

    val ACTIVE_STATES = listOf(AttentionState.OPEN, AttentionState.READ, AttentionState.SNOOZED)
    val DONE_STATES = listOf(AttentionState.HANDLED, AttentionState.WITHDRAWN)

    fun isAsk(item: AttentionItem): Boolean = item.kind == AttentionKind.ASK || item.`object`.kind == AsksOps.KIND

    /**
     * The newest listed owner message behind a message item: its message,
     * or the newest of its thread. Listed only (Repository.ownerInbox):
     * fetching one by id counts as the owner viewing it.
     */
    fun messageFor(item: AttentionItem, messages: List<MessageView>): MessageView? {
        val obj = item.`object`
        return messages.filter { m ->
            val e = m.envelope
            when (obj.kind) {
                "message" -> e.messageId == obj.id
                "thread" -> e.threadId == obj.id || e.messageId == obj.id
                else -> false
            }
        }.maxByOrNull { it.envelope.createdAt }
    }

    /**
     * Rows for [items] (any state) joined with [asks] by object id; open
     * asks without an item get their own row so nothing waiting is hidden.
     * A message row's text is the message itself when [messages] lists it,
     * else the item's summary.
     */
    fun rows(items: List<AttentionItem>, asks: List<Ask>, messages: List<MessageView> = emptyList()): List<InboxRow> {
        val byId = asks.associateBy { it.askId }
        val seen = HashSet<String>()
        val out = ArrayList<InboxRow>()
        for (item in items.distinctBy { it.attentionId }) {
            val ask = if (isAsk(item)) byId[item.`object`.id] else null
            if (ask != null) seen += ask.askId
            out += InboxRow(
                key = item.attentionId,
                attentionId = item.attentionId,
                kind = if (isAsk(item)) AttentionKind.ASK else item.kind,
                state = item.state,
                snoozedUntil = Times.parse(item.snoozedUntil),
                sessionId = item.sessionId.ifEmpty { ask?.sessionId.orEmpty() },
                projectId = item.projectId ?: ask?.projectId,
                title = ask?.title?.ifEmpty { null } ?: item.title,
                summary = (if (item.kind == AttentionKind.MESSAGE) messageFor(item, messages)?.envelope?.body?.text?.trim()?.ifEmpty { null } else null)
                    ?: item.summary,
                at = Times.parse(item.updatedAt) ?: Times.parse(item.createdAt),
                count = maxOf(item.count, ask?.count ?: 1),
                deepLink = item.deepLink,
                ask = ask,
            )
        }
        val attentionIds = out.mapTo(HashSet()) { it.attentionId }
        for (ask in asks) {
            if (ask.askId in seen || ask.decided) continue
            if (ask.attentionId.isNotEmpty() && ask.attentionId in attentionIds) continue
            out += InboxRow(
                key = "ask:" + ask.askId,
                attentionId = "",
                kind = AttentionKind.ASK,
                state = AttentionState.OPEN,
                snoozedUntil = null,
                sessionId = ask.sessionId,
                projectId = ask.projectId,
                title = ask.title,
                summary = "",
                at = Times.parse(ask.updatedAt) ?: Times.parse(ask.createdAt),
                count = ask.count,
                deepLink = "pyre://ask/" + ask.askId,
                ask = ask,
            )
        }
        return out
    }

    fun snoozed(r: InboxRow, now: Instant): Boolean =
        r.state == AttentionState.SNOOZED && (r.snoozedUntil?.isAfter(now) ?: true)

    /** Handled/withdrawn, or an ask that is already decided. */
    fun done(r: InboxRow): Boolean =
        r.state == AttentionState.HANDLED || r.state == AttentionState.WITHDRAWN || r.ask?.decided == true

    /** Waiting for the owner: open/read, or snoozed past its time (the hub may not have woken it yet). */
    fun active(r: InboxRow, now: Instant): Boolean = !done(r) && !snoozed(r, now) &&
        (r.state == AttentionState.OPEN || r.state == AttentionState.READ || r.state == AttentionState.SNOOZED)

    private fun inTab(r: InboxRow, tab: InboxTab): Boolean = when (tab) {
        InboxTab.ASKS -> r.kind == AttentionKind.ASK
        InboxTab.MESSAGES -> r.kind == AttentionKind.MESSAGE
        InboxTab.ALL, InboxTab.DONE -> true
    }

    /**
     * The rows a tab shows. Asks sort by deadline (soonest first), Done and
     * everything else newest first.
     */
    fun visible(rows: List<InboxRow>, tab: InboxTab, f: InboxFilters, now: Instant): List<InboxRow> {
        val list = rows.filter { r ->
            inTab(r, tab) &&
                when {
                    tab == InboxTab.DONE -> done(r)
                    f.snoozed -> snoozed(r, now) && !done(r)
                    else -> active(r, now)
                } &&
                (!f.unreadOnly || r.unread) &&
                (f.projectId == null || (r.projectId?.takeIf { it.isNotEmpty() } ?: NO_PROJECT) == f.projectId)
        }
        val newest = compareByDescending<InboxRow> { it.at ?: Instant.EPOCH }
        return if (tab == InboxTab.ASKS) {
            list.sortedWith(compareBy<InboxRow> { Times.parse(it.ask?.deadlineAt) ?: Instant.MAX }.then(newest))
        } else {
            list.sortedWith(newest)
        }
    }

    /** The tab the Inbox opens on: All, so the badge is what the owner sees on opening it. */
    val DEFAULT_TAB = InboxTab.ALL

    /**
     * The bottom-bar badge: the rows the Inbox lists on [DEFAULT_TAB] with
     * no filter ([visible] over [rows]; read rows count, done and snoozed
     * ones do not). Seen live: a badge of ~90 over a list of a few rows.
     */
    fun badgeCount(items: List<AttentionItem>, asks: List<Ask>, now: Instant): Int =
        visible(rows(items, asks), DEFAULT_TAB, InboxFilters(), now).size

    /** Rows per tab with no filter, as the tabs show them ("All 7" is the badge); Done has none. */
    fun tabCounts(rows: List<InboxRow>, now: Instant): Map<InboxTab, Int> =
        listOf(InboxTab.ASKS, InboxTab.MESSAGES, InboxTab.ALL).associateWith { visible(rows, it, InboxFilters(), now).size } +
            (InboxTab.DONE to 0)

    /** Unread waiting items per tab (Done has none). */
    fun unreadCounts(rows: List<InboxRow>, now: Instant): Map<InboxTab, Int> {
        val waiting = rows.filter { active(it, now) && it.unread }
        return mapOf(
            InboxTab.ASKS to waiting.count { it.kind == AttentionKind.ASK },
            InboxTab.MESSAGES to waiting.count { it.kind == AttentionKind.MESSAGE },
            InboxTab.ALL to waiting.size,
            InboxTab.DONE to 0,
        )
    }

    fun snoozedCount(rows: List<InboxRow>, now: Instant): Int = rows.count { snoozed(it, now) && !done(it) }

    fun kindLabel(kind: String): String = when (kind) {
        AttentionKind.ASK -> "Ask"
        AttentionKind.MESSAGE -> "Message"
        AttentionKind.APPROVAL -> "Approval"
        AttentionKind.QUESTION -> "Question"
        AttentionKind.FAILURE -> "Failed"
        AttentionKind.FLAG -> "Needs you"
        AttentionKind.ACCOUNT_EXHAUSTED -> "Accounts"
        AttentionKind.REVIEW_READY -> "Review"
        AttentionKind.INDETERMINATE -> "Delivery"
        AttentionKind.CONFLICT -> "Conflict"
        AttentionKind.COMMENT -> "Comment"
        AttentionKind.UPDATE -> "Update"
        AttentionKind.NODE -> "Machine"
        else -> kind.replace('_', ' ').replaceFirstChar { it.uppercase() }.ifEmpty { "Item" }
    }

    /** Kinds that block an agent until the owner acts (tinted chip). */
    fun blocking(kind: String): Boolean = kind == AttentionKind.ASK || kind == AttentionKind.APPROVAL ||
        kind == AttentionKind.QUESTION || kind == AttentionKind.FLAG

    /** Snooze for an hour. */
    fun inAnHour(now: Instant): Instant = now.plus(Duration.ofMinutes(SNOOZE_HOUR_MINUTES))

    /** "Tomorrow 8 AM" local; before 4 AM that is still tonight's morning (today 8 AM). */
    fun tomorrowMorning(now: Instant, zone: ZoneId = ZoneId.systemDefault()): Instant {
        val local = now.atZone(zone)
        val day = if (local.toLocalTime().isBefore(LocalTime.of(4, 0))) local.toLocalDate() else local.toLocalDate().plusDays(1)
        return day.atTime(8, 0).atZone(zone).toInstant()
    }
}

data class InboxUi(
    val loading: Boolean = true,
    val refreshing: Boolean = false,
    val tab: InboxTab = Inbox.DEFAULT_TAB,
    val filters: InboxFilters = InboxFilters(),
    val rows: List<InboxRow> = emptyList(),
    val asksSupported: Boolean = false,
    val sessionTitles: Map<String, String> = emptyMap(),
    val projects: Map<String, String> = emptyMap(),
    /** Long-press multi-select (row keys). */
    val selection: Set<String> = emptySet(),
    /** Answers given but not sent yet (the Undo window), by ask id; rows say "✓ B · sending…". */
    val held: Map<String, AnswerAskItem> = emptyMap(),
    /** The Undo snackbar's text while answers are held. */
    val undoText: String? = null,
    val openAskId: String? = null,
    /** The open ask when it is not among the rows (a link to a decided or unlisted ask). */
    val sheetAsk: Ask? = null,
    val sheetError: String? = null,
    val sending: Boolean = false,
    val notice: String? = null,
    val noticeTone: Tone = Tone.MUTED,
    /** The last answer's outcome is unknown; Retry resends the same batch (same operation id). */
    val canRetry: Boolean = false,
    val cachedAt: Instant? = null,
    val cachedError: PyreApiException? = null,
    val error: String? = null,
    val now: Instant = Instant.EPOCH,
) {
    fun ask(id: String): Ask? = rows.firstOrNull { it.ask?.askId == id }?.ask ?: sheetAsk?.takeIf { it.askId == id }
    val openAsk: Ask? get() = openAskId?.let { ask(it) }
}

class InboxViewModel(
    private val repo: Repository,
    private val answerer: AskAnswerer = AskAnswerer(repo),
    openAskId: String = "",
    private val zone: ZoneId = ZoneId.systemDefault(),
) : ViewModel() {
    private val _ui = MutableStateFlow(InboxUi(now = repo.clock.now()))
    val ui: StateFlow<InboxUi> = _ui.asStateFlow()

    private var items: List<AttentionItem> = emptyList()
    private var asks: List<Ask> = emptyList()
    private var messages: List<MessageView> = emptyList()
    /** Local read/done/snooze changes not yet reflected by the hub: attentionId → (state, snoozed until). */
    private val overrides = HashMap<String, Pair<String, String?>>()
    private var lastBatch: List<AnswerAskItem> = emptyList()
    private val window = AnswerWindow()
    private var windowJob: Job? = null
    /** The batch with the hub (resent under its own operation_id if the screen goes away mid-call). */
    private var inFlight: List<AnswerAskItem>? = null

    init {
        refresh()
        if (openAskId.isNotEmpty()) openAsk(openAskId)
    }

    fun refresh() {
        viewModelScope.launch { load() }
    }

    fun pullRefresh() {
        _ui.value = _ui.value.copy(refreshing = true)
        viewModelScope.launch {
            try {
                load()
            } finally {
                _ui.value = _ui.value.copy(refreshing = false)
            }
        }
    }

    private suspend fun load() {
        val now = repo.clock.now()
        val asksOn = repo.asksSupported()
        val active = repo.inboxItems("active", Inbox.ACTIVE_STATES)
        val done = repo.inboxItems("done", Inbox.DONE_STATES, maxPages = 1)
        val openAsks = if (asksOn) repo.asks("open", listOf(AskState.OPEN)) else null
        val decided = if (asksOn) repo.asks("decided", AskState.DECIDED, since = now.minus(Inbox.DONE_WINDOW).toString()) else null
        val owner = repo.ownerInbox().valueOrNull()
        val sessions = repo.sessions().valueOrNull().orEmpty()
        val projects = repo.projects().valueOrNull().orEmpty()
        val cur = _ui.value
        val ready = when (active) {
            is Load.Ready -> active
            is Load.Failed -> {
                _ui.value = cur.copy(loading = false, error = active.error.describe(), now = now)
                return
            }
            Load.Loading -> return
        }
        items = ready.value + done.valueOrNull().orEmpty()
        asks = openAsks?.valueOrNull().orEmpty() + decided?.valueOrNull().orEmpty().filterNot { d -> openAsks?.valueOrNull().orEmpty().any { it.askId == d.askId } }
        if (owner != null) messages = owner // listed, never fetched one by one (that counts as viewing)
        // Drop overrides the hub now reflects.
        items.forEach { i -> if (overrides[i.attentionId]?.first == i.state) overrides.remove(i.attentionId) }
        val rows = Inbox.rows(applyOverrides(items), asks, messages)
        val sheet = cur.openAskId?.let { id -> asks.firstOrNull { it.askId == id } } ?: cur.sheetAsk
        val tab = cur.tab
        _ui.value = cur.copy(
            loading = false,
            rows = rows,
            tab = if (!asksOn && tab == InboxTab.ASKS) InboxTab.ALL else tab,
            asksSupported = asksOn,
            sessionTitles = sessions.associate { s -> s.session.sessionId to ((if (s.session.icon.isNotEmpty()) s.session.icon + " " else "") + s.session.title.ifEmpty { s.session.sessionId }) },
            projects = projects.associate { it.projectId to it.name.ifEmpty { it.projectId } },
            sheetAsk = sheet,
            cachedAt = ready.cachedAt,
            cachedError = ready.error,
            error = null,
            now = now,
        )
    }

    private fun applyOverrides(list: List<AttentionItem>): List<AttentionItem> = list.map { i ->
        overrides[i.attentionId]?.let { (s, until) -> i.copy(state = s, snoozedUntil = until) } ?: i
    }

    private fun rebuild() {
        _ui.value = _ui.value.copy(rows = Inbox.rows(applyOverrides(items), asks, messages), now = repo.clock.now())
    }

    // ---- tabs, filters, selection ----

    fun setTab(t: InboxTab) {
        _ui.value = _ui.value.copy(tab = t, selection = emptySet(), filters = _ui.value.filters.copy(snoozed = false))
    }

    fun toggleUnread() {
        _ui.value = _ui.value.copy(filters = _ui.value.filters.copy(unreadOnly = !_ui.value.filters.unreadOnly))
    }

    fun toggleSnoozed() {
        _ui.value = _ui.value.copy(filters = _ui.value.filters.copy(snoozed = !_ui.value.filters.snoozed), selection = emptySet())
    }

    fun setProject(id: String?) {
        _ui.value = _ui.value.copy(filters = _ui.value.filters.copy(projectId = id))
    }

    fun toggleSelect(key: String) {
        val s = _ui.value.selection
        _ui.value = _ui.value.copy(selection = if (key in s) s - key else s + key)
    }

    fun clearSelection() {
        _ui.value = _ui.value.copy(selection = emptySet())
    }

    fun dismissNotice() {
        _ui.value = _ui.value.copy(notice = null, canRetry = false)
    }

    // ---- read / done / snooze (attention.update through the durable outbox) ----

    fun markRead(keys: Collection<String>) = update(keys, AttentionState.READ, null)

    fun markUnread(keys: Collection<String>) = update(keys, AttentionState.OPEN, null)

    fun markDone(keys: Collection<String>) = update(keys, AttentionState.HANDLED, null)

    fun snooze(keys: Collection<String>, until: Instant) = update(keys, AttentionState.SNOOZED, until)

    fun snoozeHour(keys: Collection<String>) = snooze(keys, Inbox.inAnHour(repo.clock.now()))

    fun snoozeMorning(keys: Collection<String>) = snooze(keys, Inbox.tomorrowMorning(repo.clock.now(), zone))

    /**
     * Applies the change at once (optimistic) and records each item in the
     * outbox with a fixed operation_id, so an offline change replays on
     * reconnect without applying twice. A refused change is put back and
     * explained.
     */
    private fun update(keys: Collection<String>, state: String, until: Instant?) {
        val rows = _ui.value.rows.filter { it.key in keys && it.attentionId.isNotEmpty() && it.state != state }
        _ui.value = _ui.value.copy(selection = emptySet())
        if (rows.isEmpty()) return
        val untilText = until?.toString()
        val reqs = rows.map { r ->
            overrides[r.attentionId] = state to untilText
            UpdateAttentionRequest(operationId = Ids.operation(), attentionId = r.attentionId, state = state, snoozedUntil = untilText)
        }
        rebuild()
        viewModelScope.launch {
            reqs.forEach { repo.outbox.enqueueAttention(it) }
            val drained = repo.flushOutbox()
            val ops = reqs.associateBy { it.operationId }
            // Stored by the hub: its state is authoritative from here on (an ask
            // answered later moves its item to handled; a kept override would hide that).
            for (d in repo.outbox.items.value) {
                val req = ops[d.operationId] ?: continue
                if (d.state == OutboxItem.DONE && overrides[req.attentionId]?.first == req.state) overrides.remove(req.attentionId)
            }
            val failed = repo.outbox.items.value.filter { it.operationId in ops && it.state == OutboxItem.FAILED }
            if (failed.isNotEmpty()) {
                failed.forEach { f ->
                    ops[f.operationId]?.let { overrides.remove(it.attentionId) }
                    repo.outbox.discard(f.operationId)
                }
                rebuild()
                _ui.value = _ui.value.copy(notice = "Could not update ${failed.size} item${if (failed.size == 1) "" else "s"}: ${failed.first().lastError}", noticeTone = Tone.ERROR)
            } else if (!drained) {
                _ui.value = _ui.value.copy(notice = "Saved on this phone; the hub gets it when it is reachable again.", noticeTone = Tone.MUTED)
            }
            if (drained) load()
        }
    }

    // ---- asks ----

    fun openAsk(id: String) {
        _ui.value = _ui.value.copy(openAskId = id, sheetError = null)
        val row = _ui.value.rows.firstOrNull { it.ask?.askId == id }
        if (row != null && row.unread && row.attentionId.isNotEmpty()) markRead(listOf(row.key))
        if (row == null) {
            viewModelScope.launch {
                try {
                    val a = repo.call { getAsk(id) }
                    if (_ui.value.openAskId == id) _ui.value = _ui.value.copy(sheetAsk = a)
                } catch (e: PyreApiException) {
                    if (_ui.value.openAskId == id) {
                        _ui.value = _ui.value.copy(sheetError = if (e.code == "not_found") "This question no longer exists." else e.describe())
                    }
                }
            }
        }
    }

    fun closeAsk() {
        _ui.value = _ui.value.copy(openAskId = null, sheetError = null)
    }

    /**
     * An option or free text from the sheet answers now: the sheet closes,
     * the row says "✓ B · sending…" and an Undo snackbar shows. The answer
     * is sent when the window ends, with every other answer given in it.
     */
    fun choose(askId: String, optionId: String, text: String = "") {
        val item = AnswerAskItem(askId = askId, optionId = optionId, text = text.trim())
        if (item.optionId.isEmpty() && item.text.isEmpty()) return
        window.hold(item)
        _ui.value = _ui.value.copy(openAskId = if (_ui.value.openAskId == askId) null else _ui.value.openAskId, sheetError = null, notice = null, canRetry = false)
        showHeld()
        windowJob?.cancel()
        windowJob = viewModelScope.launch {
            delay(AnswerWindow.WINDOW.toMillis())
            flushNow()
        }
    }

    /** Undo: takes back the last answer (or [askId]'s) before it is sent. */
    fun undo(askId: String? = null) {
        window.undo(askId)
        if (window.isEmpty()) windowJob?.cancel()
        showHeld()
    }

    /** Sends what is held now (the window ended, or the inbox is being left). */
    fun flushNow() {
        windowJob?.cancel()
        windowJob = null
        if (window.isEmpty()) return
        if (_ui.value.sending) {
            // One batch at a time: look again once the current one is answered.
            windowJob = viewModelScope.launch {
                delay(500)
                flushNow()
            }
            return
        }
        send(window.take())
        showHeld()
    }

    private fun showHeld() {
        val items = window.items
        val last = items.lastOrNull()
        _ui.value = _ui.value.copy(
            held = (inFlight.orEmpty() + items).associateBy { it.askId },
            undoText = last?.let { Asks.held(_ui.value.ask(it.askId), it, items.size - 1) },
        )
    }

    /** Answers are never dropped: what is held or in flight goes out as the inbox's model ends (same operation ids). */
    override fun onCleared() {
        val batches = listOfNotNull(inFlight, window.take().ifEmpty { null })
        windowJob?.cancel()
        if (batches.isEmpty()) return
        CoroutineScope(SupervisorJob()).launch {
            batches.forEach { runCatching { answerer.answer(it) } }
        }
    }

    fun retry() {
        if (lastBatch.isNotEmpty()) send(lastBatch)
    }

    private fun send(batch: List<AnswerAskItem>) {
        if (batch.isEmpty() || _ui.value.sending) return // double-tap guard (the stable operation_id is the second)
        lastBatch = batch
        inFlight = batch
        _ui.value = _ui.value.copy(sending = true, notice = null, canRetry = false)
        viewModelScope.launch {
            val r = answerer.answer(batch)
            inFlight = null
            val cur = _ui.value
            val settled: Set<String> = when (r) {
                is AsksAnswerResult.Sent -> r.results.filter { it.error == null || it.ask?.decided == true }.mapTo(HashSet()) { it.askId }
                is AsksAnswerResult.NotOpen -> batch.mapTo(HashSet()) { it.askId }
                else -> emptySet()
            }
            // Show the hub's view of each ask at once (decided ones move to Done).
            val returned = when (r) {
                is AsksAnswerResult.Sent -> r.results.mapNotNull { it.ask }
                is AsksAnswerResult.NotOpen -> r.asks
                else -> emptyList()
            }
            if (returned.isNotEmpty()) {
                val byId = returned.associateBy { it.askId }
                asks = asks.map { byId[it.askId] ?: it } + returned.filter { a -> asks.none { it.askId == a.askId } }
            }
            val tone = when (r) {
                is AsksAnswerResult.Sent -> if (r.refused.isEmpty()) Tone.MUTED else Tone.WARN
                is AsksAnswerResult.NotOpen -> Tone.WARN
                is AsksAnswerResult.OutcomeUnknown -> Tone.ATTENTION
                else -> Tone.ERROR
            }
            val sheetDone = cur.openAskId != null && cur.openAskId in settled
            _ui.value = cur.copy(
                sending = false,
                notice = Asks.summary(r),
                noticeTone = tone,
                canRetry = r is AsksAnswerResult.OutcomeUnknown,
                openAskId = if (sheetDone) null else cur.openAskId,
                sheetAsk = cur.sheetAsk?.let { s -> returned.firstOrNull { it.askId == s.askId } ?: s },
            )
            showHeld()
            if (returned.isNotEmpty()) rebuild()
            if (r is AsksAnswerResult.Sent || r is AsksAnswerResult.NotOpen) {
                lastBatch = emptyList()
                load()
            }
        }
    }
}

/** The Inbox's default rows, for the bottom-bar badge (Inbox.badgeCount). Polls only while the app is in front. */
class InboxCountViewModel(private val repo: Repository) : ViewModel() {
    private val _count = MutableStateFlow(0)
    val count: StateFlow<Int> = _count.asStateFlow()

    init { refresh() }

    /** The same loads as the Inbox (shared cache keys); a failed load keeps the last count. */
    fun refresh() {
        viewModelScope.launch {
            val now = repo.clock.now()
            val active = repo.inboxItems("active", Inbox.ACTIVE_STATES).valueOrNull() ?: return@launch
            val asks = if (repo.asksSupported()) repo.asks("open", listOf(AskState.OPEN)).valueOrNull().orEmpty() else emptyList()
            _count.value = Inbox.badgeCount(active, asks, now)
        }
    }
}
