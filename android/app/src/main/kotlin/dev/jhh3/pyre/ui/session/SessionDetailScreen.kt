package dev.jhh3.pyre.ui.session

import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.text.style.TextAlign
import kotlinx.coroutines.launch
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.foundation.layout.Box
import androidx.compose.material3.Checkbox
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.IconButton
import androidx.compose.material3.FilledIconButton
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import dev.jhh3.pyre.R
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.domain.Delivery
import dev.jhh3.pyre.domain.Identity
import dev.jhh3.pyre.domain.Pins
import dev.jhh3.pyre.domain.Status
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.Approval
import dev.jhh3.pyre.model.DeliveryState
import dev.jhh3.pyre.model.MessageMode
import dev.jhh3.pyre.model.MessageView
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.model.TranscriptEntry
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.CachedBanner
import dev.jhh3.pyre.ui.common.CardBlock
import dev.jhh3.pyre.ui.common.ErrorBox
import dev.jhh3.pyre.ui.common.Expandable
import dev.jhh3.pyre.ui.common.KeyValue
import dev.jhh3.pyre.ui.common.LoadingBox
import dev.jhh3.pyre.ui.common.Pill
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.RefreshWhileResumed
import dev.jhh3.pyre.ui.common.ScreenTitle
import dev.jhh3.pyre.ui.common.SectionHeader
import dev.jhh3.pyre.ui.common.StatusDot
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.common.shortId
import dev.jhh3.pyre.ui.theme.toneColor

/**
 * Session detail as a chat: a one-line status strip, the conversation
 * (opens at the newest message and stays pinned there while you are at the
 * bottom), anything waiting for you right above the reply box, and quick
 * actions (answer, terminal, interrupt) in the bottom bar. The terminal (the
 * real harness TUI) is one tap away and kept separate from chat.
 */
@Composable
fun SessionDetailScreen(
    vm: SessionDetailViewModel,
    onOpenApproval: (String) -> Unit,
    onOpenTerminal: (String) -> Unit,
    onOpenLink: (String) -> Unit = {},
) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    RefreshWhileResumed { vm.poll() }
    var confirmInterrupt by remember { mutableStateOf(false) }
    var renaming by remember { mutableStateOf(false) }
    val composerFocus = remember { FocusRequester() }
    ScreenTitle((ui.session as? Load.Ready)?.value?.session?.title?.ifEmpty { null })
    Column(Modifier.fillMaxSize().imePadding()) {
        when (val s = ui.session) {
            Load.Loading -> LoadingBox()
            is Load.Failed -> ErrorBox(s.error.describe(), vm::refresh)
            is Load.Ready -> {
                StatusStrip(s.value, ui, onRename = { renaming = true }, onTogglePin = vm::togglePin)
                ui.messageForYou?.let { m ->
                    MessageForYouCard(
                        m,
                        ui,
                        canReply = ui.canQueue || ui.canSteer,
                        onReply = {
                            vm.startReply()
                            runCatching { composerFocus.requestFocus() }
                        },
                        onDismiss = vm::dismissMessage,
                    )
                }
                Box(Modifier.weight(1f)) {
                    ChatList(s, ui, vm, onOpenApproval, onOpenLink)
                }
                ui.notice?.let { Banner(it, Tone.MUTED, Modifier.testTag("notice")) { TextButton(onClick = vm::dismissNotice) { Text("OK") } } }
                BottomBar(s.value, ui, vm, onOpenApproval, onOpenTerminal, onInterrupt = { confirmInterrupt = true }, composerFocus = composerFocus)
            }
        }
    }
    ui.reconcile?.let { ReconcileDialog(it, ui, vm) }
    if (renaming) {
        (ui.session as? Load.Ready)?.value?.let { v -> RenameDialog(v.session.title, onDismiss = { renaming = false }, onSave = { if (vm.rename(it)) renaming = false }) }
    }
    if (confirmInterrupt) {
        AlertDialog(
            onDismissRequest = { confirmInterrupt = false },
            title = { Text("Interrupt this session?") },
            text = { Text("Stops the agent's current turn, like pressing Esc in its terminal. The session keeps running.") },
            confirmButton = { TextButton(onClick = { confirmInterrupt = false; vm.interrupt() }, modifier = Modifier.testTag("confirm-interrupt")) { Text("Interrupt") } },
            dismissButton = { TextButton(onClick = { confirmInterrupt = false }) { Text("Cancel") } },
        )
    }
}

/**
 * The conversation. A reversed LazyColumn (index 0 is the bottom), so it
 * opens at the newest item with no jump. While the user is at the bottom new
 * items keep it pinned there; once they scroll up, new items show a
 * "New messages" pill instead of moving the page. Reaching the top loads
 * older activity.
 */
@Composable
private fun ChatList(
    s: Load.Ready<SessionView>,
    ui: SessionDetailUi,
    vm: SessionDetailViewModel,
    onOpenApproval: (String) -> Unit,
    onOpenLink: (String) -> Unit,
) {
    val state = rememberLazyListState()
    val scope = rememberCoroutineScope()
    // Pinned changes only on scrolls (the user's or ours), never because
    // items were inserted, so an arriving item cannot unpin the list.
    var pinned by remember { mutableStateOf(true) }
    var unseen by remember { mutableStateOf(false) }
    val atBottom by remember { derivedStateOf { state.firstVisibleItemIndex == 0 && state.firstVisibleItemScrollOffset < 48 } }
    LaunchedEffect(state) {
        snapshotFlow { state.isScrollInProgress to atBottom }.collect { (scrolling, bottom) ->
            if (scrolling || bottom) pinned = bottom
            if (bottom) unseen = false
        }
    }
    val open = ui.messages.filter { it.needsAttention() }
    val newest = ui.chat.lastOrNull()?.key.orEmpty() + "|" + open.size + "|" + ui.approvals.size
    LaunchedEffect(newest) {
        if (pinned) state.scrollToItem(0) else unseen = true
    }
    // Load older activity when the top (the end of the reversed list) comes into view.
    val nearTop by remember { derivedStateOf { state.layoutInfo.visibleItemsInfo.lastOrNull()?.let { it.index >= state.layoutInfo.totalItemsCount - 3 } == true } }
    LaunchedEffect(nearTop, ui.canLoadOlder, ui.transcriptLoaded) {
        if (nearTop && ui.canLoadOlder && ui.transcriptLoaded) vm.loadOlder()
    }
    LazyColumn(Modifier.fillMaxSize().testTag("session-detail"), state = state, reverseLayout = true) {
        item(key = "bottom") { Spacer(Modifier.size(8.dp)) }
        items(open.asReversed(), key = { "m_" + it.view.envelope.messageId }) { row -> MessageCard(row, vm, ui, onOpenLink) }
        items(ui.approvals.asReversed(), key = { it.approvalId }) { a -> RequestCard(a, ui, onOpenApproval) }
        items(ui.chat.asReversed(), key = { it.key }, contentType = { it::class }) { ChatRow(it) }
        item(key = "top") {
            Column(Modifier.fillMaxWidth()) {
                CachedBanner(s.cachedAt, s.error, ui.now)
                SessionInfo(s.value, ui, vm, onOpenLink)
                CachedBanner(ui.transcriptCachedAt, null, ui.now)
                when {
                    ui.loadingOlder || ui.canLoadOlder -> Row(Modifier.fillMaxWidth().padding(12.dp), horizontalArrangement = Arrangement.Center) {
                        CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp)
                    }
                    ui.transcriptGap -> Banner("Some earlier activity was lost or expired before this point.", Tone.MUTED)
                    ui.transcriptLoaded && ui.chat.isEmpty() -> Supporting("No activity yet.", Modifier.padding(horizontal = 16.dp, vertical = 24.dp).fillMaxWidth())
                    ui.transcriptLoaded -> Text(
                        "Start of this run",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.7f),
                        modifier = Modifier.fillMaxWidth().padding(vertical = 12.dp),
                        textAlign = TextAlign.Center,
                    )
                }
            }
        }
    }
    if (unseen && !pinned) {
        Box(Modifier.fillMaxSize().padding(bottom = 12.dp), contentAlignment = Alignment.BottomCenter) {
            Surface(
                onClick = { scope.launch { state.animateScrollToItem(0); pinned = true; unseen = false } },
                shape = RoundedCornerShape(50),
                color = MaterialTheme.colorScheme.primaryContainer,
                contentColor = MaterialTheme.colorScheme.onPrimaryContainer,
                shadowElevation = 3.dp,
                modifier = Modifier.testTag("new-messages"),
            ) {
                Text("↓ New messages", style = MaterialTheme.typography.labelLarge, modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp))
            }
        }
    }
}

/** Messages that still need a look: not yet stored or delivered, or with an unknown outcome. */
private fun MessageRow.needsAttention(): Boolean {
    val f = Delivery.facts(view)
    return localOnly || f.needsReconciliation || f.canCancel || f.canReissue
}

/** One line on top: what the session is doing and where, plus its menu. */
@Composable
private fun StatusStrip(v: SessionView, ui: SessionDetailUi, onRename: () -> Unit, onTogglePin: () -> Unit) {
    val status = Status.session(v, ui.node, ui.now)
    val where = listOf(
        v.session.harness,
        v.nodeName.ifEmpty { v.run?.nodeId.orEmpty() }.takeIf { it.isNotEmpty() }?.let { "on $it" + (ui.node?.let { n -> if (n.connection.state != "connected") " (" + Status.node(n, ui.now).label.lowercase() + ")" else "" } ?: "") },
    ).filterNotNull().filter { it.isNotEmpty() }.joinToString(" ")
    Surface(color = MaterialTheme.colorScheme.surface) {
        Row(Modifier.fillMaxWidth().padding(start = 16.dp, end = 4.dp), verticalAlignment = Alignment.CenterVertically) {
            Row(Modifier.weight(1f).semantics(mergeDescendants = true) {}, verticalAlignment = Alignment.CenterVertically) {
                StatusDot(status.tone, size = 10)
                Spacer(Modifier.width(8.dp))
                Text(
                    status.label,
                    style = MaterialTheme.typography.titleSmall,
                    color = if (status.tone == Tone.ATTENTION || status.tone == Tone.ERROR) toneColor(status.tone) else MaterialTheme.colorScheme.onSurface,
                    modifier = Modifier.testTag("status"),
                )
                Text(
                    listOf(where, status.detail).filter { it.isNotEmpty() }.joinToString(" · ").let { if (it.isEmpty()) "" else " · $it" },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            SessionMenu(Pins.isPinned(v), onRename, onTogglePin)
        }
    }
}

/**
 * "✉ message for you: …" — the newest unacknowledged message this session
 * sent you (its flag), with Reply (threads the next send to it) and Dismiss
 * (acknowledge without replying; clears the flag).
 */
@Composable
private fun MessageForYouCard(m: MessageView, ui: SessionDetailUi, canReply: Boolean, onReply: () -> Unit, onDismiss: () -> Unit) {
    val e = m.envelope
    Surface(
        color = MaterialTheme.colorScheme.tertiaryContainer,
        contentColor = MaterialTheme.colorScheme.onTertiaryContainer,
        shape = RoundedCornerShape(12.dp),
        modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp).testTag("message-for-you"),
    ) {
        Column(Modifier.padding(start = 16.dp, end = 8.dp, top = 12.dp, bottom = 4.dp)) {
            Text(
                "✉ message for you" + (e.createdAt.takeIf { it.isNotEmpty() }?.let { " · " + Times.ago(it, ui.now) } ?: ""),
                style = MaterialTheme.typography.labelLarge,
                fontWeight = FontWeight.SemiBold,
            )
            Text(
                e.body.text.trim().ifEmpty { "(no text)" },
                style = MaterialTheme.typography.bodyMedium,
                maxLines = 6,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.padding(top = 4.dp, end = 8.dp),
            )
            Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                if (canReply) TextButton(onClick = onReply, modifier = Modifier.heightIn(min = 48.dp).testTag("message-reply")) { Text("Reply") }
                TextButton(onClick = onDismiss, modifier = Modifier.heightIn(min = 48.dp).testTag("message-dismiss")) { Text("Dismiss") }
            }
        }
    }
}

/** At the top of the conversation: session details and the full message history, both collapsed. */
@Composable
private fun SessionInfo(v: SessionView, ui: SessionDetailUi, vm: SessionDetailViewModel, onOpenLink: (String) -> Unit) {
    val run = v.run
    Column(Modifier.padding(top = 4.dp)) {
        Expandable("Details", tag = "session-details") {
            KeyValue("Harness", listOf(v.session.harness, run?.runtime?.version.orEmpty(), run?.adapter.orEmpty()).filter { it.isNotEmpty() }.joinToString(" · "))
            KeyValue("Machine", v.nodeName.ifEmpty { run?.nodeId.orEmpty() } + (ui.node?.let { " · " + Status.node(it, ui.now).label } ?: ""))
            v.session.lineage.parentSessionId.takeIf { it.isNotEmpty() }?.let { parent ->
                // Sessions started by another session report back to it unless opted out (report:parent tag).
                KeyValue("Started by", (ui.titles[parent] ?: shortId(parent)) + if ("report:parent" in v.session.tags) " · reports to parent" else "")
            }
            run?.let {
                KeyValue("Directory", it.cwd, mono = true)
                KeyValue("Account", ui.accountLabel ?: if (it.accountId.isEmpty()) "Not assigned (uses the machine's own login)" else it.accountId)
                KeyValue("Last observed", Times.ago(it.freshness.observedAt, ui.now) + " via " + it.freshness.source + if (it.freshness.stale) " (stale)" else "")
                it.lease?.let { l -> KeyValue("Typing in the terminal", "${l.holder?.name?.ifEmpty { l.holder.id } ?: "someone"} until ${Times.clock(l.expiresAt)}") }
                if (it.continuationKind.isNotEmpty()) KeyValue("Continues", it.continuationKind + if (it.previousRunId.isNotEmpty()) " from ${shortId(it.previousRunId)}" else "")
                KeyValue("Can do", it.capabilities.joinToString(", ").ifEmpty { "nothing advertised" })
            }
            Supporting(
                "The terminal is the real harness screen. Messages go through the session's input queue instead.",
                Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
            )
        }
        val settled = ui.messages.filterNot { it.needsAttention() }
        if (settled.isNotEmpty()) {
            Expandable("Messages (${settled.size})", tag = "all-messages") {
                settled.forEach { row -> MessageCard(row, vm, ui, onOpenLink) }
            }
        }
    }
}

/** The session's overflow menu (top right of the header). */
@Composable
private fun SessionMenu(pinned: Boolean, onRename: () -> Unit, onTogglePin: () -> Unit) {
    var open by remember { mutableStateOf(false) }
    Box {
        IconButton(onClick = { open = true }, modifier = Modifier.testTag("session-menu")) { PyreIcon(R.drawable.ic_more_vert, "Session options") }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            DropdownMenuItem(text = { Text("Rename") }, onClick = { open = false; onRename() }, modifier = Modifier.testTag("menu-rename"))
            DropdownMenuItem(text = { Text(if (pinned) "Unpin" else "Pin") }, onClick = { open = false; onTogglePin() }, modifier = Modifier.testTag("menu-pin"))
        }
    }
}

/** Rename dialog, prefilled with the current title. Blank cannot be saved. */
@Composable
internal fun RenameDialog(current: String, onDismiss: () -> Unit, onSave: (String) -> Unit) {
    var text by remember { mutableStateOf(current) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Rename session") },
        text = {
            Column {
                OutlinedTextField(
                    value = text,
                    onValueChange = { text = it.replace("\n", " ") },
                    singleLine = true,
                    label = { Text("Name") },
                    modifier = Modifier.fillMaxWidth().testTag("rename-field"),
                )
                Supporting("Your name sticks: Pyre stops naming this session automatically.", Modifier.padding(top = 8.dp))
            }
        },
        confirmButton = {
            TextButton(onClick = { onSave(text) }, enabled = text.isNotBlank(), modifier = Modifier.testTag("rename-save")) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onDismiss, modifier = Modifier.testTag("rename-cancel")) { Text("Cancel") } },
    )
}

@Composable
private fun RequestCard(a: Approval, ui: SessionDetailUi, onOpenApproval: (String) -> Unit) {
    CardBlock(onClick = { onOpenApproval(a.approvalId) }) {
        Text(
            (if (a.kind == "question") "Question" else "Approval requested") + " · asked " + Times.ago(a.createdAt, ui.now),
            style = MaterialTheme.typography.labelLarge,
            color = MaterialTheme.colorScheme.primary,
        )
        Text(a.title, style = MaterialTheme.typography.titleMedium, modifier = Modifier.padding(top = 2.dp))
        if (a.body.isNotEmpty()) Supporting(a.body, Modifier.padding(top = 4.dp), maxLines = 3)
        Button(onClick = { onOpenApproval(a.approvalId) }, modifier = Modifier.padding(top = 12.dp).heightIn(min = 48.dp)) {
            Text(if (a.kind == "question") "Answer" else "Review and answer")
        }
    }
}

@Composable
private fun MessageCard(row: MessageRow, vm: SessionDetailViewModel, ui: SessionDetailUi, onOpenLink: (String) -> Unit) {
    val m = row.view
    val e = m.envelope
    val f = Delivery.facts(m)
    val outgoing = Identity.sentBy(e, vm.sessionId)
    CardBlock(modifier = Modifier.testTag("message-${e.messageId}")) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(
                (if (outgoing) "← Sent" else "→ Received") + (if (e.mode == MessageMode.STEER) " · steer" else "") +
                    (e.createdAt.takeIf { it.isNotEmpty() }?.let { " · " + Times.ago(it, ui.now) } ?: ""),
                style = MaterialTheme.typography.labelLarge,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.weight(1f),
            )
            if (row.localOnly) Pill(if (row.localFailed) "Not sent" else "Waiting to send", if (row.localFailed) Tone.ERROR else Tone.MUTED)
            if (f.needsReconciliation) Pill("Outcome unknown", Tone.ATTENTION)
        }
        Text("From " + Identity.principal(e.sender, ui.titles), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(top = 4.dp))
        Text("To " + Identity.address(e.recipient, ui.titles), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(e.body.text, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(vertical = 8.dp))
        e.attachments.forEach { a ->
            val (label, link) = Identity.attachment(a)
            Row(
                Modifier.fillMaxWidth()
                    .let { mod -> if (link != null) mod.clickable(role = Role.Button) { onOpenLink(link) } else mod }
                    .heightIn(min = 40.dp)
                    .testTag("attachment-${e.messageId}"),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                PyreIcon(R.drawable.ic_link, null, Modifier.size(18.dp), tint = if (link != null) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant)
                Spacer(Modifier.width(8.dp))
                Text(
                    "Attached: $label",
                    style = MaterialTheme.typography.bodyMedium,
                    color = if (link != null) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurface,
                )
            }
        }
        // The runtime fact is the one that changes; all four stay one tap away.
        val runtimeColor = if (f.runtime.tone == Tone.ERROR || f.runtime.tone == Tone.ATTENTION) toneColor(f.runtime.tone) else MaterialTheme.colorScheme.onSurface
        Text(
            buildAnnotatedString {
                append("Delivery: ")
                withStyle(SpanStyle(color = runtimeColor)) { append(f.runtime.value) }
            },
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Expandable("Delivery details", initiallyExpanded = f.needsReconciliation || row.localFailed, modifier = Modifier.padding(top = 4.dp)) {
            listOf(f.storage, f.runtime, f.ack, f.outcome).forEach { fact -> FactLine(fact.label, fact.value, fact.tone) }
            f.evidence?.let { FactLine("Evidence", it, Tone.MUTED) }
            Identity.threadLine(e).takeIf { it.isNotEmpty() }?.let { FactLine("Thread", it, Tone.MUTED, mono = true) }
            FactLine("Message", shortId(e.messageId), Tone.MUTED, mono = true)
            if (row.localOnly && row.localError.isNotEmpty()) FactLine("Last attempt (${row.attempts})", row.localError, if (row.localFailed) Tone.ERROR else Tone.MUTED)
        }
        Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
            if (f.needsReconciliation) TextButton(onClick = { vm.openReconcile(e.messageId) }, modifier = Modifier.heightIn(min = 48.dp).testTag("resolve-${e.messageId}")) { Text("Resolve…") }
            else if (f.canReissue) TextButton(onClick = { vm.openReconcile(e.messageId) }, modifier = Modifier.heightIn(min = 48.dp)) { Text("Send again…") }
            if (f.canCancel) TextButton(onClick = { vm.cancel(e.messageId) }, modifier = Modifier.heightIn(min = 48.dp)) { Text("Cancel") }
            if (row.localOnly && row.localFailed) TextButton(onClick = { vm.discardLocal(e.operationId) }, modifier = Modifier.heightIn(min = 48.dp)) { Text("Discard") }
            if (!row.localOnly) TextButton(onClick = { onOpenLink("pyre://message/${e.messageId}") }, modifier = Modifier.heightIn(min = 48.dp)) { Text("Open") }
        }
    }
}

@Composable
private fun FactLine(label: String, value: String, tone: Tone, mono: Boolean = false) {
    Column(Modifier.fillMaxWidth().padding(vertical = 4.dp).semantics(mergeDescendants = true) {}) {
        Text(label, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(value, style = MaterialTheme.typography.bodyMedium, color = toneColor(tone).takeIf { tone == Tone.ERROR || tone == Tone.ATTENTION } ?: MaterialTheme.colorScheme.onSurface, fontFamily = if (mono) FontFamily.Monospace else null)
    }
}

@Composable
private fun ReconcileDialog(r: ReconcileUi, ui: SessionDetailUi, vm: SessionDetailViewModel) {
    val row = ui.messages.firstOrNull { it.view.envelope.messageId == r.messageId } ?: return
    val indeterminate = row.view.status.runtime == DeliveryState.INDETERMINATE
    AlertDialog(
        onDismissRequest = vm::closeReconcile,
        title = { Text(if (indeterminate) "Did this message arrive?" else "Send it again?") },
        text = {
            Column(Modifier.testTag("reconcile-dialog")) {
                if (indeterminate) {
                    Text("Pyre can't tell whether the agent received it. Check the terminal or the activity first.")
                    Delivery.facts(row.view).evidence?.let { Supporting("Evidence: $it", Modifier.padding(top = 8.dp)) }
                    Text(
                        "Sending again creates a NEW message. If the first one was received, the agent may do the work twice.",
                        color = MaterialTheme.colorScheme.error,
                        modifier = Modifier.padding(top = 12.dp),
                    )
                    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(top = 4.dp)) {
                        Checkbox(checked = r.acceptRisk, onCheckedChange = vm::setAcceptRisk, modifier = Modifier.testTag("accept-risk"))
                        Text("I accept the duplication risk")
                    }
                } else {
                    Text("The original was not delivered (${row.view.status.runtime}). A new message that refers to it will be queued.")
                }
            }
        },
        confirmButton = {
            Column(horizontalAlignment = Alignment.End) {
                if (indeterminate) {
                    TextButton(enabled = !r.busy, onClick = { vm.resolve(r.messageId, DeliveryState.ACCEPTED) }) { Text("It was delivered (mark accepted)") }
                    TextButton(enabled = !r.busy, onClick = { vm.resolve(r.messageId, DeliveryState.REJECTED) }) { Text("It was not delivered (mark rejected)") }
                }
                TextButton(
                    enabled = !r.busy && (!indeterminate || r.acceptRisk),
                    onClick = { vm.reissue(r.messageId) },
                    modifier = Modifier.testTag("reissue"),
                ) { Text("Reissue as new message") }
            }
        },
        dismissButton = { TextButton(onClick = vm::closeReconcile) { Text("Close") } },
    )
}

/**
 * The bottom bar: quick actions (answer when something waits, terminal,
 * interrupt) over the reply box. It sits on the keyboard (imePadding on the
 * screen column), so typing pushes the conversation up.
 */
@Composable
private fun BottomBar(
    v: SessionView,
    ui: SessionDetailUi,
    vm: SessionDetailViewModel,
    onOpenApproval: (String) -> Unit,
    onOpenTerminal: (String) -> Unit,
    onInterrupt: () -> Unit,
    composerFocus: FocusRequester = remember { FocusRequester() },
) {
    Surface(color = MaterialTheme.colorScheme.surfaceContainer, modifier = Modifier.fillMaxWidth()) {
        Column(Modifier.navigationBarsPadding().padding(start = 12.dp, end = 12.dp, top = 6.dp, bottom = 8.dp)) {
            Row(Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                ui.approvals.firstOrNull()?.let { a ->
                    Button(onClick = { onOpenApproval(a.approvalId) }, modifier = Modifier.heightIn(min = 40.dp).testTag("quick-answer"), contentPadding = PaddingValues(horizontal = 14.dp)) {
                        Text(if (a.kind == "question") "Answer" else "Review request" + if (ui.approvals.size > 1) " (${ui.approvals.size})" else "")
                    }
                }
                FilledTonalButton(
                    onClick = { onOpenTerminal(v.session.sessionId) },
                    enabled = ui.canAttach,
                    modifier = Modifier.heightIn(min = 40.dp).testTag("open-terminal"),
                    contentPadding = PaddingValues(horizontal = 14.dp),
                ) {
                    PyreIcon(R.drawable.ic_terminal, null, Modifier.size(16.dp))
                    Spacer(Modifier.width(6.dp))
                    Text("Terminal")
                }
                if (ui.canInterrupt) {
                    OutlinedButton(onClick = onInterrupt, modifier = Modifier.heightIn(min = 40.dp).testTag("interrupt"), contentPadding = PaddingValues(horizontal = 14.dp)) {
                        PyreIcon(R.drawable.ic_stop, null, Modifier.size(16.dp))
                        Spacer(Modifier.width(6.dp))
                        Text("Interrupt")
                    }
                }
                if (ui.canSteer) {
                    TextButton(
                        onClick = { vm.send(steer = true) },
                        enabled = ui.draft.isNotBlank() && !ui.sending,
                        modifier = Modifier.heightIn(min = 40.dp).testTag("steer"),
                    ) { Text("Steer now") }
                }
            }
            Spacer(Modifier.size(6.dp))
            if (!ui.canQueue && !ui.canSteer) {
                Supporting("This session doesn't take messages. Use the terminal.", Modifier.padding(8.dp))
                return@Column
            }
            ui.replyTo?.let { r ->
                Row(Modifier.fillMaxWidth().testTag("replying-to"), verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        "Replying to: " + r.envelope.body.text.trim().lineSequence().firstOrNull().orEmpty(),
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.weight(1f).padding(start = 8.dp),
                    )
                    TextButton(onClick = vm::cancelReply) { Text("Cancel") }
                }
            }
            Row(verticalAlignment = Alignment.CenterVertically) {
                OutlinedTextField(
                    value = ui.draft,
                    onValueChange = vm::setDraft,
                    modifier = Modifier.weight(1f).focusRequester(composerFocus).testTag("composer"),
                    placeholder = { Text(if (ui.replyTo != null) "Reply to the message" else if (ui.canQueue) "Reply to the agent" else "Steer the agent") },
                    shape = RoundedCornerShape(24.dp),
                    maxLines = 5,
                )
                if (ui.canQueue) {
                    Spacer(Modifier.width(8.dp))
                    FilledIconButton(
                        onClick = { vm.send() },
                        enabled = ui.draft.isNotBlank() && !ui.sending,
                        modifier = Modifier.size(48.dp).testTag("send"),
                    ) { PyreIcon(R.drawable.ic_send, "Send (delivered at the agent's next turn)") }
                }
            }
        }
    }
}
