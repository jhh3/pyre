package dev.jhh3.pyre.notify

import android.annotation.SuppressLint
import android.app.NotificationManager
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import androidx.core.content.ContextCompat
import androidx.lifecycle.LifecycleService
import androidx.lifecycle.lifecycleScope
import dev.jhh3.pyre.graph
import dev.jhh3.pyre.net.EventStream
import dev.jhh3.pyre.net.StreamSignal
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch

/**
 * Optional, user-enabled "hub stream" notification mode for Firebase-free
 * builds: a foreground service holds /v1/events/stream open (reconnecting
 * with backoff, cursor persisted) and posts notifications for attention
 * changes. Battery cost is real; the user turns it on in Settings and may
 * exempt the app from battery optimization for screen-off delivery.
 */
class HubStreamService : LifecycleService() {
    private var job: Job? = null

    // The merged manifest declares foregroundServiceType="remoteMessaging"; lint
    // misreports this only when analysing the optional fcm flavor's manifest.
    @SuppressLint("ForegroundServiceType")
    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        super.onStartCommand(intent, flags, startId)
        if (intent?.action == ACTION_STOP) {
            stopSelf()
            return START_NOT_STICKY
        }
        val n = Notifications.streamNotification(this, "Connecting…")
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(Notifications.STREAM_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_REMOTE_MESSAGING)
        } else {
            startForeground(Notifications.STREAM_ID, n)
        }
        if (job == null) job = lifecycleScope.launch { run() }
        return START_STICKY
    }

    private suspend fun run() {
        val graph = applicationContext.graph
        val session = graph.session.value ?: run { stopSelf(); return }
        val notifier = AttentionNotifier(session.repo, Notifications.AndroidSink(this))
        notifier.refresh()
        val stream = EventStream(session.client)
        stream.signals { graph.settings.prefs.value.eventCursor }.collect { s ->
            when (s) {
                StreamSignal.Connected -> {
                    status("Connected — listening for attention events")
                    graph.flushOutbox()
                    notifier.refresh()
                }
                is StreamSignal.Received -> {
                    notifier.onEvent(s.event)
                    // Commit the cursor only after the event was applied.
                    graph.settings.update { it.copy(eventCursor = maxOf(it.eventCursor, s.event.cursor)) }
                }
                is StreamSignal.ResetRequired -> {
                    notifier.refresh()
                    graph.settings.update { it.copy(eventCursor = s.snapshotCursor ?: 0) }
                }
                is StreamSignal.Disconnected -> status("Reconnecting in ${s.retryInMillis / 1000}s (${s.reason})")
                // Keep the pairing and the setting: the hub or app may be
                // updated or rolled back, and the stream resumes by itself.
                is StreamSignal.Incompatible -> {
                    status("Paused: ${s.error.describe()} Retrying in ${s.retryInMillis / 1000}s.")
                    graph.health.failed(s.error)
                }
                is StreamSignal.Fatal -> {
                    status("Stopped: ${s.error.describe()}")
                    graph.health.failed(s.error)
                    graph.settings.update { it.copy(hubStreamEnabled = false) }
                    stopSelf()
                }
            }
        }
    }

    private fun status(text: String) {
        getSystemService(NotificationManager::class.java)
            ?.notify(Notifications.STREAM_ID, Notifications.streamNotification(this, text))
    }

    companion object {
        private const val ACTION_STOP = "dev.jhh3.pyre.action.STOP_STREAM"

        fun start(ctx: Context) = ContextCompat.startForegroundService(ctx, Intent(ctx, HubStreamService::class.java))

        fun stop(ctx: Context) {
            ctx.stopService(Intent(ctx, HubStreamService::class.java))
        }
    }
}
