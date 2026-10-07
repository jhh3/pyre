package dev.jhh3.pyre.domain

import dev.jhh3.pyre.model.Address
import dev.jhh3.pyre.model.Attachment
import dev.jhh3.pyre.model.DeliveryState
import dev.jhh3.pyre.model.DeliveryStatus
import dev.jhh3.pyre.model.MessageBody
import dev.jhh3.pyre.model.MessageEnvelope
import dev.jhh3.pyre.model.MessageView
import dev.jhh3.pyre.model.Principal
import dev.jhh3.pyre.model.SendMessageRequest
import dev.jhh3.pyre.model.StorageState
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.offline
import dev.jhh3.pyre.testing.testRepo
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * A04 on Android (found on a Pixel 10 Pro): message cards named the sender
 * only by kind ("session") and showed no recipient, thread, reply reference
 * or attached artifact, so the phone could not be matched with the CLI/TUI.
 */
class IdentityAndHealthTest {
    private val shell = "ses_synth-shell"
    private val mock = "ses_synth-mock"
    private val titles = mapOf(shell to "synthetic-shell-peer", mock to "synthetic-mock-peer")

    private val reply = MessageEnvelope(
        messageId = "msg_synth-reply",
        sender = Principal("session", mock),
        recipient = Address("session", shell),
        threadId = "thr-synth",
        inReplyTo = "msg_synth-request",
        body = MessageBody("synthetic reply"),
        attachments = listOf(Attachment(artifactVersionId = "ver_synth-notes-v2")),
    )

    @Test
    fun namesPeersLikeTheCliAndTui() {
        assertEquals("synthetic-mock-peer (session:$mock)", Identity.principal(reply.sender, titles))
        assertEquals("session:$mock", Identity.principal(reply.sender))
        assertEquals("synthetic-shell-peer (session:$shell)", Identity.address(reply.recipient, titles))
        assertEquals("owner (Android synthetic)", Identity.principal(Principal("owner", "own_x", "Android synthetic")))
        assertEquals("owner inbox", Identity.address(Address("owner", "own_x")))
        assertEquals("thread thr-synth · reply to msg_synth-request", Identity.threadLine(reply))
        assertTrue(Identity.sentBy(reply, mock))
        assertFalse(Identity.sentBy(reply, shell))
    }

    @Test
    fun artifactAttachmentOpensTheExactVersion() {
        val (label, link) = Identity.attachment(reply.attachments.single())
        assertEquals("artifact version ver_synth-notes-v2", label)
        assertEquals("pyre://artifact_version/ver_synth-notes-v2", link)
        assertNull(Identity.attachment(Attachment(documentRevisionId = "rev_synth")).second)
    }

    /** Found on the phone: the "Hub unreachable" banner stayed after the outbox replayed. */
    @Test
    fun successfulOutboxReplayClearsUnreachable() = runTest {
        val hub = object : FakePyreApi() {
            var online = false
            override suspend fun sendMessage(req: SendMessageRequest): MessageView {
                if (!online) throw offline()
                return MessageView(MessageEnvelope(messageId = req.messageId, recipient = req.recipient, body = req.body), DeliveryStatus(storage = StorageState.STORED, runtime = DeliveryState.QUEUED))
            }
        }
        val repo = testRepo(hub)
        repo.outbox.enqueueSend(SendMessageRequest(operationId = "op_synth", messageId = "msg_synth-off", recipient = Address("session", shell), body = MessageBody("x")))
        assertFalse(repo.flushOutbox())
        repo.health.failed(offline())
        assertFalse(repo.health.state.value.reachable)
        hub.online = true
        assertTrue(repo.flushOutbox())
        assertTrue(repo.health.state.value.reachable)
        // An empty outbox proves nothing about the hub.
        repo.health.failed(offline())
        assertTrue(repo.flushOutbox())
        assertFalse(repo.health.state.value.reachable)
    }
}
