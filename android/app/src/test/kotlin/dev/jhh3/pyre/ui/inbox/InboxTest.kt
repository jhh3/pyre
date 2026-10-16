package dev.jhh3.pyre.ui.inbox

import dev.jhh3.pyre.data.InMemoryBlobStore
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.testing.FIXED_NOW
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.testRepo
import java.time.Duration
import java.time.Instant
import java.time.ZoneId
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

private fun at(d: Duration): String = FIXED_NOW.plus(d).toString()

private fun item(
    id: String,
    kind: String,
    state: String = AttentionState.OPEN,
    obj: ObjectRef = ObjectRef("message", "msg_$id"),
    session: String = "ses_a",
    project: String? = null,
    ago: Duration = Duration.ofMinutes(5),
    snoozedUntil: String? = null,
) = AttentionItem(
    attentionId = id,
    kind = kind,
    `object` = obj,
    sessionId = session,
    projectId = project,
    title = "Synthetic $id",
    state = state,
    createdAt = at(ago.negated()),
    updatedAt = at(ago.negated()),
    snoozedUntil = snoozedUntil,
)

private fun ask(id: String, session: String = "ses_a", deadlineIn: Duration = Duration.ofMinutes(42), state: String = AskState.OPEN) = Ask(
    askId = id,
    topicKey = "synthetic-$id",
    sessionId = session,
    title = "Question $id",
    context = "Synthetic context.",
    options = listOf(AskOption("a", "A"), AskOption("b", "B")),
    defaultOption = "b",
    deadlineAt = at(deadlineIn),
    state = state,
    createdAt = at(Duration.ofMinutes(-3)),
)

class InboxTest {
    private val asks = listOf(ask("ask_1", deadlineIn = Duration.ofMinutes(50)), ask("ask_2", deadlineIn = Duration.ofMinutes(10)), ask("ask_3", session = "ses_b"))
    private val items = listOf(
        item("att_m1", AttentionKind.MESSAGE, project = "prj_x"),
        item("att_m2", AttentionKind.MESSAGE, state = AttentionState.READ, ago = Duration.ofMinutes(1)),
        item("att_a1", AttentionKind.ASK, obj = ObjectRef("ask", "ask_1")),
        item("att_a2", AttentionKind.ASK, obj = ObjectRef("ask", "ask_2"), state = AttentionState.READ),
        item("att_s", AttentionKind.MESSAGE, state = AttentionState.SNOOZED, snoozedUntil = at(Duration.ofHours(1))),
        item("att_d", AttentionKind.MESSAGE, state = AttentionState.HANDLED),
    )
    private val rows = Inbox.rows(items, asks)

    @Test
    fun asksJoinTheirItemsAndOrphanAsksGetARow() {
        assertEquals("ask_1", rows.first { it.key == "att_a1" }.ask?.askId)
        val orphan = rows.single { it.key == "ask:ask_3" }
        assertEquals("", orphan.attentionId)
        assertTrue(orphan.unread)
        assertEquals("pyre://ask/ask_3", orphan.deepLink)
    }

    @Test
    fun tabsFilterAndSort() {
        val f = InboxFilters()
        // Asks by deadline: ask_2 (10m), ask_3 (42m), ask_1 (50m).
        assertEquals(listOf("att_a2", "ask:ask_3", "att_a1"), Inbox.visible(rows, InboxTab.ASKS, f, FIXED_NOW).map { it.key })
        // Messages newest first; snoozed and handled ones hidden.
        assertEquals(listOf("att_m2", "att_m1"), Inbox.visible(rows, InboxTab.MESSAGES, f, FIXED_NOW).map { it.key })
        assertEquals(5, Inbox.visible(rows, InboxTab.ALL, f, FIXED_NOW).size)
        assertEquals(listOf("att_d"), Inbox.visible(rows, InboxTab.DONE, f, FIXED_NOW).map { it.key })
        assertEquals(listOf("att_s"), Inbox.visible(rows, InboxTab.ALL, f.copy(snoozed = true), FIXED_NOW).map { it.key })
        // A snooze that has passed is back in the inbox.
        assertTrue(Inbox.visible(rows, InboxTab.ALL, f, FIXED_NOW.plus(Duration.ofHours(2))).any { it.key == "att_s" })
        assertEquals(listOf("att_m1"), Inbox.visible(rows, InboxTab.MESSAGES, f.copy(unreadOnly = true), FIXED_NOW).map { it.key })
        assertEquals(listOf("att_m1"), Inbox.visible(rows, InboxTab.ALL, f.copy(projectId = "prj_x"), FIXED_NOW).map { it.key })
        assertFalse(Inbox.visible(rows, InboxTab.ALL, f.copy(projectId = Inbox.NO_PROJECT), FIXED_NOW).any { it.key == "att_m1" })
    }

    @Test
    fun decidedAskMovesToDone() {
        val decided = Inbox.rows(items, listOf(ask("ask_1", state = AskState.ANSWERED)))
        assertTrue(Inbox.visible(decided, InboxTab.DONE, InboxFilters(), FIXED_NOW).any { it.key == "att_a1" })
        assertFalse(Inbox.visible(decided, InboxTab.ASKS, InboxFilters(), FIXED_NOW).any { it.key == "att_a1" })
    }

    @Test
    fun unreadCounts() {
        val counts = Inbox.unreadCounts(rows, FIXED_NOW)
        assertEquals(2, counts[InboxTab.ASKS]) // att_a1 + the orphan ask_3
        assertEquals(1, counts[InboxTab.MESSAGES])
    }

    @Test
    fun badgeIsTheDefaultTabsRows() {
        // Read rows count, the orphan ask counts, snoozed and done ones do not.
        val shown = Inbox.visible(rows, Inbox.DEFAULT_TAB, InboxFilters(), FIXED_NOW)
        assertEquals(InboxTab.ALL, Inbox.DEFAULT_TAB)
        assertEquals(5, shown.size)
        assertEquals(shown.size, Inbox.badgeCount(items, asks, FIXED_NOW))
        assertEquals(shown.size, Inbox.tabCounts(rows, FIXED_NOW)[InboxTab.ALL])
    }

    @Test
    fun messageRowsShowTheListedMessage() {
        fun msg(id: String, thread: String, text: String, ago: Long) = MessageView(
            envelope = MessageEnvelope(messageId = id, recipient = Address("owner", "owner"), threadId = thread, body = MessageBody(text), createdAt = at(Duration.ofMinutes(-ago))),
        )
        val listed = listOf(
            msg("msg_att_m1", "", "Synthetic: the whole message, not a dash.", 5),
            msg("msg_t1", "thr_1", "Synthetic: older in the thread.", 9),
            msg("msg_t2", "thr_1", "Synthetic: newest in the thread.", 2),
        )
        val threaded = item("att_t", AttentionKind.MESSAGE, obj = ObjectRef("thread", "thr_1"))
        val unlisted = item("att_u", AttentionKind.MESSAGE, obj = ObjectRef("message", "msg_gone")).copy(summary = "Synthetic summary")
        val r = Inbox.rows(items + threaded + unlisted, asks, listed).associateBy { it.key }
        assertEquals("Synthetic: the whole message, not a dash.", r["att_m1"]?.summary)
        assertEquals("Synthetic: newest in the thread.", r["att_t"]?.summary)
        assertEquals("Synthetic summary", r["att_u"]?.summary)
    }

    @Test
    fun snoozeTimes() {
        val zone = ZoneId.of("America/Los_Angeles")
        // FIXED_NOW is 05:00:30 in Los Angeles: tomorrow 8 AM.
        assertEquals(Instant.parse("2026-10-05T15:00:00Z"), Inbox.tomorrowMorning(FIXED_NOW, zone))
        // At 02:00 local, "tomorrow morning" is this morning.
        assertEquals(Instant.parse("2026-10-04T15:00:00Z"), Inbox.tomorrowMorning(Instant.parse("2026-10-04T09:00:00Z"), zone))
        assertEquals(FIXED_NOW.plus(Duration.ofHours(1)), Inbox.inAnHour(FIXED_NOW))
    }
}

@OptIn(ExperimentalCoroutinesApi::class)
class InboxViewModelTest {
    private val dispatcher = StandardTestDispatcher()

    @Before fun setUp() { Dispatchers.setMain(dispatcher) }
    @After fun tearDown() { Dispatchers.resetMain() }

    private inner class Hub(val features: List<String> = listOf(AsksOps.FEATURE)) : FakePyreApi() {
        var items = listOf(
            item("att_m1", AttentionKind.MESSAGE),
            item("att_a1", AttentionKind.ASK, obj = ObjectRef("ask", "ask_1")),
            item("att_a2", AttentionKind.ASK, obj = ObjectRef("ask", "ask_2")),
        )
        var asks = listOf(ask("ask_1"), ask("ask_2"))
        val updates = mutableListOf<UpdateAttentionRequest>()
        val answers = mutableListOf<AnswerAsksRequest>()
        override suspend fun hello(): HelloResponse = HelloResponse(protocol = 1, features = features)
        override suspend fun listAttention(req: ListAttentionRequest): Page<AttentionItem> = Page(items.filter { it.state in req.states })
        override suspend fun updateAttention(req: UpdateAttentionRequest): AttentionItem {
            updates += req
            val i = items.first { it.attentionId == req.attentionId }.copy(state = req.state, snoozedUntil = req.snoozedUntil)
            items = items.map { if (it.attentionId == i.attentionId) i else it }
            return i
        }
        override suspend fun listAsks(req: ListAsksRequest): Page<Ask> {
            record(AsksOps.LIST, req)
            return Page(asks.filter { it.state in req.states })
        }
        override suspend fun answerAsks(req: AnswerAsksRequest): AnswerAsksResponse {
            answers += req
            val results = req.answers.map { a ->
                val done = asks.first { it.askId == a.askId }.copy(state = AskState.ANSWERED, answer = AskAnswer(optionId = a.optionId))
                asks = asks.map { if (it.askId == done.askId) done else it }
                items = items.map { if (it.`object`.id == done.askId) it.copy(state = AttentionState.HANDLED) else it }
                AnswerAskResult(a.askId, done)
            }
            return AnswerAsksResponse(results)
        }
    }

    @Test
    fun tapAnswersNowAndOneWindowGoesOutInOneCall() = runTest(dispatcher) {
        val hub = Hub()
        val vm = InboxViewModel(testRepo(hub, InMemoryBlobStore()))
        advanceUntilIdle()
        assertEquals(InboxTab.ALL, vm.ui.value.tab) // what the badge counts
        assertTrue(vm.ui.value.asksSupported)
        vm.openAsk("ask_1")
        advanceUntilIdle()
        // Tapping an option answers: the sheet closes, the row is marked, Undo shows; nothing is sent yet.
        vm.choose("ask_1", "a")
        assertEquals(null, vm.ui.value.openAskId)
        assertEquals("a", vm.ui.value.held["ask_1"]?.optionId)
        assertTrue(vm.ui.value.undoText!!.startsWith("Answered “Question ask_1” → A"))
        assertTrue(hub.answers.isEmpty())
        advanceTimeBy(3_000)
        vm.choose("ask_2", "", "synthetic free text") // restarts the window
        assertTrue(vm.ui.value.undoText!!.contains("and 1 more"))
        advanceTimeBy(4_000) // 7 s after the first, 4 s after the second
        assertTrue(hub.answers.isEmpty())
        advanceUntilIdle()
        assertEquals(1, hub.answers.size)
        assertEquals(listOf("a", ""), hub.answers.single().answers.map { it.optionId })
        assertTrue(vm.ui.value.held.isEmpty())
        assertEquals(null, vm.ui.value.undoText)
        assertTrue(Inbox.visible(vm.ui.value.rows, InboxTab.ASKS, InboxFilters(), vm.ui.value.now).isEmpty())
    }

    @Test
    fun undoInsideTheWindowSendsNothing() = runTest(dispatcher) {
        val hub = Hub()
        val vm = InboxViewModel(testRepo(hub, InMemoryBlobStore()))
        advanceUntilIdle()
        vm.choose("ask_1", "a")
        vm.choose("ask_2", "b")
        vm.undo() // the last one
        assertEquals(setOf("ask_1"), vm.ui.value.held.keys)
        vm.undo("ask_1")
        assertTrue(vm.ui.value.held.isEmpty())
        assertEquals(null, vm.ui.value.undoText)
        advanceUntilIdle()
        assertTrue(hub.answers.isEmpty())
    }

    @Test
    fun leavingTheInboxSendsHeldAnswersAtOnce() = runTest(dispatcher) {
        val hub = Hub()
        val vm = InboxViewModel(testRepo(hub, InMemoryBlobStore()))
        advanceUntilIdle()
        vm.choose("ask_1", "b")
        vm.flushNow() // the screen's onDispose
        advanceTimeBy(100)
        assertEquals(1, hub.answers.size)
        advanceUntilIdle()
        assertEquals(1, hub.answers.size) // the window's timer was cancelled
    }

    @Test
    fun doneAndSnoozeUpdateAttention() = runTest(dispatcher) {
        val hub = Hub()
        val vm = InboxViewModel(testRepo(hub, InMemoryBlobStore()), zone = ZoneId.of("UTC"))
        advanceUntilIdle()
        vm.markDone(listOf("att_m1"))
        // Optimistic: gone from the inbox before the hub answers.
        assertFalse(Inbox.visible(vm.ui.value.rows, InboxTab.ALL, InboxFilters(), vm.ui.value.now).any { it.key == "att_m1" })
        advanceUntilIdle()
        assertEquals(AttentionState.HANDLED, hub.updates.single().state)
        vm.snoozeMorning(listOf("att_a1"))
        advanceUntilIdle()
        assertEquals(AttentionState.SNOOZED, hub.updates.last().state)
        assertEquals("2026-10-05T08:00:00Z", hub.updates.last().snoozedUntil)
    }

    @Test
    fun badgeEqualsTheRowsTheInboxOpensOn() = runTest(dispatcher) {
        val hub = Hub()
        hub.items = hub.items + item("att_read", AttentionKind.MESSAGE, state = AttentionState.READ) +
            item("att_snz", AttentionKind.FAILURE, state = AttentionState.SNOOZED, snoozedUntil = at(Duration.ofHours(1)))
        val repo = testRepo(hub, InMemoryBlobStore())
        val vm = InboxViewModel(repo)
        val badge = InboxCountViewModel(repo)
        advanceUntilIdle()
        val shown = Inbox.visible(vm.ui.value.rows, vm.ui.value.tab, vm.ui.value.filters, vm.ui.value.now)
        assertEquals(4, shown.size) // m1, a1, a2 and the read message; not the snoozed failure
        assertEquals(shown.size, badge.count.value)
    }

    @Test
    fun hubWithoutAsksShowsAllAndNeverListsAsks() = runTest(dispatcher) {
        val hub = Hub(features = emptyList())
        val vm = InboxViewModel(testRepo(hub, InMemoryBlobStore()))
        advanceUntilIdle()
        assertFalse(vm.ui.value.asksSupported)
        assertEquals(InboxTab.ALL, vm.ui.value.tab)
        assertEquals(0, hub.count(AsksOps.LIST))
    }
}
