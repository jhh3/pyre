package dev.jhh3.pyre.ui.settings

import android.os.Build
import android.text.format.DateFormat
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TimePicker
import androidx.compose.material3.rememberTimePickerState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.data.HubCredentials
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.HelloResponse
import dev.jhh3.pyre.model.NotificationPrefs
import dev.jhh3.pyre.model.SetNotificationPrefsRequest
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.Expandable
import dev.jhh3.pyre.ui.common.KeyValue
import dev.jhh3.pyre.ui.common.OnRetry
import dev.jhh3.pyre.ui.common.SectionHeader
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.common.ToneText
import dev.jhh3.pyre.ui.theme.ThemeMode
import java.time.LocalTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.time.format.FormatStyle
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

data class SettingsUi(
    val hello: HelloResponse? = null,
    val helloError: String? = null,
    val prefs: NotificationPrefs? = null,
    val prefsError: String? = null,
    val saving: Boolean = false,
    val saved: String? = null,
    /** The last change that failed to save, so Retry can resend it. */
    val failed: NotificationPrefs? = null,
)

class SettingsViewModel(private val repo: Repository) : ViewModel() {
    private val _ui = MutableStateFlow(SettingsUi())
    val ui: StateFlow<SettingsUi> = _ui.asStateFlow()

    init { refresh() }

    fun refresh() = viewModelScope.launch {
        try {
            _ui.value = _ui.value.copy(hello = repo.call { hello() }, helloError = null)
        } catch (e: PyreApiException) {
            _ui.value = _ui.value.copy(helloError = e.describe())
        }
        try {
            _ui.value = _ui.value.copy(prefs = repo.call { getNotificationPrefs() }, prefsError = null)
        } catch (e: PyreApiException) {
            _ui.value = _ui.value.copy(prefsError = e.describe())
        }
    }

    /** Quiet hours live on the hub and apply to every device; each change is saved right away. */
    fun save(p: NotificationPrefs) {
        _ui.value = _ui.value.copy(saving = true, saved = null, failed = null, prefs = p)
        viewModelScope.launch {
            _ui.value = try {
                val r = repo.call { setNotificationPrefs(SetNotificationPrefsRequest(operationId = Ids.operation(), prefs = p)) }
                _ui.value.copy(prefs = r, saving = false, saved = "Saved on your hub. Applies to all your devices.")
            } catch (e: PyreApiException) {
                _ui.value.copy(saving = false, saved = "Couldn't save: ${e.describe()}", failed = p)
            }
        }
    }
}

data class DeviceControls(
    /** Plain-language state of provider push (FCM); never build instructions. */
    val pushStatus: String,
    val notificationsAllowed: Boolean,
    val streamEnabled: Boolean,
    val batteryExempt: Boolean,
    val themeMode: String = ThemeMode.SYSTEM,
    val dynamicColor: Boolean = true,
    val dynamicColorSupported: Boolean = true,
    val onRequestNotifications: () -> Unit,
    val onOpenNotificationSettings: () -> Unit = {},
    val onToggleStream: (Boolean) -> Unit,
    val onRequestBatteryExemption: () -> Unit,
    val onThemeMode: (String) -> Unit = {},
    val onDynamicColor: (Boolean) -> Unit = {},
    val onUnpair: () -> Unit,
)

/** Push wording for "About this connection". */
fun pushLabel(fcmBuild: Boolean, unavailableReason: String?, registration: String): String = when {
    !fcmBuild -> "Not part of this build. Live alerts use a direct connection to your hub instead."
    unavailableReason != null -> "Not available on this phone right now."
    registration.isNotEmpty() -> registration
    else -> "Available"
}

/**
 * Only what a person changes: notifications, quiet hours, appearance and
 * unpairing. Connection internals sit in a collapsed "About this connection".
 */
@Composable
fun SettingsScreen(vm: SettingsViewModel, creds: HubCredentials, device: DeviceControls) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    var confirmUnpair by remember { mutableStateOf(false) }
    OnRetry { vm.refresh() }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).testTag("settings-screen")) {
        SectionHeader("Notifications")
        SwitchRow(
            title = "Allow notifications",
            body = if (device.notificationsAllowed) "On. Pyre can alert you when something needs you." else "Off. Pyre can't alert you.",
            checked = device.notificationsAllowed,
            tag = "notifications-toggle",
        ) {
            if (!device.notificationsAllowed && Build.VERSION.SDK_INT >= 33) device.onRequestNotifications() else device.onOpenNotificationSettings()
        }
        SwitchRow(
            title = "Live alerts",
            body = "Keeps a connection to your hub so alerts arrive right away. Shows a small ongoing notification and uses some battery.",
            checked = device.streamEnabled,
            tag = "stream-toggle",
        ) { device.onToggleStream(it) }
        if (device.streamEnabled) {
            SwitchRow(
                title = "Alerts with the screen off",
                body = if (device.batteryExempt) "Allowed. Android won't pause Pyre to save battery." else "Lets alerts arrive while the phone sleeps. Android asks you to allow it.",
                checked = device.batteryExempt,
                enabled = !device.batteryExempt,
                tag = "battery-toggle",
            ) { device.onRequestBatteryExemption() }
        }

        SectionHeader("Quiet hours")
        val prefs = ui.prefs
        if (prefs == null) {
            if (ui.prefsError != null) {
                Row(Modifier.padding(horizontal = 16.dp), verticalAlignment = Alignment.CenterVertically) {
                    Supporting("Couldn't load quiet hours from your hub.", Modifier.weight(1f))
                    TextButton(onClick = { vm.refresh() }) { Text("Retry") }
                }
            } else {
                Supporting("Loading…", Modifier.padding(horizontal = 16.dp))
            }
        } else {
            QuietHours(prefs, ui, vm)
        }

        SectionHeader("Appearance")
        Column(Modifier.selectableGroup()) {
            listOf(ThemeMode.SYSTEM to "Match the system", ThemeMode.LIGHT to "Light", ThemeMode.DARK to "Dark").forEach { (mode, label) ->
                RadioRow(label, selected = device.themeMode == mode, tag = "theme-$mode") { device.onThemeMode(mode) }
            }
        }
        if (device.dynamicColorSupported) {
            SwitchRow(
                title = "Wallpaper colours",
                body = "Use colours from your wallpaper (Material You).",
                checked = device.dynamicColor,
                tag = "dynamic-color",
            ) { device.onDynamicColor(it) }
        }

        SectionHeader("This phone")
        Row(
            Modifier.fillMaxWidth().clickable(role = Role.Button) { confirmUnpair = true }.heightIn(min = 56.dp).padding(horizontal = 16.dp).testTag("unpair"),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(Modifier.weight(1f)) {
                Text("Unpair", style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.error)
                Supporting("Forget your hub on this phone")
            }
        }

        HorizontalDivider(Modifier.padding(top = 16.dp), color = MaterialTheme.colorScheme.outlineVariant)
        Expandable("About this connection", tag = "about-connection") {
            AboutConnection(creds, ui, device)
        }
        Spacer(Modifier.size(32.dp))
    }
    if (confirmUnpair) {
        AlertDialog(
            onDismissRequest = { confirmUnpair = false },
            title = { Text("Unpair this phone?") },
            text = {
                Text("This phone forgets your hub and deletes its saved data. To revoke it on the hub as well, run `pyre clients` and `pyre revoke client <id>` on a computer.")
            },
            confirmButton = {
                TextButton(onClick = { confirmUnpair = false; device.onUnpair() }, modifier = Modifier.testTag("confirm-unpair")) {
                    Text("Unpair", color = MaterialTheme.colorScheme.error)
                }
            },
            dismissButton = { TextButton(onClick = { confirmUnpair = false }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun QuietHours(p: NotificationPrefs, ui: SettingsUi, vm: SettingsViewModel) {
    val ctx = LocalContext.current
    val is24 = remember { DateFormat.is24HourFormat(ctx) }
    var picking by remember { mutableStateOf<String?>(null) } // "start" | "end"
    var pickingZone by remember { mutableStateOf(false) }
    val on = p.quietHoursStart.isNotEmpty() && p.quietHoursEnd.isNotEmpty()
    val phoneZone = ZoneId.systemDefault().id
    SwitchRow(
        title = "Use quiet hours",
        body = if (on) "Alerts stay silent ${display(p.quietHoursStart, is24)}–${display(p.quietHoursEnd, is24)}." else "Off",
        checked = on,
        tag = "quiet-toggle",
    ) { enable ->
        vm.save(
            if (enable) p.copy(quietHoursStart = "22:00", quietHoursEnd = "07:00", tz = p.tz.ifEmpty { phoneZone })
            else p.copy(quietHoursStart = "", quietHoursEnd = ""),
        )
    }
    if (on) {
        ValueRow("Starts", display(p.quietHoursStart, is24), "quiet-start") { picking = "start" }
        ValueRow("Ends", display(p.quietHoursEnd, is24), "quiet-end") { picking = "end" }
        ValueRow("Time zone", p.tz.ifEmpty { phoneZone } + if (p.tz.isEmpty() || p.tz == phoneZone) " (this phone)" else "", "quiet-tz") { pickingZone = true }
    }
    Text(
        "Alert me about",
        style = MaterialTheme.typography.bodyLarge,
        modifier = Modifier.padding(start = 16.dp, end = 16.dp, top = 12.dp, bottom = 4.dp),
    )
    Column(Modifier.selectableGroup()) {
        listOf("" to "Everything", "normal" to "Normal and urgent", "urgent" to "Only urgent").forEach { (v, label) ->
            RadioRow(label, selected = p.minSeverity == v, tag = "severity-${v.ifEmpty { "all" }}") { if (p.minSeverity != v) vm.save(p.copy(minSeverity = v)) }
        }
    }
    Row(Modifier.padding(horizontal = 16.dp, vertical = 4.dp).heightIn(min = 32.dp), verticalAlignment = Alignment.CenterVertically) {
        when {
            ui.saving -> Supporting("Saving…")
            ui.failed != null -> {
                ToneText(ui.saved.orEmpty(), Tone.ERROR, Modifier.weight(1f).testTag("prefs-status"))
                TextButton(onClick = { vm.save(ui.failed) }) { Text("Retry") }
            }
            ui.saved != null -> Supporting(ui.saved, Modifier.testTag("prefs-status"))
            else -> Supporting("Quiet hours only silence alerts. Everything still shows up in your inbox.")
        }
    }

    picking?.let { which ->
        val current = runCatching { LocalTime.parse(if (which == "start") p.quietHoursStart else p.quietHoursEnd) }.getOrDefault(LocalTime.of(22, 0))
        TimeDialog(
            title = if (which == "start") "Quiet hours start" else "Quiet hours end",
            initial = current,
            is24 = is24,
            onDismiss = { picking = null },
        ) { t ->
            picking = null
            val hhmm = "%02d:%02d".format(t.hour, t.minute)
            vm.save(if (which == "start") p.copy(quietHoursStart = hhmm) else p.copy(quietHoursEnd = hhmm))
        }
    }
    if (pickingZone) {
        ZoneDialog(current = p.tz.ifEmpty { phoneZone }, phoneZone = phoneZone, onDismiss = { pickingZone = false }) { z ->
            pickingZone = false
            vm.save(p.copy(tz = z))
        }
    }
}

private fun display(hhmm: String, is24: Boolean): String = runCatching {
    val t = LocalTime.parse(hhmm)
    if (is24) "%02d:%02d".format(t.hour, t.minute) else t.format(DateTimeFormatter.ofLocalizedTime(FormatStyle.SHORT))
}.getOrDefault(hhmm.ifEmpty { "—" })

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TimeDialog(title: String, initial: LocalTime, is24: Boolean, onDismiss: () -> Unit, onPick: (LocalTime) -> Unit) {
    val state = rememberTimePickerState(initialHour = initial.hour, initialMinute = initial.minute, is24Hour = is24)
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = { TimePicker(state = state, modifier = Modifier.testTag("time-picker")) },
        confirmButton = { TextButton(onClick = { onPick(LocalTime.of(state.hour, state.minute)) }, modifier = Modifier.testTag("time-ok")) { Text("OK") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun ZoneDialog(current: String, phoneZone: String, onDismiss: () -> Unit, onPick: (String) -> Unit) {
    var query by remember { mutableStateOf("") }
    val all = remember { ZoneId.getAvailableZoneIds().filter { '/' in it && !it.startsWith("Etc/") && !it.startsWith("SystemV/") }.sorted() }
    val shown = remember(query) {
        val q = query.trim().replace(' ', '_')
        (listOf(phoneZone) + all.filter { it != phoneZone && (q.isEmpty() || it.contains(q, ignoreCase = true)) }).take(200)
    }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Time zone") },
        text = {
            Column {
                OutlinedTextField(query, { query = it }, Modifier.fillMaxWidth().testTag("tz-search"), placeholder = { Text("Search, e.g. London") }, singleLine = true)
                LazyColumn(Modifier.heightIn(max = 360.dp).padding(top = 8.dp)) {
                    items(shown, key = { it }) { z ->
                        RadioRow(z.replace('_', ' ') + if (z == phoneZone) " (this phone)" else "", selected = z == current) { onPick(z) }
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun AboutConnection(creds: HubCredentials, ui: SettingsUi, device: DeviceControls) {
    val ep = creds.endpoint
    KeyValue("Hub address", ep.baseUrl, mono = true)
    KeyValue(
        "Security",
        when {
            ep.devCleartext -> "Not encrypted. Developer mode for this computer or emulator only."
            ep.pinned -> "Encrypted. This phone trusts only your hub's own certificate."
            else -> "Encrypted with a publicly trusted certificate."
        },
    )
    if (ep.pinned && !ep.devCleartext) KeyValue("Certificate fingerprint", fingerprint(ep.certSha256), mono = true)
    KeyValue("This phone's name", creds.clientName)
    KeyValue("Client ID", creds.clientId, mono = true)
    ui.hello?.let { h ->
        KeyValue("Hub version", h.hubVersion)
        KeyValue("Protocol", if (h.minProtocol == h.protocol) "${h.protocol}" else "${h.minProtocol}–${h.protocol}")
        if (h.mock) KeyValue("Data", "This is a demo hub. Everything it shows is synthetic.")
    }
    ui.helloError?.let { KeyValue("Hub", "Couldn't reach it: $it") }
    KeyValue("Instant push", device.pushStatus)
}

/** "110F 10D7 37E9 …" — groups of four so a person can compare it with their computer. */
fun fingerprint(hex: String): String = hex.trim().removePrefix("sha256:").removePrefix("SHA256:").filter { it.isLetterOrDigit() }.uppercase().chunked(4).joinToString(" ")

@Composable
private fun SwitchRow(title: String, body: String?, checked: Boolean, enabled: Boolean = true, tag: String? = null, onChange: (Boolean) -> Unit) {
    Row(
        Modifier.fillMaxWidth()
            .toggleable(value = checked, enabled = enabled, role = Role.Switch, onValueChange = onChange)
            .heightIn(min = 56.dp)
            .padding(horizontal = 16.dp, vertical = 12.dp)
            .let { if (tag != null) it.testTag(tag) else it },
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            if (!body.isNullOrEmpty()) Supporting(body)
        }
        Spacer(Modifier.width(16.dp))
        Switch(checked = checked, onCheckedChange = null, enabled = enabled)
    }
}

@Composable
private fun ValueRow(title: String, value: String, tag: String, onClick: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().clickable(role = Role.Button, onClick = onClick).heightIn(min = 56.dp).padding(horizontal = 16.dp, vertical = 8.dp).testTag(tag),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(title, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.weight(1f))
        Text(value, style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.primary)
    }
}

@Composable
private fun RadioRow(label: String, selected: Boolean, tag: String? = null, onClick: () -> Unit) {
    Row(
        Modifier.fillMaxWidth()
            .selectable(selected = selected, role = Role.RadioButton, onClick = onClick)
            .heightIn(min = 48.dp)
            .padding(horizontal = 16.dp)
            .let { if (tag != null) it.testTag(tag) else it },
        verticalAlignment = Alignment.CenterVertically,
    ) {
        RadioButton(selected = selected, onClick = null)
        Spacer(Modifier.width(16.dp))
        Text(label, style = MaterialTheme.typography.bodyLarge)
    }
}
