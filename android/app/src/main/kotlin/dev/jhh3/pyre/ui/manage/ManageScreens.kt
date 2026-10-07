package dev.jhh3.pyre.ui.manage

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.AnnotatedString
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
import dev.jhh3.pyre.domain.Status
import dev.jhh3.pyre.model.AccountView
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.NodeResources
import dev.jhh3.pyre.ui.common.CardBlock
import dev.jhh3.pyre.ui.common.EmptyState
import dev.jhh3.pyre.ui.common.Expandable
import dev.jhh3.pyre.ui.common.KeyValue
import dev.jhh3.pyre.ui.common.LoadView
import dev.jhh3.pyre.ui.common.OnRetry
import dev.jhh3.pyre.ui.common.StatusChip
import dev.jhh3.pyre.ui.common.Supporting
import java.time.Instant
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

// ---------------- machines ----------------

class MachinesViewModel(private val repo: Repository) : ViewModel() {
    private val _ui = MutableStateFlow<Load<List<Node>>>(Load.Loading)
    val ui: StateFlow<Load<List<Node>>> = _ui.asStateFlow()
    val now: Instant get() = repo.clock.now()

    init { refresh() }

    fun refresh() = viewModelScope.launch { _ui.value = repo.nodes() }
}

@Composable
fun MachinesScreen(vm: MachinesViewModel, highlight: String? = null) {
    val load by vm.ui.collectAsStateWithLifecycle()
    val clipboard = LocalClipboardManager.current
    val now = vm.now
    OnRetry { vm.refresh() }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).testTag("machines-screen")) {
        LoadView(load, now, { vm.refresh() }) { nodes ->
            if (nodes.isEmpty()) EmptyState("No machines yet", "Enroll a computer with `pyre enroll-node` to run sessions on it.", R.drawable.ic_dns)
            nodes.sortedWith(compareByDescending<Node> { it.nodeId == highlight }.thenBy { it.name }).forEach { n ->
                val st = Status.node(n, now)
                CardBlock(modifier = Modifier.testTag("node-${n.nodeId}")) {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text(n.name, style = MaterialTheme.typography.titleMedium, modifier = Modifier.weight(1f))
                        StatusChip(st)
                    }
                    Supporting(st.detail.replaceFirstChar { it.uppercase() } + " · " + "${n.platform}/${n.arch}", Modifier.padding(top = 2.dp))
                    val r = n.resources
                    Supporting(
                        "${r.activeRuns}" + (if (r.maxRuns > 0) " of ${r.maxRuns}" else "") + " sessions running · load %.1f on %d CPUs · %s free".format(r.load1, r.cpuCount, mb(r.memAvailMb)),
                        Modifier.padding(top = 2.dp),
                    )
                    machineStatsLine(r)?.let { Supporting(it, Modifier.padding(top = 2.dp)) }
                    machineWarnings(r).takeIf { it.isNotEmpty() }?.let {
                        Text(it.joinToString(" · "), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error, modifier = Modifier.padding(top = 2.dp))
                    }
                    Text("Agents", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(top = 12.dp))
                    n.harnesses.forEach { h ->
                        Text(
                            h.harness + if (h.available) " ${h.version}" else " · unavailable (${h.reason.ifEmpty { "unknown" }})",
                            style = MaterialTheme.typography.bodyMedium,
                            color = if (h.available) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    if (n.sshAlias.isNotEmpty()) {
                        TextButton(onClick = { clipboard.setText(AnnotatedString("ssh ${n.sshAlias}")) }, modifier = Modifier.heightIn(min = 48.dp)) {
                            Text("Copy “ssh ${n.sshAlias}”")
                        }
                    }
                    Expandable("Details", initiallyExpanded = n.nodeId == highlight) {
                        if (n.hostname.isNotEmpty()) KeyValue("Hostname", n.hostname)
                        KeyValue("Versions", n.versions.entries.joinToString(", ") { "${it.key} ${it.value}" })
                        KeyValue("Memory", "${mb(r.memAvailMb)} free of ${mb(r.memTotalMb)}")
                        KeyValue("Pyre itself", "%.0f MB memory · %.1f%% CPU".format(r.pyreRssMb, r.pyreCpuPct))
                        KeyValue("Agent processes", "%.0f MB memory · %.1f%% CPU".format(r.agentRssMb, r.agentCpuPct))
                        KeyValue("Measured", Times.ago(r.observedAt, now))
                        n.connection.remoteAddr.takeIf { it.isNotEmpty() }?.let { KeyValue("Connected from", it) }
                    }
                }
            }
        }
    }
}

private fun mb(v: Long): String = if (v >= 1024) "%.1f GB".format(v / 1024.0) else "$v MB"

/** "CPU 35% · memory 63% · swap 7.6 GB of 9.0 GB · disk 15.1 GB free", or null for an older pyred. */
internal fun machineStatsLine(r: NodeResources): String? {
    if (r.cpuPct <= 0 && r.swapTotalMb <= 0 && r.memPressure.isEmpty() && r.disks.isEmpty()) return null
    val parts = mutableListOf<String>()
    if (r.cpuPct > 0) parts += "CPU %.0f%%".format(r.cpuPct)
    if (r.memTotalMb > 0) parts += "memory %.0f%%".format(100.0 * (r.memTotalMb - r.memAvailMb) / r.memTotalMb)
    if (r.swapTotalMb > 0) parts += "swap ${mb(r.swapUsedMb)} of ${mb(r.swapTotalMb)}"
    r.disks.minByOrNull { if (it.totalMb > 0) it.freeMb.toDouble() / it.totalMb else 1.0 }?.let { parts += "disk ${mb(it.freeMb)} free" }
    return parts.joinToString(" · ")
}

/** Same thresholds as the CLI/TUI: disk < 10% free, pressure warn/critical, swap >= 50% and >= 1 GiB. */
internal fun machineWarnings(r: NodeResources): List<String> {
    val out = mutableListOf<String>()
    r.disks.filter { it.totalMb > 0 && it.freeMb < 0.10 * it.totalMb }.forEach { out += "low disk: ${mb(it.freeMb)} free" }
    if (r.memPressure == "warn" || r.memPressure == "critical") out += "memory pressure ${r.memPressure}"
    if (r.swapTotalMb > 0 && r.swapUsedMb >= 1024 && r.swapUsedMb >= 0.5 * r.swapTotalMb) out += "heavy swap"
    return out
}

// ---------------- accounts ----------------

/** Accounts plus what the screen needs to name machines and sessions (both optional). */
data class AccountsData(
    val accounts: List<AccountView>,
    val nodes: List<Node> = emptyList(),
    val sessionTitles: Map<String, String> = emptyMap(),
)

class AccountsViewModel(private val repo: Repository) : ViewModel() {
    private val _ui = MutableStateFlow<Load<AccountsData>>(Load.Loading)
    val ui: StateFlow<Load<AccountsData>> = _ui.asStateFlow()
    val now: Instant get() = repo.clock.now()
    val deviceLogin = DeviceLoginController(repo, viewModelScope) // DeviceLogin.kt

    init { refresh() }

    fun refresh() = viewModelScope.launch {
        val accounts = repo.accounts()
        // Machine names and session titles are a nicety: ids are shown if they fail to load.
        val nodes = (repo.nodes() as? Load.Ready)?.value.orEmpty()
        val titles = (repo.sessions() as? Load.Ready)?.value.orEmpty()
            .associate { it.session.sessionId to it.session.title }
        _ui.value = when (accounts) {
            is Load.Ready -> Load.Ready(AccountsData(accounts.value, nodes, titles), accounts.cachedAt, accounts.error)
            is Load.Failed -> accounts
            Load.Loading -> Load.Loading
        }
    }
}

/** Theme colour for a usage band: tertiary (green in Pyre's palette) / primary (amber-flame) / error. */
@Composable
private fun levelColor(l: UsageLevel): Color = when (l) {
    UsageLevel.LOW -> MaterialTheme.colorScheme.tertiary
    UsageLevel.HIGH -> MaterialTheme.colorScheme.primary
    UsageLevel.CRITICAL -> MaterialTheme.colorScheme.error
}

@Composable
private fun stateColor(s: CardState): Color = when (s) {
    CardState.NEEDS_REAUTH, CardState.AT_LIMIT -> MaterialTheme.colorScheme.error
    CardState.NEAR_LIMIT -> MaterialTheme.colorScheme.primary
    CardState.HEALTHY, CardState.READY_ON_USE -> MaterialTheme.colorScheme.tertiary
    CardState.UNKNOWN, CardState.DISABLED, CardState.AMBIENT -> MaterialTheme.colorScheme.onSurfaceVariant
}

@Composable
fun AccountsScreen(vm: AccountsViewModel) {
    val load by vm.ui.collectAsStateWithLifecycle()
    val now = vm.now
    OnRetry { vm.refresh() }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).testTag("accounts-screen")) {
        LoadView(load, now, { vm.refresh() }) { data ->
            var addLogin by rememberSaveable { mutableStateOf(false) } // DeviceLogin.kt
            TextButton(onClick = { addLogin = true }, modifier = Modifier.padding(horizontal = 8.dp).testTag("add-device-login")) { Text("Add login on a machine") }
            if (addLogin) DeviceLoginDialog(vm.deviceLogin, data.nodes) { addLogin = false; vm.refresh() }
            val sections = remember(data, now) { AccountUsage.sections(data.accounts, data.nodes, data.sessionTitles, now) }
            if (sections.isEmpty()) {
                EmptyState(
                    "No accounts reported",
                    "Machines report their Claude and Codex subscriptions here once rota manages them.",
                    R.drawable.ic_person,
                )
            }
            sections.forEach { s ->
                ProviderHeader(s)
                s.groups.forEach { g ->
                    Text(
                        g.title,
                        style = MaterialTheme.typography.labelLarge,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.fillMaxWidth().padding(start = 16.dp, end = 16.dp, top = 12.dp, bottom = 2.dp).semantics { heading() },
                    )
                    g.cards.forEach { c -> UsageCard(c) }
                }
            }
        }
        Supporting(
            "Only subscription accounts are listed. Pyre never falls back to API billing or to another provider, and never shows tokens.",
            Modifier.padding(16.dp),
        )
    }
}

/** "Anthropic · 3 ready · 5 near limit": amber when every available account is near its limit, red when none is available. */
@Composable
private fun ProviderHeader(s: ProviderSection) {
    Text(
        s.header,
        style = MaterialTheme.typography.titleMedium,
        color = when {
            s.bad -> MaterialTheme.colorScheme.error
            s.warn -> MaterialTheme.colorScheme.primary
            else -> MaterialTheme.colorScheme.onSurface
        },
        fontWeight = if (s.warn || s.bad) FontWeight.SemiBold else FontWeight.Normal,
        modifier = Modifier.fillMaxWidth().padding(start = 16.dp, end = 16.dp, top = 24.dp, bottom = 4.dp)
            .semantics { heading() }.testTag("provider-${s.provider}"),
    )
}

/** Dot + word, coloured by state (never colour alone). */
@Composable
private fun StateChip(s: CardState) {
    val c = stateColor(s)
    val strong = s == CardState.NEEDS_REAUTH || s == CardState.AT_LIMIT || s == CardState.NEAR_LIMIT
    Row(Modifier.testTag("status").semantics(mergeDescendants = true) {}, verticalAlignment = Alignment.CenterVertically) {
        Canvas(Modifier.size(10.dp)) {
            if (s == CardState.UNKNOWN || s == CardState.DISABLED || s == CardState.AMBIENT) {
                drawCircle(c, radius = size.minDimension / 2 - 1.dp.toPx(), style = Stroke(1.5.dp.toPx()))
            } else {
                drawCircle(c)
            }
        }
        Spacer(Modifier.width(6.dp))
        Text(s.label, color = c, style = MaterialTheme.typography.labelLarge, fontWeight = if (strong) FontWeight.SemiBold else FontWeight.Normal)
    }
}

@Composable
private fun UsageCard(c: AccountCard) {
    val muted = c.state == CardState.DISABLED || c.state == CardState.AMBIENT
    CardBlock(
        modifier = Modifier.testTag(if (c.accountId.isEmpty()) "account-${c.key}" else "account-${c.accountId}-${c.nodeId}"),
        color = if (muted) MaterialTheme.colorScheme.surfaceContainerLowest else MaterialTheme.colorScheme.surfaceContainerLow,
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(
                c.title,
                style = MaterialTheme.typography.titleMedium,
                color = if (muted) MaterialTheme.colorScheme.onSurfaceVariant else MaterialTheme.colorScheme.onSurface,
                modifier = Modifier.weight(1f).padding(end = 8.dp),
            )
            if (c.state != CardState.AMBIENT) StateChip(c.state)
        }
        c.detail?.let { d ->
            val strong = c.state == CardState.NEEDS_REAUTH || c.state == CardState.AT_LIMIT
            Text(
                d,
                style = MaterialTheme.typography.bodyMedium,
                color = if (strong) stateColor(c.state) else MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(top = 2.dp),
            )
        }
        if (c.bars.isNotEmpty()) {
            Column(Modifier.padding(top = 8.dp).alpha(if (c.lastKnown) 0.5f else 1f)) {
                c.bars.forEach { b -> UsageBarRow(b, c.lastKnown) }
            }
        }
        c.secondary?.let { Supporting(it, Modifier.padding(top = 6.dp)) }
        c.inUse.forEach { Text(it, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.padding(top = 6.dp)) }
        val footer = listOfNotNull(c.observed, c.login).joinToString(" · ")
        if (footer.isNotEmpty()) {
            Text(footer, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(top = 6.dp))
        }
    }
}

@Composable
private fun UsageBarRow(b: UsageBar, lastKnown: Boolean) {
    val color = levelColor(b.level)
    val label = if (lastKnown) "${b.label} · last known" else b.label
    Column(
        Modifier.padding(vertical = 4.dp).semantics(mergeDescendants = true) {
            contentDescription = "$label ${AccountUsage.pct(b.usedPercent)} used" + (b.resetText?.let { ", $it" } ?: "")
        },
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(label, style = MaterialTheme.typography.labelLarge, modifier = Modifier.weight(1f))
            Text(AccountUsage.pct(b.usedPercent), style = MaterialTheme.typography.labelLarge, color = if (b.level == UsageLevel.LOW) MaterialTheme.colorScheme.onSurface else color)
        }
        LinearProgressIndicator(
            progress = { (b.usedPercent / 100.0).toFloat().coerceIn(0f, 1f) },
            color = color,
            trackColor = MaterialTheme.colorScheme.surfaceContainerHighest,
            drawStopIndicator = {},
            modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp).height(6.dp),
        )
        b.resetText?.let { Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
    }
}
