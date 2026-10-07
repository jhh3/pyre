package dev.jhh3.pyre.integration

import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.data.InMemoryCredentialStore
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.domain.AnswerResult
import dev.jhh3.pyre.domain.ApprovalAnswerer
import dev.jhh3.pyre.files.ArtifactFiles
import dev.jhh3.pyre.files.HubArtifactContent
import dev.jhh3.pyre.files.Verified
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.EventStream
import dev.jhh3.pyre.net.HttpPyreApi
import dev.jhh3.pyre.net.HubClient
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.net.StreamSignal
import dev.jhh3.pyre.pairing.PairingOutcome
import dev.jhh3.pyre.pairing.PairingService
import dev.jhh3.pyre.pairing.PairingUri
import dev.jhh3.pyre.testing.testRepo
import java.io.File
import java.nio.file.Files
import kotlinx.coroutines.flow.take
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeoutOrNull
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Test

/**
 * End-to-end client run against `pyre-mockhub --tls` (MOCK BACKEND — not
 * runtime evidence). Skipped unless -Ppyre.mockhub.url/.token/.pin are set;
 * android/scripts/mockhub-it.sh starts the mock hub and runs this.
 *
 * Covers: clients.pair → pyre://pair link → /v1/pair/exchange over pinned
 * TLS → authenticated reads; approval double answer; message idempotency;
 * artifact download + digest check; event stream.
 */
class MockHubIntegrationTest {
    private val url = System.getProperty("pyre.mockhub.url")
    private val ownerToken = System.getProperty("pyre.mockhub.token")
    private val pin = System.getProperty("pyre.mockhub.pin")
    private val log = StringBuilder()

    private fun note(s: String) {
        log.appendLine(s)
        println("[mockhub-it] $s")
    }

    @Test
    fun fullClientFlowAgainstMockHub() = runBlocking {
        assumeTrue("pyre-mockhub not configured", url != null && ownerToken != null && pin != null)
        note("MOCK BACKEND — not runtime evidence. hub=$url")
        val ep = HubEndpoint.parse(url!!, pin, allowDevCleartext = false).getOrThrow()
        assertTrue("pinned TLS", ep.pinned)

        // 1. Owner mints a pairing ticket (stands in for `pyre pair --android`).
        val owner = HubClient(ep, { ownerToken })
        val ticket = owner.rpc(
            "clients.pair",
            PairingRequest(operationId = Ids.operation(), clientName = "android-it", kind = "android"),
            PairingRequest.serializer(), PairingTicket.serializer(), mutation = true,
        )
        val uri = PairingUri.parse(ticket.uri).getOrThrow()
        assertEquals(ep.certSha256, uri.pin.lowercase())
        note("clients.pair -> pyre://pair link parsed (pin matches banner)")

        // 2. Pair exactly as the app does.
        val store = InMemoryCredentialStore()
        val paired = PairingService(store).pair(uri, "android-it", allowDevCleartext = false)
        assertTrue(paired.toString(), paired is PairingOutcome.Paired)
        paired as PairingOutcome.Paired
        assertTrue("mock hub must say so", paired.hello!!.mock)
        assertFalse(store.load()!!.token == ownerToken)
        note("pair/exchange ok; client=${paired.credentials.clientId}; hello protocol ${paired.hello!!.minProtocol}..${paired.hello!!.protocol} mock=true")

        // Reusing the one-time code must fail.
        val again = PairingService(InMemoryCredentialStore()).pair(uri, "android-it-2", allowDevCleartext = false)
        assertTrue("one-time code", again is PairingOutcome.Failed)
        note("second exchange of the same code rejected: ${(again as PairingOutcome.Failed).message}")

        val client = HubClient(ep, { store.load()!!.token })
        val api = HttpPyreApi(client)
        val repo = testRepo(api)

        // 3. Reads.
        val nodes = (repo.nodes() as Load.Ready).value
        val sessions = (repo.sessions() as Load.Ready).value
        val attention = (repo.attention() as Load.Ready).value
        note("nodes=${nodes.size} sessions=${sessions.size} attention=${attention.size}")
        assertTrue(nodes.isNotEmpty() && sessions.isNotEmpty())
        attention.firstOrNull()?.let { a ->
            try {
                val one = api.getAttention(a.attentionId)
                assertEquals(a.attentionId, one.attentionId)
                note("attention.get ${a.attentionId} -> ${one.kind}/${one.state}")
            } catch (e: PyreApiException) {
                note("attention.get -> ${e.code} (mock hub may not implement v1.2)")
            }
        }
        sessions.firstOrNull { it.run != null }?.let { v ->
            val t = api.readSession(ReadSessionRequest(sessionId = v.session.sessionId, tail = true, limit = 20))
            note("sessions.read ${v.session.sessionId}: ${t.entries.size} entries, gap=${t.gap}")
        }

        // 4. Approval: answer once, second answer sees the existing resolution.
        val pending = api.listApprovals(ListApprovalsRequest(states = listOf(ApprovalState.PENDING))).items
        note("pending approvals=${pending.size}")
        pending.firstOrNull { a -> a.options.isNotEmpty() }?.let { a ->
            val answerer = ApprovalAnswerer(repo)
            val first = answerer.answer(a.approvalId, a.runId, a.options.first().id, "")
            assertTrue(first.toString(), first is AnswerResult.Answered)
            val second = answerer.answer(a.approvalId, a.runId, a.options.first().id, "")
            assertTrue(second.toString(), second is AnswerResult.NotPending)
            note("approval ${a.approvalId}: answered once; repeat -> ${(second as AnswerResult.NotPending).explanation}")
        }

        // 5. Message idempotency: same operation_id + message_id twice -> one stored message.
        val target = sessions.first { Caps.INPUT_QUEUE in it.run?.capabilities.orEmpty() }
        val req = SendMessageRequest(
            operationId = Ids.operation(), messageId = Ids.message(),
            recipient = Address("session", target.session.sessionId), mode = MessageMode.QUEUE,
            body = MessageBody("Synthetic Android IT message"),
        )
        val m1 = api.sendMessage(req)
        val m2 = api.sendMessage(req)
        assertEquals(m1.envelope.messageId, m2.envelope.messageId)
        assertEquals(m1.cursor, m2.cursor)
        note("messages.send x2 same op id -> one message ${m1.envelope.messageId} storage=${m1.status.storage} runtime=${m1.status.runtime}")
        try {
            api.sendMessage(req.copy(body = MessageBody("different body")))
            note("WARNING: reused operation_id with different body was accepted")
        } catch (e: PyreApiException) {
            assertEquals(ErrorCodes.CONFLICT, e.code)
            note("reused operation_id with different body -> conflict")
        }

        // 6. Artifact download + digest.
        val arts = api.listArtifacts(ListArtifactsRequest(limit = 50)).items
        note("artifacts=${arts.size}")
        val dir = Files.createTempDirectory("pyre-it").toFile()
        arts.firstOrNull()?.let { a ->
            val v = api.listVersions(ListVersionsRequest(artifactId = a.artifactId)).items.first()
            val r = ArtifactFiles.fetch(dir, v, HubArtifactContent(client))
            assertFalse("published bytes must match", r is Verified.Mismatch)
            note("downloaded ${v.versionId} (${v.mediaType}) -> ${r.javaClass.simpleName}")
        }
        dir.deleteRecursively()

        // 7. Event stream over pinned TLS.
        val signals = withTimeoutOrNull(8_000) { EventStream(client).signals { 0 }.take(2).toList() }
        note("event stream: ${signals?.map { it.javaClass.simpleName } ?: "timeout"}")
        assertTrue(signals?.firstOrNull() == StreamSignal.Connected)

        // 8. Terminal attach descriptor (mechanism only; ticket never logged).
        try {
            val d = api.attach(AttachRequest(operationId = Ids.operation(), sessionId = target.session.sessionId, preferred = listOf(AttachMechanism.WEB_TERMINAL, AttachMechanism.SSH_TMUX)))
            note("sessions.attach -> ${d.mechanism}")
        } catch (e: PyreApiException) {
            note("sessions.attach -> ${e.code}: ${e.message}")
        }

        File(System.getProperty("pyre.mockhub.evidence") ?: "build/mockhub-it.txt").apply {
            parentFile?.mkdirs()
            writeText(log.toString())
        }
        Unit
    }
}
