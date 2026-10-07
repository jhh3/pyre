package dev.jhh3.pyre.net

import dev.jhh3.pyre.model.ApiError
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.model.ObjectRef
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.contentOrNull

/**
 * A Pyre error: either an ErrorBody from the hub or a transport failure
 * (`transport = true`, code `unavailable`). Messages never contain tokens.
 */
class PyreApiException(
    val code: String,
    override val message: String,
    val retryable: Boolean = false,
    val httpStatus: Int = 0,
    val ref: ObjectRef? = null,
    val details: JsonObject? = null,
    val transport: Boolean = false,
    cause: Throwable? = null,
) : Exception(message, cause) {

    val isUnauthorized get() = code == ErrorCodes.UNAUTHORIZED
    val isProtocolMismatch get() = code == ErrorCodes.PROTOCOL_MISMATCH
    val isOffline get() = transport

    fun detail(key: String): String? = (details?.get(key) as? JsonPrimitive)?.contentOrNull

    /** Human-facing one-liner. */
    fun describe(): String = when {
        transport -> "Hub unreachable: $message"
        code == ErrorCodes.PROTOCOL_MISMATCH -> "Protocol mismatch — $message. Update the app or hub so their protocol versions overlap."
        code == ErrorCodes.UNAUTHORIZED -> "This device is no longer authorized (revoked or expired). Pair again."
        else -> "$message ($code)"
    }

    companion object {
        fun from(err: ApiError, status: Int) = PyreApiException(
            code = err.code,
            message = err.message.ifBlank { err.code },
            retryable = err.retryable,
            httpStatus = status,
            ref = err.ref,
            details = err.details,
        )

        fun fromStatus(status: Int): PyreApiException {
            val code = when (status) {
                400 -> ErrorCodes.INVALID
                401 -> ErrorCodes.UNAUTHORIZED
                403 -> ErrorCodes.FORBIDDEN
                404 -> ErrorCodes.NOT_FOUND
                409 -> ErrorCodes.CONFLICT
                410 -> ErrorCodes.EXPIRED
                413 -> ErrorCodes.PAYLOAD_TOO_LARGE
                426 -> ErrorCodes.PROTOCOL_MISMATCH
                429 -> ErrorCodes.CAPACITY_EXHAUSTED
                501 -> ErrorCodes.UNSUPPORTED
                502, 503 -> ErrorCodes.UNAVAILABLE
                504 -> ErrorCodes.OUTCOME_INDETERMINATE
                else -> ErrorCodes.INTERNAL
            }
            val retryable = code == ErrorCodes.UNAVAILABLE || code == ErrorCodes.CAPACITY_EXHAUSTED
            return PyreApiException(code, "HTTP $status", retryable, status)
        }

        fun transport(t: Throwable) = PyreApiException(
            code = ErrorCodes.UNAVAILABLE,
            message = t.javaClass.simpleName + (t.message?.let { ": $it" } ?: ""),
            retryable = true,
            transport = true,
            cause = t,
        )
    }
}
