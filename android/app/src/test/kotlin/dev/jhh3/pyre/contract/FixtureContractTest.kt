package dev.jhh3.pyre.contract

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.domain.Delivery
import dev.jhh3.pyre.domain.Status
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.testing.FIXED_NOW
import dev.jhh3.pyre.testing.FixtureExchange
import dev.jhh3.pyre.testing.Fixtures
import kotlinx.serialization.KSerializer
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Every shared contract fixture (tests/fixtures/contracts, every .json, read from
 * the repo at test time) must decode STRICTLY into the Kotlin models —
 * an unknown key fails, so the models track the Go contract — and every
 * exchange request must round-trip byte-for-byte (as JSON) through the
 * request models, proving we send what the hub expects.
 */
class FixtureContractTest {
    private val strict = PyreJson.strict

    private val requestSerializers: Map<String, KSerializer<*>> = mapOf(
        Ops.START_SESSION to StartSessionRequest.serializer(),
        Ops.SEND_MESSAGE to SendMessageRequest.serializer(),
        Ops.HELLO to HelloRequest.serializer(),
        Ops.ACQUIRE_LEASE to LeaseRequest.serializer(),
        Ops.PROPOSE_REVISION to ProposeRevisionRequest.serializer(),
        Ops.FINALIZE_UPLOAD to FinalizeUploadRequest.serializer(),
        Ops.ANSWER_APPROVAL to AnswerApprovalRequest.serializer(),
    )

    private val responseSerializers: Map<String, KSerializer<*>> = mapOf(
        Ops.START_SESSION to StartSessionResponse.serializer(),
        Ops.SEND_MESSAGE to MessageView.serializer(),
        Ops.HELLO to HelloResponse.serializer(),
        Ops.ACQUIRE_LEASE to InputLease.serializer(),
        Ops.PROPOSE_REVISION to DocumentWithRevision.serializer(),
        Ops.FINALIZE_UPLOAD to PublishReceipt.serializer(),
        Ops.ANSWER_APPROVAL to Approval.serializer(),
    )

    // Node-protocol exchanges are not part of the owner-client API.
    private val nonClientOps = setOf("node.connect")

    @Test
    fun everyFixtureDecodesStrictly() {
        val files = Fixtures.files()
        assertTrue("expected the 10 shared fixtures, found ${files.size}", files.size >= 10)
        for (f in files) {
            val b = try {
                strict.decodeFromString(dev.jhh3.pyre.testing.FixtureBundle.serializer(), f.readText())
            } catch (e: Exception) {
                throw AssertionError("${f.name}: ${e.message}", e)
            }
            assertEquals(f.name, 1, b.schemaVersion)
            assertTrue("${f.name} must be synthetic", b.synthetic)
            b.exchanges.forEach { checkExchange(f.name, it) }
        }
    }

    @Suppress("UNCHECKED_CAST")
    private fun checkExchange(file: String, ex: FixtureExchange) {
        val where = "$file / ${ex.name}"
        if (ex.op !in nonClientOps) {
            val reqSer = requestSerializers[ex.op] as KSerializer<Any>? ?: throw AssertionError("$where: no request model for ${ex.op}")
            val decoded = try { strict.decodeFromJsonElement(reqSer, ex.request) } catch (e: Exception) { throw AssertionError("$where request: ${e.message}", e) }
            val reencoded: JsonElement = PyreJson.wire.encodeToJsonElement(reqSer, decoded)
            assertEquals("$where: request must round-trip", ex.request, reencoded)
            ex.response?.let { resp ->
                val resSer = responseSerializers[ex.op] as KSerializer<Any>? ?: throw AssertionError("$where: no response model")
                try { strict.decodeFromJsonElement(resSer, resp) } catch (e: Exception) { throw AssertionError("$where response: ${e.message}", e) }
            }
        }
        ex.error?.let { assertTrue("$where: error code", it.code.isNotEmpty()) }
        assertTrue("$where: exactly one of response/error", (ex.response == null) != (ex.error == null))
    }

    @Test
    fun disconnectedNodeRunIsShownAsStaleUnknownNeverIdle() {
        val b = Fixtures.bundle("02")
        val view = b.sessions.single()
        val node = b.nodes.first { it.nodeId == view.run!!.nodeId }
        val s = Status.session(view, node, FIXED_NOW)
        assertEquals("Unknown (stale)", s.label)
        assertTrue(s.detail, s.detail.contains("node disconnected"))
        assertFalse(s.label.contains("idle", ignoreCase = true) || s.label.contains("Waiting"))
        assertEquals("Offline", Status.node(node, FIXED_NOW).label)
    }

    @Test
    fun deliveryFactsStayIndependent() {
        val msgs = Fixtures.bundle("03").messages.associateBy { it.envelope.messageId }
        val indet = Delivery.facts(msgs.getValue("msg_synth-indeterminate"))
        assertTrue(indet.needsReconciliation)
        assertTrue(indet.reissueDuplicateRisk)
        assertTrue(indet.canReissue)
        assertNotNull(indet.evidence)

        val accepted = Delivery.facts(msgs.getValue("msg_synth-accepted"))
        assertFalse(accepted.needsReconciliation)
        assertTrue(accepted.runtime.value.startsWith("accepted"))
        assertEquals("no acknowledgement", accepted.ack.value)

        val replied = Delivery.facts(msgs.getValue("msg_synth-replied"))
        assertTrue(replied.ack.value.startsWith("replied"))

        val pending = Delivery.facts(msgs.getValue("msg_synth-pending-local"))
        assertTrue(pending.storage.value.contains("not yet stored"))

        val rejected = Delivery.facts(msgs.getValue("msg_synth-rejected"))
        assertTrue(rejected.canReissue)
        assertFalse(rejected.reissueDuplicateRisk)

        assertTrue(Delivery.facts(msgs.getValue("msg_synth-queued")).canCancel)
    }

    @Test
    fun errorBodiesDecodeIntoApiError() {
        val dup = Fixtures.bundle("04").exchanges.first { it.error?.code == ErrorCodes.CONFLICT }
        assertEquals("operation", dup.error!!.ref!!.kind)
        val mismatch = Fixtures.bundle("04").exchanges.first { it.op == Ops.HELLO }
        assertEquals(ErrorCodes.PROTOCOL_MISMATCH, mismatch.error!!.code)
        val propose = Fixtures.bundle("06").exchanges.first { it.op == Ops.PROPOSE_REVISION }
        assertEquals("prp_synth-bob-1", (propose.error!!.details as JsonObject)["proposal_id"].toString().trim('"'))
    }

    @Test
    fun forwardCompatibleWithUnknownEnumValuesAndFields() {
        val json = """{"session":{"session_id":"ses_x","harness":"future-harness","project_id":null,"lineage":{},"archived":false,"new_field":1},
            "run":{"run_id":"run_x","session_id":"ses_x","state":"hibernating","freshness":{"observed_at":"2026-10-04T12:00:00Z","source":"hub","stale":false},"capabilities":null},
            "attention":0,"freshness":{"observed_at":"2026-10-04T12:00:00Z","source":"hub","stale":false}}"""
        val v = PyreJson.wire.decodeFromString(SessionView.serializer(), json)
        assertEquals("hibernating", v.run!!.state)
        assertTrue(v.run!!.capabilities.isEmpty())
        val s = Status.session(v, null, FIXED_NOW)
        assertTrue(s.detail.contains("unrecognized"))
    }
}
