package dev.jhh3.pyre.terminal

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.ApiError
import dev.jhh3.pyre.model.AttachDescriptor
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.net.HubClient
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.put
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString
import okio.ByteString.Companion.toByteString

/**
 * Client for `/v1/terminal` (routes.go PathTerminal).
 *
 * The one-time ticket from the AttachDescriptor is sent in the FIRST frame,
 * never in the URL. Frames (routes.go PathTerminal, v1.2):
 *   client → hub, first text frame: {"type":"auth","ticket":"…","cols":N,"rows":M}
 *   binary frames: raw terminal bytes in both directions
 *   client → hub text: {"type":"resize","cols":N,"rows":M}
 *   hub → client text: {"type":"error","error":{…}} before close
 *   close code: 4000 + the error's HTTP status (4401, 4409, 4410, 4503…), 1000 normal.
 * Error frames and close codes are surfaced as errors, never drawn as output.
 * The socket uses the hub client's pinned TLS configuration.
 */
class TerminalSocket(
    private val client: HubClient,
    private val descriptor: AttachDescriptor,
    private val listener: Listener,
) {
    interface Listener {
        fun onOpen()
        /** Raw terminal output bytes (UTF-8 decoding is the emulator's job). */
        fun onOutput(bytes: ByteArray)
        /** [error] is null for a normal close (1000). */
        fun onClosed(reason: String, error: PyreApiException?)
    }

    @Volatile private var serverError: PyreApiException? = null
    @Volatile private var closedReported = false

    private var ws: WebSocket? = null

    // OkHttp queues frames sent before the handshake and writes them first, so
    // a resize (the TerminalView lays out right after connect) or a key typed
    // before onOpen would reach the relay ahead of the auth frame and be
    // refused. Until the auth frame is out, input is buffered and resizes
    // only update the size the auth frame carries.
    private val gate = Any()
    private var authSent = false
    private var cols = 80
    private var rows = 24
    private val early = ArrayList<ByteString>()
    /** Resolves the descriptor URL; refuses to send the ticket anywhere but the paired hub. */
    fun url(): okhttp3.HttpUrl {
        val raw = descriptor.url.ifEmpty { "/v1/terminal" }
        val abs = raw.replaceFirst(Regex("^wss://"), "https://").replaceFirst(Regex("^ws://"), "http://").toHttpUrlOrNull()
        val hub = client.endpoint.httpUrl
        if (abs == null) return client.endpoint.resolve(raw)
        require(abs.host == hub.host && abs.port == hub.port && abs.scheme == hub.scheme) {
            "terminal URL is not on the paired hub; refusing to send the ticket"
        }
        return abs
    }

    fun connect(cols: Int, rows: Int) {
        synchronized(gate) {
            this.cols = cols
            this.rows = rows
        }
        val req = client.authed(Request.Builder().url(url())).build()
        ws = client.okHttp.newWebSocket(req, object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                synchronized(gate) {
                    webSocket.send(
                        buildJsonObject {
                            put("type", "auth")
                            put("ticket", descriptor.ticket)
                            put("cols", this@TerminalSocket.cols)
                            put("rows", this@TerminalSocket.rows)
                        }.toString(),
                    )
                    early.forEach { webSocket.send(it) }
                    early.clear()
                    authSent = true
                }
                listener.onOpen()
            }

            override fun onMessage(webSocket: WebSocket, text: String) {
                when (val f = classifyText(text)) {
                    is TextFrame.Error -> serverError = f.error
                    TextFrame.Ignored -> Unit
                    is TextFrame.Output -> listener.onOutput(f.text.toByteArray(Charsets.UTF_8))
                }
            }

            override fun onMessage(webSocket: WebSocket, bytes: ByteString) = listener.onOutput(bytes.toByteArray())

            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                webSocket.close(code, null)
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) = reportClose(code, reason)

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                val status = response?.code ?: 0
                response?.close()
                if (closedReported) return
                closedReported = true
                val err = serverError ?: if (status != 0) PyreApiException.fromStatus(status) else PyreApiException.transport(t)
                listener.onClosed(err.describe(), err)
            }
        })
    }

    private fun reportClose(code: Int, reason: String) {
        if (closedReported) return
        closedReported = true
        val err = closeError(code, reason, serverError)
        listener.onClosed(err?.describe() ?: "closed", err)
    }

    sealed interface TextFrame {
        data class Error(val error: PyreApiException) : TextFrame
        data class Output(val text: String) : TextFrame
        data object Ignored : TextFrame
    }

    companion object {
        /** Server text frames: `{"type":"error",...}` is an error; other JSON control frames are ignored. */
        fun classifyText(text: String): TextFrame {
            val obj = runCatching { PyreJson.wire.parseToJsonElement(text) as? JsonObject }.getOrNull()
                ?: return TextFrame.Output(text)
            val type = (obj["type"] as? JsonPrimitive)?.contentOrNull ?: return TextFrame.Output(text)
            if (type != "error") return TextFrame.Ignored
            val err = runCatching { PyreJson.wire.decodeFromJsonElement(ApiError.serializer(), obj["error"]!!) }.getOrNull()
                ?: ApiError(ErrorCodes.INTERNAL, "terminal error")
            return TextFrame.Error(PyreApiException.from(err, err.code.let { statusFor(it) }))
        }

        private fun statusFor(code: String) = when (code) {
            ErrorCodes.UNAUTHORIZED -> 401
            ErrorCodes.CONFLICT -> 409
            ErrorCodes.EXPIRED -> 410
            ErrorCodes.UNAVAILABLE -> 503
            ErrorCodes.NOT_FOUND -> 404
            ErrorCodes.FORBIDDEN -> 403
            else -> 0
        }

        /** 1000 → normal; 4000+status → error for that HTTP status (preferring the error frame's detail). */
        fun closeError(code: Int, reason: String, frame: PyreApiException?): PyreApiException? = when {
            frame != null -> frame
            code == 1000 -> null
            code in 4000..4999 -> PyreApiException.fromStatus(code - 4000).let {
                if (reason.isNotEmpty()) PyreApiException(it.code, reason, it.retryable, it.httpStatus) else it
            }
            else -> PyreApiException(ErrorCodes.UNAVAILABLE, "terminal closed ($code${if (reason.isNotEmpty()) ": $reason" else ""})", retryable = true)
        }
    }

    fun sendInput(s: String) = sendBytes(s.toByteArray(Charsets.UTF_8))

    fun sendBytes(data: ByteArray, offset: Int = 0, count: Int = data.size) {
        val bytes = data.toByteString(offset, count)
        synchronized(gate) {
            if (!authSent) {
                early += bytes
                return
            }
        }
        ws?.send(bytes)
    }

    fun resize(cols: Int, rows: Int) {
        synchronized(gate) {
            if (!authSent) {
                this.cols = cols
                this.rows = rows
                return
            }
        }
        ws?.send(buildJsonObject { put("type", "resize"); put("cols", cols); put("rows", rows) }.toString())
    }

    fun close() {
        ws?.close(1000, "detach")
        ws = null
    }
}
