package dev.jhh3.pyre

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import dev.jhh3.pyre.core.Clock
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.data.CacheStore
import dev.jhh3.pyre.data.CredentialStore
import dev.jhh3.pyre.data.FileBlobStore
import dev.jhh3.pyre.data.FileCacheStore
import dev.jhh3.pyre.data.HubCredentials
import dev.jhh3.pyre.data.HubHealthTracker
import dev.jhh3.pyre.data.IntentIds
import dev.jhh3.pyre.data.LocalSettings
import dev.jhh3.pyre.data.Outbox
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.ApprovalAnswerer
import dev.jhh3.pyre.domain.AskAnswerer
import dev.jhh3.pyre.model.PushRegistration
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.notify.HubStreamService
import dev.jhh3.pyre.notify.PushSupport
import dev.jhh3.pyre.net.HttpPyreApi
import dev.jhh3.pyre.net.HubClient
import dev.jhh3.pyre.pairing.PairingService
import java.io.File
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

/** Everything that exists only while a hub is paired. */
class HubSession(val credentials: HubCredentials, val client: HubClient, val repo: Repository) {
    val answerer = ApprovalAnswerer(repo)
    val askAnswerer = AskAnswerer(repo)
}

/**
 * Manual dependency container. One per process (see [PyreApp]); tests build
 * ViewModels directly from fakes instead.
 */
class AppGraph(
    context: Context,
    val credentials: CredentialStore,
    val clock: Clock = Clock.System,
) {
    val appContext: Context = context.applicationContext
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val blobs = FileBlobStore(File(appContext.filesDir, "state"))
    val cache: CacheStore = FileCacheStore(File(appContext.filesDir, "hub-cache"), clock)
    val outbox = Outbox(blobs, clock)
    val intents = IntentIds(blobs)
    val settings = LocalSettings(blobs)
    val health = HubHealthTracker(clock)
    val pairing = PairingService(credentials, clock = clock)

    private val _session = MutableStateFlow(credentials.load()?.let { build(it) })
    val session: StateFlow<HubSession?> = _session.asStateFlow()

    private fun build(c: HubCredentials): HubSession {
        val client = HubClient(c.endpoint, { credentials.load()?.token }, clientVersion = BuildConfig.VERSION_NAME)
        return HubSession(c, client, Repository(HttpPyreApi(client), cache, outbox, intents, health, clock))
    }

    fun onPaired(c: HubCredentials) {
        _session.value = build(c)
        // A running hub-stream service captured the previous pairing (found on
        // a Pixel: it kept dialling the old hub with the old pin). Event
        // cursors belong to the previous hub, so start the new stream fresh.
        settings.update { it.copy(eventCursor = 0) }
        if (settings.prefs.value.hubStreamEnabled) {
            HubStreamService.stop(appContext)
            HubStreamService.start(appContext)
        }
        flushOutbox()
        registerPushIfAvailable()
    }

    fun unpair() {
        credentials.clear()
        cache.clear()
        outbox.clear()
        settings.update { it.copy(hubStreamEnabled = false, eventCursor = 0) }
        _session.value = null
    }

    fun flushOutbox() {
        val s = _session.value ?: return
        scope.launch { s.repo.flushOutbox() }
    }

    /** fcm flavor only: fetch the FCM token and register it with the hub. */
    fun registerPushIfAvailable() {
        val provider = PushSupport.provider() ?: return
        scope.launch { provider.token(appContext)?.let { registerPushToken(it) } }
    }

    /** The FCM token is a provider push token, not a Pyre credential; still never logged. */
    fun registerPushToken(token: String) {
        val s = _session.value ?: return
        scope.launch {
            val status = try {
                val r = s.repo.call {
                    registerPush(PushRegistration(operationId = Ids.operation(), provider = "fcm", deviceToken = token, clientId = s.credentials.clientId))
                }
                if (r.enabled) "Registered with hub" else "Hub: ${r.detail.ifEmpty { "push not enabled" }}"
            } catch (e: PyreApiException) {
                "Registration failed: ${e.describe()}"
            }
            settings.update { it.copy(pushStatus = status) }
        }
    }

    fun watchConnectivity() {
        val cm = appContext.getSystemService(ConnectivityManager::class.java) ?: return
        cm.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) = flushOutbox()
        })
    }
}
