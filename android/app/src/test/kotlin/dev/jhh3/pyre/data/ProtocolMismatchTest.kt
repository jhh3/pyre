package dev.jhh3.pyre.data

import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.testRepo
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * A16: a hub upgraded to an incompatible protocol, then rolled back. The
 * hub refuses every call before running it (protocol_mismatch), so queued
 * actions stay pending and replay once a compatible hub answers.
 */
class ProtocolMismatchTest {
    private fun mismatch() = PyreApiException(
        ErrorCodes.PROTOCOL_MISMATCH, "client protocol 1 not supported; hub supports 2..2", httpStatus = 426,
        details = buildJsonObject { put("min_protocol", JsonPrimitive(2)); put("protocol", JsonPrimitive(2)) },
    )

    private inner class Hub : FakePyreApi() {
        var incompatible = true
        val stored = mutableListOf<SendMessageRequest>()
        override suspend fun listNodes(req: ListNodesRequest): Page<Node> {
            if (incompatible) throw mismatch()
            return Page(emptyList())
        }
        override suspend fun sendMessage(req: SendMessageRequest): MessageView {
            record(Ops.SEND_MESSAGE, req)
            if (incompatible) throw mismatch()
            if (stored.none { it.messageId == req.messageId }) stored += req
            return MessageView(
                MessageEnvelope(messageId = req.messageId, operationId = req.operationId, recipient = req.recipient, mode = req.mode, body = req.body),
                DeliveryStatus(storage = StorageState.STORED, runtime = DeliveryState.QUEUED),
                cursor = 1,
            )
        }
    }

    @Test
    fun queuedActionsStayPendingAcrossAnIncompatibleHubAndReplayOnce() = runBlocking {
        val hub = Hub()
        val repo = testRepo(hub)
        val req = SendMessageRequest(operationId = "op_synth-1", messageId = "msg_synth-1", recipient = Address("session", "ses_synth-a"), mode = MessageMode.QUEUE, body = MessageBody("synthetic"))
        repo.outbox.enqueueSend(req)

        assertFalse(repo.outbox.flush(hub))
        val item = repo.outbox.items.value.single()
        assertEquals(OutboxItem.PENDING, item.state)
        assertTrue(item.lastError, item.lastError.contains("hub supports 2..2"))

        hub.incompatible = false
        assertTrue(repo.outbox.flush(hub))
        assertEquals(OutboxItem.DONE, repo.outbox.items.value.single().state)
        assertEquals(1, hub.stored.size)
        assertEquals(setOf("op_synth-1"), hub.calls.map { (it.second as SendMessageRequest).operationId }.toSet())
    }

    @Test
    fun healthShowsTheMismatchThenClearsWhenTheHubIsCompatibleAgain() = runBlocking {
        val hub = Hub()
        val repo = testRepo(hub)
        assertTrue(repo.nodes() is Load.Failed)
        val h = repo.health.state.value
        assertTrue(h.reachable)
        assertFalse("a protocol mismatch is not a revoked credential", h.unauthorized)
        assertTrue(h.protocolMismatch!!.contains("hub supports 2..2"))

        hub.incompatible = false
        assertTrue(repo.nodes() is Load.Ready)
        assertNull(repo.health.state.value.protocolMismatch)
        assertNotNull(repo.health.state.value.lastOkAt)
    }
}
