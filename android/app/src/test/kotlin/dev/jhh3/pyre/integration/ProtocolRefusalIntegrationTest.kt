package dev.jhh3.pyre.integration

import android.content.Context
import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import androidx.datastore.preferences.core.Preferences
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.data.CredentialStore
import dev.jhh3.pyre.data.DataStoreCredentialStore
import dev.jhh3.pyre.data.AddressKinds
import dev.jhh3.pyre.data.InMemoryCredentialStore
import dev.jhh3.pyre.data.KeystoreAesGcmCipher
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.OutboxItem
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.EventStream
import dev.jhh3.pyre.net.HttpPyreApi
import dev.jhh3.pyre.net.HubClient
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.net.StreamSignal
import dev.jhh3.pyre.pairing.PairingOutcome
import dev.jhh3.pyre.pairing.PairingService
import dev.jhh3.pyre.pairing.PairingUri
import dev.jhh3.pyre.testing.FakeAndroidKeyStore
import dev.jhh3.pyre.testing.testRepo
import java.io.File
import java.util.Collections
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/**
 * A16 (mobile half): the Android client against a REAL pyre-hub that is
 * swapped underneath it by android/scripts/protocol-it.sh:
 *
 *   A  protocol 1..1 hub   pair; credential in the real encrypted store
 *   B  protocol 2..2 hub   every call refused clearly; pairing refused without
 *                          using the code; credential, queued action kept
 *   C  protocol 1..1 again same credential works; queued action replays once;
 *                          the event stream resumed by itself; the code from
 *                          B still pairs (it was never consumed)
 *   D  protocol 1..2 hub   compatible upgrade keeps working
 *
 * Phases are synchronised through marker files in -Ppyre.realhub.protodir
 * (the script writes `<P>.ready` after swapping the hub, this test writes
 * `<P>.done`). Skipped unless that property is set. Pairing links are read
 * from 0600 files, never from command lines.
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = android.app.Application::class)
class ProtocolRefusalIntegrationTest {
    private val dir = System.getProperty("pyre.realhub.protodir")?.let(::File)
    private val log = StringBuilder()
    private val ctx: Context get() = ApplicationProvider.getApplicationContext()

    private fun note(s: String) {
        log.appendLine(s)
        println("[protocol-it] $s")
    }

    private fun waitFor(name: String) {
        val f = File(dir, name)
        val deadline = System.currentTimeMillis() + 300_000
        while (!f.exists()) {
            check(System.currentTimeMillis() < deadline) { "timed out waiting for $name" }
            Thread.sleep(200)
        }
        note("--- phase ${name.substringBefore('.')}: ${f.readText().trim()}")
    }

    private fun done(phase: String) = File(dir, "$phase.done").writeText("ok\n")

    private fun link(name: String) = PairingUri.parse(File(dir, name).readText().trim()).getOrThrow()

    // One DataStore per file per process; a new DataStoreCredentialStore on
    // it starts with an empty cache, which models an app restart.
    private lateinit var ds: DataStore<Preferences>
    private fun restartStore(): CredentialStore = DataStoreCredentialStore(ds, KeystoreAesGcmCipher(), null)

    @Test
    fun incompatibleHubIsRefusedAndCompatibleOneWorksAgain() = runBlocking {
        assumeTrue("protocol IT not configured", dir != null)
        try {
            run()
        } finally {
            File(System.getProperty("pyre.realhub.evidence") ?: "build/protocol-it.txt").apply {
                parentFile?.mkdirs()
                writeText(log.toString())
            }
        }
    }

    private suspend fun run() {
        FakeAndroidKeyStore.install()
        FakeAndroidKeyStore.reset()
        val file = File(ctx.filesDir, "datastore/${DataStoreCredentialStore.FILE}")
        ds = PreferenceDataStoreFactory.create(scope = CoroutineScope(Dispatchers.IO + SupervisorJob()), produceFile = { file })

        // ---- A: protocol 1 hub ----
        waitFor("A.ready")
        val store = restartStore()
        val paired = PairingService(store).pair(link("link-a"), "android-protocol-it", allowDevCleartext = false)
        assertTrue(paired.toString(), paired is PairingOutcome.Paired)
        paired as PairingOutcome.Paired
        val creds = paired.credentials
        val helloA = assertNotNull(paired.hello).let { paired.hello!! }
        note("A paired client ${creds.clientId}; hub ${helloA.hubVersion} protocol ${helloA.minProtocol}..${helloA.protocol}; app speaks ${Protocol.VERSION}")
        assertEquals(1, helloA.protocol)

        val client = HubClient(creds.endpoint, { restartStore().load()?.token })
        val api = HttpPyreApi(client)
        val repo = testRepo(api)
        val nodesA = repo.nodes()
        assertTrue(nodesA is Load.Ready && nodesA.cachedAt == null)
        note("A nodes.list fresh (${(nodesA as Load.Ready).value.size} nodes; cached for later)")

        // One stream for the whole run, as HubStreamService holds it.
        val signals = Collections.synchronizedList(mutableListOf<StreamSignal>())
        val streamScope = CoroutineScope(Dispatchers.IO + SupervisorJob())
        val stream = streamScope.launch {
            EventStream(client, minBackoffMillis = 500, maxBackoffMillis = 3_000).signals { 0 }.collect {
                signals += it
                note("  stream: ${describe(it)}")
            }
        }
        awaitSignal(signals, 0) { it == StreamSignal.Connected }
        // The stream reconnects as soon as the next hub listens, possibly
        // before this test sees <P>.ready: each phase looks at the signals
        // since the previous phase ended.
        val fromB = signals.size
        done("A")

        // ---- B: protocol 2..2 hub (upgraded, incompatible) ----
        waitFor("B.ready")
        assertEquals("credential survives an app restart", creds, restartStore().load())
        val err = try {
            api.hello()
            null
        } catch (e: PyreApiException) {
            e
        }
        assertNotNull("hello must be refused", err)
        err!!
        note("B hello -> ${err.code} (HTTP ${err.httpStatus}): ${err.describe()}")
        assertTrue(err.isProtocolMismatch)
        assertEquals(426, err.httpStatus)
        assertTrue(err.describe(), err.describe().contains("hub supports 2..2"))
        assertEquals("2", err.detail("min_protocol"))

        val nodesB = repo.nodes()
        assertTrue("cached data stays readable", nodesB is Load.Ready && nodesB.cachedAt != null)
        val health = repo.health.state.value
        assertTrue(health.protocolMismatch != null && !health.unauthorized && health.reachable)
        note("B nodes.list -> cached copy; health protocolMismatch='${health.protocolMismatch}' unauthorized=${health.unauthorized}")

        val send = SendMessageRequest(
            operationId = Ids.operation(), messageId = Ids.message(),
            recipient = Address(AddressKinds.OWNER, "owner"), mode = MessageMode.QUEUE,
            body = MessageBody("Synthetic protocol-IT note, queued while the hub was incompatible"),
        )
        repo.outbox.enqueueSend(send)
        assertFalse(repo.outbox.flush(api))
        val pending = repo.outbox.items.value.single()
        assertEquals(OutboxItem.PENDING, pending.state)
        note("B outbox: messages.send queued -> ${pending.state} (attempts ${pending.attempts}, '${pending.lastError}')")

        val paused = awaitSignal(signals, fromB) { it is StreamSignal.Incompatible } as StreamSignal.Incompatible
        assertTrue(paused.error.describe().contains("hub supports 2..2"))

        val keep = InMemoryCredentialStore(creds)
        val refused = PairingService(keep).pair(link("link-b"), "android-protocol-it-b", allowDevCleartext = false)
        assertTrue(refused.toString(), refused is PairingOutcome.Failed)
        note("B pairing (existing pairing on device) -> Failed: ${(refused as PairingOutcome.Failed).message}")
        assertTrue(refused.message.contains("Protocol mismatch") && refused.message.contains("not used"))
        assertEquals("existing pairing kept", creds, keep.load())
        val empty = InMemoryCredentialStore()
        val refused2 = PairingService(empty).pair(link("link-b"), "android-protocol-it-b", allowDevCleartext = false)
        assertTrue(refused2 is PairingOutcome.Failed)
        assertNull("nothing stored by a refused pairing", empty.load())
        note("B pairing (fresh device) -> Failed, nothing stored")
        assertEquals("encrypted store unchanged", creds, restartStore().load())
        val fromC = signals.size
        done("B")

        // ---- C: back to a protocol 1 hub (rolled back) ----
        waitFor("C.ready")
        val restored = restartStore().load()
        assertEquals("same credential after the rollback", creds, restored)
        val helloC = api.hello()
        note("C hello ok with the same credential: hub ${helloC.hubVersion} protocol ${helloC.minProtocol}..${helloC.protocol}")
        val nodesC = repo.nodes()
        assertTrue(nodesC is Load.Ready && nodesC.cachedAt == null)
        assertNull(repo.health.state.value.protocolMismatch)
        note("C nodes.list fresh; health protocolMismatch cleared")
        assertTrue(repo.outbox.flush(api))
        val replayed = repo.outbox.items.value.single()
        assertEquals(OutboxItem.DONE, replayed.state)
        assertEquals(send.messageId, replayed.stored?.envelope?.messageId)
        val again = api.sendMessage(send)
        assertEquals("replay is idempotent", send.messageId, again.envelope.messageId)
        note("C outbox replayed -> ${replayed.state}, stored ${replayed.stored?.status?.storage}; attempts ${replayed.attempts}; resend same op -> same message")
        awaitSignal(signals, fromC) { it == StreamSignal.Connected }
        note("C event stream resumed by itself (no restart, no re-pair)")
        val second = PairingService(InMemoryCredentialStore()).pair(link("link-b"), "android-protocol-it-b", allowDevCleartext = false)
        assertTrue("the code refused in B was not consumed: $second", second is PairingOutcome.Paired)
        note("C pairing with B's link -> Paired (${(second as PairingOutcome.Paired).credentials.clientId}): the refused attempt never used the code")
        val fromD = signals.size
        done("C")

        // ---- D: compatible upgrade (protocol 1..2) ----
        waitFor("D.ready")
        val helloD = api.hello()
        note("D hello ok: hub ${helloD.hubVersion} protocol ${helloD.minProtocol}..${helloD.protocol}")
        assertEquals(1, helloD.minProtocol)
        assertEquals(2, helloD.protocol)
        assertTrue(repo.nodes().let { it is Load.Ready && it.cachedAt == null })
        // The hub restart drops the stream; it must come back on the new hub.
        val drop = awaitSignal(signals, fromD) { it is StreamSignal.Disconnected }
        awaitSignal(signals, synchronized(signals) { signals.indexOf(drop) } + 1) { it == StreamSignal.Connected }
        assertEquals(creds, restartStore().load())
        note("D nodes.list fresh, stream connected, credential unchanged")

        stream.cancelAndJoin()
        done("D")
    }

    private suspend fun awaitSignal(signals: List<StreamSignal>, from: Int, match: (StreamSignal) -> Boolean): StreamSignal =
        withTimeout(60_000) {
            var hit: StreamSignal? = null
            while (hit == null) {
                hit = synchronized(signals) { signals.drop(from).firstOrNull(match) }
                if (hit == null) delay(100)
            }
            hit
        }

    private fun describe(s: StreamSignal) = when (s) {
        StreamSignal.Connected -> "Connected"
        is StreamSignal.Received -> "event ${s.event.kind} #${s.event.cursor}"
        is StreamSignal.ResetRequired -> "reset"
        is StreamSignal.Disconnected -> "disconnected (${s.reason}), retry ${s.retryInMillis}ms"
        is StreamSignal.Incompatible -> "Incompatible: ${s.error.describe()} retry ${s.retryInMillis}ms"
        is StreamSignal.Fatal -> "FATAL ${s.error.describe()}"
    }
}
