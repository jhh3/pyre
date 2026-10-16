package dev.jhh3.pyre.notify

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.testing.FIXED_NOW
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.testRepo
import java.time.Duration
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** Ask notifications: option buttons (max 3, default noted), a tap opens the ask, decided asks withdraw. */
class AskNotificationTest {
    private val ask = Ask(
        askId = "ask_synth",
        topicKey = "synthetic-topic",
        sessionId = "ses_synth",
        title = "Synthetic: rebase or merge?",
        context = "Synthetic context line.",
        options = listOf(AskOption("a", "Rebase"), AskOption("b", "Merge"), AskOption("c", "Wait"), AskOption("d", "Drop")),
        defaultOption = "d",
        deadlineAt = FIXED_NOW.plus(Duration.ofMinutes(42)).toString(),
        attentionId = "att_ask",
    )
    private val item = AttentionItem(
        attentionId = "att_ask",
        kind = AttentionKind.ASK,
        `object` = ObjectRef(AsksOps.KIND, "ask_synth"),
        sessionId = "ses_synth",
        title = "Question from synthetic session",
        updatedAt = FIXED_NOW.toString(),
        notification = NotificationStatus(provider = "hub-stream", state = "sent"),
    )

    private class Sink : NotificationSink {
        val posted = mutableListOf<NotificationSpec>()
        val cancelled = mutableListOf<String>()
        override fun post(spec: NotificationSpec) { posted += spec }
        override fun cancel(attentionId: String) { cancelled += attentionId }
    }

    private inner class Hub : FakePyreApi() {
        var current = ask
        override suspend fun getAsk(id: String): Ask = current
    }

    private fun attentionEvent(i: AttentionItem) = Event(
        eventId = "evt_a", cursor = 1, kind = EventKinds.ATTENTION_CHANGED, `object` = ObjectRef("attention", i.attentionId),
        payload = PyreJson.wire.encodeToJsonElement(AttentionItem.serializer(), i),
    )

    private fun askEvent(a: Ask) = Event(
        eventId = "evt_b", cursor = 2, kind = EventKinds.ASK_CHANGED, `object` = ObjectRef(AsksOps.KIND, a.askId),
        payload = PyreJson.wire.encodeToJsonElement(Ask.serializer(), a),
    )

    @Test
    fun askPostsOptionButtonsWithTheDefaultNoted() = runBlocking {
        val sink = Sink()
        val n = AttentionNotifier(testRepo(Hub()), sink)
        assertTrue(n.onEvent(attentionEvent(item)))
        val spec = sink.posted.single()
        // Four options: three buttons, the default kept.
        assertEquals(listOf("a", "b", "d"), spec.actions.map { it.option.id })
        assertEquals("Drop (default)", spec.actions.last().option.label)
        assertTrue(spec.actions.all { it.askId == "ask_synth" && it.approvalId.isEmpty() })
        assertTrue(spec.text, spec.text.contains("Defaults to Drop in 42m"))
        assertTrue(spec.text.contains("Open Pyre for all 4 options."))
        assertEquals("pyre://ask/ask_synth", spec.deepLink)
        assertTrue(spec.urgent)
    }

    @Test
    fun decidedAskWithdrawsTheNotification() = runBlocking {
        val sink = Sink()
        val hub = Hub()
        val n = AttentionNotifier(testRepo(hub), sink)
        n.onEvent(attentionEvent(item))
        // Answered elsewhere (TUI, another phone) or defaulted at the deadline.
        hub.current = ask.copy(state = AskState.DEFAULTED)
        assertFalse(n.onEvent(askEvent(hub.current)))
        assertEquals(listOf("att_ask"), sink.cancelled)
        // Still open: nothing withdrawn.
        val sink2 = Sink()
        val n2 = AttentionNotifier(testRepo(Hub()), sink2)
        n2.onEvent(attentionEvent(item))
        n2.onEvent(askEvent(ask))
        assertTrue(sink2.cancelled.isEmpty())
    }

    @Test
    fun staleAskGetsNoButtons() = runBlocking {
        val sink = Sink()
        val hub = Hub().apply { current = ask.copy(state = AskState.ANSWERED) }
        assertFalse(AttentionNotifier(testRepo(hub), sink).onEvent(attentionEvent(item)))
        assertTrue(sink.posted.isEmpty())
    }

    @Test
    fun threeOrFewerOptionsAreAllButtons() {
        val three = ask.copy(options = ask.options.take(3), defaultOption = "b")
        assertEquals(listOf("a", "b", "c"), AttentionNotifier.askActions(three).map { it.option.id })
        assertEquals("Merge (default)", AttentionNotifier.askActions(three)[1].option.label)
    }
}
