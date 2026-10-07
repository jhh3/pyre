package dev.jhh3.pyre.ui

import dev.jhh3.pyre.data.InMemoryBlobStore
import dev.jhh3.pyre.data.OutboxItem
import dev.jhh3.pyre.data.valueOrNull
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.Fixtures
import dev.jhh3.pyre.testing.offline
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.session.SessionDetailViewModel
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class SessionDetailViewModelTest {
    private val dispatcher = StandardTestDispatcher()
    private val bundle = Fixtures.bundle("03")
    private val worker = bundle.sessions.first { it.session.sessionId == "ses_synth-b" }

    @Before fun setUp() { Dispatchers.setMain(dispatcher) }
    @After fun tearDown() { Dispatchers.resetMain() }

    private open inner class Hub : FakePyreApi() {
        var online = true
        val stored = bundle.messages.filter { it.envelope.recipient.id == "ses_synth-b" && it.status.storage == StorageState.STORED }.toMutableList()
        val sends = mutableListOf<SendMessageRequest>()
        val reissues = mutableListOf<ReissueMessageRequest>()
        val resolves = mutableListOf<ResolveDeliveryRequest>()
        override suspend fun getSession(id: String) = worker
        override suspend fun listMessages(req: ListMessagesRequest) = Page(stored.toList())
        override suspend fun sendMessage(req: SendMessageRequest): MessageView {
            sends += req
            if (!online) throw offline()
            stored.firstOrNull { it.envelope.messageId == req.messageId }?.let { return it }
            val mv = MessageView(
                MessageEnvelope(messageId = req.messageId, operationId = req.operationId, recipient = req.recipient, mode = req.mode, body = req.body),
                DeliveryStatus(storage = StorageState.STORED, runtime = DeliveryState.QUEUED),
                cursor = 100L + stored.size,
            )
            stored += mv
            return mv
        }
        override suspend fun reissueMessage(req: ReissueMessageRequest): MessageView {
            reissues += req
            val orig = stored.first { it.envelope.messageId == req.messageId }
            return orig.copy(envelope = orig.envelope.copy(messageId = "msg_synth-reissued", reissues = req.messageId), status = DeliveryStatus(storage = StorageState.STORED))
        }
        override suspend fun resolveDelivery(req: ResolveDeliveryRequest): MessageView {
            resolves += req
            val i = stored.indexOfFirst { it.envelope.messageId == req.messageId }
            stored[i] = stored[i].copy(status = stored[i].status.copy(runtime = req.resolution))
            return stored[i]
        }
    }

    @Test
    fun indeterminateDeliveryRequiresExplicitRiskAcceptanceToReissue() = runTest(dispatcher) {
        val hub = Hub()
        val vm = SessionDetailViewModel(testRepo(hub), "ses_synth-b")
        advanceUntilIdle()
        val row = vm.ui.value.messages.first { it.view.envelope.messageId == "msg_synth-indeterminate" }
        assertEquals(DeliveryState.INDETERMINATE, row.view.status.runtime)

        vm.openReconcile("msg_synth-indeterminate")
        vm.reissue("msg_synth-indeterminate")
        advanceUntilIdle()
        assertTrue("no reissue without accepting duplication risk", hub.reissues.isEmpty())
        assertNotNull(vm.ui.value.notice)

        vm.setAcceptRisk(true)
        vm.reissue("msg_synth-indeterminate")
        vm.reissue("msg_synth-indeterminate")
        advanceUntilIdle()
        assertTrue(hub.reissues.all { it.acceptDuplicateRisk })
        assertEquals("double tap reuses one operation id", 1, hub.reissues.map { it.operationId }.toSet().size)
        assertEquals(null, vm.ui.value.reconcile)
    }

    @Test
    fun resolveRecordsHumanDecisionWithoutResending() = runTest(dispatcher) {
        val hub = Hub()
        val vm = SessionDetailViewModel(testRepo(hub), "ses_synth-b")
        advanceUntilIdle()
        vm.openReconcile("msg_synth-indeterminate")
        vm.resolve("msg_synth-indeterminate", DeliveryState.ACCEPTED)
        advanceUntilIdle()
        assertEquals(DeliveryState.ACCEPTED, hub.resolves.single().resolution)
        assertTrue(hub.sends.isEmpty())
        val row = vm.ui.value.messages.first { it.view.envelope.messageId == "msg_synth-indeterminate" }
        assertEquals(DeliveryState.ACCEPTED, row.view.status.runtime)
    }

    @Test
    fun offlineSendStaysPendingAndReconcilesWithoutDuplicate() = runTest(dispatcher) {
        val hub = Hub()
        val blobs = InMemoryBlobStore()
        val repo = testRepo(hub, blobs)
        val vm = SessionDetailViewModel(repo, "ses_synth-b")
        advanceUntilIdle()
        hub.online = false
        vm.setDraft("Synthetic: run the linter")
        vm.send()
        advanceUntilIdle()
        val pending = vm.ui.value.messages.single { it.localOnly }
        assertEquals(StorageState.PENDING_LOCAL, pending.view.status.storage)
        assertTrue(vm.ui.value.notice!!.contains("pending"))

        hub.online = true
        repo.outbox.flush(hub)
        repo.outbox.flush(hub) // reconnect callback + screen refresh both flush
        advanceUntilIdle()
        assertEquals("first offline attempt + one successful replay", 2, hub.sends.size)
        assertEquals(1, hub.sends.map { it.messageId }.toSet().size)
        assertEquals(1, hub.sends.map { it.operationId }.toSet().size)
        assertEquals(1, hub.stored.count { it.envelope.body.text == "Synthetic: run the linter" })
        vm.refresh()
        advanceUntilIdle()
        assertFalse(vm.ui.value.messages.any { it.localOnly })
        assertEquals(OutboxItem.DONE, repo.outbox.items.value.single().state)
    }

    /** Found on the emulator: with the screen open and no hub stream, a queued reply never replayed after the hub returned. */
    @Test
    fun pollReplaysTheOutboxOnceAndClearsThePendingNotice() = runTest(dispatcher) {
        val hub = Hub()
        val repo = testRepo(hub, InMemoryBlobStore())
        val vm = SessionDetailViewModel(repo, "ses_synth-b")
        advanceUntilIdle()
        hub.online = false
        vm.setDraft("Synthetic: queued while offline")
        vm.send()
        advanceUntilIdle()
        assertTrue(vm.ui.value.notice!!.contains("pending"))
        vm.poll() // still offline: stays pending, no duplicate
        advanceUntilIdle()
        assertTrue(vm.ui.value.messages.any { it.localOnly })
        hub.online = true
        vm.poll()
        advanceUntilIdle()
        vm.poll()
        advanceUntilIdle()
        assertEquals(1, hub.stored.count { it.envelope.body.text == "Synthetic: queued while offline" })
        assertEquals(1, hub.sends.map { it.messageId }.toSet().size)
        assertFalse(vm.ui.value.messages.any { it.localOnly })
        assertEquals(null, vm.ui.value.notice)
        assertTrue(repo.health.state.value.reachable)
    }

    @Test
    fun renameSendsTitleOnceAndRefusesBlank() = runTest(dispatcher) {
        val updates = mutableListOf<UpdateSessionRequest>()
        val hub = object : Hub() {
            var current = worker
            override suspend fun getSession(id: String) = current
            override suspend fun updateSession(req: UpdateSessionRequest): SessionView {
                updates += req
                current = worker.copy(session = worker.session.copy(title = req.title!!))
                return current
            }
        }
        val vm = SessionDetailViewModel(testRepo(hub), "ses_synth-b")
        advanceUntilIdle()
        assertFalse(vm.rename("  "))
        assertTrue(vm.ui.value.notice!!.contains("blank"))
        assertTrue(vm.rename("  Synthetic rename "))
        advanceUntilIdle()
        assertEquals(1, updates.size)
        assertEquals("ses_synth-b", updates[0].sessionId)
        assertEquals("Synthetic rename", updates[0].title)
        assertTrue(updates[0].operationId.isNotEmpty())
        assertEquals("Synthetic rename", vm.ui.value.session.valueOrNull()?.session?.title)
    }

    @Test
    fun steeringIsGatedOnCapability() = runTest(dispatcher) {
        val hub = Hub()
        val vm = SessionDetailViewModel(testRepo(hub), "ses_synth-b")
        advanceUntilIdle()
        assertTrue(vm.ui.value.canQueue)
        assertFalse("fixture claude run lacks input.steer", vm.ui.value.canSteer)
        vm.setDraft("steer!")
        vm.send(steer = true)
        advanceUntilIdle()
        assertTrue(hub.sends.isEmpty())
        assertTrue(vm.ui.value.notice!!.contains("input.steer"))
    }
}
