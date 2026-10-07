package dev.jhh3.pyre.notify

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.Fixtures
import dev.jhh3.pyre.testing.offline
import dev.jhh3.pyre.testing.testRepo
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class AttentionNotifierTest {
    private val b = Fixtures.bundle("09")
    private val item = b.attention.single().copy(notification = NotificationStatus(provider = "hub-stream", state = "sent"))

    private class Sink : NotificationSink {
        val posted = mutableListOf<NotificationSpec>()
        val cancelled = mutableListOf<String>()
        override fun post(spec: NotificationSpec) { posted += spec }
        override fun cancel(attentionId: String) { cancelled += attentionId }
    }

    private inner class Hub : FakePyreApi() {
        var approval = b.approvals.first { it.approvalId == "apr_synth-pending" }
        var items = listOf(item)
        var down = false
        override suspend fun getApproval(id: String): Approval { if (down) throw offline(); return approval }
        override suspend fun listAttention(req: ListAttentionRequest): Page<AttentionItem> { if (down) throw offline(); return Page(items) }
        override suspend fun getAttention(id: String): AttentionItem {
            record(Ops.GET_ATTENTION, id)
            if (down) throw offline()
            return items.firstOrNull { it.attentionId == id } ?: throw PyreApiException(ErrorCodes.NOT_FOUND, "no such item")
        }
    }

    private fun event(i: AttentionItem) = Event(
        eventId = "evt_x", cursor = 5, kind = EventKinds.ATTENTION_CHANGED, `object` = ObjectRef("attention", i.attentionId),
        payload = PyreJson.wire.encodeToJsonElement(AttentionItem.serializer(), i),
    )

    @Test
    fun sentItemPostsWithLiveApprovalActions() = runBlocking {
        val sink = Sink()
        val n = AttentionNotifier(testRepo(Hub()), sink)
        assertTrue(n.onEvent(event(item)))
        val spec = sink.posted.single()
        assertEquals(listOf("allow", "deny"), spec.actions.map { it.option.id })
        assertEquals("run_synth-apr", spec.actions.first().runId)
        assertEquals(if (item.sessionId.isNotEmpty()) "pyre://session/${item.sessionId}" else "pyre://approval/apr_synth-pending", spec.deepLink)
        // Same version again: no re-alert.
        assertTrue(!n.onEvent(event(item)))
        assertEquals(1, sink.posted.size)
    }

    @Test
    fun hubSuppressedItemIsNotAlerted() = runBlocking {
        val sink = Sink()
        val n = AttentionNotifier(testRepo(Hub()), sink)
        n.onEvent(event(item.copy(notification = NotificationStatus("hub-stream", "suppressed_quiet_hours"))))
        assertTrue(sink.posted.isEmpty())
    }

    @Test
    fun approvalResolvedElsewhereWithdrawsTheNotification() = runBlocking {
        val sink = Sink()
        val hub = Hub()
        val n = AttentionNotifier(testRepo(hub), sink)
        n.onEvent(event(item))
        hub.approval = hub.approval.copy(state = ApprovalState.RESOLVED)
        n.onEvent(event(item.copy(state = AttentionState.WITHDRAWN, updatedAt = "2026-10-04T12:01:00Z")))
        assertEquals(listOf(item.attentionId), sink.cancelled)
    }

    @Test
    fun staleApprovalNeverGetsControls() = runBlocking {
        val sink = Sink()
        val hub = Hub().apply { approval = approval.copy(state = ApprovalState.RESOLVED) }
        AttentionNotifier(testRepo(hub), sink).onEvent(event(item))
        assertTrue(sink.posted.isEmpty())
        assertEquals(listOf(item.attentionId), sink.cancelled)
    }

    @Test
    fun pushDismissCancelsAndNotifyFetchesCanonicalState() = runBlocking {
        val sink = Sink()
        val hub = Hub()
        val n = AttentionNotifier(testRepo(hub), sink)
        n.onPush(mapOf(PushKeys.ATTENTION_ID to item.attentionId, PushKeys.ACTION to PushKeys.ACTION_NOTIFY, PushKeys.TITLE to "Pyre: approval needed"))
        assertEquals(1, sink.posted.size)
        n.onPush(mapOf(PushKeys.ATTENTION_ID to item.attentionId, PushKeys.ACTION to PushKeys.ACTION_DISMISS))
        assertEquals(listOf(item.attentionId), sink.cancelled)
        assertEquals("uses attention.get, not a list scan", 1, hub.count(Ops.GET_ATTENTION))
        hub.items = emptyList()
        n.onPush(mapOf(PushKeys.ATTENTION_ID to item.attentionId, PushKeys.ACTION to PushKeys.ACTION_NOTIFY))
        assertEquals("deleted item -> cancel", 2, sink.cancelled.size)
    }

    @Test
    fun pushWhileHubUnreachableShowsGenericAlertWithoutControls() = runBlocking {
        val sink = Sink()
        val hub = Hub().apply { down = true }
        AttentionNotifier(testRepo(hub), sink).onPush(
            mapOf(PushKeys.ATTENTION_ID to "att_x", PushKeys.ACTION to PushKeys.ACTION_NOTIFY, PushKeys.TITLE to "Pyre: approval needed", PushKeys.KIND to "approval", PushKeys.DEEP_LINK to "pyre://approval/apr_x"),
        )
        val spec = sink.posted.single()
        assertEquals("Pyre: approval needed", spec.title)
        assertTrue(spec.actions.isEmpty())
    }

    @Test
    fun eventWithoutPayloadFetchesTheItem() = runBlocking {
        val sink = Sink()
        val hub = Hub()
        assertTrue(AttentionNotifier(testRepo(hub), sink).onEvent(event(item).copy(payload = null)))
        assertEquals(1, hub.count(Ops.GET_ATTENTION))
    }

    @Test
    fun snapshotReconcileCancelsItemsThatLeftTheInbox() = runBlocking {
        val sink = Sink()
        val hub = Hub()
        val n = AttentionNotifier(testRepo(hub), sink)
        n.refresh()
        assertEquals(1, sink.posted.size)
        hub.items = emptyList()
        n.refresh()
        assertEquals(listOf(item.attentionId), sink.cancelled)
    }
}
