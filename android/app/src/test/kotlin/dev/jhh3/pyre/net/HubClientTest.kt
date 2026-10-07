package dev.jhh3.pyre.net

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.testing.Fixtures
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import mockwebserver3.MockResponse
import mockwebserver3.MockWebServer
import mockwebserver3.SocketEffect
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test

class HubClientTest {
    private lateinit var server: MockWebServer
    private val token = "synthetic-token-DO-NOT-LOG"

    @Before fun setUp() { server = MockWebServer().also { it.start() } }
    @After fun tearDown() { runCatching { server.close() } }

    private fun api(maxAttempts: Int = 3): HttpPyreApi {
        val ep = HubEndpoint(server.url("/").toString().trimEnd('/'), devCleartext = true)
        return HttpPyreApi(HubClient(ep, { token }, maxAttempts = maxAttempts, backoffMillis = { 1 }))
    }

    private fun json(body: String, code: Int = 200) = MockResponse.Builder().code(code).setHeader("Content-Type", "application/json").body(body).build()

    private val answerReq = AnswerApprovalRequest(operationId = "op_synth-apr-1", approvalId = "apr_synth-pending", runId = "run_synth-apr", optionId = "allow")

    private fun exchange(prefix: String, name: String) = Fixtures.bundle(prefix).exchanges.first { it.name == name }

    @Test
    fun rpcBindingHeadersAndBody() = runBlocking {
        server.enqueue(json(exchange("09", "answer pending").response.toString()))
        val r = api().answerApproval(answerReq)
        assertEquals(ApprovalState.RESOLVED, r.state)
        val rec = server.takeRequest()
        assertEquals("POST", rec.method)
        assertEquals("/v1/approvals.answer", rec.target)
        assertEquals("Bearer $token", rec.headers["Authorization"])
        assertEquals("1", rec.headers["X-Pyre-Protocol"])
        assertFalse("token must never appear in the URL", rec.url.toString().contains(token))
        val body = PyreJson.wire.parseToJsonElement(rec.body!!.utf8())
        assertEquals(exchange("09", "answer pending").request, body)
    }

    @Test
    fun conflictErrorBodyIsDecodedAndNotRetried() = runBlocking {
        val err = exchange("09", "stale link to natively resolved approval").error!!
        server.enqueue(json(PyreJson.wire.encodeToString(ErrorBody.serializer(), ErrorBody(err)), 409))
        try {
            api().answerApproval(answerReq.copy(approvalId = "apr_synth-resolved", operationId = "op_synth-apr-2"))
            fail("expected conflict")
        } catch (e: PyreApiException) {
            assertEquals(ErrorCodes.CONFLICT, e.code)
            assertEquals(409, e.httpStatus)
            assertEquals("apr_synth-resolved", e.ref!!.id)
            assertFalse(e.retryable)
        }
        assertEquals(1, server.requestCount)
    }

    @Test
    fun protocolMismatchIsSurfacedAndNotRetried() = runBlocking {
        val err = exchange("04", "protocol mismatch").error!!
        server.enqueue(json(PyreJson.wire.encodeToString(ErrorBody.serializer(), ErrorBody(err)), 426))
        try {
            api().hello()
            fail("expected mismatch")
        } catch (e: PyreApiException) {
            assertTrue(e.isProtocolMismatch)
            assertEquals("1", e.detail("protocol"))
            assertTrue(e.describe().contains("Protocol mismatch"))
        }
        assertEquals(1, server.requestCount)
    }

    @Test
    fun retryableUnavailableIsRetriedWithTheSameOperationId() = runBlocking {
        server.enqueue(json("""{"error":{"code":"unavailable","message":"hub restarting","retryable":true}}""", 503))
        server.enqueue(MockResponse.Builder().onResponseStart(SocketEffect.CloseSocket()).build())
        server.enqueue(json(exchange("09", "double tap returns existing result").response.toString()))
        val r = api().answerApproval(answerReq)
        assertEquals("pyre", r.resolution!!.source)
        assertEquals(3, server.requestCount)
        val ops = (1..3).map { PyreJson.wire.parseToJsonElement(server.takeRequest().body!!.utf8()).jsonObject["operation_id"]!!.jsonPrimitive.content }
        assertEquals(listOf("op_synth-apr-1", "op_synth-apr-1", "op_synth-apr-1"), ops)
    }

    @Test
    fun outcomeIndeterminateRetriesMutationsOnly() = runBlocking {
        server.enqueue(json("""{"error":{"code":"outcome_indeterminate","message":"node timeout","retryable":false}}""", 504))
        server.enqueue(json(exchange("09", "answer pending").response.toString()))
        api().answerApproval(answerReq)
        assertEquals(2, server.requestCount)

        server.enqueue(json("""{"error":{"code":"outcome_indeterminate","message":"x","retryable":false}}""", 504))
        try { api().getApproval("apr_x"); fail() } catch (e: PyreApiException) { assertEquals(ErrorCodes.OUTCOME_INDETERMINATE, e.code) }
        assertEquals(3, server.requestCount)
    }

    @Test
    fun transportFailureAfterRetriesIsReportedAsOffline() = runBlocking {
        val ep = HubEndpoint(server.url("/").toString().trimEnd('/'), devCleartext = true)
        // Simulate an unreachable hub without sending bytes to a freed port
        // (an ephemeral port may be reused by another local process).
        var attempts = 0
        val http = HttpClients.forEndpoint(ep).newBuilder().addInterceptor { attempts++; throw java.io.IOException("synthetic: network down") }.build()
        val api = HttpPyreApi(HubClient(ep, { token }, http = http, maxAttempts = 3, backoffMillis = { 1 }))
        try { api.listSessions(); fail() } catch (e: PyreApiException) {
            assertTrue(e.transport)
            assertEquals(ErrorCodes.UNAVAILABLE, e.code)
            assertFalse(e.message.contains(token))
        }
        assertEquals("bounded retries", 3, attempts)
    }

    @Test
    fun unauthorizedWithoutBodyMapsFromStatus() = runBlocking {
        server.enqueue(MockResponse.Builder().code(401).build())
        try { api().listNodes(); fail() } catch (e: PyreApiException) { assertTrue(e.isUnauthorized) }
    }

    @Test
    fun emptyRequestEncodesAsEmptyObject() = runBlocking {
        server.enqueue(json("{}"))
        val p = api().getNotificationPrefs()
        assertEquals("", p.quietHoursStart)
        assertEquals("{}", server.takeRequest().body!!.utf8())
    }

    @Test
    fun omitemptyIsRespected() {
        val req = SendMessageRequest(operationId = "op_1", recipient = Address("session", "ses_1"), body = MessageBody("hi"))
        val o = PyreJson.wire.encodeToJsonElement(SendMessageRequest.serializer(), req) as JsonObject
        assertEquals(setOf("operation_id", "recipient", "body"), o.keys)
        val reissue = PyreJson.wire.encodeToJsonElement(ReissueMessageRequest.serializer(), ReissueMessageRequest(operationId = "op", messageId = "msg", acceptDuplicateRisk = true)) as JsonObject
        assertEquals("true", reissue["accept_duplicate_risk"].toString())
        assertNull(reissue["body"])
    }
}
