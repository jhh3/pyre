package dev.jhh3.pyre.net

import android.annotation.SuppressLint
import java.security.KeyStore
import java.security.MessageDigest
import java.security.cert.Certificate
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.util.Base64
import java.util.concurrent.TimeUnit
import javax.net.ssl.HostnameVerifier
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSession
import javax.net.ssl.TrustManagerFactory
import javax.net.ssl.X509TrustManager
import okhttp3.OkHttpClient
import okhttp3.internal.tls.OkHostnameVerifier

object CertPins {
    /**
     * Normalizes a SHA-256 certificate fingerprint to 64 lowercase hex chars.
     * Accepts `AB:CD:..`, `abcd..`, `sha256:<hex>`, and `sha256/<base64>`.
     */
    fun normalize(raw: String): String? {
        var s = raw.trim()
        if (s.startsWith("sha256/", ignoreCase = true)) {
            return try {
                val bytes = Base64.getDecoder().decode(s.substring(7))
                if (bytes.size == 32) hex(bytes) else null
            } catch (_: IllegalArgumentException) {
                null
            }
        }
        if (s.startsWith("sha256:", ignoreCase = true)) s = s.substring(7)
        s = s.replace(":", "").replace(" ", "").lowercase()
        return if (s.length == 64 && s.all { it in '0'..'9' || it in 'a'..'f' }) s else null
    }

    fun sha256Hex(cert: Certificate): String = hex(MessageDigest.getInstance("SHA-256").digest(cert.encoded))

    fun hex(b: ByteArray): String = b.joinToString("") { "%02x".format(it) }

    /** Constant-time comparison of two normalized pins. */
    fun matches(cert: Certificate, pinHex: String): Boolean =
        MessageDigest.isEqual(sha256Hex(cert).toByteArray(), pinHex.toByteArray())
}

/**
 * Pinned mode accepts exactly the certificate whose SHA-256 equals [pinHex]
 * as the leaf; nothing else (no CA fallback, so a CA-valid impostor is
 * rejected). CA mode (pin empty) delegates to the platform trust store.
 */
@SuppressLint("CustomX509TrustManager") // Deliberate: exact-leaf pinning; CA mode delegates to the platform.
class PinnedTrustManager(
    private val pinHex: String,
    private val system: X509TrustManager = systemTrustManager(),
) : X509TrustManager {
    override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {
        throw CertificateException("client certificates are not accepted")
    }

    override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
        if (chain.isNullOrEmpty()) throw CertificateException("empty certificate chain")
        if (pinHex.isEmpty()) {
            system.checkServerTrusted(chain, authType)
            return
        }
        if (!CertPins.matches(chain[0], pinHex)) {
            throw CertificateException("hub certificate does not match the paired SHA-256 fingerprint")
        }
    }

    override fun getAcceptedIssuers(): Array<X509Certificate> =
        if (pinHex.isEmpty()) system.acceptedIssuers else emptyArray()

    companion object {
        fun systemTrustManager(): X509TrustManager {
            val tmf = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm())
            tmf.init(null as KeyStore?)
            return tmf.trustManagers.filterIsInstance<X509TrustManager>().first()
        }
    }
}

/** In pinned mode the pin, not the DNS name, is the identity of the hub. */
class PinnedHostnameVerifier(private val pinHex: String) : HostnameVerifier {
    override fun verify(hostname: String, session: SSLSession): Boolean {
        if (pinHex.isEmpty()) return OkHostnameVerifier.verify(hostname, session)
        val leaf = try {
            session.peerCertificates.firstOrNull()
        } catch (_: Exception) {
            null
        } ?: return false
        return CertPins.matches(leaf, pinHex)
    }
}

object HttpClients {
    /** Shared base (connection pool, dispatcher). No logging interceptor by design. */
    val base: OkHttpClient by lazy {
        OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(30, TimeUnit.SECONDS)
            .writeTimeout(30, TimeUnit.SECONDS)
            .retryOnConnectionFailure(false) // retries are explicit, keyed by operation_id
            .build()
    }

    fun forEndpoint(endpoint: HubEndpoint, base: OkHttpClient = HttpClients.base): OkHttpClient {
        val b = base.newBuilder()
        if (!endpoint.devCleartext) {
            val tm = PinnedTrustManager(endpoint.certSha256)
            val ctx = SSLContext.getInstance("TLS")
            ctx.init(null, arrayOf(tm), null)
            b.sslSocketFactory(ctx.socketFactory, tm)
            b.hostnameVerifier(PinnedHostnameVerifier(endpoint.certSha256))
        }
        return b.build()
    }
}
