package dev.jhh3.pyre.net

import kotlinx.coroutines.runBlocking
import mockwebserver3.MockResponse
import mockwebserver3.MockWebServer
import okhttp3.tls.HandshakeCertificates
import okhttp3.tls.HeldCertificate
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test
import java.util.Base64

/** TLS trust: exact leaf pin for self-signed hubs; CA mode otherwise; https enforced. */
class PinningTest {
    private lateinit var server: MockWebServer
    // Deliberately NOT named "localhost": in pinned mode the pin is the identity.
    private val hubCert = HeldCertificate.Builder().commonName("synthetic-hub").addSubjectAlternativeName("synthetic-hub.invalid").build()
    private val otherCert = HeldCertificate.Builder().commonName("impostor").addSubjectAlternativeName("localhost").build()

    @Before fun setUp() {
        server = MockWebServer()
        val hs = HandshakeCertificates.Builder().heldCertificate(hubCert).build()
        server.useHttps(hs.sslSocketFactory())
        server.start()
    }

    @After fun tearDown() { server.close() }

    private fun pinOf(c: HeldCertificate) = CertPins.sha256Hex(c.certificate)

    private fun api(pin: String) = HttpPyreApi(
        HubClient(HubEndpoint(server.url("/").toString().trimEnd('/'), pin), { "tok" }, maxAttempts = 1),
    )

    @Test
    fun acceptsExactlyThePinnedSelfSignedCertificate() = runBlocking {
        server.enqueue(MockResponse.Builder().body("""{"items":[],"snapshot_at":"2026-10-04T12:00:00Z"}""").build())
        val page = api(pinOf(hubCert)).listSessions()
        assertTrue(page.items.isEmpty())
    }

    @Test
    fun rejectsAWrongPin() = runBlocking {
        server.enqueue(MockResponse.Builder().body("{}").build())
        try {
            api(pinOf(otherCert)).listSessions()
            fail("must reject")
        } catch (e: PyreApiException) {
            assertTrue(e.transport)
        }
        assertEquals(0, server.requestCount)
    }

    @Test
    fun caModeRejectsSelfSignedCertificate() = runBlocking {
        server.enqueue(MockResponse.Builder().body("{}").build())
        try {
            api("").listSessions()
            fail("self-signed must not pass CA validation")
        } catch (e: PyreApiException) {
            assertTrue(e.transport)
        }
    }

    @Test
    fun pinNormalization() {
        val hex = pinOf(hubCert)
        val colon = hex.uppercase().chunked(2).joinToString(":")
        assertEquals(hex, CertPins.normalize(colon))
        assertEquals(hex, CertPins.normalize("sha256:$hex"))
        val b64 = Base64.getEncoder().encodeToString(hex.chunked(2).map { it.toInt(16).toByte() }.toByteArray())
        assertEquals(hex, CertPins.normalize("sha256/$b64"))
        assertNull(CertPins.normalize("not-a-pin"))
        assertNull(CertPins.normalize("abcd"))
    }

    @Test
    fun endpointRequiresHttpsExceptLabelledLoopbackDevMode() {
        assertTrue(HubEndpoint.parse("http://hub.example:8443", null, allowDevCleartext = false).isFailure)
        assertTrue(HubEndpoint.parse("http://hub.example:8443", null, allowDevCleartext = true).isFailure)
        assertTrue(HubEndpoint.parse("http://10.0.2.2:8443", null, allowDevCleartext = false).isFailure)
        val dev = HubEndpoint.parse("http://10.0.2.2:8443/", null, allowDevCleartext = true).getOrThrow()
        assertTrue(dev.devCleartext)
        assertTrue(HubEndpoint.parse("https://user:pw@hub.example", null, false).isFailure)
        assertTrue(HubEndpoint.parse("https://hub.example/?token=x", null, false).isFailure)
        assertTrue(HubEndpoint.parse("https://hub.example", "zz", false).isFailure)
        val ok = HubEndpoint.parse("https://hub.example:8443/pyre/", pinOf(hubCert), false).getOrThrow()
        assertEquals("https://hub.example:8443/pyre", ok.baseUrl)
        assertEquals("https://hub.example:8443/pyre/v1/sessions.list", ok.resolve("/v1/sessions.list").toString())
    }
}
