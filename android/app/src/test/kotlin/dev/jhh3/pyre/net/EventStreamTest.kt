package dev.jhh3.pyre.net

import kotlinx.coroutines.flow.take
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import mockwebserver3.MockResponse
import mockwebserver3.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

class EventStreamTest {
    private lateinit var server: MockWebServer

    @Before fun setUp() { server = MockWebServer().also { it.start() } }
    @After fun tearDown() { server.close() }

    private fun client() = HubClient(HubEndpoint(server.url("/").toString().trimEnd('/'), devCleartext = true), { "tok-stream" })

    private fun ws(vararg frames: String) = mockwebserver3.MockResponse.Builder().webSocketUpgrade(object : WebSocketListener() {
        override fun onOpen(webSocket: WebSocket, response: Response) {
            frames.forEach { webSocket.send(it) }
            webSocket.close(1000, "done")
        }
    }).build()

    private val event = """{"event_id":"evt_1","cursor":42,"origin":"hub","kind":"attention.changed","object":{"kind":"attention","id":"att_1"},"project_id":null,"payload_version":1,
        "payload":{"attention_id":"att_1","kind":"approval","severity":"urgent","group_key":"g","object":{"kind":"approval","id":"apr_1"},"project_id":null,"title":"t","state":"open","count":1,"created_at":"2026-10-04T12:00:00Z","updated_at":"2026-10-04T12:00:00Z","deep_link":"pyre://approval/apr_1"},
        "occurred_at":"2026-10-04T12:00:00Z","ingested_at":"2026-10-04T12:00:00Z"}"""

    @Test
    fun deliversEventsWithCursorResumeAndHeaderAuth() = runBlocking {
        server.enqueue(ws(event))
        val signals = withTimeout(10_000) { EventStream(client(), minBackoffMillis = 10).signals { 41 }.take(3).toList() }
        assertEquals(StreamSignal.Connected, signals[0])
        assertEquals(42L, (signals[1] as StreamSignal.Received).event.cursor)
        assertTrue(signals[2] is StreamSignal.Disconnected)
        val req = server.takeRequest()
        assertEquals("41", req.url.queryParameter("after_cursor"))
        assertEquals("Bearer tok-stream", req.headers["Authorization"])
        assertTrue(req.url.toString().contains("tok-stream").not())
    }

    @Test
    fun staleCursorYieldsResetRequired() = runBlocking {
        server.enqueue(ws("""{"error":{"code":"reset_required","message":"cursor too old","retryable":false,"details":{"oldest_cursor":100,"snapshot_cursor":250}}}"""))
        val signals = withTimeout(10_000) { EventStream(client(), minBackoffMillis = 10).signals { 1 }.take(2).toList() }
        assertEquals(250L, (signals[1] as StreamSignal.ResetRequired).snapshotCursor)
    }

    @Test
    fun unauthorizedUpgradeIsFatal() = runBlocking {
        server.enqueue(MockResponse.Builder().code(401).build())
        val signals = withTimeout(10_000) { EventStream(client()).signals { 0 }.toList() }
        assertTrue((signals.single() as StreamSignal.Fatal).error.isUnauthorized)
    }

    @Test
    fun protocolMismatchPausesThenResumesWhenTheHubIsCompatibleAgain() = runBlocking {
        server.enqueue(
            MockResponse.Builder().code(426).body(
                """{"error":{"code":"protocol_mismatch","message":"client protocol 1 not supported; hub supports 2..2","retryable":false,"details":{"min_protocol":2,"protocol":2}}}""",
            ).build(),
        )
        server.enqueue(ws(event))
        val signals = withTimeout(10_000) {
            EventStream(client(), minBackoffMillis = 10, maxBackoffMillis = 40).signals { 0 }.take(3).toList()
        }
        val paused = signals[0] as StreamSignal.Incompatible
        assertTrue(paused.error.isProtocolMismatch)
        assertTrue(paused.error.describe(), paused.error.describe().contains("hub supports 2..2"))
        assertTrue(paused.retryInMillis in 20..40)
        assertEquals(StreamSignal.Connected, signals[1])
        assertEquals(42L, (signals[2] as StreamSignal.Received).event.cursor)
        assertEquals("Bearer tok-stream", server.takeRequest().headers["Authorization"])
        assertEquals("same credential on the retry", "Bearer tok-stream", server.takeRequest().headers["Authorization"])
    }

    @Test
    fun backoffIsBoundedWithJitter() {
        val s = EventStream(client(), minBackoffMillis = 1000, maxBackoffMillis = 60_000)
        repeat(50) {
            val b = s.backoff(20)
            assertTrue(b in 30_000..60_000)
        }
        assertTrue(s.backoff(0) in 500..1000)
    }
}
