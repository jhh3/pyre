package dev.jhh3.pyre.ui.pairing

import android.os.Build
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.R
import dev.jhh3.pyre.data.HubCredentials
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.pairing.PairingOutcome
import dev.jhh3.pyre.pairing.PairingService
import dev.jhh3.pyre.pairing.PairingUri
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.common.ToneText
import dev.jhh3.pyre.ui.settings.fingerprint
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull

data class PairingUi(
    val link: String = "",
    val parsed: PairingUri? = null,
    val parseError: String? = null,
    val clientName: String = "Android " + (Build.MODEL ?: "phone"),
    val devCleartext: Boolean = false,
    val busy: Boolean = false,
    val error: String? = null,
    val warning: String? = null,
    val paired: HubCredentials? = null,
)

/** Paste or open a `pyre://pair?...` link, confirm, then exchange the one-time code. */
class PairingViewModel(private val service: PairingService, prefill: String?, devAllowed: Boolean) : ViewModel() {
    private val _ui = MutableStateFlow(PairingUi(devCleartext = devAllowed))
    val ui: StateFlow<PairingUi> = _ui.asStateFlow()

    init { prefill?.let { setLink(it) } }

    fun setLink(s: String) {
        val r = if (s.isBlank()) null else PairingUri.parse(s)
        _ui.value = _ui.value.copy(link = s, parsed = r?.getOrNull(), parseError = r?.exceptionOrNull()?.message, error = null)
    }

    fun setName(s: String) { _ui.value = _ui.value.copy(clientName = s) }
    fun setDev(v: Boolean) { _ui.value = _ui.value.copy(devCleartext = v) }

    fun pair() {
        val p = _ui.value.parsed ?: return
        if (_ui.value.busy) return
        _ui.value = _ui.value.copy(busy = true, error = null)
        viewModelScope.launch {
            _ui.value = when (val r = service.pair(p, _ui.value.clientName.trim().ifEmpty { "Android" }, _ui.value.devCleartext)) {
                is PairingOutcome.Paired -> _ui.value.copy(busy = false, paired = r.credentials, warning = r.warning)
                is PairingOutcome.Failed -> _ui.value.copy(busy = false, error = r.message)
            }
        }
    }
}

/**
 * First run: get the link (open it on the phone, or paste it), confirm the
 * hub and its certificate in plain words, connect. The one-time code in the
 * link is never shown on screen.
 */
@Composable
fun PairingScreen(vm: PairingViewModel, onCancel: (() -> Unit)? = null, onPaired: (HubCredentials) -> Unit) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    LaunchedEffect(ui.paired) { ui.paired?.let(onPaired) }
    Box(Modifier.fillMaxSize().safeDrawingPadding().imePadding(), contentAlignment = Alignment.TopCenter) {
        Column(
            Modifier.widthIn(max = 560.dp).fillMaxWidth().verticalScroll(rememberScrollState()).padding(horizontal = 24.dp, vertical = 16.dp)
                .testTag("pairing-screen"),
        ) {
            if (onCancel != null) {
                TextButton(onClick = onCancel, modifier = Modifier.heightIn(min = 48.dp)) { Text("Cancel") }
            }
            val p = ui.parsed
            if (p == null) GetLink(ui, vm) else Confirm(p, ui, vm)
        }
    }
}

@Composable
private fun Mark() {
    Box(
        Modifier.size(72.dp).background(MaterialTheme.colorScheme.primaryContainer, CircleShape),
        contentAlignment = Alignment.Center,
    ) {
        Image(painterResource(R.drawable.ic_launcher_foreground), contentDescription = null, modifier = Modifier.size(72.dp))
    }
}

@Composable
private fun GetLink(ui: PairingUi, vm: PairingViewModel) {
    val clipboard = LocalClipboardManager.current
    var typing by remember { mutableStateOf(ui.link.isNotEmpty()) }
    Spacer(Modifier.size(24.dp))
    Mark()
    Spacer(Modifier.size(24.dp))
    Text("Connect to your hub", style = MaterialTheme.typography.headlineMedium, modifier = Modifier.semantics { heading() })
    Spacer(Modifier.size(12.dp))
    Text(
        "On a computer that runs Pyre, create a pairing link:",
        style = MaterialTheme.typography.bodyLarge,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
    Surface(color = MaterialTheme.colorScheme.surfaceContainerHigh, shape = RoundedCornerShape(12.dp), modifier = Modifier.fillMaxWidth().padding(vertical = 12.dp)) {
        Text("pyre pair-client --kind android", fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.padding(16.dp))
    }
    Text(
        "Then open the link on this phone, or copy it and paste it here.",
        style = MaterialTheme.typography.bodyLarge,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
    Spacer(Modifier.size(24.dp))
    Button(
        onClick = {
            val text = clipboard.getText()?.text.orEmpty()
            typing = true
            vm.setLink(text)
        },
        modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp).testTag("paste-link"),
    ) {
        PyreIcon(R.drawable.ic_content_paste, null, Modifier.size(20.dp))
        Spacer(Modifier.width(10.dp))
        Text("Paste link")
    }
    if (!typing) {
        TextButton(onClick = { typing = true }, modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp)) { Text("Type it instead") }
    } else {
        OutlinedTextField(
            ui.link,
            vm::setLink,
            Modifier.fillMaxWidth().padding(top = 16.dp).testTag("pair-link"),
            label = { Text("Pairing link") },
            placeholder = { Text("pyre://pair?…") },
            visualTransformation = PasswordVisualTransformation(),
            singleLine = true,
            isError = ui.parseError != null,
            supportingText = { ui.parseError?.let { Text(it) } ?: Text("Starts with pyre://pair") },
        )
    }
    Spacer(Modifier.size(16.dp))
    Supporting("The link works once and expires soon. Treat it like a password.")
}

@Composable
private fun Confirm(p: PairingUri, ui: PairingUi, vm: PairingViewModel) {
    val cleartext = p.hub.trim().startsWith("http://", ignoreCase = true)
    val ep = HubEndpoint.parse(p.hub, p.pin, ui.devCleartext)
    val url = p.hub.toHttpUrlOrNull()
    Spacer(Modifier.size(16.dp))
    Text("Connect to this hub?", style = MaterialTheme.typography.headlineMedium, modifier = Modifier.semantics { heading() })
    Spacer(Modifier.size(20.dp))
    Surface(color = MaterialTheme.colorScheme.surfaceContainerLow, shape = RoundedCornerShape(16.dp), modifier = Modifier.fillMaxWidth()) {
        Column(Modifier.padding(vertical = 8.dp)) {
            Fact(R.drawable.ic_dns, "Hub", url?.let { it.host + ":" + it.port } ?: p.hub, url?.encodedPath?.takeIf { it.length > 1 }?.let { p.hub })
            Fact(
                R.drawable.ic_lock,
                "Security",
                when {
                    cleartext -> "Not encrypted"
                    p.pin.isNotEmpty() -> "Encrypted. This phone will trust only this certificate:"
                    else -> "Encrypted with a publicly trusted certificate"
                },
                if (p.pin.isNotEmpty() && !cleartext) fingerprint(p.pin) else null,
                mono = true,
            )
            if (p.pin.isNotEmpty() && !cleartext) {
                Supporting("It should match the fingerprint your computer shows.", Modifier.padding(start = 56.dp, end = 16.dp, bottom = 8.dp))
            }
        }
    }
    OutlinedTextField(
        ui.clientName,
        vm::setName,
        Modifier.fillMaxWidth().padding(top = 20.dp).testTag("client-name"),
        label = { Text("Name this phone") },
        supportingText = { Text("Shown on your hub's list of devices") },
        singleLine = true,
    )
    if (cleartext) {
        Row(
            Modifier.fillMaxWidth().padding(top = 12.dp)
                .toggleable(value = ui.devCleartext, role = Role.Switch, onValueChange = vm::setDev)
                .heightIn(min = 56.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(Modifier.weight(1f)) {
                Text("Allow an unencrypted connection", style = MaterialTheme.typography.bodyLarge)
                Supporting("Developer use only: localhost or the emulator's 10.0.2.2. Never for a real hub.")
            }
            Spacer(Modifier.width(16.dp))
            Switch(ui.devCleartext, null, Modifier.testTag("dev-cleartext"))
        }
    }
    ep.exceptionOrNull()?.let { ToneText(it.message.orEmpty(), Tone.ERROR, Modifier.padding(top = 12.dp)) }
    ui.error?.let { Banner(it, Tone.ERROR, Modifier.padding(top = 12.dp)) }
    ui.warning?.let { Banner(it, Tone.MUTED, Modifier.padding(top = 12.dp)) }
    Spacer(Modifier.size(24.dp))
    Button(
        onClick = vm::pair,
        enabled = !ui.busy && ep.isSuccess,
        modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp).testTag("pair-confirm"),
    ) { Text(if (ui.busy) "Connecting…" else "Connect") }
    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.Center) {
        TextButton(onClick = { vm.setLink("") }, enabled = !ui.busy, modifier = Modifier.heightIn(min = 48.dp)) { Text("Use a different link") }
    }
}

@Composable
private fun Fact(icon: Int, label: String, value: String, detail: String?, mono: Boolean = false) {
    Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 10.dp).semantics(mergeDescendants = true) {}, verticalAlignment = Alignment.Top) {
        PyreIcon(icon, null, Modifier.padding(top = 2.dp).size(24.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Spacer(Modifier.width(16.dp))
        Column(Modifier.weight(1f)) {
            Text(label, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Text(value, style = MaterialTheme.typography.bodyLarge)
            if (detail != null) {
                Text(
                    detail,
                    style = MaterialTheme.typography.bodyMedium,
                    fontFamily = if (mono) FontFamily.Monospace else null,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 2.dp),
                )
            }
        }
    }
}
