package dev.jhh3.pyre.integration

import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.data.InMemoryCredentialStore
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.domain.Delivery
import dev.jhh3.pyre.domain.Status
import dev.jhh3.pyre.files.ArtifactFiles
import dev.jhh3.pyre.files.HubArtifactContent
import dev.jhh3.pyre.files.Verified
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.EventStream
import dev.jhh3.pyre.net.HttpPyreApi
import dev.jhh3.pyre.net.HubClient
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.net.StreamSignal
import dev.jhh3.pyre.pairing.PairingOutcome
import dev.jhh3.pyre.pairing.PairingService
import dev.jhh3.pyre.pairing.PairingUri
import dev.jhh3.pyre.terminal.TerminalSocket
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.viewer.HtmlPreviewViewModel
import java.io.File
import java.nio.file.Files
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.take
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeoutOrNull
import okhttp3.Request
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.terminal.RemoteTerminal

/**
 * JVM client against a REAL local pyre-hub + pyred (android/scripts/realhub-it.sh).
 * Skipped unless -Ppyre.realhub.urifile (a 0600 file holding the one-time
 * pairing link; kept off command lines) or -Ppyre.realhub.uri is set. The node runs the shell adapter
 * (real tmux on an isolated socket) and the dev mock adapter; mock-harness
 * results are labelled as such.
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = android.app.Application::class)
class RealHubIntegrationTest {
    private val uriProp = System.getProperty("pyre.realhub.urifile")?.let { File(it).readText().trim() }
        ?: System.getProperty("pyre.realhub.uri")
    private val cwd = System.getProperty("pyre.realhub.cwd") ?: "/tmp"
    private val log = StringBuilder()

    private fun note(s: String) {
        log.appendLine(s)
        println("[realhub-it] $s")
    }

    @Test
    fun androidClientAgainstRealHub() = runBlocking {
        assumeTrue("real hub not configured", uriProp != null)
        try {
            run()
        } finally {
            File(System.getProperty("pyre.realhub.evidence") ?: "build/realhub-it.txt").apply {
                parentFile?.mkdirs()
                writeText(log.toString())
            }
        }
    }

    private suspend fun run() {
        note("REAL pyre-hub + pyred (local, isolated state). Not a device test.")
        val uri = PairingUri.parse(uriProp!!).getOrThrow()
        note("pair-client link parsed: hub=${uri.hub} pinned=${uri.pin.isNotEmpty()}")
        val store = InMemoryCredentialStore()
        val paired = PairingService(store).pair(uri, "android-it", allowDevCleartext = false)
        assertTrue(paired.toString(), paired is PairingOutcome.Paired)
        paired as PairingOutcome.Paired
        val hello = assertNotNull(paired.hello).let { paired.hello!! }
        assertFalse("real hub must not report mock", hello.mock)
        note("paired client ${paired.credentials.clientId}; hub ${hello.hubVersion} protocol ${hello.minProtocol}..${hello.protocol} mock=${hello.mock} features=${hello.features}")
        val reuse = PairingService(InMemoryCredentialStore()).pair(uri, "android-it-2", allowDevCleartext = false)
        assertTrue("one-time code", reuse is PairingOutcome.Failed)
        note("re-using the pairing code: ${(reuse as PairingOutcome.Failed).message}")

        val client = HubClient(paired.credentials.endpoint, { store.load()!!.token })
        val api = HttpPyreApi(client)
        val repo = testRepo(api)

        val nodes = (repo.nodes() as Load.Ready).value
        nodes.forEach { n -> note("node ${n.name}: ${Status.node(n, java.time.Instant.now()).label}; harnesses ${n.harnesses.filter { it.available }.map { it.harness }}") }
        val node = nodes.first { it.connection.state == ConnectionState.CONNECTED }
        val available = node.harnesses.filter { it.available }.map { it.harness }

        // ---- launch (idempotent by operation_id) ----
        suspend fun launch(harness: String, title: String): SessionView {
            val req = StartSessionRequest(operationId = Ids.operation(), nodeId = node.nodeId, harness = harness, title = title, cwd = cwd, accountPolicy = AccountPolicy(AccountPolicyMode.ELIGIBLE))
            val a = api.startSession(req)
            val b = api.startSession(req)
            assertEquals("same operation_id -> same session", a.session.session.sessionId, b.session.session.sessionId)
            var v = a.session
            repeat(40) {
                v = api.getSession(v.session.sessionId)
                if (v.run?.state != null && v.run?.state != RunState.STARTING) return@repeat
                delay(500)
            }
            note("started $harness session ${v.session.sessionId}: ${Status.session(v, node, java.time.Instant.now()).let { "${it.label} — ${it.detail}" }}; caps=${v.run?.capabilities}")
            return v
        }

        val started = mutableListOf<SessionView>()
        try {
            // ---- structured messaging (mock harness: delivery path is real hub/daemon, runtime is mock) ----
            if (Harness.MOCK in available) {
                val m = launch(Harness.MOCK, "android-it mock").also { started += it }
                val send = SendMessageRequest(operationId = Ids.operation(), messageId = Ids.message(), recipient = Address("session", m.session.sessionId), mode = MessageMode.QUEUE, body = MessageBody("Synthetic Android IT message"))
                val m1 = api.sendMessage(send)
                val m2 = api.sendMessage(send)
                assertEquals(m1.envelope.messageId, m2.envelope.messageId)
                note("messages.send twice (same op) -> one message, storage=${m1.status.storage}")
                var mv = m1
                repeat(20) {
                    mv = api.getMessage(m1.envelope.messageId)
                    if (mv.status.runtime !in setOf(DeliveryState.QUEUED, DeliveryState.NODE_RECEIVED, DeliveryState.ATTEMPTING)) return@repeat
                    delay(500)
                }
                val f = Delivery.facts(mv)
                note("delivery facts [mock runtime]: ${f.storage.value} | ${f.runtime.value} | ${f.ack.value} | evidence=${f.evidence}")
                try {
                    api.sendMessage(send.copy(body = MessageBody("different")))
                    note("WARNING: op reuse with different body accepted")
                } catch (e: PyreApiException) {
                    assertEquals(ErrorCodes.CONFLICT, e.code)
                    note("op reuse with different body -> conflict")
                }
                val t = api.readSession(ReadSessionRequest(sessionId = m.session.sessionId, tail = true, limit = 50))
                note("sessions.read: ${t.entries.size} entries, gap=${t.gap}, freshness=${t.freshness.source}")
            }

            // ---- real terminal relay (shell adapter, isolated tmux) ----
            if (Harness.SHELL in available) {
                val s = launch(Harness.SHELL, "android-it shell").also { started += it }
                val d = api.attach(AttachRequest(operationId = Ids.operation(), sessionId = s.session.sessionId, preferred = dev.jhh3.pyre.ui.terminal.TerminalViewModel.PREFERRED))
                note("sessions.attach -> ${d.mechanism} (readOnly=${d.readOnly})")
                if (d.mechanism == AttachMechanism.WEB_TERMINAL) {
                    // Same wiring as TerminalViewModel: relay bytes -> vendored Termux emulator.
                    val term = RemoteTerminal(mainHandler = android.os.Handler(android.os.Looper.getMainLooper()))
                    term.session.updateSize(100, 30, 10, 20)
                    val opened = CountDownLatch(1)
                    val closed = CountDownLatch(1)
                    var closeError: PyreApiException? = null
                    val sock = TerminalSocket(client, d, object : TerminalSocket.Listener {
                        override fun onOpen() = opened.countDown()
                        override fun onOutput(bytes: ByteArray) = term.feed(bytes)
                        override fun onClosed(reason: String, error: PyreApiException?) { closeError = error; closed.countDown() }
                    })
                    term.sink = object : RemoteTerminal.Sink {
                        override fun write(data: ByteArray, offset: Int, count: Int) = sock.sendBytes(data, offset, count)
                        override fun resize(columns: Int, rows: Int) = sock.resize(columns, rows)
                    }
                    sock.connect(100, 30)
                    assertTrue("terminal opened", opened.await(10, TimeUnit.SECONDS))
                    val marker = "pyre-android-it-${System.nanoTime() % 100000}"
                    delay(1000)
                    term.sendText("echo $marker")
                    term.sendKey(RemoteTerminal.ENTER)
                    var seen = false
                    var screen = ""
                    repeat(30) {
                        shadowOf(android.os.Looper.getMainLooper()).idle()
                        val emu = term.session.emulator
                        screen = emu.screen.getSelectedText(0, 0, emu.mColumns, emu.mRows - 1)
                        // The command line, then a row that is exactly the echoed output.
                        if (screen.lines().any { it.trim() == marker } && screen.contains("echo $marker")) { seen = true; return@repeat }
                        delay(300)
                    }
                    note("web terminal (Termux emulator): typed echo via extra-key path, output row on screen=$seen; closeError=${closeError?.code}")
                    assertTrue("echo output rendered from the real PTY:\n$screen", seen)
                    // Resize through the session (what TerminalView does on layout) reaches the relay.
                    term.session.updateSize(90, 25, 10, 20)
                    sock.close()
                    closed.await(5, TimeUnit.SECONDS)
                    // The ticket is one-time: reusing it must fail with an error frame/close code, not output.
                    val again = CountDownLatch(1)
                    var reuseError: PyreApiException? = null
                    val out2 = StringBuilder()
                    TerminalSocket(client, d, object : TerminalSocket.Listener {
                        override fun onOpen() {}
                        override fun onOutput(bytes: ByteArray) { out2.append(String(bytes, Charsets.UTF_8)) }
                        override fun onClosed(reason: String, error: PyreApiException?) { reuseError = error; again.countDown() }
                    }).connect(80, 24)
                    again.await(10, TimeUnit.SECONDS)
                    note("re-using the terminal ticket -> ${reuseError?.code} (HTTP ${reuseError?.httpStatus}); output bytes=${out2.length}")
                    assertNotNull("ticket reuse must be an error", reuseError)
                }
            }

            // ---- attention ----
            val att = (repo.attention() as Load.Ready).value
            note("attention items=${att.size}")
            att.firstOrNull()?.let { a ->
                val one = api.getAttention(a.attentionId)
                assertEquals(a.attentionId, one.attentionId)
                note("attention.get ${a.attentionId} -> ${one.kind}/${one.state} notification=${one.notification?.state}")
            }

            // ---- artifact download + isolated preview ----
            val arts = api.listArtifacts(ListArtifactsRequest(limit = 50)).items
            val dir = Files.createTempDirectory("pyre-realhub").toFile()
            for (a in arts) {
                val v = api.listVersions(ListVersionsRequest(artifactId = a.artifactId)).items.maxBy { it.seq }
                val r = ArtifactFiles.fetch(dir, v, HubArtifactContent(client))
                assertTrue("digest verified: $r", r is Verified.Ok)
                note("artifact ${a.title} v${v.seq} ${v.mediaType} ${v.size}B -> digest Ok")
                if (ArtifactFiles.isHtml(v.mediaType)) {
                    val p = api.previewUrl(MintPreviewRequest(versionId = v.versionId))
                    val url = HtmlPreviewViewModel.resolve(client.endpoint, p.url)
                    assertNotNull("preview url acceptable: ${p.url.substringBefore("/preview/")}/preview/…", url)
                    val plain = client.okHttp.newBuilder().build()
                    val first = plain.newCall(Request.Builder().url(url!!).build()).execute().use { it.code to (it.header("Content-Security-Policy") ?: "") }
                    val second = plain.newCall(Request.Builder().url(url).build()).execute().use { it.code }
                    note("artifacts.preview_url -> ${if (url.host == client.endpoint.httpUrl.host && url.port == client.endpoint.httpUrl.port) "hub origin" else "separate preview origin"}, expires ${p.expiresAt}; GET without bearer -> ${first.first}, CSP='${first.second.take(80)}'; second GET -> $second")
                    assertEquals(200, first.first)
                    assertTrue("sandboxed CSP", first.second.contains("sandbox"))
                }
            }
            dir.deleteRecursively()

            // ---- event stream ----
            val signals = withTimeoutOrNull(10_000) { EventStream(client).signals { 0 }.take(2).toList() }
            note("event stream: ${signals?.map { it.javaClass.simpleName } ?: "timeout"}")
            assertEquals(StreamSignal.Connected, signals?.firstOrNull())
        } finally {
            for (s in started) {
                runCatching { api.stop(SessionCommandRequest(operationId = Ids.operation(), sessionId = s.session.sessionId, reason = "android IT cleanup")) }
                    .onSuccess { note("stopped ${s.session.sessionId}: accepted=${it.accepted} outcome=${it.outcome}") }
                    .onFailure { note("stop ${s.session.sessionId} failed: ${it.message}") }
            }
        }
    }
}
