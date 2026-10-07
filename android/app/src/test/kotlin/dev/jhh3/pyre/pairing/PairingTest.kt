package dev.jhh3.pyre.pairing

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.data.HubCredentials
import dev.jhh3.pyre.data.InMemoryCredentialStore
import dev.jhh3.pyre.net.CertPins
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.testing.fixedClock
import java.net.URLEncoder
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import mockwebserver3.MockResponse
import mockwebserver3.MockWebServer
import okhttp3.tls.HandshakeCertificates
import okhttp3.tls.HeldCertificate
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PairingTest {
    private val pin = "ab".repeat(32)

    @Test
    fun parsesPairingLink() {
        val hub = URLEncoder.encode("https://hub.synthetic:8443", "UTF-8")
        val u = PairingUri.parse("pyre://pair?hub=$hub&pin=$pin&id=pair_synth-1&code=CODE-123").getOrThrow()
        assertEquals("https://hub.synthetic:8443", u.hub)
        assertEquals("pair_synth-1", u.pairingId)
        assertEquals("CODE-123", u.code)
        assertFalse("code must be redacted in toString", u.toString().contains("CODE-123"))
    }

    @Test
    fun rejectsBadLinks() {
        assertTrue(PairingUri.parse("https://hub/pair?code=1").isFailure)
        assertTrue(PairingUri.parse("pyre://session/ses_1").isFailure)
        assertTrue(PairingUri.parse("pyre://pair?hub=https%3A%2F%2Fh&id=x").isFailure)
        assertTrue(PairingUri.parse("pyre://pair?hub=https%3A%2F%2Fh&id=x&code=y&pin=nothex").isFailure)
    }

    @Test
    fun exchangesCodeOverPinnedTlsAndStoresToken() = runBlocking {
        val cert = HeldCertificate.Builder().commonName("synthetic-hub").build()
        val server = MockWebServer()
        server.useHttps(HandshakeCertificates.Builder().heldCertificate(cert).build().sslSocketFactory())
        server.enqueue(MockResponse.Builder().body("""{"client_id":"cli_synth-phone","token":"tok-synthetic"}""").build())
        server.enqueue(MockResponse.Builder().body("""{"protocol":1,"min_protocol":1,"hub_id":"hub_synth","hub_version":"0.1.0","server_time":"2026-10-04T12:00:00Z","principal":{"kind":"owner","id":"cli_synth-phone"},"mock":true,"features":[]}""").build())
        server.start()
        try {
            val store = InMemoryCredentialStore()
            val uri = PairingUri(server.url("/").toString().trimEnd('/'), CertPins.sha256Hex(cert.certificate), "pair_synth-1", "CODE-123")
            val out = PairingService(store, clock = fixedClock).pair(uri, "test phone", allowDevCleartext = false)
            assertTrue(out.toString(), out is PairingOutcome.Paired)
            out as PairingOutcome.Paired
            assertTrue(out.warning!!.contains("MOCK"))
            assertEquals("tok-synthetic", store.load()!!.token)
            assertTrue(store.load()!!.endpoint.pinned)
            assertFalse(store.load().toString().contains("tok-synthetic"))

            val ex = server.takeRequest()
            assertEquals("/v1/pair/exchange", ex.target)
            assertNull("pair exchange is unauthenticated", ex.headers["Authorization"])
            assertFalse("one-time code never in the URL", ex.url.toString().contains("CODE-123"))
            val body = PyreJson.wire.parseToJsonElement(ex.body!!.utf8()).jsonObject
            assertEquals("CODE-123", body["code"]!!.jsonPrimitive.content)
            assertEquals("android", body["kind"]!!.jsonPrimitive.content)
            assertEquals("Bearer tok-synthetic", server.takeRequest().headers["Authorization"])
        } finally {
            server.close()
        }
    }

    @Test
    fun rejectedCodeIsExplained() = runBlocking {
        val server = MockWebServer()
        server.enqueue(MockResponse.Builder().code(410).body("""{"error":{"code":"expired","message":"pairing expired","retryable":false}}""").build())
        server.start()
        try {
            val uri = PairingUri("http://localhost:${server.port}", "", "pair_x", "c")
            val out = PairingService(InMemoryCredentialStore(), clock = fixedClock).pair(uri, "p", allowDevCleartext = true)
            assertTrue(out is PairingOutcome.Failed && out.message.contains("expired"))
        } finally {
            server.close()
        }
    }

    @Test
    fun incompatibleHubIsRefusedAndTheExistingPairingKept() = runBlocking {
        val server = MockWebServer()
        server.enqueue(
            MockResponse.Builder().code(426).body(
                """{"error":{"code":"protocol_mismatch","message":"client protocol 1 not supported; hub supports 2..2","retryable":false,"details":{"min_protocol":2,"protocol":2}}}""",
            ).build(),
        )
        server.start()
        try {
            val prior = HubCredentials(HubEndpoint("https://hub.synthetic:7443", "ab".repeat(32)), "cli_synth-old", "tok-synthetic-old", "old", 1)
            val store = InMemoryCredentialStore(prior)
            val uri = PairingUri("http://localhost:${server.port}", "", "pair_x", "CODE-1")
            val out = PairingService(store, clock = fixedClock).pair(uri, "p", allowDevCleartext = true)
            assertTrue(out.toString(), out is PairingOutcome.Failed)
            val msg = (out as PairingOutcome.Failed).message
            assertTrue(msg, msg.contains("Protocol mismatch") && msg.contains("hub supports 2..2") && msg.contains("not used"))
            assertEquals("existing pairing untouched", prior, store.load())
            assertEquals("1", server.takeRequest().headers["X-Pyre-Protocol"])
            assertEquals("no hello after a refused exchange", 1, server.requestCount)
        } finally {
            server.close()
        }
    }

    @Test
    fun plainHttpRefusedWithoutDevOption() = runBlocking {
        val out = PairingService(InMemoryCredentialStore(), clock = fixedClock).pair(PairingUri("http://hub.example", "", "p", "c"), "x", allowDevCleartext = false)
        assertTrue(out is PairingOutcome.Failed && out.message.contains("HTTPS"))
    }
}
