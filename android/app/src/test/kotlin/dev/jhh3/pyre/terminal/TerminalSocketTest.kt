package dev.jhh3.pyre.terminal

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.domain.Status
import dev.jhh3.pyre.model.AttachDescriptor
import dev.jhh3.pyre.model.ConnectionState
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.NodeConnection
import dev.jhh3.pyre.net.HubClient
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.testing.FIXED_NOW
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import mockwebserver3.MockResponse
import mockwebserver3.MockWebServer
import okio.ByteString.Companion.encodeUtf8
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

class TerminalSocketTest {
    private lateinit var server: MockWebServer

    @Before fun setUp() { server = MockWebServer().also { it.start() } }
    @After fun tearDown() { server.close() }

    private class Recorder : TerminalSocket.Listener {
        val output = StringBuilder()
        var error: PyreApiException? = null
        var closed = CountDownLatch(1)
        var opened = false
        override fun onOpen() { opened = true }
        override fun onOutput(bytes: ByteArray) { output.append(String(bytes, Charsets.UTF_8)) }
        override fun onClosed(reason: String, error: PyreApiException?) { this.error = error; closed.countDown() }
    }

    private fun connect(serverSide: (WebSocket, String) -> Unit): Pair<Recorder, String> {
        val firstFrame = arrayOfNulls<String>(1)
        server.enqueue(mockwebserver3.MockResponse.Builder().webSocketUpgrade(object : WebSocketListener() {
            override fun onMessage(webSocket: WebSocket, text: String) {
                if (firstFrame[0] == null) {
                    firstFrame[0] = text
                    serverSide(webSocket, text)
                }
            }
        }).build())
        val ep = HubEndpoint(server.url("/").toString().trimEnd('/'), devCleartext = true)
        val client = HubClient(ep, { "owner-token" })
        val d = AttachDescriptor(runId = "run_1", mechanism = "web_terminal", url = "/v1/terminal", ticket = "one-time-ticket")
        val rec = Recorder()
        TerminalSocket(client, d, rec).connect(80, 24)
        assertTrue(rec.closed.await(10, TimeUnit.SECONDS))
        val req = server.takeRequest()
        assertFalse("ticket never in the URL", req.url.toString().contains("one-time-ticket"))
        return rec to firstFrame[0]!!
    }

    /** Found on a Pixel 10 Pro: the view resized before the handshake and the relay refused the session. */
    @Test
    fun resizeAndInputBeforeOpenNeverPrecedeTheAuthFrame() {
        val frames = java.util.Collections.synchronizedList(ArrayList<String>())
        val done = CountDownLatch(1)
        server.enqueue(MockResponse.Builder().webSocketUpgrade(object : WebSocketListener() {
            override fun onMessage(webSocket: WebSocket, text: String) { frames += "text:$text" }
            override fun onMessage(webSocket: WebSocket, bytes: okio.ByteString) {
                frames += "bin:" + bytes.utf8()
                webSocket.close(1000, null)
                done.countDown()
            }
        }).headersDelay(500, TimeUnit.MILLISECONDS).build())
        val ep = HubEndpoint(server.url("/").toString().trimEnd('/'), devCleartext = true)
        val d = AttachDescriptor(runId = "run_1", mechanism = "web_terminal", url = "/v1/terminal", ticket = "one-time-ticket")
        val s = TerminalSocket(HubClient(ep, { "owner-token" }), d, Recorder())
        s.connect(80, 24)
        s.resize(120, 40) // before the handshake completes
        s.sendInput("ls\r")
        assertTrue(done.await(10, TimeUnit.SECONDS))
        val first = PyreJson.wire.parseToJsonElement(frames[0].removePrefix("text:")).jsonObject
        assertEquals("auth", first["type"]!!.jsonPrimitive.content)
        assertEquals("120", first["cols"]!!.jsonPrimitive.content)
        assertEquals("40", first["rows"]!!.jsonPrimitive.content)
        assertEquals(listOf("bin:ls\r"), frames.drop(1))
    }

    @Test
    fun ticketInFirstFrameThenBinaryOutputThenNormalClose() {
        val (rec, first) = connect { ws, _ ->
            ws.send("hello \u001b[1mworld\u001b[0m".encodeUtf8())
            ws.close(1000, null)
        }
        val auth = PyreJson.wire.parseToJsonElement(first).jsonObject
        assertEquals("auth", auth["type"]!!.jsonPrimitive.content)
        assertEquals("one-time-ticket", auth["ticket"]!!.jsonPrimitive.content)
        assertTrue(rec.output.contains("hello"))
        assertNull(rec.error)
    }

    @Test
    fun errorFrameAndCloseCodeBecomeAnErrorNotOutput() {
        val (rec, _) = connect { ws, _ ->
            ws.send("""{"type":"error","error":{"code":"unauthorized","message":"ticket expired","retryable":false}}""")
            ws.close(4401, "unauthorized")
        }
        assertEquals("", rec.output.toString())
        assertEquals("unauthorized", rec.error!!.code)
        assertEquals("ticket expired", rec.error!!.message)
    }

    @Test
    fun closeCodeAloneMapsToHttpStatus() {
        val (rec, _) = connect { ws, _ -> ws.close(4503, "node offline") }
        assertEquals(503, rec.error!!.httpStatus)
        assertEquals("unavailable", rec.error!!.code)
        assertEquals("node offline", rec.error!!.message)
    }

    @Test
    fun classification() {
        assertTrue(TerminalSocket.classifyText("""{"type":"error","error":{"code":"conflict","message":"leased"}}""") is TerminalSocket.TextFrame.Error)
        assertEquals(TerminalSocket.TextFrame.Ignored, TerminalSocket.classifyText("""{"type":"pong"}"""))
        assertEquals(TerminalSocket.TextFrame.Output("plain"), TerminalSocket.classifyText("plain"))
        assertNull(TerminalSocket.closeError(1000, "", null))
        assertEquals(409, TerminalSocket.closeError(4409, "", null)!!.httpStatus)
        assertEquals(410, TerminalSocket.closeError(4410, "", null)!!.httpStatus)
    }

    @Test
    fun enrollmentPendingNodeLabel() {
        val n = Node(nodeId = "node_x", name = "new-box", connection = NodeConnection(state = ConnectionState.NEVER), labels = mapOf("pyre.enrollment" to "pending"))
        assertEquals("Enrollment pending", Status.node(n, FIXED_NOW).label)
        assertEquals("Never connected", Status.node(n.copy(labels = emptyMap()), FIXED_NOW).label)
    }
}
