package dev.jhh3.pyre.domain

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.testing.FIXED_NOW
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.offline
import dev.jhh3.pyre.testing.testRepo
import java.time.Duration
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/** Asks answers: one asks.answer call per batch, one operation_id per intent, decided asks never answered. */
class AskAnswererTest {
    private fun ask(id: String, state: String = AskState.OPEN, deadlineIn: Duration? = Duration.ofMinutes(42), default: String = "b") = Ask(
        askId = id,
        topicKey = "synthetic-topic-$id",
        sessionId = "ses_synth",
        title = "Synthetic question $id",
        context = "Synthetic context.",
        options = listOf(AskOption("a", "Rebase"), AskOption("b", "Merge"), AskOption("c", "Wait")),
        defaultOption = default,
        deadlineAt = deadlineIn?.let { FIXED_NOW.plus(it).toString() },
        state = state,
    )

    /** Idempotent by operation_id, like the hub. */
    private inner class Hub : FakePyreApi() {
        val asks = mutableMapOf("ask_1" to ask("ask_1"), "ask_2" to ask("ask_2"))
        val answerCalls = mutableListOf<AnswerAsksRequest>()
        val committed = mutableMapOf<String, AnswerAsksResponse>()
        var loseNextResponse = false
        override suspend fun getAsk(id: String): Ask = asks[id] ?: throw PyreApiException(ErrorCodes.NOT_FOUND, "no such ask")
        override suspend fun answerAsks(req: AnswerAsksRequest): AnswerAsksResponse {
            answerCalls += req
            committed[req.operationId]?.let { return it }
            val results = req.answers.map { a ->
                val cur = asks.getValue(a.askId)
                if (cur.decided) {
                    AnswerAskResult(a.askId, cur, ApiError(ErrorCodes.CONFLICT, "already decided"))
                } else {
                    val done = cur.copy(state = AskState.ANSWERED, answer = AskAnswer(optionId = a.optionId, text = a.text))
                    asks[a.askId] = done
                    AnswerAskResult(a.askId, done)
                }
            }
            val r = AnswerAsksResponse(results)
            committed[req.operationId] = r
            if (loseNextResponse) {
                loseNextResponse = false
                throw offline()
            }
            return r
        }
    }

    @Test
    fun batchIsOneCallWithEveryAnswer() = runBlocking {
        val hub = Hub()
        val r = AskAnswerer(testRepo(hub)).answer(listOf(AnswerAskItem("ask_1", "a"), AnswerAskItem("ask_2", text = "do both")))
        assertEquals(1, hub.answerCalls.size)
        assertEquals(listOf("ask_1", "ask_2"), hub.answerCalls.single().answers.map { it.askId })
        assertTrue(r is AsksAnswerResult.Sent)
        assertEquals(2, (r as AsksAnswerResult.Sent).committed.size)
        assertEquals("2 answers sent. The agent gets them as one message.", Asks.summary(r))
    }

    @Test
    fun retryAfterLostResponseReusesTheOperationId() = runBlocking {
        val hub = Hub().apply { loseNextResponse = true }
        val answerer = AskAnswerer(testRepo(hub))
        val batch = listOf(AnswerAskItem("ask_1", "a"))
        assertTrue(answerer.answer(batch) is AsksAnswerResult.OutcomeUnknown)
        val again = answerer.answer(batch)
        assertTrue(again is AsksAnswerResult.Sent)
        assertEquals(2, hub.answerCalls.size)
        assertEquals(hub.answerCalls[0].operationId, hub.answerCalls[1].operationId)
        assertTrue((again as AsksAnswerResult.Sent).refused.isEmpty())
    }

    @Test
    fun decidedAskIsNotAnsweredFromANotification() = runBlocking {
        val hub = Hub()
        hub.asks["ask_1"] = ask("ask_1", state = AskState.DEFAULTED).copy(answer = AskAnswer(optionId = "b", byDefault = true))
        val r = AskAnswerer(testRepo(hub)).answerOne("ask_1", "a", "")
        assertTrue(r is AsksAnswerResult.NotOpen)
        assertEquals("The deadline passed; the default applied: Merge.", (r as AsksAnswerResult.NotOpen).explanation)
        assertTrue(hub.answerCalls.isEmpty())
    }

    @Test
    fun deadlineWording() {
        assertEquals("defaults to Merge in 42m", Asks.deadline(ask("x"), FIXED_NOW))
        assertEquals("expires in 42m", Asks.deadline(ask("x", default = ""), FIXED_NOW))
        assertEquals("defaults to Merge in 2h 5m", Asks.deadline(ask("x", deadlineIn = Duration.ofMinutes(125)), FIXED_NOW))
        assertEquals("defaulting to Merge", Asks.deadline(ask("x", deadlineIn = Duration.ofMinutes(-1)), FIXED_NOW))
        assertEquals("", Asks.deadline(ask("x", state = AskState.ANSWERED), FIXED_NOW))
        assertEquals("", Asks.deadline(ask("x", deadlineIn = null), FIXED_NOW))
        assertTrue(Asks.soon(ask("x", deadlineIn = Duration.ofMinutes(10)), FIXED_NOW))
    }

    @Test
    fun askDecodesFromTheWireShape() {
        val json = """{"ask_id":"ask_w","topic_key":"t","project_id":null,"session_id":"ses_1","title":"T","context":"C",
            "options":[{"id":"a","label":"A"},{"id":"b","label":"B","detail":"d"}],"default_option":"b",
            "deadline_at":"2026-10-04T12:42:30Z","deadline_rule":"1h","state":"open","count":2,"created_at":"2026-10-04T11:42:30Z",
            "updated_at":"2026-10-04T11:50:00Z","updates":[{"seq":1,"text":"more","at":"2026-10-04T11:50:00Z"}]}"""
        val a = PyreJson.wire.decodeFromString(Ask.serializer(), json)
        assertEquals("b", a.defaultOption)
        assertEquals("B", a.option("b")?.label)
        assertEquals(1, a.updates.size)
        assertEquals("defaults to B in 42m", Asks.deadline(a, FIXED_NOW))
        val req = PyreJson.wire.encodeToString(AnswerAsksRequest.serializer(), AnswerAsksRequest(operationId = "op_1", answers = listOf(AnswerAskItem("ask_w", "b"))))
        assertEquals("""{"operation_id":"op_1","answers":[{"ask_id":"ask_w","option_id":"b"}]}""", req)
    }

    @Test
    fun answerWindowHoldsReplacesUndoesAndTakes() {
        val w = AnswerWindow()
        w.hold(AnswerAskItem(askId = "ask_1", optionId = "a"))
        w.hold(AnswerAskItem(askId = "ask_2", optionId = "b"))
        w.hold(AnswerAskItem(askId = "ask_1", optionId = "c")) // a newer answer replaces, and is now the last
        assertEquals(listOf("ask_2" to "b", "ask_1" to "c"), w.items.map { it.askId to it.optionId })
        assertEquals("ask_1", w.undo()?.askId)
        assertEquals(null, w.undo("ask_9")) // not held: nothing else is taken back
        assertEquals(listOf("ask_2"), w.take().map { it.askId })
        assertTrue(w.isEmpty())
        assertEquals(null, w.undo())
    }
}
