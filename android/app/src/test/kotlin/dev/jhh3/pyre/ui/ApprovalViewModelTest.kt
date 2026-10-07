package dev.jhh3.pyre.ui

import dev.jhh3.pyre.data.InMemoryBlobStore
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.Fixtures
import dev.jhh3.pyre.testing.offline
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.approvals.ApprovalViewModel
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
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

/**
 * Approval semantics from fixture 09: answer by (approval_id, run_id), one
 * stable operation_id per answer intent, stale links show the existing
 * resolution, replaced runs never receive an answer.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class ApprovalViewModelTest {
    private val dispatcher = StandardTestDispatcher()
    private val approvals = Fixtures.bundle("09").approvals.associateBy { it.approvalId }
    private val answered = Fixtures.bundle("09").exchanges.first { it.name == "answer pending" }

    @Before fun setUp() { Dispatchers.setMain(dispatcher) }
    @After fun tearDown() { Dispatchers.resetMain() }

    /** Hub fake with idempotency by operation_id, like the real hub. */
    private open inner class Hub : FakePyreApi() {
        var current = approvals.getValue("apr_synth-pending")
        val answers = mutableListOf<AnswerApprovalRequest>()
        val committedOps = mutableSetOf<String>()
        var failNextAnswerWithTransport = false
        override suspend fun getApproval(id: String): Approval { record(Ops.GET_APPROVAL, id); return if (id == current.approvalId) current else approvals.getValue(id) }
        override suspend fun answerApproval(req: AnswerApprovalRequest): Approval {
            answers += req
            if (failNextAnswerWithTransport) {
                failNextAnswerWithTransport = false
                committedOps += req.operationId // committed, but the response was lost
                throw offline()
            }
            val a = approvals.getValue(req.approvalId)
            if (req.operationId in committedOps || current.state == ApprovalState.PENDING) {
                committedOps += req.operationId
                current = dev.jhh3.pyre.core.PyreJson.wire.decodeFromJsonElement(Approval.serializer(), answered.response!!)
                return current
            }
            throw PyreApiException(ErrorCodes.CONFLICT, "approval already resolved", ref = ObjectRef("approval", a.approvalId))
        }
    }

    @Test
    fun doubleTapAnswersOnceWithOneOperationId() = runTest(dispatcher) {
        val hub = Hub()
        val vm = ApprovalViewModel(testRepo(hub), "apr_synth-pending")
        advanceUntilIdle()
        assertTrue(vm.ui.value.answerable)
        vm.answer("allow")
        vm.answer("allow") // second tap while in flight
        vm.answer("deny")
        advanceUntilIdle()
        assertEquals(1, hub.answers.size)
        assertEquals("run_synth-apr", hub.answers.single().runId)
        assertEquals("allow", hub.answers.single().optionId)
        assertEquals(Tone.OK, vm.ui.value.resultTone)
        assertFalse("resolved request has no controls", vm.ui.value.answerable)
    }

    @Test
    fun staleLinkShowsExistingNativeResolutionAndSendsNothing() = runTest(dispatcher) {
        val hub = Hub()
        val vm = ApprovalViewModel(testRepo(hub), "apr_synth-resolved")
        advanceUntilIdle()
        assertFalse(vm.ui.value.answerable)
        vm.answer("deny")
        advanceUntilIdle()
        assertEquals(0, hub.answers.size)
        assertTrue(vm.ui.value.result!!.contains("native terminal"))
    }

    @Test
    fun resolvedElsewhereBetweenOpenAndTapShowsCurrentState() = runTest(dispatcher) {
        val hub = Hub()
        val vm = ApprovalViewModel(testRepo(hub), "apr_synth-pending")
        advanceUntilIdle()
        // The terminal answers first.
        hub.current = approvals.getValue("apr_synth-pending").copy(
            state = ApprovalState.RESOLVED,
            resolution = ApprovalResolution(optionId = "deny", source = "native", effect = "effective"),
        )
        vm.answer("allow")
        advanceUntilIdle()
        assertEquals("re-validated before answering; nothing sent", 0, hub.answers.size)
        assertTrue(vm.ui.value.result!!, vm.ui.value.result!!.contains("Deny") && vm.ui.value.result!!.contains("native"))
        assertFalse(vm.ui.value.answerable)
    }

    @Test
    fun replacedRunIsNeverAnswered() = runTest(dispatcher) {
        val hub = Hub()
        val vm = ApprovalViewModel(testRepo(hub), "apr_synth-expired")
        advanceUntilIdle()
        vm.answer("", "main")
        advanceUntilIdle()
        assertEquals(0, hub.answers.size)
        assertTrue(vm.ui.value.result!!.contains("Withdrawn"))
    }

    @Test
    fun outcomeUnknownRetryReusesOperationId() = runTest(dispatcher) {
        val hub = Hub().apply { failNextAnswerWithTransport = true }
        val blobs = InMemoryBlobStore()
        val vm = ApprovalViewModel(testRepo(hub, blobs), "apr_synth-pending")
        advanceUntilIdle()
        vm.answer("allow")
        advanceUntilIdle()
        // HubClient-level retries are below this layer; here the whole call failed.
        assertTrue(vm.ui.value.canRetry)
        assertEquals(Tone.ATTENTION, vm.ui.value.resultTone)
        // Simulate app restart: a new VM over the same persisted intent ids.
        val vm2 = ApprovalViewModel(testRepo(hub, blobs), "apr_synth-pending")
        advanceUntilIdle()
        // Hub has committed; the refreshed approval may still read pending in a racy hub, so force pending.
        hub.current = approvals.getValue("apr_synth-pending")
        vm2.answer("allow")
        advanceUntilIdle()
        assertEquals(2, hub.answers.size)
        assertEquals("same operation_id on retry", hub.answers[0].operationId, hub.answers[1].operationId)
        assertTrue(vm2.ui.value.approval is Load.Ready)
        assertEquals(ApprovalState.RESOLVED, vm2.ui.value.current!!.state)
    }

    @Test
    fun offlineBeforeValidationSendsNothing() = runTest(dispatcher) {
        val hub = object : Hub() {
            var down = false
            override suspend fun getApproval(id: String): Approval = if (down) throw offline() else super.getApproval(id)
        }
        val vm = ApprovalViewModel(testRepo(hub), "apr_synth-pending")
        advanceUntilIdle()
        hub.down = true
        vm.answer("allow")
        advanceUntilIdle()
        assertEquals(0, hub.answers.size)
        assertTrue(vm.ui.value.result!!.startsWith("Not sent"))
    }
}
