package dev.jhh3.pyre.ui.manage

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.R
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.AuditEntry
import dev.jhh3.pyre.model.AuditOps
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.model.ListAuditRequest
import dev.jhh3.pyre.model.Page
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.CachedBanner
import dev.jhh3.pyre.ui.common.EmptyState
import dev.jhh3.pyre.ui.common.ErrorBox
import dev.jhh3.pyre.ui.common.LinkButton
import dev.jhh3.pyre.ui.common.LoadingBox
import dev.jhh3.pyre.ui.common.OnRetry
import dev.jhh3.pyre.ui.common.Pill
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.Locale
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

// Activity: the hub's audit log (`audit.list`), newest first. Read-only.

enum class ActivityFilter(val label: String, val kinds: List<String>) {
    ALL("All", emptyList()),
    ACCOUNTS("Accounts", listOf(AuditOps.KIND_ACCOUNT)),
    SESSIONS("Sessions", listOf(AuditOps.KIND_SESSION)),
    FLEET("Fleet", listOf(AuditOps.KIND_FLEET)),
}

/** Pure formatting behind the Activity screen. */
object Activity {
    const val PAGE = 100
    const val UNSUPPORTED_TEXT = "This hub does not offer the activity log yet; upgrade pyre-hub."

    /** A hub that predates `audit.list` answers 404 for the path (501/unsupported treated alike). */
    fun unsupported(e: PyreApiException): Boolean = e.httpStatus == 404 || e.code == ErrorCodes.UNSUPPORTED

    /** Account switches (and "no other account was eligible") are tinted so they are easy to find. */
    fun isSwitch(e: AuditEntry): Boolean = e.action == AuditOps.ACCOUNT_SWITCHED || e.action == AuditOps.ACCOUNT_WAITING

    fun machine(e: AuditEntry): String = e.nodeName.ifEmpty { e.nodeId.removePrefix("node_") }

    /** "14:05" today, "Tue 14:05" within the last week, else "Sep 28 14:05". */
    fun time(at: String, now: Instant, zone: ZoneId = ZoneId.systemDefault(), locale: Locale = Locale.getDefault()): String {
        val t = Times.parse(at)?.atZone(zone) ?: return "—"
        val day = t.toLocalDate()
        val today = now.atZone(zone).toLocalDate()
        val pattern = when {
            day == today -> "HH:mm"
            day.isBefore(today) && !day.isBefore(today.minusDays(6)) -> "EEE HH:mm"
            else -> "MMM d HH:mm"
        }
        return DateTimeFormatter.ofPattern(pattern, locale).format(t)
    }

    data class Row(
        val key: String,
        val time: String,
        val machine: String,
        val summary: String,
        val automatic: Boolean,
        val highlight: Boolean,
        val details: List<Pair<String, String>>,
    ) {
        val header: String get() = listOf(time, machine).filter { it.isNotEmpty() }.joinToString(" · ")
    }

    fun row(e: AuditEntry, now: Instant, zone: ZoneId = ZoneId.systemDefault(), locale: Locale = Locale.getDefault()) = Row(
        key = "audit-${e.auditId}",
        time = time(e.at, now, zone, locale),
        machine = machine(e),
        summary = e.summary.ifBlank { e.action },
        automatic = e.automatic,
        highlight = isSwitch(e),
        details = details(e),
    )

    fun rows(entries: List<AuditEntry>, now: Instant, zone: ZoneId = ZoneId.systemDefault(), locale: Locale = Locale.getDefault()): List<Row> =
        entries.sortedWith(compareByDescending<AuditEntry> { Times.parse(it.at) ?: Instant.EPOCH }.thenByDescending { it.auditId })
            .map { row(it, now, zone, locale) }

    /** Shown when a row is expanded: what/why/who and the ids. */
    fun details(e: AuditEntry): List<Pair<String, String>> = buildList {
        add("What" to e.action + if (e.automatic) " (automatic)" else "")
        if (e.reason.isNotBlank()) add("Why" to e.reason)
        val who = listOf(e.actor.kind, e.actor.name.ifEmpty { e.actor.id }).filter { it.isNotEmpty() }.joinToString(" · ")
        if (who.isNotEmpty()) add("Who" to who)
        if (e.from.isNotEmpty() || e.to.isNotEmpty()) add("Change" to "${noted(e.from, e.fromNote)} → ${noted(e.to, e.toNote)}")
        if (e.sessionId.isNotEmpty()) add("Session" to named(e.sessionTitle, e.sessionId))
        if (e.accountId.isNotEmpty()) add("Account" to named(e.accountLabel, e.accountId))
        if (e.fromAccountId.isNotEmpty() || e.toAccountId.isNotEmpty()) add("Account ids" to "${e.fromAccountId.ifEmpty { "—" }} → ${e.toAccountId.ifEmpty { "—" }}")
        if (e.runId.isNotEmpty()) add("Run" to e.runId)
        if (e.nodeId.isNotEmpty()) add("Machine" to named(e.nodeName, e.nodeId))
        if (e.at.isNotEmpty()) add("At" to e.at)
    }

    private fun noted(v: String, note: String) = v.ifEmpty { "—" } + if (note.isNotEmpty()) " ($note)" else ""
    private fun named(name: String, id: String) = if (name.isEmpty() || name == id) id else "$name ($id)"
}

data class ActivityUi(
    val filter: ActivityFilter = ActivityFilter.ALL,
    val entries: List<AuditEntry> = emptyList(),
    val nextCursor: String = "",
    val loading: Boolean = true,
    val refreshing: Boolean = false,
    val loadingMore: Boolean = false,
    val unsupported: Boolean = false,
    val error: String? = null,
    val cachedAt: Instant? = null,
    val cachedError: PyreApiException? = null,
)

class ActivityViewModel(private val repo: Repository) : ViewModel() {
    private val _ui = MutableStateFlow(ActivityUi())
    val ui: StateFlow<ActivityUi> = _ui.asStateFlow()
    val now: Instant get() = repo.clock.now()
    private var job: Job? = null

    init { refresh() }

    fun setFilter(f: ActivityFilter) {
        if (_ui.value.filter == f) return
        _ui.value = ActivityUi(filter = f)
        refresh()
    }

    fun refresh() {
        job?.cancel()
        job = viewModelScope.launch { loadFirst() }
    }

    fun pullRefresh() {
        _ui.value = _ui.value.copy(refreshing = true)
        job?.cancel()
        job = viewModelScope.launch {
            try {
                loadFirst()
            } finally {
                _ui.value = _ui.value.copy(refreshing = false)
            }
        }
    }

    fun loadMore() {
        val cur = _ui.value
        if (cur.nextCursor.isEmpty() || cur.loadingMore || cur.unsupported) return
        _ui.value = cur.copy(loadingMore = true)
        viewModelScope.launch {
            try {
                val p = repo.call { listAudit(ListAuditRequest(cursor = cur.nextCursor, limit = Activity.PAGE, kinds = cur.filter.kinds)) }
                val now = _ui.value
                if (now.filter != cur.filter) return@launch
                val seen = now.entries.mapTo(HashSet()) { it.auditId }
                _ui.value = now.copy(entries = now.entries + p.items.filterNot { it.auditId in seen }, nextCursor = p.nextCursor, loadingMore = false)
            } catch (e: PyreApiException) {
                if (_ui.value.filter == cur.filter) _ui.value = _ui.value.copy(loadingMore = false, error = e.describe())
            }
        }
    }

    private suspend fun loadFirst() {
        val f = _ui.value.filter
        val r = repo.cachedFetch("audit_${f.name.lowercase()}", Page.serializer(AuditEntry.serializer())) {
            listAudit(ListAuditRequest(limit = Activity.PAGE, kinds = f.kinds))
        }
        val cur = _ui.value
        if (cur.filter != f) return
        _ui.value = when (r) {
            is Load.Ready ->
                if (r.error?.let(Activity::unsupported) == true) ActivityUi(filter = f, loading = false, refreshing = cur.refreshing, unsupported = true)
                else cur.copy(entries = r.value.items, nextCursor = r.value.nextCursor, loading = false, unsupported = false, error = null, cachedAt = r.cachedAt, cachedError = r.error)
            is Load.Failed ->
                if (Activity.unsupported(r.error)) ActivityUi(filter = f, loading = false, refreshing = cur.refreshing, unsupported = true)
                else cur.copy(loading = false, error = r.error.describe())
            Load.Loading -> cur
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ActivityScreen(vm: ActivityViewModel) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    val now = vm.now
    OnRetry { vm.refresh() }
    Column(Modifier.fillMaxSize().testTag("activity-screen")) {
        Row(
            Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(horizontal = 16.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            ActivityFilter.entries.forEach { f ->
                FilterChip(
                    selected = ui.filter == f,
                    onClick = { vm.setFilter(f) },
                    label = { Text(f.label) },
                    modifier = Modifier.heightIn(min = 48.dp).testTag("activity-filter-${f.name.lowercase()}"),
                )
            }
        }
        PullToRefreshBox(isRefreshing = ui.refreshing, onRefresh = vm::pullRefresh, modifier = Modifier.weight(1f).fillMaxWidth()) {
            when {
                ui.unsupported -> EmptyState("Activity log not available", Activity.UNSUPPORTED_TEXT, R.drawable.ic_history, Modifier.testTag("activity-unsupported"))
                ui.loading && ui.entries.isEmpty() -> LoadingBox()
                ui.entries.isEmpty() && ui.error != null -> ErrorBox(ui.error!!, vm::refresh)
                ui.entries.isEmpty() -> EmptyState(
                    "Nothing recorded yet",
                    "Automatic account switches, session starts and stops, and machine changes show up here.",
                    R.drawable.ic_history,
                )
                else -> {
                    val rows = remember(ui.entries, now) { Activity.rows(ui.entries, now) }
                    LazyColumn(Modifier.fillMaxSize().testTag("activity-list")) {
                        item(key = "cached") { CachedBanner(ui.cachedAt, ui.cachedError, now) }
                        items(rows, key = { it.key }) { r -> ActivityRow(r) }
                        if (ui.nextCursor.isNotEmpty()) {
                            item(key = "more") {
                                LinkButton(if (ui.loadingMore) "Loading…" else "Show older", Modifier.padding(horizontal = 8.dp).testTag("activity-more")) { vm.loadMore() }
                            }
                        }
                    }
                }
            }
        }
    }
}

/** time · machine, an "auto" chip, the hub's summary; account switches tinted. Tap for details. */
@Composable
private fun ActivityRow(r: Activity.Row) {
    var open by rememberSaveable(r.key) { mutableStateOf(false) }
    val cs = MaterialTheme.colorScheme
    val fg = if (r.highlight) cs.onPrimaryContainer else cs.onSurface
    val muted = if (r.highlight) cs.onPrimaryContainer else cs.onSurfaceVariant
    Column(
        Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 2.dp)
            .background(if (r.highlight) cs.primaryContainer else Color.Transparent, RoundedCornerShape(12.dp))
            .clickable(role = Role.Button, onClickLabel = if (open) "Hide details" else "Show details") { open = !open }
            .heightIn(min = 48.dp)
            .padding(horizontal = 8.dp, vertical = 10.dp)
            .testTag("activity-row-${r.key}"),
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(r.header, style = MaterialTheme.typography.labelMedium, color = muted, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
            if (r.automatic) Pill("auto", Tone.MUTED, Modifier.padding(start = 8.dp).testTag("activity-auto"))
        }
        Text(
            r.summary,
            style = MaterialTheme.typography.bodyMedium,
            color = fg,
            fontWeight = if (r.highlight) FontWeight.SemiBold else FontWeight.Normal,
            modifier = Modifier.padding(top = 2.dp),
        )
        if (open) {
            Column(Modifier.padding(top = 6.dp).testTag("activity-details")) {
                r.details.forEach { (k, v) ->
                    Text("$k: $v", style = MaterialTheme.typography.bodySmall, color = muted, modifier = Modifier.padding(top = 2.dp))
                }
            }
        }
    }
}
