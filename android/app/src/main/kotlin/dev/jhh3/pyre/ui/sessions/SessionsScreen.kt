package dev.jhh3.pyre.ui.sessions

import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
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
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.R
import dev.jhh3.pyre.core.Clock
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.LocalSettings
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.data.valueOrNull
import dev.jhh3.pyre.digest.Digest
import dev.jhh3.pyre.digest.DigestFetcher
import dev.jhh3.pyre.digest.LastSeen
import dev.jhh3.pyre.digest.LastSeenStore
import dev.jhh3.pyre.domain.Flags
import dev.jhh3.pyre.domain.LeadKind
import dev.jhh3.pyre.domain.LeadSignal
import dev.jhh3.pyre.domain.Leads
import dev.jhh3.pyre.domain.Pins
import dev.jhh3.pyre.domain.Status
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.Ask
import dev.jhh3.pyre.model.AskState
import dev.jhh3.pyre.model.ConnectionState
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.model.UpdateSessionRequest
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.CachedBanner
import dev.jhh3.pyre.ui.common.CountBadge
import dev.jhh3.pyre.ui.common.EmptyState
import dev.jhh3.pyre.ui.common.ErrorBox
import dev.jhh3.pyre.ui.common.LoadingBox
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.RefreshWhileResumed
import dev.jhh3.pyre.ui.common.StatusChip
import dev.jhh3.pyre.ui.common.StatusDot
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.theme.toneColor
import java.time.Instant
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

/**
 * A list group: a project, an attention class or a machine (by [grouping]);
 * [nodeId] is the group key (the machine id in machine mode).
 */
data class NodeGroup(val nodeId: String, val title: String, val node: Node?, val sessions: List<SessionView>) {
    val pinned: Boolean get() = nodeId == PINNED_GROUP
}

/** The id of the "Pinned" group (always first; not a machine). */
const val PINNED_GROUP = "@pinned"

/** How the Sessions list groups rows (the TUI's gb modes; project is the default). */
enum class Grouping(val label: String) { PROJECT("Project"), NEEDS_YOU("Needs you"), MACHINE("Machine") }

data class SessionsUi(
    val loading: Boolean = true,
    val refreshing: Boolean = false,
    val groups: List<NodeGroup> = emptyList(),
    val nodes: Map<String, Node> = emptyMap(),
    val cachedAt: Instant? = null,
    val error: String? = null,
    val now: Instant = Instant.EPOCH,
    val grouping: Grouping = Grouping.PROJECT,
    val projects: Map<String, String> = emptyMap(),
    /** Sessions with a blocking flag / messages for you (header line). */
    val needsYou: Int = 0,
    val messages: Int = 0,
    /** Ended sessions are listed (otherwise one "N ended — show" row). */
    val showEnded: Boolean = false,
    /** Ended sessions the list currently hides. */
    val endedHidden: Int = 0,
    /** "While you were away" digest; null when not loaded, dismissed or empty. */
    val digest: Digest? = null,
    /** Lead signals by lead session id (domain/Leads); a session without an entry is not a lead. */
    val leads: Map<String, LeadSignal> = emptyMap(),
)

class SessionsViewModel(
    private val repo: Repository,
    private val clock: Clock = repo.clock,
    private val settings: LocalSettings? = null,
    /** Where this client keeps its digest "last seen" mark; null disables the digest card. */
    private val digestStore: LastSeenStore? = null,
) : ViewModel() {
    private val _ui = MutableStateFlow(
        SessionsUi(
            now = clock.now(),
            grouping = settings?.prefs?.value?.sessionsGrouping?.let { g -> Grouping.entries.firstOrNull { it.name == g } } ?: Grouping.PROJECT,
            showEnded = settings?.prefs?.value?.showEndedSessions ?: false,
        ),
    )
    val ui: StateFlow<SessionsUi> = _ui.asStateFlow()
    private var all: List<SessionView> = emptyList()
    private var openAsks: List<Ask> = emptyList()

    /** Lead signals over the loaded sessions and open asks, with this app's "opened" times. */
    private fun leads(): Map<String, LeadSignal> {
        val prefs = settings?.prefs?.value
        val since = Times.parse(prefs?.leadSeenSince) ?: firstLook.also { t ->
            // The first launch with lead signals: nothing that finished earlier is new.
            settings?.update { it.copy(leadSeenSince = t.toString()) }
        }
        val seen = prefs?.leadSeen.orEmpty().mapNotNull { (k, v) -> Times.parse(v)?.let { k to it } }.toMap() + localSeen
        return Leads.signals(all, openAsks, seen, since)
    }

    /** This model's start: the seen baseline without settings. */
    private val firstLook: Instant = clock.now()

    /** Opened sessions, for a model without settings (tests). */
    private val localSeen = HashMap<String, Instant>()

    /**
     * Records that the owner opened [id] (its lead loses "new"). Bounded:
     * archived sessions go, then the oldest beyond Leads.SEEN_MAX.
     */
    fun markSeen(id: String) {
        val now = clock.now()
        if (settings == null) localSeen[id] = now
        val archived = all.filter { it.session.archived }.map { it.session.sessionId }.toSet()
        settings?.update { p ->
            val next = (p.leadSeen + (id to now.toString())).filterKeys { it !in archived }
            p.copy(leadSeen = next.entries.sortedByDescending { it.value }.take(Leads.SEEN_MAX).associate { it.key to it.value })
        }
        _ui.value = _ui.value.copy(leads = leads())
    }

    init {
        refresh()
        loadDigest()
    }

    fun refresh() {
        viewModelScope.launch { load() }
    }

    fun pullRefresh() {
        _ui.value = _ui.value.copy(refreshing = true)
        loadDigest()
        viewModelScope.launch {
            load()
            _ui.value = _ui.value.copy(refreshing = false)
        }
    }

    /**
     * Computes the digest since this client's last-seen mark from existing hub
     * reads. Not part of the periodic refresh (it reads transcripts): on open
     * and on pull-to-refresh only. A failure just leaves the card out.
     */
    fun loadDigest() = viewModelScope.launch {
        val store = digestStore ?: return@launch
        val d = try {
            DigestFetcher(repo.api).fetch(store.load(), clock.now())
        } catch (e: PyreApiException) {
            null
        }
        _ui.value = _ui.value.copy(digest = d?.takeUnless { it.isEmpty })
    }

    /** Marks everything up to the digest's head cursor as seen and hides the card. */
    fun dismissDigest() {
        val d = _ui.value.digest ?: return
        digestStore?.save(LastSeen(d.headCursor, clock.now()))
        _ui.value = _ui.value.copy(digest = null)
    }

    fun setGrouping(g: Grouping) {
        val u = _ui.value
        settings?.update { it.copy(sessionsGrouping = g.name) }
        _ui.value = u.copy(grouping = g, groups = group(all, u.nodes.values.toList(), u.projects, g, u.showEnded))
    }

    /** Shows or hides ended sessions and remembers the choice. */
    fun toggleEnded() {
        val u = _ui.value
        val show = !u.showEnded
        settings?.update { it.copy(showEndedSessions = show) }
        _ui.value = u.copy(showEnded = show, groups = group(all, u.nodes.values.toList(), u.projects, u.grouping, show), endedHidden = if (show) 0 else hiddenEnded(all))
    }

    private suspend fun load() {
        _ui.value = _ui.value.copy(loading = true)
        val sessions = repo.sessions()
        val nodes = repo.nodes()
        val nodeList = (nodes as? Load.Ready)?.value.orEmpty()
        val projects = (repo.projects() as? Load.Ready)?.value.orEmpty().associate { it.projectId to it.name.ifEmpty { it.projectId } }
        val g = _ui.value.grouping
        val showEnded = _ui.value.showEnded
        openAsks = if (repo.asksSupported()) repo.asks("open", listOf(AskState.OPEN)).valueOrNull().orEmpty() else emptyList()
        _ui.value = when (sessions) {
            is Load.Ready -> {
                all = sessions.value
                val live = all.filterNot { it.session.archived }
                SessionsUi(
                    loading = false,
                    refreshing = _ui.value.refreshing,
                    groups = group(all, nodeList, projects, g, showEnded),
                    nodes = nodeList.associateBy { it.nodeId },
                    cachedAt = sessions.cachedAt,
                    error = sessions.error?.describe(),
                    now = clock.now(),
                    grouping = g,
                    projects = projects,
                    needsYou = live.count(Flags::needsYou),
                    messages = live.sumOf(Flags::messages),
                    showEnded = showEnded,
                    endedHidden = if (showEnded) 0 else hiddenEnded(all),
                    digest = _ui.value.digest,
                    leads = leads(),
                )
            }
            is Load.Failed -> SessionsUi(loading = false, refreshing = _ui.value.refreshing, error = sessions.error.describe(), now = clock.now(), grouping = g, showEnded = showEnded, digest = _ui.value.digest)
            Load.Loading -> _ui.value
        }
    }

    /**
     * Pins or unpins [s] (reserved session tags via sessions.update, shared
     * with the TUI). The list updates at once; a failure puts it back.
     */
    fun togglePin(s: SessionView) {
        val tags = Pins.toggle(s.session.tags, clock.now())
        val before = _ui.value
        all = all.map { if (it.session.sessionId == s.session.sessionId) it.copy(session = it.session.copy(tags = tags)) else it }
        _ui.value = before.copy(groups = group(all, before.nodes.values.toList(), before.projects, before.grouping, before.showEnded))
        viewModelScope.launch {
            try {
                repo.call { updateSession(UpdateSessionRequest(operationId = Ids.operation(), sessionId = s.session.sessionId, tags = tags)) }
                load()
            } catch (e: PyreApiException) {
                _ui.value = _ui.value.copy(groups = before.groups, error = (if (Pins.isPinned(tags)) "Pin" else "Unpin") + ": " + e.describe())
            }
        }
    }

    companion object {
        const val NO_PROJECT = "@none"

        /** Hidden while ended sessions are not shown: ended, not pinned, nothing for you. */
        fun hideable(s: SessionView): Boolean = Status.ended(s) && !Pins.isPinned(s) && Flags.rank(s) >= 2

        fun hiddenEnded(sessions: List<SessionView>): Int = sessions.count { !it.session.archived && hideable(it) }

        /** Inside a group: needs you, then a message for you, then open items, then the most recently observed. */
        private val rowOrder = compareBy<SessionView> { Flags.rank(it) }
            .thenByDescending { it.attention }
            .thenByDescending { it.run?.freshness?.observedAt.orEmpty() }

        /**
         * Pinned sessions first (a "Pinned" group, in pin order) in every
         * mode, then the mode's groups:
         * - [Grouping.PROJECT]: projects by their most recently started
         *   session (start times, so a group does not jump while you watch),
         *   "No project" last;
         * - [Grouping.NEEDS_YOU]: Needs you · Message for you · Everything else;
         * - [Grouping.MACHINE]: by the current run's machine, "Not running" last.
         * Flagged sessions first within a group.
         */
        fun group(
            sessions: List<SessionView>,
            nodes: List<Node>,
            projects: Map<String, String> = emptyMap(),
            grouping: Grouping = Grouping.MACHINE,
            showEnded: Boolean = true,
        ): List<NodeGroup> {
            val byId = nodes.associateBy { it.nodeId }
            val live = sessions.filterNot { it.session.archived || (!showEnded && hideable(it)) }
            val pinned = Pins.ordered(live)
            val head = if (pinned.isEmpty()) emptyList() else listOf(NodeGroup(PINNED_GROUP, "Pinned", null, pinned))
            val rest = live.filterNot(Pins::isPinned)
            val body = when (grouping) {
                Grouping.PROJECT -> rest.groupBy { it.session.projectId?.takeIf(String::isNotEmpty) ?: NO_PROJECT }
                    .map { (pid, list) ->
                        NodeGroup(pid, if (pid == NO_PROJECT) "No project" else projects[pid] ?: pid, null, list.sortedWith(rowOrder))
                    }
                    .sortedWith(
                        compareBy<NodeGroup> { it.nodeId == NO_PROJECT }
                            .thenByDescending { g -> g.sessions.maxOfOrNull { it.session.createdAt }.orEmpty() }
                            .thenBy { it.title.lowercase() },
                    )
                Grouping.NEEDS_YOU -> rest.groupBy { Flags.rank(it) }.toSortedMap().map { (rank, list) ->
                    val title = when (rank) {
                        0 -> "Needs you"
                        1 -> "Message for you"
                        else -> "Everything else"
                    }
                    NodeGroup("@attn$rank", title, null, list.sortedWith(rowOrder))
                }
                Grouping.MACHINE -> rest.groupBy { it.run?.nodeId.orEmpty() }
                    .map { (nodeId, list) ->
                        val node = byId[nodeId]
                        val title = when {
                            nodeId.isEmpty() -> "Not running"
                            node != null -> node.name
                            else -> list.first().nodeName.ifEmpty { nodeId }
                        }
                        NodeGroup(nodeId, title, node, list.sortedWith(rowOrder))
                    }
                    .sortedWith(compareBy<NodeGroup> { it.nodeId.isEmpty() }.thenBy { it.title })
            }
            return head + body
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SessionsScreen(
    vm: SessionsViewModel,
    onOpenMachine: (String) -> Unit = {},
    onLaunch: () -> Unit = {},
    /** A digest line's deep link (the request, or the transcript moment). */
    onOpenLink: (String) -> Unit = {},
    onOpen: (String) -> Unit,
) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    RefreshWhileResumed { vm.refresh() }
    PullToRefreshBox(isRefreshing = ui.refreshing, onRefresh = vm::pullRefresh, modifier = Modifier.fillMaxSize()) {
        when {
            ui.loading && ui.groups.isEmpty() && ui.error == null -> LoadingBox()
            ui.groups.isEmpty() && ui.error != null -> ErrorBox(ui.error!!, vm::refresh)
            ui.groups.isEmpty() && ui.endedHidden == 0 -> EmptyState(
                title = "No sessions yet",
                body = "Start a Claude, Codex or shell session on any of your machines.",
                icon = R.drawable.ic_terminal,
            ) { FilledTonalButton(onClick = onLaunch, modifier = Modifier.heightIn(min = 48.dp)) { Text("New session") } }
            else -> LazyColumn(Modifier.fillMaxSize().testTag("sessions-list")) {
                item(key = "cached") { CachedBanner(ui.cachedAt, null, ui.now) }
                ui.digest?.let { d ->
                    item(key = "digest") {
                        Box(Modifier.padding(horizontal = 16.dp, vertical = 4.dp)) { DigestCard(d, ui.now, onOpenLink, vm::dismissDigest) }
                    }
                }
                item(key = "grouping") { GroupingBar(ui, vm::setGrouping) }
                ui.groups.forEach { g ->
                    // A project header sums its leads (the TUI's project header); quiet when they all are.
                    val summary = if (ui.grouping == Grouping.PROJECT && !g.pinned) Leads.summary(g.sessions.mapNotNull { ui.leads[it.session.sessionId] }) else null
                    item(key = "h_" + g.nodeId) { GroupHeader(g, ui.now, onOpenMachine, summary) }
                    items(g.sessions, key = { it.session.sessionId }) { s ->
                        SessionRow(
                            s,
                            g.node ?: s.run?.nodeId?.let { ui.nodes[it] },
                            ui.now,
                            showMachine = ui.grouping != Grouping.MACHINE || g.pinned,
                            onTogglePin = { vm.togglePin(s) },
                            lead = ui.leads[s.session.sessionId],
                        ) {
                            vm.markSeen(s.session.sessionId)
                            onOpen(s.session.sessionId)
                        }
                    }
                }
                if (ui.endedHidden > 0 || ui.showEnded) {
                    item(key = "ended-toggle") { EndedToggle(ui.endedHidden, ui.showEnded, vm::toggleEnded) }
                }
                item(key = "fab-space") { Spacer(Modifier.size(88.dp)) }
            }
        }
    }
}

/**
 * "2 need you · 1 message" and the grouping toggle: Project (default) ·
 * Needs you · Machine.
 */
@Composable
private fun GroupingBar(ui: SessionsUi, onGrouping: (Grouping) -> Unit) {
    Column(Modifier.fillMaxWidth().padding(start = 16.dp, end = 16.dp, top = 4.dp)) {
        val parts = buildList {
            if (ui.needsYou > 0) add("! ${ui.needsYou} need${if (ui.needsYou == 1) "s" else ""} you")
            if (ui.messages > 0) add("✉ ${ui.messages} message${if (ui.messages == 1) "" else "s"}")
        }
        Text(
            if (parts.isEmpty()) "✓ Nothing needs you" else parts.joinToString(" · "),
            style = MaterialTheme.typography.bodyMedium,
            color = if (ui.needsYou > 0) toneColor(Tone.ATTENTION) else MaterialTheme.colorScheme.onSurfaceVariant,
            fontWeight = if (ui.needsYou > 0) FontWeight.SemiBold else FontWeight.Normal,
            modifier = Modifier.testTag("flag-summary"),
        )
        Row(Modifier.padding(top = 4.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Grouping.entries.forEach { g ->
                FilterChip(
                    selected = ui.grouping == g,
                    onClick = { onGrouping(g) },
                    label = { Text(g.label) },
                    modifier = Modifier.testTag("group-" + g.name.lowercase()),
                )
            }
        }
    }
}

/** "3 ended — show" / "Ended shown — hide": the collapsed ended sessions (the TUI's "."). */
@Composable
private fun EndedToggle(hidden: Int, shown: Boolean, onToggle: () -> Unit) {
    Row(
        Modifier.fillMaxWidth()
            .clickable(role = Role.Button, onClick = onToggle)
            .heightIn(min = 48.dp)
            .padding(horizontal = 16.dp, vertical = 12.dp)
            .testTag("ended-toggle"),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            if (shown) "Ended shown — " else "$hidden ended — ",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Text(
            if (shown) "hide" else "show",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.primary,
            fontWeight = FontWeight.SemiBold,
        )
    }
}

/**
 * Group name and count; a machine group adds a status dot and word, and an
 * unhealthy machine says when it was last seen. A project group's [summary]
 * sums its leads ("● 2 asks · ◆ 1 waiting · • 1 new"); "" reads quiet (dim name).
 */
@Composable
private fun GroupHeader(g: NodeGroup, now: Instant, onOpenMachine: (String) -> Unit, summary: String? = null) {
    val node = g.node
    Column(
        Modifier.fillMaxWidth()
            .let { if (node != null) it.clickable(role = Role.Button) { onOpenMachine(node.nodeId) } else it }
            .padding(start = 16.dp, end = 16.dp, top = 20.dp, bottom = 4.dp),
    ) {
        Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.heightIn(min = 32.dp)) {
            val quiet = summary != null && summary.isEmpty()
            Text(
                g.title,
                style = MaterialTheme.typography.titleSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = if (quiet) 0.6f else 1f),
                modifier = Modifier.weight(1f, fill = false).semantics { heading() },
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            Text(
                "  ${g.sessions.size}",
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.7f),
                modifier = Modifier.weight(1f).testTag("count-" + g.nodeId),
            )
            node?.let { StatusChip(Status.node(it, now)) }
        }
        if (!summary.isNullOrEmpty()) {
            Text(
                summary,
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.testTag("lead-summary-" + g.nodeId),
            )
        }
        if (node != null && node.connection.state != ConnectionState.CONNECTED) {
            Supporting(Status.node(node, now).detail.replaceFirstChar { it.uppercase() })
        }
    }
}

/**
 * One row per session: dot · title (a small pin when pinned) · its
 * attention flag ("! needs you · approval" amber, "✉ message" accent) or
 * status word · harness · machine (outside machine grouping) · age. A long
 * press offers Pin / Unpin. A lead ([lead] non-null) carries its signal as a
 * chip at the end of the title line ("● asks 2", "◆ waiting", "• new",
 * "◌ working"), left out when the row itself already says it; a quiet lead's
 * title is dim.
 */
@OptIn(ExperimentalFoundationApi::class)
@Composable
fun SessionRow(
    s: SessionView,
    node: Node?,
    now: Instant,
    showMachine: Boolean = false,
    onTogglePin: (() -> Unit)? = null,
    lead: LeadSignal? = null,
    onClick: () -> Unit,
) {
    val status = Status.row(s, node, now)
    val level = Flags.of(s).first
    val needsYou = Flags.needsYou(s) && level == Flags.NEEDS_YOU
    val hasMessage = level == Flags.MESSAGE
    val history = Flags.history(s) // an ended session's failure: shown, not counted
    val pinned = Pins.isPinned(s)
    var menu by remember { mutableStateOf(false) }
    Box {
    Row(
        Modifier.fillMaxWidth()
            .combinedClickable(
                role = Role.Button,
                onClick = onClick,
                onLongClickLabel = if (onTogglePin == null) null else if (pinned) "Unpin" else "Pin",
                onLongClick = onTogglePin?.let { { menu = true } },
            )
            .heightIn(min = 72.dp)
            .padding(horizontal = 16.dp, vertical = 10.dp)
            .testTag("session-${s.session.sessionId}"),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        StatusDot(if (needsYou && status.tone != Tone.ERROR) Tone.ATTENTION else status.tone)
        Spacer(Modifier.width(16.dp))
        Column(Modifier.weight(1f)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                val quietLead = lead != null && lead.kind == LeadKind.QUIET
                // The row's own status already says "working" / "needs you" for the lead itself.
                val redundant = lead != null && ((lead.kind == LeadKind.WORKING && Leads.working(s)) || (lead.kind == LeadKind.WAITING && Flags.needsYou(s)))
                val chip = if (lead != null && !redundant) lead.chip else ""
                Text(
                    (if (s.session.icon.isNotEmpty()) s.session.icon + " " else "") + s.session.title.ifEmpty { s.session.sessionId },
                    style = MaterialTheme.typography.bodyLarge,
                    color = if (quietLead) MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.7f) else MaterialTheme.colorScheme.onSurface,
                    fontWeight = if (needsYou || hasMessage) FontWeight.SemiBold else FontWeight.Normal,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f, fill = chip.isNotEmpty()),
                )
                if (pinned) {
                    Spacer(Modifier.width(6.dp))
                    PyreIcon(
                        R.drawable.ic_push_pin,
                        "Pinned",
                        Modifier.size(14.dp).testTag("pin-${s.session.sessionId}"),
                        tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                if (lead != null && chip.isNotEmpty()) {
                    Spacer(Modifier.width(8.dp))
                    Text(
                        chip,
                        style = MaterialTheme.typography.labelMedium,
                        color = when (lead.kind) {
                            LeadKind.ASKS -> MaterialTheme.colorScheme.error
                            LeadKind.WAITING -> toneColor(Tone.ATTENTION)
                            LeadKind.NEW -> MaterialTheme.colorScheme.tertiary
                            else -> MaterialTheme.colorScheme.onSurfaceVariant
                        },
                        fontWeight = if (lead.kind >= LeadKind.WAITING) FontWeight.SemiBold else FontWeight.Normal,
                        maxLines = 1,
                        modifier = Modifier.testTag("lead-chip-${s.session.sessionId}"),
                    )
                }
            }
            val emphasize = status.tone == Tone.ATTENTION || status.tone == Tone.ERROR
            val wordColor = if (emphasize) toneColor(status.tone) else MaterialTheme.colorScheme.onSurfaceVariant
            val flagColor = when {
                needsYou -> toneColor(Tone.ATTENTION)
                history -> toneColor(Tone.ERROR)
                else -> MaterialTheme.colorScheme.tertiary
            }
            val machine = if (showMachine) s.nodeName.ifEmpty { node?.name ?: s.run?.nodeId.orEmpty() }.let { n ->
                if (n.isNotEmpty() && node != null && node.connection.state != ConnectionState.CONNECTED) "$n (" + Status.node(node, now).label.lowercase() + ")" else n
            } else ""
            Text(
                buildAnnotatedString {
                    if (needsYou || hasMessage || history) {
                        withStyle(SpanStyle(color = flagColor, fontWeight = FontWeight.SemiBold)) { append(Flags.label(s)) }
                        append(" · ")
                    }
                    withStyle(SpanStyle(color = wordColor, fontWeight = if (emphasize) FontWeight.SemiBold else FontWeight.Normal)) { append(status.label) }
                    listOf(status.phrase, s.session.harness, machine, status.age).filter { it.isNotEmpty() }.forEach { append(" · "); append(it) }
                },
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.testTag("row-line-${s.session.sessionId}"),
            )
            s.flag?.text?.takeIf { it.isNotEmpty() }?.let {
                Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
            }
        }
        if (needsYou && s.attention > 0) {
            Spacer(Modifier.width(12.dp))
            CountBadge(s.attention, "${s.attention} waiting for you")
        }
    }
    if (onTogglePin != null) {
        DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
            DropdownMenuItem(
                text = { Text(if (pinned) "Unpin" else "Pin") },
                onClick = { menu = false; onTogglePin() },
                modifier = Modifier.testTag("row-pin-${s.session.sessionId}"),
            )
        }
    }
    }
}
