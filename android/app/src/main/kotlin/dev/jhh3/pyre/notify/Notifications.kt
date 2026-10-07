package dev.jhh3.pyre.notify

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import androidx.core.net.toUri
import dev.jhh3.pyre.MainActivity
import dev.jhh3.pyre.R

object Notifications {
    const val CH_ATTENTION = "attention"
    const val CH_INFO = "info"
    const val CH_STREAM = "stream"
    const val STREAM_ID = 7001
    private const val ATTENTION_ID = 1

    fun ensureChannels(ctx: Context) {
        val nm = ctx.getSystemService(NotificationManager::class.java) ?: return
        nm.createNotificationChannel(
            NotificationChannel(CH_ATTENTION, ctx.getString(R.string.channel_attention), NotificationManager.IMPORTANCE_HIGH)
                .apply { description = ctx.getString(R.string.channel_attention_desc) },
        )
        nm.createNotificationChannel(
            NotificationChannel(CH_INFO, ctx.getString(R.string.channel_info), NotificationManager.IMPORTANCE_DEFAULT)
                .apply { description = ctx.getString(R.string.channel_info_desc) },
        )
        nm.createNotificationChannel(
            NotificationChannel(CH_STREAM, ctx.getString(R.string.channel_stream), NotificationManager.IMPORTANCE_MIN)
                .apply { description = ctx.getString(R.string.channel_stream_desc) },
        )
    }

    fun canPost(ctx: Context): Boolean =
        (Build.VERSION.SDK_INT < 33 ||
            ContextCompat.checkSelfPermission(ctx, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED) &&
            NotificationManagerCompat.from(ctx).areNotificationsEnabled()

    fun openIntent(ctx: Context, deepLink: String, requestCode: Int): PendingIntent {
        val i = Intent(Intent.ACTION_VIEW, deepLink.ifEmpty { "pyre://attention/inbox" }.toUri(), ctx, MainActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP)
        return PendingIntent.getActivity(ctx, requestCode, i, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
    }

    fun streamNotification(ctx: Context, text: String): Notification =
        NotificationCompat.Builder(ctx, CH_STREAM)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle("Pyre hub stream")
            .setContentText(text)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setContentIntent(openIntent(ctx, "pyre://attention/inbox", 0))
            .build()

    /** Replace an attention notification with a plain result line (after an action). */
    fun showResult(ctx: Context, attentionId: String, title: String, text: String, deepLink: String) {
        if (!canPost(ctx)) return
        val n = NotificationCompat.Builder(ctx, CH_ATTENTION)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(title)
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setOnlyAlertOnce(true)
            .setSilent(true)
            .setAutoCancel(true)
            .setContentIntent(openIntent(ctx, deepLink, attentionId.hashCode()))
            .build()
        post(ctx, attentionId, n)
    }

    @Suppress("MissingPermission") // guarded by canPost()
    private fun post(ctx: Context, tag: String, n: Notification) {
        if (!canPost(ctx)) return
        NotificationManagerCompat.from(ctx).notify(tag, ATTENTION_ID, n)
    }

    fun cancel(ctx: Context, tag: String) = NotificationManagerCompat.from(ctx).cancel(tag, ATTENTION_ID)

    class AndroidSink(private val ctx: Context) : NotificationSink {
        override fun post(spec: NotificationSpec) {
            val b = NotificationCompat.Builder(ctx, if (spec.urgent) CH_ATTENTION else CH_INFO)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle(spec.title)
                .setContentText(spec.text)
                .setStyle(NotificationCompat.BigTextStyle().bigText(spec.text))
                .setCategory(NotificationCompat.CATEGORY_MESSAGE)
                .setGroup("pyre-attention")
                .setOnlyAlertOnce(true)
                .setAutoCancel(true)
                .setContentIntent(openIntent(ctx, spec.deepLink, spec.attentionId.hashCode()))
            spec.actions.forEachIndexed { i, a ->
                val intent = Intent(ctx, NotificationActionReceiver::class.java)
                    .setAction(NotificationActionReceiver.ACTION_ANSWER)
                    .putExtra(NotificationActionReceiver.EXTRA_ATTENTION, spec.attentionId)
                    .putExtra(NotificationActionReceiver.EXTRA_APPROVAL, a.approvalId)
                    .putExtra(NotificationActionReceiver.EXTRA_RUN, a.runId)
                    .putExtra(NotificationActionReceiver.EXTRA_OPTION, a.option.id)
                    .putExtra(NotificationActionReceiver.EXTRA_LABEL, a.option.label)
                    .putExtra(NotificationActionReceiver.EXTRA_LINK, spec.deepLink)
                val pi = PendingIntent.getBroadcast(
                    ctx,
                    (spec.attentionId + a.option.id).hashCode() + i,
                    intent,
                    PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
                )
                b.addAction(0, a.option.label.ifEmpty { a.option.id }, pi)
            }
            post(ctx, spec.attentionId, b.build())
        }

        override fun cancel(attentionId: String) = Notifications.cancel(ctx, attentionId)
    }
}
