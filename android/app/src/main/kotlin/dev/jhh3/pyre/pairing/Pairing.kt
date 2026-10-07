package dev.jhh3.pyre.pairing

import dev.jhh3.pyre.core.Clock
import dev.jhh3.pyre.data.CredentialStore
import dev.jhh3.pyre.data.HubCredentials
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.model.HelloResponse
import dev.jhh3.pyre.model.PairingExchange
import dev.jhh3.pyre.net.CertPins
import dev.jhh3.pyre.net.HttpPyreApi
import dev.jhh3.pyre.net.HubClient
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.net.PyreApiException
import java.net.URI
import java.net.URLDecoder

/**
 * `pyre://pair?hub=<https url>&pin=<sha256 leaf fingerprint>&id=<pairing_id>&code=<one-time code>`
 * (PairingTicket.URI). The code is a one-time secret: it is never logged and
 * only sent in the POST body of /v1/pair/exchange.
 */
data class PairingUri(val hub: String, val pin: String, val pairingId: String, val code: String) {
    override fun toString(): String = "PairingUri(hub=$hub, pin=$pin, id=$pairingId, code=<redacted>)"

    companion object {
        fun parse(raw: String): Result<PairingUri> {
            val uri = try {
                URI(raw.trim())
            } catch (e: Exception) {
                return Result.failure(IllegalArgumentException("Not a pairing link"))
            }
            if (!uri.scheme.equals("pyre", ignoreCase = true)) return Result.failure(IllegalArgumentException("Not a pyre:// link"))
            val host = uri.host ?: uri.rawSchemeSpecificPart.removePrefix("//").substringBefore('?').substringBefore('/')
            if (!host.equals("pair", ignoreCase = true)) return Result.failure(IllegalArgumentException("Not a pairing link"))
            val q = parseQuery(uri.rawQuery ?: uri.rawSchemeSpecificPart.substringAfter('?', ""))
            val hub = q["hub"].orEmpty()
            val id = q["id"].orEmpty()
            val code = q["code"].orEmpty()
            val pin = q["pin"].orEmpty()
            if (hub.isEmpty() || id.isEmpty() || code.isEmpty()) {
                return Result.failure(IllegalArgumentException("Pairing link is missing hub, id or code"))
            }
            if (pin.isNotEmpty() && CertPins.normalize(pin) == null) {
                return Result.failure(IllegalArgumentException("Pairing link has an invalid certificate pin"))
            }
            return Result.success(PairingUri(hub, pin, id, code))
        }

        private fun parseQuery(q: String): Map<String, String> = q.split('&').filter { it.isNotEmpty() }.associate {
            val k = it.substringBefore('=')
            val v = it.substringAfter('=', "")
            URLDecoder.decode(k, "UTF-8") to URLDecoder.decode(v, "UTF-8")
        }
    }
}

sealed interface PairingOutcome {
    data class Paired(val credentials: HubCredentials, val hello: HelloResponse?, val warning: String?) : PairingOutcome
    data class Failed(val message: String) : PairingOutcome
}

/** Exchanges a pairing code for a client token and stores it. */
class PairingService(
    private val store: CredentialStore,
    private val clientFactory: (HubEndpoint, () -> String?) -> HubClient = { ep, tok -> HubClient(ep, tok) },
    private val clock: Clock = Clock.System,
) {
    suspend fun pair(uri: PairingUri, clientName: String, allowDevCleartext: Boolean): PairingOutcome {
        val endpoint = HubEndpoint.parse(uri.hub, uri.pin, allowDevCleartext).getOrElse {
            return PairingOutcome.Failed(it.message ?: "Invalid hub address")
        }
        // No pin => normal CA validation (publicly trusted hub certificate).
        val anon = clientFactory(endpoint) { null }
        val cred = try {
            anon.pairExchange(PairingExchange(uri.pairingId, uri.code, clientName, "android"))
        } catch (e: PyreApiException) {
            return PairingOutcome.Failed(
                when (e.code) {
                    // The hub checks the protocol before redeeming the code
                    // (httpapi exchangePaths): nothing was used or stored.
                    ErrorCodes.PROTOCOL_MISMATCH -> "${e.describe()} The pairing code was not used and nothing was stored on this device; pair again after updating."
                    "unauthorized", "not_found", "conflict" -> "Pairing code was rejected (already used or wrong)."
                    else -> e.describe()
                },
            )
        }
        val creds = HubCredentials(endpoint, cred.clientId, cred.token, clientName, clock.now().toEpochMilli())
        store.save(creds)
        val authed = HttpPyreApi(clientFactory(endpoint) { creds.token })
        return try {
            val hello = authed.hello()
            val warn = if (hello.mock) "Connected to the deterministic MOCK hub — data is synthetic." else null
            PairingOutcome.Paired(creds, hello, warn)
        } catch (e: PyreApiException) {
            PairingOutcome.Paired(creds, null, e.describe())
        }
    }
}
