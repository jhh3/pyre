package dev.jhh3.pyre.ui.manage

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.model.CommandOutcome
import dev.jhh3.pyre.model.ConnectionState
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.NodeActionRequest
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.serialization.KSerializer
import kotlinx.serialization.Serializable

// Remote device login (internal/devlogin): the machine runs `codex login
// --device-auth` in a private temporary home and only the link, the one-time
// code, the state and the account email come back. The credential never
// leaves the machine.

object DeviceLoginActions {
    const val START = "account.device_login"
    const val STATUS = "account.device_login_status"
    const val CANCEL = "account.device_login_cancel"
}

@Serializable
data class DeviceLoginParams(val provider: String)

@Serializable
data class DeviceLoginStatusParams(val loginId: String, val waitSeconds: Int = 0)

@Serializable
data class DeviceLoginView(
    val loginId: String = "",
    val provider: String = "",
    val nodeId: String = "",
    val state: String = "",
    val verificationUrl: String = "",
    val userCode: String = "",
    val email: String = "",
    val rotaName: String = "",
    val replaced: Boolean = false,
    val liveLogin: Boolean = false,
    val detail: String = "",
) {
    val done: Boolean get() = state in setOf("completed", "failed", "expired", "cancelled")

    /** Plain-words status, matching the CLI and TUI. */
    val describe: String get() = when (state) {
        "starting" -> "Starting the login on the machine…"
        "waiting" -> "Waiting for you to approve the code."
        "completed" -> "Logged in as $email; saved on the machine as rota account $rotaName." +
            (if (replaced) " The earlier copy was kept as a backup." else "") +
            (if (liveLogin) " This machine is already logged in to that account; Pyre already uses it." else "")
        "expired" -> "The code expired before it was approved; start again."
        "cancelled" -> "Cancelled; nothing was saved."
        "failed" -> "Login failed" + (if (detail.isNotEmpty()) ": $detail" else ".")
        else -> state
    }
}

data class DeviceLoginUi(
    val node: Node? = null,
    val view: DeviceLoginView? = null,
    val error: String? = null,
)

/** Drives one login: start, then status reads (long-polled on the node) until it ends. */
class DeviceLoginController(
    private val repo: Repository,
    private val scope: CoroutineScope,
    private val paceMillis: Long = 1000,
) {
    private val _ui = MutableStateFlow(DeviceLoginUi())
    val ui: StateFlow<DeviceLoginUi> = _ui.asStateFlow()
    private var job: Job? = null

    fun start(node: Node) {
        job?.cancel()
        _ui.value = DeviceLoginUi(node = node, view = DeviceLoginView(state = "starting"))
        job = scope.launch {
            try {
                var v = action(node.nodeId, DeviceLoginActions.START, DeviceLoginParams.serializer(), DeviceLoginParams("openai"), 60)
                _ui.update { it.copy(view = v) }
                while (!v.done) {
                    delay(paceMillis)
                    v = action(node.nodeId, DeviceLoginActions.STATUS, DeviceLoginStatusParams.serializer(), DeviceLoginStatusParams(v.loginId, 8), 30)
                    _ui.update { it.copy(view = v) }
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _ui.update { it.copy(error = e.message ?: "The machine did not answer.") }
            }
        }
    }

    /** Closes the dialog; a login still waiting is cancelled on the machine. */
    fun dismiss() {
        val s = _ui.value
        job?.cancel()
        val v = s.view
        val node = s.node
        if (node != null && v != null && v.loginId.isNotEmpty() && !v.done) {
            scope.launch {
                runCatching { action(node.nodeId, DeviceLoginActions.CANCEL, DeviceLoginStatusParams.serializer(), DeviceLoginStatusParams(v.loginId), 20) }
            }
        }
        _ui.value = DeviceLoginUi()
    }

    private suspend fun <P> action(nodeId: String, action: String, pSer: KSerializer<P>, params: P, timeout: Int): DeviceLoginView {
        val res = repo.call {
            nodeAction(NodeActionRequest(operationId = Ids.operation(), nodeId = nodeId, action = action, params = PyreJson.wire.encodeToJsonElement(pSer, params), timeoutSeconds = timeout))
        }
        res.error?.let { throw IllegalStateException(it.message.ifEmpty { it.code }) }
        if (res.outcome != CommandOutcome.SUCCEEDED || res.result == null) throw IllegalStateException("The machine did not answer in time (${res.outcome}).")
        return PyreJson.wire.decodeFromJsonElement(DeviceLoginView.serializer(), res.result)
    }
}

/** "Add login on a machine": pick a machine, then show the code and a button that opens the link. */
@Composable
fun DeviceLoginDialog(controller: DeviceLoginController, nodes: List<Node>, onClose: () -> Unit) {
    val ui by controller.ui.collectAsStateWithLifecycle()
    val uri = LocalUriHandler.current
    val close = { controller.dismiss(); onClose() }
    val v = ui.view
    AlertDialog(
        onDismissRequest = close,
        modifier = Modifier.testTag("device-login"),
        title = { Text(if (ui.node == null) "Add login on a machine" else "Log ${ui.node!!.name.ifEmpty { ui.node!!.nodeId }} into Codex") },
        text = {
            Column(Modifier.fillMaxWidth()) {
                when {
                    ui.node == null -> {
                        Text("The machine signs in itself with a one-time code; the login never leaves it.", style = MaterialTheme.typography.bodyMedium)
                        Spacer(Modifier.height(8.dp))
                        val online = nodes.filter { it.connection.state == ConnectionState.CONNECTED }
                        if (online.isEmpty()) Text("No machine is online.", color = MaterialTheme.colorScheme.onSurfaceVariant)
                        online.forEach { n ->
                            TextButton(onClick = { controller.start(n) }) { Text(n.name.ifEmpty { n.nodeId }) }
                        }
                    }
                    ui.error != null -> Text(ui.error!!, color = MaterialTheme.colorScheme.error)
                    v != null && v.state == "waiting" -> {
                        Text("1. Open the link and sign in", style = MaterialTheme.typography.bodyMedium)
                        Text(v.verificationUrl, color = MaterialTheme.colorScheme.primary, style = MaterialTheme.typography.bodySmall)
                        Spacer(Modifier.height(12.dp))
                        Text("2. Enter this code", style = MaterialTheme.typography.bodyMedium)
                        Text(
                            v.userCode,
                            fontFamily = FontFamily.Monospace,
                            fontWeight = FontWeight.Bold,
                            style = MaterialTheme.typography.headlineMedium,
                            modifier = Modifier.testTag("device-login-code"),
                        )
                        Spacer(Modifier.height(12.dp))
                        Button(onClick = { uri.openUri(v.verificationUrl) }) { Text("Open link") }
                        Spacer(Modifier.height(8.dp))
                        Text(v.describe, color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodySmall)
                    }
                    v != null -> Text(
                        v.describe,
                        color = if (v.state == "completed") MaterialTheme.colorScheme.tertiary else MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        },
        confirmButton = {
            TextButton(onClick = close) { Text(if (v != null && !v.done && ui.error == null) "Cancel login" else "Close") }
        },
    )
}
