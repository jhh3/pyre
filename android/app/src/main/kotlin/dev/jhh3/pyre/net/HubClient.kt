package dev.jhh3.pyre.net

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.ClientCredential
import dev.jhh3.pyre.model.ErrorBody
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.model.PairingExchange
import dev.jhh3.pyre.model.Paths
import dev.jhh3.pyre.model.Protocol
import java.io.IOException
import kotlin.coroutines.resumeWithException
import kotlinx.coroutines.delay
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.serialization.KSerializer
import kotlinx.serialization.SerializationException
import kotlinx.serialization.json.Json
import okhttp3.Call
import okhttp3.Callback
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response

/**
 * JSON-RPC binding: `POST /v1/<op>`, bearer token, `X-Pyre-Protocol`.
 *
 * Retry policy: only transport failures and errors the hub marks retryable
 * (plus `outcome_indeterminate` for mutations) are retried, a bounded number
 * of times, with the *identical* request body — so a mutation's
 * operation_id makes the retry return the stored result rather than
 * performing the action twice. Conflicts, expiry, auth and protocol errors
 * are never retried.
 */
class HubClient(
    val endpoint: HubEndpoint,
    private val token: () -> String?,
    private val http: OkHttpClient = HttpClients.forEndpoint(endpoint),
    private val json: Json = PyreJson.wire,
    private val clientVersion: String = "0.1.0",
    private val maxAttempts: Int = 3,
    private val backoffMillis: (attempt: Int) -> Long = { attempt -> 400L * attempt * attempt },
) {
    val okHttp: OkHttpClient get() = http

    suspend fun <Req, Res> rpc(
        op: String,
        request: Req,
        reqSer: KSerializer<Req>,
        resSer: KSerializer<Res>,
        mutation: Boolean = false,
    ): Res {
        val body = json.encodeToString(reqSer, request)
        var attempt = 1
        while (true) {
            try {
                return execute(op, body, resSer)
            } catch (e: PyreApiException) {
                val again = attempt < maxAttempts && shouldRetry(e, mutation)
                if (!again) throw e
                delay(backoffMillis(attempt))
                attempt++
            }
        }
    }

    private fun shouldRetry(e: PyreApiException, mutation: Boolean): Boolean = when {
        e.transport -> true
        e.code == ErrorCodes.OUTCOME_INDETERMINATE -> mutation
        else -> e.retryable && (e.code == ErrorCodes.UNAVAILABLE || e.code == ErrorCodes.CAPACITY_EXHAUSTED)
    }

    private suspend fun <Res> execute(op: String, body: String, resSer: KSerializer<Res>): Res {
        val req = authed(Request.Builder().url(endpoint.resolve("/v1/$op")))
            .post(body.toRequestBody(JSON))
            .build()
        val response = call(req)
        return response.use { decode(it, resSer) }
    }

    fun authed(b: Request.Builder): Request.Builder {
        b.header(Protocol.HEADER, Protocol.VERSION.toString())
        b.header("User-Agent", "pyre-android/$clientVersion")
        token()?.takeIf { it.isNotEmpty() }?.let { b.header("Authorization", "Bearer $it") }
        return b
    }

    /** Unauthenticated one-time pairing exchange. */
    suspend fun pairExchange(exchange: PairingExchange): ClientCredential {
        val req = Request.Builder().url(endpoint.resolve(Paths.PAIR_EXCHANGE))
            .header(Protocol.HEADER, Protocol.VERSION.toString())
            .header("User-Agent", "pyre-android/$clientVersion")
            .post(json.encodeToString(PairingExchange.serializer(), exchange).toRequestBody(JSON))
            .build()
        return call(req).use { decode(it, ClientCredential.serializer()) }
    }

    /** Raw authenticated GET (artifact content). Caller closes the response. */
    suspend fun get(path: String): Response {
        val req = authed(Request.Builder().url(endpoint.resolve(path))).get().build()
        val resp = call(req)
        if (!resp.isSuccessful) {
            resp.use { throw errorFrom(it) }
        }
        return resp
    }

    private fun <Res> decode(resp: Response, resSer: KSerializer<Res>): Res {
        if (!resp.isSuccessful) throw errorFrom(resp)
        val text = try {
            resp.body.string()
        } catch (e: IOException) {
            throw PyreApiException.transport(e)
        }
        return try {
            json.decodeFromString(resSer, text.ifEmpty { "{}" })
        } catch (e: SerializationException) {
            throw PyreApiException(ErrorCodes.INTERNAL, "Unexpected response from hub (${e.message?.take(120)})", httpStatus = resp.code, cause = e)
        } catch (e: IllegalArgumentException) {
            throw PyreApiException(ErrorCodes.INTERNAL, "Unexpected response from hub", httpStatus = resp.code, cause = e)
        }
    }

    private fun errorFrom(resp: Response): PyreApiException {
        val text = try {
            resp.body.string()
        } catch (_: IOException) {
            ""
        }
        val parsed = try {
            json.decodeFromString(ErrorBody.serializer(), text).error
        } catch (_: Exception) {
            null
        }
        return parsed?.let { PyreApiException.from(it, resp.code) } ?: PyreApiException.fromStatus(resp.code)
    }

    private suspend fun call(req: Request): Response = suspendCancellableCoroutine { cont ->
        val call = http.newCall(req)
        cont.invokeOnCancellation { call.cancel() }
        call.enqueue(object : Callback {
            override fun onFailure(call: Call, e: IOException) {
                cont.resumeWithException(PyreApiException.transport(e))
            }

            override fun onResponse(call: Call, response: Response) {
                cont.resume(response) { _, value, _ -> value.close() }
            }
        })
    }

    companion object {
        val JSON = "application/json; charset=utf-8".toMediaType()
    }
}
