package dev.jhh3.pyre.ui.inbox

import androidx.compose.animation.core.animate
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.gestures.Orientation
import androidx.compose.foundation.gestures.draggable
import androidx.compose.foundation.gestures.rememberDraggableState
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.Snackbar
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.CustomAccessibilityAction
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.customActions
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import dev.jhh3.pyre.R
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.domain.Asks
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.AnswerAskItem
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.CachedBanner
import dev.jhh3.pyre.ui.common.EmptyState
import dev.jhh3.pyre.ui.common.ErrorBox
import dev.jhh3.pyre.ui.common.LoadingBox
import dev.jhh3.pyre.ui.common.Pill
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.RefreshWhileResumed
import dev.jhh3.pyre.ui.theme.toneColor
import java.time.Instant
import kotlin.math.abs
import kotlin.math.roundToInt
import kotlinx.coroutines.launch

/**
 * The owner inbox: Asks · Messages · All · Done. Tap opens (an ask opens
 * its sheet, where tapping an option answers with a 5 s Undo snackbar;
 * anything else its session or object); swipe right = done,
 * swipe left = snooze (1 hour / tomorrow 8 AM); long press starts
 * multi-select for batch read / done / snooze.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun InboxScreen(
    vm: InboxViewModel,
    onOpenSession: (String) -> Unit,
    onOpenLink: (String) -> Unit,
) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    RefreshWhileResumed(15_000) { vm.refresh() }
    // Leaving the inbox sends answers still waiting out their Undo window.
    DisposableEffect(vm) { onDispose { vm.flushNow() } }
    var snoozeKeys by remember { mutableStateOf<List<String>?>(null) }
    val visible = remember(ui.rows, ui.tab, ui.filters, ui.now) { Inbox.visible(ui.rows, ui.tab, ui.filters, ui.now) }
    val counts = remember(ui.rows, ui.now) { Inbox.tabCounts(ui.rows, ui.now) }
    val snoozedCount = remember(ui.rows, ui.now) { Inbox.snoozedCount(ui.rows, ui.now) }
    val open: (InboxRow) -> Unit = { r ->
        val ask = r.ask
        when {
            ask != null -> vm.openAsk(ask.askId)
            else -> {
                if (r.unread) vm.markRead(listOf(r.key))
                if (r.sessionId.isNotEmpty()) onOpenSession(r.sessionId) else if (r.deepLink.isNotEmpty()) onOpenLink(r.deepLink)
            }
        }
    }

    Column(Modifier.fillMaxSize().testTag("inbox-screen")) {
        if (ui.selection.isNotEmpty()) {
            SelectionBar(
                count = ui.selection.size,
                onRead = { vm.markRead(ui.selection) },
                onDone = { vm.markDone(ui.selection) },
                onSnooze = { snoozeKeys = ui.selection.toList() },
                onCancel = vm::clearSelection,
            )
        } else {
            Tabs(ui, counts, vm::setTab)
        }
        FilterRow(ui, snoozedCount, vm)
        ui.notice?.let { msg ->
            Banner(msg, ui.noticeTone.let { if (it == Tone.OK) Tone.MUTED else it }, Modifier.testTag("inbox-notice")) {
                if (ui.canRetry) TextButton(onClick = vm::retry) { Text("Retry") } else TextButton(onClick = vm::dismissNotice) { Text("OK") }
            }
        }
        PullToRefreshBox(isRefreshing = ui.refreshing, onRefresh = vm::pullRefresh, modifier = Modifier.weight(1f).fillMaxWidth()) {
            when {
                ui.loading && ui.rows.isEmpty() && ui.error == null -> LoadingBox()
                ui.rows.isEmpty() && ui.error != null -> ErrorBox(ui.error!!, vm::refresh)
                visible.isEmpty() -> EmptyState(
                    title = emptyTitle(ui),
                    body = emptyBody(ui),
                    icon = R.drawable.ic_inbox,
                    modifier = Modifier.testTag("inbox-empty"),
                )
                else -> LazyColumn(Modifier.fillMaxSize().testTag("inbox-list")) {
                    item(key = "cached") { CachedBanner(ui.cachedAt, ui.cachedError, ui.now) }
                    items(visible, key = { it.key }) { r ->
                        val selecting = ui.selection.isNotEmpty()
                        InboxRowView(
                            r,
                            sessionTitle = ui.sessionTitles[r.sessionId] ?: r.sessionId,
                            projectName = if (ui.filters.projectId == null) r.projectId?.let { ui.projects[it] } else null,
                            now = ui.now,
                            selected = r.key in ui.selection,
                            held = r.ask?.let { ui.held[it.askId] },
                            swipeEnabled = !selecting && r.attentionId.isNotEmpty() && ui.tab != InboxTab.DONE,
                            onClick = { if (selecting) vm.toggleSelect(r.key) else open(r) },
                            onLongClick = { vm.toggleSelect(r.key) },
                            onDone = { vm.markDone(listOf(r.key)) },
                            onSnooze = { snoozeKeys = listOf(r.key) },
                        )
                    }
                    item(key = "end-space") { Spacer(Modifier.size(24.dp)) }
                }
            }
        }
        ui.undoText?.let { msg ->
            Snackbar(
                action = { TextButton(onClick = { vm.undo() }, modifier = Modifier.testTag("inbox-undo")) { Text("Undo") } },
                modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp).testTag("inbox-undo-bar"),
            ) { Text(msg, maxLines = 2, overflow = TextOverflow.Ellipsis) }
        }
    }

    snoozeKeys?.let { keys ->
        SnoozeDialog(
            count = keys.size,
            morningLabel = "Tomorrow 8 AM",
            onHour = { vm.snoozeHour(keys); snoozeKeys = null },
            onMorning = { vm.snoozeMorning(keys); snoozeKeys = null },
            onDismiss = { snoozeKeys = null },
        )
    }

    if (ui.openAskId != null) {
        AskSheet(ui, vm, onOpenSession = { id ->
            vm.closeAsk()
            onOpenSession(id)
        })
    }
}

private fun emptyTitle(ui: InboxUi): String = when {
    ui.filters.snoozed -> "Nothing snoozed"
    ui.tab == InboxTab.DONE -> "Nothing done yet"
    ui.filters.unreadOnly || ui.filters.projectId != null -> "Nothing matches"
    ui.tab == InboxTab.ASKS -> "No questions waiting"
    ui.tab == InboxTab.MESSAGES -> "No messages waiting"
    else -> "Inbox zero"
}

private fun emptyBody(ui: InboxUi): String? = when {
    ui.filters.unreadOnly || ui.filters.projectId != null -> "Clear a filter to see more."
    ui.tab == InboxTab.ASKS -> "When an agent needs a decision it asks here, with a default and a deadline."
    ui.tab == InboxTab.DONE -> "Items you mark done or answer move here."
    ui.filters.snoozed -> null
    else -> "Messages, questions and requests from your sessions show up here."
}

/** Segmented tabs with their row counts ("Asks 2"; All is the badge); Asks only when the hub offers them. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun Tabs(ui: InboxUi, counts: Map<InboxTab, Int>, onTab: (InboxTab) -> Unit) {
    val tabs = InboxTab.entries.filter { it != InboxTab.ASKS || ui.asksSupported }
    SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp).testTag("inbox-tabs")) {
        tabs.forEachIndexed { i, t ->
            val n = counts[t] ?: 0
            SegmentedButton(
                selected = ui.tab == t,
                onClick = { onTab(t) },
                shape = SegmentedButtonDefaults.itemShape(index = i, count = tabs.size),
                icon = {},
                modifier = Modifier.heightIn(min = 48.dp).testTag("inbox-tab-" + t.name.lowercase()),
            ) {
                Text(if (n > 0) "${t.label} $n" else t.label, maxLines = 1, overflow = TextOverflow.Ellipsis)
            }
        }
    }
}

/** Unread · Project ▾ · Snoozed (N). */
@Composable
private fun FilterRow(ui: InboxUi, snoozedCount: Int, vm: InboxViewModel) {
    var projectMenu by remember { mutableStateOf(false) }
    Row(
        Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(horizontal = 16.dp),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        FilterChip(
            selected = ui.filters.unreadOnly,
            onClick = vm::toggleUnread,
            label = { Text("Unread") },
            modifier = Modifier.heightIn(min = 48.dp).testTag("inbox-filter-unread"),
        )
        Box {
            val pid = ui.filters.projectId
            FilterChip(
                selected = pid != null,
                onClick = { projectMenu = true },
                label = {
                    Text(
                        when (pid) {
                            null -> "Project"
                            Inbox.NO_PROJECT -> "No project"
                            else -> ui.projects[pid] ?: pid
                        },
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                    )
                },
                trailingIcon = { PyreIcon(R.drawable.ic_expand_more, null, Modifier.size(18.dp)) },
                modifier = Modifier.heightIn(min = 48.dp).testTag("inbox-filter-project"),
            )
            DropdownMenu(expanded = projectMenu, onDismissRequest = { projectMenu = false }) {
                DropdownMenuItem(text = { Text("Any project") }, onClick = { projectMenu = false; vm.setProject(null) })
                val used = ui.rows.mapNotNull { it.projectId?.takeIf(String::isNotEmpty) }.toSet()
                ui.projects.filterKeys { it in used }.entries.sortedBy { it.value.lowercase() }.forEach { (id, name) ->
                    DropdownMenuItem(text = { Text(name) }, onClick = { projectMenu = false; vm.setProject(id) })
                }
                DropdownMenuItem(text = { Text("No project") }, onClick = { projectMenu = false; vm.setProject(Inbox.NO_PROJECT) })
            }
        }
        if (ui.tab != InboxTab.DONE && (snoozedCount > 0 || ui.filters.snoozed)) {
            FilterChip(
                selected = ui.filters.snoozed,
                onClick = vm::toggleSnoozed,
                label = { Text("Snoozed $snoozedCount") },
                modifier = Modifier.heightIn(min = 48.dp).testTag("inbox-filter-snoozed"),
            )
        }
    }
}

@Composable
private fun SelectionBar(count: Int, onRead: () -> Unit, onDone: () -> Unit, onSnooze: () -> Unit, onCancel: () -> Unit) {
    Surface(color = MaterialTheme.colorScheme.secondaryContainer, modifier = Modifier.fillMaxWidth().testTag("inbox-selection")) {
        Row(Modifier.padding(horizontal = 8.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
            TextButton(onClick = onCancel, modifier = Modifier.heightIn(min = 48.dp)) { Text("Cancel") }
            Text("$count selected", style = MaterialTheme.typography.titleSmall, modifier = Modifier.weight(1f))
            TextButton(onClick = onRead, modifier = Modifier.heightIn(min = 48.dp).testTag("batch-read")) { Text("Read") }
            TextButton(onClick = onSnooze, modifier = Modifier.heightIn(min = 48.dp).testTag("batch-snooze")) { Text("Snooze") }
            TextButton(onClick = onDone, modifier = Modifier.heightIn(min = 48.dp).testTag("batch-done")) { Text("Done") }
        }
    }
}

@Composable
private fun SnoozeDialog(count: Int, morningLabel: String, onHour: () -> Unit, onMorning: () -> Unit, onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(if (count == 1) "Snooze until…" else "Snooze $count items until…") },
        text = { Text("It comes back to the inbox then. An ask still defaults at its deadline.") },
        confirmButton = {
            Column(horizontalAlignment = Alignment.End) {
                TextButton(onClick = onHour, modifier = Modifier.heightIn(min = 48.dp).testTag("snooze-hour")) { Text("In 1 hour") }
                TextButton(onClick = onMorning, modifier = Modifier.heightIn(min = 48.dp).testTag("snooze-morning")) { Text(morningLabel) }
            }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

/**
 * One inbox line: unread dot · kind chip · session · age, the title, then
 * the ask's deadline ("defaults to B in 42m") or the summary. Swipe right
 * marks done, swipe left snoozes (also offered as accessibility actions).
 */
@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun InboxRowView(
    r: InboxRow,
    sessionTitle: String,
    projectName: String?,
    now: Instant,
    selected: Boolean,
    held: AnswerAskItem?,
    swipeEnabled: Boolean,
    onClick: () -> Unit,
    onLongClick: () -> Unit,
    onDone: () -> Unit,
    onSnooze: () -> Unit,
) {
    val cs = MaterialTheme.colorScheme
    val scope = rememberCoroutineScope()
    var dx by remember(r.key) { mutableFloatStateOf(0f) }
    var width by remember { mutableFloatStateOf(0f) }
    val drag = rememberDraggableState { delta -> dx = (dx + delta).coerceIn(-width, width) }
    val settle: () -> Unit = {
        scope.launch {
            val threshold = width * 0.35f
            when {
                width > 0f && dx > threshold -> {
                    animate(dx, width) { v, _ -> dx = v }
                    onDone()
                    dx = 0f
                }
                width > 0f && dx < -threshold -> {
                    animate(dx, 0f) { v, _ -> dx = v }
                    onSnooze()
                }
                else -> animate(dx, 0f) { v, _ -> dx = v }
            }
        }
    }
    Box(
        Modifier.fillMaxWidth()
            .onSizeChanged { width = it.width.toFloat() }
            .testTag("inbox-row-" + r.key),
    ) {
        // What the swipe reveals: Done on the left (swiping right), Snooze on the right.
        if (abs(dx) > 0.5f) {
            val right = dx > 0
            Row(
                Modifier.matchParentSize().background(if (right) cs.primaryContainer else cs.tertiaryContainer).padding(horizontal = 24.dp),
                horizontalArrangement = if (right) Arrangement.Start else Arrangement.End,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                val fg = if (right) cs.onPrimaryContainer else cs.onTertiaryContainer
                PyreIcon(if (right) R.drawable.ic_check else R.drawable.ic_history, null, tint = fg)
                Spacer(Modifier.width(8.dp))
                Text(if (right) "Done" else "Snooze", color = fg, style = MaterialTheme.typography.labelLarge)
            }
        }
        Row(
            Modifier.fillMaxWidth()
                .offset { IntOffset(dx.roundToInt(), 0) }
                .background(if (selected) cs.secondaryContainer else cs.surface)
                .draggable(drag, Orientation.Horizontal, enabled = swipeEnabled, onDragStopped = { _ -> settle() })
                .combinedClickable(
                    role = Role.Button,
                    onClick = onClick,
                    onLongClickLabel = "Select",
                    onLongClick = onLongClick,
                )
                .semantics {
                    this.selected = selected
                    if (swipeEnabled) {
                        customActions = listOf(
                            CustomAccessibilityAction("Done") { onDone(); true },
                            CustomAccessibilityAction("Snooze") { onSnooze(); true },
                        )
                    }
                }
                .heightIn(min = 72.dp)
                .padding(horizontal = 16.dp, vertical = 10.dp),
            verticalAlignment = Alignment.Top,
        ) {
            // Unread dot (or the selection check).
            Box(Modifier.padding(top = 6.dp).size(12.dp), contentAlignment = Alignment.Center) {
                when {
                    selected -> PyreIcon(R.drawable.ic_check, "Selected", Modifier.size(16.dp), tint = cs.primary)
                    r.unread -> Box(Modifier.size(10.dp).background(cs.primary, CircleShape).testTag("unread-" + r.key))
                }
            }
            Spacer(Modifier.width(12.dp))
            Column(Modifier.weight(1f)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Pill(Inbox.kindLabel(r.kind), if (Inbox.blocking(r.kind) && !Inbox.done(r)) Tone.ATTENTION else Tone.MUTED)
                    Spacer(Modifier.width(8.dp))
                    Text(
                        listOfNotNull(sessionTitle.ifEmpty { null }, projectName).joinToString(" · "),
                        style = MaterialTheme.typography.labelMedium,
                        color = cs.onSurfaceVariant,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.weight(1f),
                    )
                    r.at?.let { Text(Times.ago(it, now), style = MaterialTheme.typography.labelSmall, color = cs.onSurfaceVariant, modifier = Modifier.padding(start = 8.dp)) }
                }
                Text(
                    r.title + if (r.count > 1) "  ×${r.count}" else "",
                    style = MaterialTheme.typography.bodyLarge,
                    fontWeight = if (r.unread) FontWeight.SemiBold else FontWeight.Normal,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.padding(top = 4.dp),
                )
                val ask = r.ask
                if (ask != null) {
                    val line = listOfNotNull(
                        if (ask.decided) Asks.explain(ask) else Asks.deadline(ask, now).ifEmpty { null },
                        held?.takeIf { !ask.decided }?.let { s -> "✓ " + (s.optionId.takeIf { it.isNotEmpty() }?.let { Asks.optionLabel(ask, it) } ?: "“${s.text}”") + " · sending…" },
                    ).joinToString(" · ")
                    if (line.isNotEmpty()) {
                        Text(
                            line,
                            style = MaterialTheme.typography.bodyMedium,
                            color = if (Asks.soon(ask, now) || held != null) toneColor(Tone.ATTENTION) else cs.onSurfaceVariant,
                            fontWeight = if (Asks.soon(ask, now)) FontWeight.SemiBold else FontWeight.Normal,
                            maxLines = 2,
                            overflow = TextOverflow.Ellipsis,
                            modifier = Modifier.testTag("deadline-" + r.key),
                        )
                    }
                } else if (r.summary.isNotEmpty()) {
                    Text(r.summary, style = MaterialTheme.typography.bodyMedium, color = cs.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
                }
                if (Inbox.snoozed(r, now)) {
                    Text("Snoozed until " + Times.clock(r.snoozedUntil?.toString()), style = MaterialTheme.typography.bodySmall, color = cs.onSurfaceVariant)
                }
            }
        }
    }
}
