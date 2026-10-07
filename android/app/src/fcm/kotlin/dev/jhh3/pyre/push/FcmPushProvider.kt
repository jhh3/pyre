package dev.jhh3.pyre.push

import android.content.Context
import com.google.firebase.FirebaseApp
import com.google.firebase.messaging.FirebaseMessaging
import dev.jhh3.pyre.notify.PushProvider
import kotlin.coroutines.resume
import kotlinx.coroutines.suspendCancellableCoroutine

/** Located reflectively by PushSupport; compiled only into the `fcm` flavor. */
class FcmPushProvider : PushProvider {
    override fun unavailableReason(ctx: Context): String? =
        if (FirebaseApp.getApps(ctx).isEmpty() && FirebaseApp.initializeApp(ctx) == null) {
            "Unavailable — Firebase did not initialize (check app/google-services.json for this application id)."
        } else null

    override suspend fun token(ctx: Context): String? {
        if (unavailableReason(ctx) != null) return null
        return suspendCancellableCoroutine { cont ->
            FirebaseMessaging.getInstance().token.addOnCompleteListener { t ->
                cont.resume(if (t.isSuccessful) t.result else null)
            }
        }
    }
}
