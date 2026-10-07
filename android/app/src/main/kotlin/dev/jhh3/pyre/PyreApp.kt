package dev.jhh3.pyre

import android.app.Application
import dev.jhh3.pyre.data.DataStoreCredentialStore
import dev.jhh3.pyre.notify.Notifications

class PyreApp : Application() {
    lateinit var graph: AppGraph
        private set

    override fun onCreate() {
        super.onCreate()
        graph = AppGraph(this, DataStoreCredentialStore.create(this))
        Notifications.ensureChannels(this)
        graph.watchConnectivity()
        graph.flushOutbox()
        graph.registerPushIfAvailable()
    }
}

val android.content.Context.graph: AppGraph get() = (applicationContext as PyreApp).graph
