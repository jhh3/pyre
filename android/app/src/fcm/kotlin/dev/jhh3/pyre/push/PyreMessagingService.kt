package dev.jhh3.pyre.push

import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage
import dev.jhh3.pyre.graph
import dev.jhh3.pyre.notify.AttentionNotifier
import dev.jhh3.pyre.notify.Notifications
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeoutOrNull

/**
 * FCM data-only wake-ups (contracts.PushDataKeys). The payload carries only
 * minimal metadata; AttentionNotifier fetches canonical state before showing
 * controls, and `action=dismiss` withdraws the alert for that attention_id.
 */
class PyreMessagingService : FirebaseMessagingService() {
    override fun onNewToken(token: String) {
        applicationContext.graph.registerPushToken(token)
    }

    override fun onMessageReceived(message: RemoteMessage) {
        val session = applicationContext.graph.session.value ?: return
        // onMessageReceived runs on a background thread with a short budget.
        runBlocking {
            withTimeoutOrNull(8_000) {
                AttentionNotifier(session.repo, Notifications.AndroidSink(applicationContext)).onPush(message.data)
            }
        }
    }
}
