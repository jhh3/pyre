package dev.jhh3.pyre.net

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.ErrorBody
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.model.Event
import dev.jhh3.pyre.model.Paths
import kotlin.random.Random
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.flow
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.longOrNull
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener

sealed interface StreamSignal {
    data object Connected : StreamSignal
    data class Received(val event: Event) : StreamSignal
    /** Cursor older than retention: re-snapshot, then resume from [snapshotCursor]. */
    data class ResetRequired(val snapshotCursor: Long?) : StreamSignal
    data class Disconnected(val reason: String, val retryInMillis: Long) : StreamSignal
    /**
     * The hub speaks an incompatible protocol. Not fatal: the hub (or app) may
     * be upgraded or rolled back out of band, so the stream keeps the
     * credential and retries at the maximum backoff.
     */
    data class Incompatible(val error: PyreApiException, val retryInMillis: Long) : StreamSignal
    /** Fatal (unauthorized): the stream stops. */
    data class Fatal(val error: PyreApiException) : StreamSignal
}

/**
 * `/v1/events/stream` WebSocket client: bearer auth in the upgrade headers
 * (never the URL), `?after_cursor=<n>` resume, bounded exponential backoff
 * with jitter. The caller persists the cursor *after* applying each event
 * (re-delivery is safe, skipping is not).
 */
class EventStream(
    private val client: HubClient,
    private val minBackoffMillis: Long = 1_000,
    private val maxBackoffMillis: Long = 60_000,
    private val random: Random = Random.Default,
) {
    fun signals(cursor: () -> Long): Flow<StreamSignal> = flow {
        var failures = 0
        while (true) {
            var fatal: PyreApiException? = null
            var sawTraffic = false
            var reason = "closed"
            connectOnce(cursor()).collect { s ->
                when (s) {
                    is Once.Opened -> { sawTraffic = true; failures = 0; emit(StreamSignal.Connected) }
                    is Once.Ev -> emit(StreamSignal.Received(s.event))
                    is Once.Reset -> emit(StreamSignal.ResetRequired(s.snapshot))
                    is Once.Closed -> reason = s.reason
                    is Once.Failed -> { fatal = s.fatal; reason = s.reason }
                }
            }
            val f = fatal
            if (f != null && !f.isProtocolMismatch) {
                emit(StreamSignal.Fatal(f))
                return@flow
            }
            if (f != null) {
                val wait = backoff(Int.MAX_VALUE)
                emit(StreamSignal.Incompatible(f, wait))
                delay(wait)
                continue
            }
            if (!sawTraffic) failures++
            val wait = backoff(failures)
            emit(StreamSignal.Disconnected(reason, wait))
            delay(wait)
        }
    }

    fun backoff(failures: Int): Long {
        val exp = (minBackoffMillis shl failures.coerceIn(0, 6)).coerceAtMost(maxBackoffMillis)
        return exp / 2 + random.nextLong(exp / 2 + 1)
    }

    private sealed interface Once {
        data object Opened : Once
        data class Ev(val event: Event) : Once
        data class Reset(val snapshot: Long?) : Once
        data class Closed(val reason: String) : Once
        data class Failed(val reason: String, val fatal: PyreApiException?) : Once
    }

    private fun connectOnce(after: Long): Flow<Once> = callbackFlow {
        val url = client.endpoint.resolve(Paths.EVENT_STREAM).newBuilder()
            .addQueryParameter("after_cursor", after.toString()).build()
        val req = client.authed(Request.Builder().url(url)).build()
        val ws = client.okHttp.newWebSocket(req, object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                trySend(Once.Opened)
            }

            override fun onMessage(webSocket: WebSocket, text: String) {
                val obj = runCatching { PyreJson.wire.parseToJsonElement(text).jsonObject }.getOrNull() ?: return
                if (obj.containsKey("error")) {
                    val err = runCatching { PyreJson.wire.decodeFromJsonElement(ErrorBody.serializer(), obj).error }.getOrNull()
                    if (err?.code == ErrorCodes.RESET_REQUIRED) {
                        trySend(Once.Reset(snapshotCursor(err.details)))
                    } else if (err != null) {
                        val ex = PyreApiException.from(err, 0)
                        val fatal = ex.isUnauthorized || ex.isProtocolMismatch
                        trySend(Once.Failed(ex.describe(), if (fatal) ex else null))
                    }
                    webSocket.close(1000, null)
                    return
                }
                runCatching { PyreJson.wire.decodeFromJsonElement(Event.serializer(), obj) }
                    .onSuccess { trySend(Once.Ev(it)) }
            }

            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                webSocket.close(code, null)
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                trySend(Once.Closed("closed ($code)"))
                close()
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                val status = response?.code ?: 0
                val fatal = when (status) {
                    401, 426 -> errorBody(response) ?: PyreApiException.fromStatus(status)
                    else -> null
                }
                response?.close()
                trySend(Once.Failed(if (status != 0) "HTTP $status" else (t.message ?: t.javaClass.simpleName), fatal))
                close()
            }
        })
        awaitClose { ws.cancel() }
    }

    /** The hub's ErrorBody on a refused upgrade (e.g. the supported protocol range). */
    private fun errorBody(response: Response?): PyreApiException? = runCatching {
        val text = response?.body?.string() ?: return null
        val err = PyreJson.wire.decodeFromString(ErrorBody.serializer(), text).error ?: return null
        PyreApiException.from(err, response.code)
    }.getOrNull()

    private fun snapshotCursor(details: JsonObject?): Long? =
        details?.get("snapshot_cursor")?.jsonPrimitive?.longOrNull
}
