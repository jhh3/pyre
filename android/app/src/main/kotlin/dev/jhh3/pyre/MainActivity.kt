package dev.jhh3.pyre

import android.Manifest
import android.annotation.SuppressLint
import android.content.Intent
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.core.net.toUri
import dev.jhh3.pyre.notify.HubStreamService
import dev.jhh3.pyre.ui.PyreRoot
import dev.jhh3.pyre.ui.nav.DeepLinks
import dev.jhh3.pyre.ui.nav.Navigator
import dev.jhh3.pyre.ui.theme.PyreTheme

class MainActivity : ComponentActivity() {
    private val navigator = Navigator()
    private val permissionTick = mutableIntStateOf(0)
    private val notificationPermission = registerForActivityResult(ActivityResultContracts.RequestPermission()) { permissionTick.intValue++ }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        handle(intent)
        if (graph.settings.prefs.value.hubStreamEnabled && graph.session.value != null) HubStreamService.start(this)
        setContent {
            val prefs by graph.settings.prefs.collectAsStateWithLifecycle()
            PyreTheme(themeMode = prefs.themeMode, dynamicColor = prefs.dynamicColor) {
                PyreRoot(
                    graph = graph,
                    navigator = navigator,
                    permissionTick = permissionTick.intValue,
                    actions = object : PlatformActions {
                        override fun requestNotificationPermission() {
                            if (Build.VERSION.SDK_INT >= 33) notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
                        }

                        override fun openNotificationSettings() {
                            runCatching {
                                startActivity(Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS).putExtra(Settings.EXTRA_APP_PACKAGE, packageName))
                            }
                        }

                        override fun setStream(enabled: Boolean) {
                            graph.settings.update { it.copy(hubStreamEnabled = enabled) }
                            if (enabled) HubStreamService.start(this@MainActivity) else HubStreamService.stop(this@MainActivity)
                        }

                        override fun batteryExempt(): Boolean =
                            getSystemService(PowerManager::class.java)?.isIgnoringBatteryOptimizations(packageName) == true

                        @SuppressLint("BatteryLife")
                        override fun requestBatteryExemption() {
                            runCatching {
                                startActivity(Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, "package:$packageName".toUri()))
                            }
                        }
                    },
                )
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handle(intent)
    }

    override fun onResume() {
        super.onResume()
        graph.flushOutbox()
    }

    private fun handle(intent: Intent?) {
        val data = intent?.data?.toString() ?: return
        DeepLinks.parse(data)?.let { navigator.push(it) }
    }
}

interface PlatformActions {
    fun requestNotificationPermission()
    fun openNotificationSettings()
    fun setStream(enabled: Boolean)
    fun batteryExempt(): Boolean
    fun requestBatteryExemption()
}
