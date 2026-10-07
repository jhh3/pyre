package dev.jhh3.pyre.net

import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull

/**
 * A validated hub address plus its TLS trust mode.
 *
 * - [certSha256] non-empty: *pinned* mode. Exactly the leaf certificate whose
 *   DER SHA-256 equals the pin is accepted (self-signed hubs). Hostname/CA
 *   checks are replaced by the pin because the pin is the hub identity.
 * - [certSha256] empty: normal CA validation + hostname verification.
 * - [devCleartext]: plain http is permitted ONLY for loopback/emulator hosts
 *   and only when the user explicitly enabled the labelled developer option.
 */
data class HubEndpoint(
    val baseUrl: String,
    val certSha256: String = "",
    val devCleartext: Boolean = false,
) {
    val httpUrl: HttpUrl get() = baseUrl.toHttpUrlOrNull() ?: error("invalid base url")

    fun resolve(path: String): HttpUrl {
        val base = httpUrl
        val p = path.trimStart('/')
        val prefix = base.encodedPath.trimEnd('/')
        return base.newBuilder().encodedPath("$prefix/$p").build()
    }

    /** ws(s):// form of [path] for WebSocket endpoints (OkHttp also accepts http(s)). */
    fun resolveWs(path: String): HttpUrl = resolve(path)

    val pinned: Boolean get() = certSha256.isNotEmpty()

    companion object {
        val DEV_HOSTS = setOf("localhost", "127.0.0.1", "10.0.2.2", "::1")

        /**
         * Validates user/deep-link supplied input. Never accepts http for a
         * non-loopback host, embedded credentials, or a query/fragment.
         */
        fun parse(rawUrl: String, rawPin: String?, allowDevCleartext: Boolean): Result<HubEndpoint> {
            val trimmed = rawUrl.trim()
            val url = trimmed.toHttpUrlOrNull()
                ?: return Result.failure(IllegalArgumentException("Hub address must be a full https:// URL"))
            if (url.username.isNotEmpty() || url.password.isNotEmpty()) {
                return Result.failure(IllegalArgumentException("Hub address must not contain credentials"))
            }
            if (url.query != null || url.fragment != null) {
                return Result.failure(IllegalArgumentException("Hub address must not contain a query or fragment"))
            }
            val pin = if (rawPin.isNullOrBlank()) "" else CertPins.normalize(rawPin)
                ?: return Result.failure(IllegalArgumentException("Certificate pin is not a SHA-256 fingerprint"))
            when (url.scheme) {
                "https" -> Unit
                "http" -> {
                    if (!allowDevCleartext) {
                        return Result.failure(IllegalArgumentException("HTTPS is required. Plain http is only available for localhost/10.0.2.2 with the developer option enabled."))
                    }
                    if (url.host !in DEV_HOSTS) {
                        return Result.failure(IllegalArgumentException("Plain http is only allowed for localhost, 127.0.0.1 or 10.0.2.2"))
                    }
                }
                else -> return Result.failure(IllegalArgumentException("Unsupported scheme ${url.scheme}"))
            }
            val base = url.newBuilder().encodedPath(url.encodedPath.trimEnd('/').ifEmpty { "/" }).build()
                .toString().trimEnd('/')
            return Result.success(HubEndpoint(base, pin, devCleartext = url.scheme == "http"))
        }
    }
}
