package dev.jhh3.pyre.notify

import android.content.Context
import dev.jhh3.pyre.BuildConfig

/** Implemented only in the optional `fcm` flavor (src/fcm). */
interface PushProvider {
    /** Human-readable availability; null when available. */
    fun unavailableReason(ctx: Context): String?

    /** Fetches the current FCM token (null if unavailable). */
    suspend fun token(ctx: Context): String?
}

object PushSupport {
    const val NOT_CONFIGURED =
        "Unavailable — configure FCM: build the `fcm` flavor with your own app/google-services.json " +
            "and configure the hub with the matching Firebase project. Use hub-stream mode meanwhile."

    /** Located reflectively so the default (oss) build contains no Firebase code. */
    fun provider(): PushProvider? {
        if (!BuildConfig.FCM_ENABLED) return null
        return runCatching {
            Class.forName("dev.jhh3.pyre.push.FcmPushProvider").getDeclaredConstructor().newInstance() as PushProvider
        }.getOrNull()
    }

    fun status(ctx: Context): String = provider()?.let { it.unavailableReason(ctx) ?: "Available (FCM)" } ?: NOT_CONFIGURED
}
