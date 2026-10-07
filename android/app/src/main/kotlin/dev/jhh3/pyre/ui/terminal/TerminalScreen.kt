package dev.jhh3.pyre.ui.terminal

import android.graphics.Typeface
import androidx.compose.foundation.background
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.ime
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.union
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.text.style.TextOverflow
import dev.jhh3.pyre.R
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.StatusDot
import dev.jhh3.pyre.ui.common.Supporting
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import com.termux.view.TerminalView
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.AttachDescriptor
import dev.jhh3.pyre.model.AttachMechanism
import dev.jhh3.pyre.model.AttachRequest
import dev.jhh3.pyre.model.LeaseRequest
import dev.jhh3.pyre.model.ReadSessionRequest
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.terminal.RemoteTerminal
import dev.jhh3.pyre.terminal.TerminalSocket
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.LoadingBox
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

data class TerminalUi(
    val loading: Boolean = true,
    val descriptor: AttachDescriptor? = null,
    /** Bumped for every new RemoteTerminal so the TerminalView re-attaches. */
    val generation: Int = 0,
    val showTerminal: Boolean = false,
    val connected: Boolean = false,
    val status: String? = null,
    val error: String? = null,
    val fontPx: Int = 28,
)

/**
 * Terminal access to a run. A `web_terminal` descriptor streams the real
 * tmux attach through the hub relay into a Termux terminal emulator (colour,
 * box drawing, wide characters, alternate screen, scrollback); `ssh_tmux`
 * shows the command; `read_only` renders the latest screen snapshot.
 */
class TerminalViewModel(
    private val repo: Repository,
    val sessionId: String,
    private val connector: ((AttachDescriptor, TerminalSocket.Listener) -> TerminalSocket)?,
    private val terminalFactory: () -> RemoteTerminal = { RemoteTerminal() },
) : ViewModel() {
    private val _ui = MutableStateFlow(TerminalUi())
    val ui: StateFlow<TerminalUi> = _ui.asStateFlow()
    var terminal: RemoteTerminal = terminalFactory()
        private set
    private var socket: TerminalSocket? = null

    init { attach() }

    fun attach() = viewModelScope.launch {
        socket?.close()
        socket = null
        terminal = terminalFactory()
        _ui.value = TerminalUi(loading = true, generation = _ui.value.generation + 1, fontPx = _ui.value.fontPx)
        try {
            val d = repo.call {
                attach(AttachRequest(operationId = Ids.operation(), sessionId = sessionId, preferred = PREFERRED))
            }
            _ui.value = _ui.value.copy(loading = false, descriptor = d)
            when (d.mechanism) {
                AttachMechanism.WEB_TERMINAL -> connect(d)
                AttachMechanism.READ_ONLY -> snapshot()
                else -> Unit
            }
        } catch (e: PyreApiException) {
            _ui.value = _ui.value.copy(loading = false, error = e.describe())
            snapshot()
        }
    }

    private fun connect(d: AttachDescriptor) {
        val c = connector ?: run { _ui.value = _ui.value.copy(status = "Web terminal not available in this build."); return }
        val term = terminal
        term.setReadOnly(d.readOnly)
        val listener = Listener(term)
        val s = try {
            c(d, listener)
        } catch (e: IllegalArgumentException) {
            _ui.value = _ui.value.copy(error = e.message)
            return
        }
        socket = s
        term.sink = object : RemoteTerminal.Sink {
            override fun write(data: ByteArray, offset: Int, count: Int) = s.sendBytes(data, offset, count)
            override fun resize(columns: Int, rows: Int) = s.resize(columns, rows)
        }
        s.connect(term.columns.takeIf { it > 0 } ?: 80, term.rows.takeIf { it > 0 } ?: 24)
        _ui.value = _ui.value.copy(showTerminal = true, status = "Connecting…")
    }

    private fun snapshot() = viewModelScope.launch {
        try {
            val r = repo.call { readSession(ReadSessionRequest(sessionId = sessionId, tail = true, limit = 1)) }
            if (r.screen.isNotEmpty()) {
                terminal.setReadOnly(true)
                terminal.feed(("\u001b[2J\u001b[H" + r.screen.replace("\r\n", "\n").replace("\n", "\r\n")).toByteArray())
                _ui.value = _ui.value.copy(showTerminal = true, status = "Read-only snapshot")
            }
        } catch (_: PyreApiException) {
        }
    }

    fun takeOverInput() {
        val d = _ui.value.descriptor ?: return
        viewModelScope.launch {
            try {
                repo.call { acquireLease(LeaseRequest(operationId = Ids.operation(), runId = d.runId, takeover = true)) }
                attach()
            } catch (e: PyreApiException) {
                _ui.value = _ui.value.copy(error = e.describe())
            }
        }
    }

    fun setFont(px: Int) { _ui.value = _ui.value.copy(fontPx = px) }

    private inner class Listener(private val term: RemoteTerminal) : TerminalSocket.Listener {
        override fun onOpen() {
            if (term === terminal) _ui.value = _ui.value.copy(connected = true, status = "Connected")
            if (term.columns > 0) socket?.resize(term.columns, term.rows)
        }

        override fun onOutput(bytes: ByteArray) = term.feed(bytes)

        override fun onClosed(reason: String, error: PyreApiException?) {
            term.finish(if (error == null) "session closed" else "disconnected")
            if (term !== terminal) return
            _ui.value = _ui.value.copy(
                connected = false,
                status = if (error == null) "Session closed" else "Disconnected",
                error = error?.let { terminalError(it) },
            )
        }
    }

    override fun onCleared() {
        socket?.close()
        terminal.onScreenUpdated = null
    }

    companion object {
        /**
         * web_terminal first. local_tmux is listed because the hub's web relay
         * asks the run's own node for its local attach (client node = run node)
         * and converts it to a web descriptor; the node's tmux adapter treats
         * "ssh_tmux without local_tmux" as a remote request needing an ssh
         * alias. A phone is never the run's node, so a plain local_tmux
         * descriptor is not otherwise returned to it.
         */
        val PREFERRED = listOf(AttachMechanism.WEB_TERMINAL, AttachMechanism.LOCAL_TMUX, AttachMechanism.SSH_TMUX, AttachMechanism.READ_ONLY)

        fun terminalError(e: PyreApiException): String = when (e.httpStatus) {
            401 -> "Terminal ticket rejected or expired (401). Reopen the terminal to get a new ticket."
            409 -> "Terminal conflict (409): ${e.message}. Another client may hold the input lease."
            410 -> "This run's terminal is gone (410): ${e.message}."
            503 -> "Machine unreachable (503): ${e.message}. The run may still be alive."
            else -> e.describe()
        }
    }
}

/**
 * Full-screen terminal: a slim header (back, what this is, live state), the
 * real harness TUI edge to edge, and the extra-keys row above the keyboard.
 */
@Composable
fun TerminalScreen(vm: TerminalViewModel, onBack: () -> Unit = {}) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    val clipboard = LocalClipboardManager.current
    val d = ui.descriptor
    val interactive = d?.mechanism == AttachMechanism.WEB_TERMINAL && !d.readOnly
    Column(
        Modifier.fillMaxSize().background(MaterialTheme.colorScheme.surface)
            .windowInsetsPadding(WindowInsets.ime.union(WindowInsets.navigationBars))
            .testTag("terminal-screen"),
    ) {
        Surface(color = MaterialTheme.colorScheme.surface) {
            Row(Modifier.fillMaxWidth().statusBarsPadding().heightIn(min = 56.dp).padding(end = 4.dp), verticalAlignment = Alignment.CenterVertically) {
                IconButton(onClick = onBack, modifier = Modifier.testTag("back")) { PyreIcon(R.drawable.ic_arrow_back, "Back") }
                Column(Modifier.weight(1f)) {
                    Text("Terminal", style = MaterialTheme.typography.titleMedium)
                    val (tone, word) = when {
                        ui.loading -> Tone.MUTED to "Opening…"
                        ui.connected && interactive -> Tone.ACTIVE to "Live · keys go straight to the session"
                        ui.connected -> Tone.MUTED to "Live · view only"
                        ui.showTerminal -> Tone.MUTED to (ui.status ?: "Read-only")
                        d?.mechanism == AttachMechanism.SSH_TMUX || d?.mechanism == AttachMechanism.LOCAL_TMUX -> Tone.MUTED to "Opens in a terminal app"
                        else -> Tone.MUTED to (ui.status ?: "Not connected")
                    }
                    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.semantics(mergeDescendants = true) {}.testTag("terminal-status")) {
                        StatusDot(tone, size = 8)
                        Spacer(Modifier.width(6.dp))
                        Text(word, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                }
                TerminalMenu(ui, vm)
            }
        }
        ui.error?.let { Banner(it, Tone.ERROR, Modifier.testTag("terminal-error")) { TextButton(onClick = { vm.attach() }) { Text("Reopen") } } }
        if (ui.loading) {
            LoadingBox()
            return@Column
        }
        d?.lease?.let { l ->
            Banner("${l.holder?.name?.ifEmpty { l.holder.id } ?: "Someone"} is typing in this terminal until ${Times.clock(l.expiresAt).substringAfterLast(' ')}.", Tone.MUTED) {
                TextButton(onClick = vm::takeOverInput) { Text("Take over") }
            }
        }
        when (d?.mechanism) {
            AttachMechanism.SSH_TMUX, AttachMechanism.LOCAL_TMUX -> {
                val cmd = d.command.joinToString(" ")
                Column(Modifier.padding(16.dp)) {
                    Text("Open it from a terminal app", style = MaterialTheme.typography.titleMedium)
                    Supporting("This session is reachable over SSH and tmux. Run this in a terminal app such as Termux:", Modifier.padding(top = 4.dp))
                    Surface(color = MaterialTheme.colorScheme.surfaceContainerHigh, shape = RoundedCornerShape(12.dp), modifier = Modifier.fillMaxWidth().padding(vertical = 12.dp)) {
                        Text(cmd, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.padding(16.dp).testTag("ssh-command"))
                    }
                    FilledTonalButton(onClick = { clipboard.setText(AnnotatedString(cmd)) }, modifier = Modifier.heightIn(min = 48.dp)) { Text("Copy command") }
                    if (d.notes.isNotEmpty()) Supporting(d.notes, Modifier.padding(top = 12.dp))
                }
            }
            else -> Unit
        }
        if (ui.showTerminal) {
            key(ui.generation) { TerminalPane(vm, ui.fontPx, Modifier.weight(1f)) }
            if (interactive) ExtraKeys(vm.terminal, ui.connected)
        } else if (d == null && ui.error != null) {
            Supporting("No way to open this terminal right now.", Modifier.padding(16.dp))
        }
    }
}

@Composable
private fun TerminalMenu(ui: TerminalUi, vm: TerminalViewModel) {
    var open by remember { mutableStateOf(false) }
    Box {
        IconButton(onClick = { open = true }, modifier = Modifier.testTag("terminal-menu")) { PyreIcon(R.drawable.ic_more_vert, "Terminal options") }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            DropdownMenuItem(text = { Text("Reconnect") }, onClick = { open = false; vm.attach() })
            if (ui.descriptor?.lease != null) DropdownMenuItem(text = { Text("Take over typing") }, onClick = { open = false; vm.takeOverInput() })
        }
    }
}

@Composable
private fun TerminalPane(vm: TerminalViewModel, fontPx: Int, modifier: Modifier) {
    val terminal = vm.terminal
    val holder = remember(terminal) { arrayOfNulls<TerminalView>(1) }
    val client = remember(terminal) { PyreTerminalViewClient(terminal, { holder[0] }, fontPx, vm::setFont) }
    DisposableEffect(terminal) {
        terminal.onScreenUpdated = { holder[0]?.onScreenUpdated() }
        onDispose {
            terminal.onScreenUpdated = null
            holder[0] = null
        }
    }
    AndroidView(
        factory = { ctx ->
            TerminalView(ctx, null).apply {
                setTerminalViewClient(client)
                setTextSize(fontPx) // creates the renderer; must precede setTypeface
                setTypeface(Typeface.MONOSPACE)
                // The renderer paints only non-default cell backgrounds.
                setBackgroundColor(android.graphics.Color.BLACK)
                isFocusable = true
                isFocusableInTouchMode = true
                keepScreenOn = true
                attachSession(terminal.session)
                holder[0] = this
            }
        },
        modifier = modifier.fillMaxWidth().background(Color.Black).testTag("terminal-view"),
    )
}

/** Soft-keyboard companion row: sticky Ctrl/Alt, Esc, Tab, arrows, paging (48dp keys). */
@Composable
private fun ExtraKeys(terminal: RemoteTerminal, enabled: Boolean) {
    val mods by terminal.modifiers.collectAsStateWithLifecycle()
    Surface(color = MaterialTheme.colorScheme.surfaceContainer) {
        Row(Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(horizontal = 4.dp).testTag("extra-keys")) {
            @Composable
            fun key(label: String, tag: String, description: String, active: Boolean = false, onClick: () -> Unit) {
                TextButton(
                    onClick = onClick,
                    enabled = enabled,
                    modifier = Modifier.heightIn(min = 48.dp).widthIn(min = 48.dp).testTag("key-$tag").semantics {
                        contentDescription = description
                        if (active) stateDescription = "On"
                    },
                    colors = if (active) ButtonDefaults.textButtonColors(containerColor = MaterialTheme.colorScheme.primaryContainer) else ButtonDefaults.textButtonColors(),
                ) {
                    Text(label, fontFamily = FontFamily.Monospace, fontWeight = if (active) FontWeight.Bold else null)
                }
            }
            key("Esc", "esc", "Escape") { terminal.sendKey(RemoteTerminal.ESC) }
            key("Ctrl", "ctrl", "Control (sticky)", mods.ctrl) { terminal.toggleCtrl() }
            key("Alt", "alt", "Alt (sticky)", mods.alt) { terminal.toggleAlt() }
            key("Tab", "tab", "Tab") { terminal.sendKey(RemoteTerminal.TAB) }
            key("↑", "up", "Up arrow") { terminal.sendKey(RemoteTerminal.UP) }
            key("↓", "down", "Down arrow") { terminal.sendKey(RemoteTerminal.DOWN) }
            key("←", "left", "Left arrow") { terminal.sendKey(RemoteTerminal.LEFT) }
            key("→", "right", "Right arrow") { terminal.sendKey(RemoteTerminal.RIGHT) }
            key("^C", "ctrl-c", "Control C") { terminal.sendCodePoint(3) }
            key("Home", "home", "Home") { terminal.sendKey(RemoteTerminal.HOME) }
            key("End", "end", "End") { terminal.sendKey(RemoteTerminal.END) }
            key("PgUp", "pgup", "Page up") { terminal.sendKey(RemoteTerminal.PAGE_UP) }
            key("PgDn", "pgdn", "Page down") { terminal.sendKey(RemoteTerminal.PAGE_DOWN) }
        }
    }
}
