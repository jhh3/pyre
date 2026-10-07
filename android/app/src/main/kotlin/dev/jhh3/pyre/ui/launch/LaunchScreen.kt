package dev.jhh3.pyre.ui.launch

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Surface
import androidx.compose.ui.Alignment
import androidx.compose.ui.semantics.Role
import dev.jhh3.pyre.R
import dev.jhh3.pyre.ui.common.ListRow
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.Supporting
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.data.valueOrNull
import dev.jhh3.pyre.domain.Status
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.AccountPolicy
import dev.jhh3.pyre.model.AccountPolicyMode
import dev.jhh3.pyre.model.AccountSelectParams
import dev.jhh3.pyre.model.AccountSelection
import dev.jhh3.pyre.model.CommandOutcome
import dev.jhh3.pyre.model.ConnectionState
import dev.jhh3.pyre.model.DirListParams
import dev.jhh3.pyre.model.DirListResult
import dev.jhh3.pyre.model.Harness
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.NodeActionRequest
import dev.jhh3.pyre.model.NodeActions
import dev.jhh3.pyre.model.Project
import dev.jhh3.pyre.model.StartSessionRequest
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.SectionHeader
import dev.jhh3.pyre.ui.common.StatusChip
import dev.jhh3.pyre.ui.common.ToneText
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.serialization.KSerializer

data class LaunchUi(
    val nodes: List<Node> = emptyList(),
    val nodesError: String? = null,
    val node: Node? = null,
    val harness: String = "",
    val path: String = "",
    val dirs: List<String> = emptyList(),
    val isGit: Boolean = false,
    val dirStatus: String? = null,
    val projects: List<Project> = emptyList(),
    val project: Project? = null,
    val title: String = "",
    val prompt: String = "",
    val account: AccountSelection? = null,
    val accountStatus: String? = null,
    val launching: Boolean = false,
    val error: String? = null,
    val launchedSessionId: String? = null,
) {
    val canLaunch get() = node != null && harness.isNotEmpty() && (path.isNotEmpty() || project != null) && !launching
}

class LaunchViewModel(private val repo: Repository) : ViewModel() {
    private val _ui = MutableStateFlow(LaunchUi())
    val ui: StateFlow<LaunchUi> = _ui.asStateFlow()
    /** One launch intent per screen visit: retries/double taps reuse this operation_id. */
    private var draftKey = "launch:" + Ids.new("draft")
    /** One-line quick launch by repository (QuickLaunch.kt). */
    val quick = QuickLaunch(repo, viewModelScope, { _ui.value.nodes }) { sid -> _ui.value = _ui.value.copy(launchedSessionId = sid) }

    init {
        viewModelScope.launch {
            when (val n = repo.nodes()) {
                is Load.Ready -> _ui.value = _ui.value.copy(nodes = n.value, nodesError = n.cachedAt?.let { "Machine list is cached (${Times.ago(it, repo.clock.now())}); launch needs the hub." })
                is Load.Failed -> _ui.value = _ui.value.copy(nodesError = n.error.describe())
                Load.Loading -> Unit
            }
            repo.projects().valueOrNull()?.let { _ui.value = _ui.value.copy(projects = it.filterNot { p -> p.archived }) }
        }
    }

    fun selectNode(n: Node) {
        if (n.connection.state != ConnectionState.CONNECTED) return
        val firstHarness = n.harnesses.firstOrNull { it.available && it.harness != Harness.MOCK }?.harness ?: n.harnesses.firstOrNull { it.available }?.harness.orEmpty()
        _ui.value = _ui.value.copy(node = n, harness = firstHarness, path = "", dirs = emptyList(), account = null)
        browse("")
        previewAccount()
    }

    fun selectHarness(h: String) {
        _ui.value = _ui.value.copy(harness = h, account = null)
        previewAccount()
    }

    fun selectProject(p: Project?) { _ui.value = _ui.value.copy(project = p) }
    fun setTitle(s: String) { _ui.value = _ui.value.copy(title = s) }
    fun setPrompt(s: String) { _ui.value = _ui.value.copy(prompt = s) }
    fun setPath(s: String) { _ui.value = _ui.value.copy(path = s) }

    fun browse(path: String) {
        val node = _ui.value.node ?: return
        _ui.value = _ui.value.copy(dirStatus = "Listing…")
        viewModelScope.launch {
            try {
                val r = action(node.nodeId, NodeActions.DIR_LIST, DirListParams.serializer(), DirListParams(path), DirListResult.serializer())
                _ui.value = _ui.value.copy(path = r.path, dirs = r.dirs, isGit = r.isGit, dirStatus = null)
            } catch (e: Exception) {
                _ui.value = _ui.value.copy(dirStatus = "Directory listing unavailable: ${e.message}. You can type a path.")
            }
        }
    }

    fun up() {
        val p = _ui.value.path.trimEnd('/')
        if (p.isEmpty() || p == "/") return
        browse(p.substringBeforeLast('/').ifEmpty { "/" })
    }

    private fun previewAccount() {
        val node = _ui.value.node ?: return
        val provider = Harness.provider(_ui.value.harness)
        if (provider.isEmpty()) {
            _ui.value = _ui.value.copy(account = null, accountStatus = "No subscription account applies to ${_ui.value.harness}.")
            return
        }
        _ui.value = _ui.value.copy(accountStatus = "Resolving account policy…")
        viewModelScope.launch {
            try {
                val sel = action(node.nodeId, NodeActions.ACCOUNT_SELECT, AccountSelectParams.serializer(), AccountSelectParams(provider, AccountPolicy(AccountPolicyMode.ELIGIBLE)), AccountSelection.serializer())
                _ui.value = _ui.value.copy(account = sel, accountStatus = null)
            } catch (e: Exception) {
                _ui.value = _ui.value.copy(account = null, accountStatus = "Account preview unavailable (${e.message}). Policy: any eligible $provider subscription; the hub decides at launch.")
            }
        }
    }

    private suspend fun <P, R> action(nodeId: String, action: String, pSer: KSerializer<P>, params: P, rSer: KSerializer<R>): R {
        val res = repo.call {
            nodeAction(NodeActionRequest(operationId = Ids.operation(), nodeId = nodeId, action = action, params = PyreJson.wire.encodeToJsonElement(pSer, params), timeoutSeconds = 10))
        }
        res.error?.let { throw IllegalStateException(it.message.ifEmpty { it.code }) }
        if (res.outcome != CommandOutcome.SUCCEEDED || res.result == null) throw IllegalStateException("node action ${res.outcome}")
        return PyreJson.wire.decodeFromJsonElement(rSer, res.result)
    }

    fun launch() {
        val u = _ui.value
        if (!u.canLaunch) return
        _ui.value = u.copy(launching = true, error = null)
        viewModelScope.launch {
            try {
                val r = repo.call {
                    startSession(
                        StartSessionRequest(
                            operationId = repo.intents.forIntent(draftKey),
                            nodeId = u.node!!.nodeId,
                            harness = u.harness,
                            title = u.title.trim(),
                            cwd = u.path,
                            projectId = u.project?.projectId,
                            accountPolicy = AccountPolicy(AccountPolicyMode.ELIGIBLE),
                            initialPrompt = u.prompt.trim(),
                            includeContext = u.project != null,
                        ),
                    )
                }
                repo.intents.complete(draftKey)
                draftKey = "launch:" + Ids.new("draft")
                _ui.value = _ui.value.copy(launching = false, launchedSessionId = r.session.session.sessionId)
            } catch (e: PyreApiException) {
                // Transport failure: the launch may have been committed; retrying reuses the same operation_id.
                _ui.value = _ui.value.copy(launching = false, error = e.describe() + if (e.transport || e.code == "outcome_indeterminate") " — retry is safe (same operation id)." else "")
            }
        }
    }
}

/**
 * New session: pick an online machine, the agent, a folder (browsed on that
 * machine), optionally a project, then start. The button stays at the bottom.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun LaunchScreen(vm: LaunchViewModel, onLaunched: (String) -> Unit) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    LaunchedEffect(ui.launchedSessionId) { ui.launchedSessionId?.let(onLaunched) }
    Column(Modifier.fillMaxSize().imePadding()) {
        Column(Modifier.weight(1f).verticalScroll(rememberScrollState()).testTag("launch-screen")) {
            ui.nodesError?.let { Banner(it, Tone.MUTED) }
            QuickLaunchSection(vm.quick)
            SectionHeader("Or choose a machine and folder")
            Column(Modifier.selectableGroup()) {
                ui.nodes.forEach { n ->
                    val online = n.connection.state == ConnectionState.CONNECTED
                    val st = Status.node(n, java.time.Instant.now())
                    Row(
                        Modifier.fillMaxWidth()
                            .selectable(selected = ui.node?.nodeId == n.nodeId, enabled = online, role = Role.RadioButton) { vm.selectNode(n) }
                            .heightIn(min = 56.dp)
                            .padding(horizontal = 16.dp)
                            .testTag("node-${n.nodeId}"),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        RadioButton(selected = ui.node?.nodeId == n.nodeId, onClick = null, enabled = online)
                        Spacer(Modifier.width(16.dp))
                        Text(n.name, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.weight(1f),
                            color = if (online) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant)
                        StatusChip(st)
                    }
                }
            }
            if (ui.nodes.isEmpty() && ui.nodesError == null) Supporting("Loading machines…", Modifier.padding(horizontal = 16.dp))
            ui.node?.let { node ->
                SectionHeader("Agent")
                FlowRow(Modifier.padding(horizontal = 16.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    node.harnesses.forEach { h ->
                        FilterChip(
                            selected = ui.harness == h.harness,
                            onClick = { if (h.available) vm.selectHarness(h.harness) },
                            enabled = h.available,
                            label = { Text(h.harness + if (h.available) "" else " (unavailable)") },
                            modifier = Modifier.heightIn(min = 48.dp),
                        )
                    }
                }
                node.harnesses.filter { !it.available && it.reason.isNotEmpty() }.forEach { Supporting("${it.harness}: ${it.reason}", Modifier.padding(horizontal = 16.dp)) }
                SectionHeader("Folder on ${node.name}")
                OutlinedTextField(
                    ui.path,
                    vm::setPath,
                    Modifier.fillMaxWidth().padding(horizontal = 16.dp).testTag("launch-path"),
                    label = { Text("Working folder") },
                    singleLine = true,
                    supportingText = { Text(if (ui.isGit) "Git repository" else ui.dirStatus ?: "Tap a folder below or type a path") },
                )
                Row(Modifier.padding(horizontal = 8.dp)) {
                    TextButton(onClick = vm::up, modifier = Modifier.heightIn(min = 48.dp)) { Text("Up one level") }
                    TextButton(onClick = { vm.browse(ui.path) }, modifier = Modifier.heightIn(min = 48.dp)) { Text("Show folders") }
                }
                ui.dirs.take(200).forEach { d ->
                    ListRow(
                        "$d/",
                        leading = { PyreIcon(R.drawable.ic_folder, null, tint = MaterialTheme.colorScheme.onSurfaceVariant) },
                        onClick = { vm.browse(ui.path.trimEnd('/') + "/" + d) },
                    )
                }
                SectionHeader("Project")
                Column(Modifier.selectableGroup()) {
                    (listOf<Project?>(null) + ui.projects).forEach { p ->
                        val sel = ui.project?.projectId == p?.projectId
                        Row(
                            Modifier.fillMaxWidth().selectable(selected = sel, role = Role.RadioButton) { vm.selectProject(p) }.heightIn(min = 48.dp).padding(horizontal = 16.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            RadioButton(selected = sel, onClick = null)
                            Spacer(Modifier.width(16.dp))
                            Text(p?.name ?: "No project", style = MaterialTheme.typography.bodyLarge)
                        }
                    }
                }
                SectionHeader("Account")
                Supporting(
                    "Any eligible ${Harness.provider(ui.harness).ifEmpty { "—" }} subscription. Pyre never switches providers.",
                    Modifier.padding(horizontal = 16.dp),
                )
                ui.account?.let { a ->
                    if (a.accountId.isNotEmpty()) {
                        Text("Will use ${a.accountId}", style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp))
                        Supporting(a.reason + " · usage " + (if (a.freshness.stale) "stale" else "fresh") + ", seen ${Times.ago(a.freshness.observedAt, java.time.Instant.now())}", Modifier.padding(horizontal = 16.dp))
                    } else {
                        ToneText("No eligible account (${a.outcome.ifEmpty { "unknown" }}): ${a.reason}", Tone.ATTENTION, Modifier.padding(horizontal = 16.dp, vertical = 4.dp))
                    }
                }
                ui.accountStatus?.let { Supporting(it, Modifier.padding(horizontal = 16.dp, vertical = 4.dp)) }
                SectionHeader("Details (optional)")
                OutlinedTextField(ui.title, vm::setTitle, Modifier.fillMaxWidth().padding(horizontal = 16.dp), label = { Text("Title") }, singleLine = true)
                OutlinedTextField(
                    ui.prompt,
                    vm::setPrompt,
                    Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp),
                    label = { Text("First instruction") },
                    minLines = 2,
                    maxLines = 6,
                )
            }
            Spacer(Modifier.size(16.dp))
        }
        Surface(color = MaterialTheme.colorScheme.surfaceContainer, modifier = Modifier.fillMaxWidth()) {
            Column(Modifier.padding(horizontal = 16.dp, vertical = 12.dp)) {
                ui.error?.let { ToneText(it, Tone.ERROR, Modifier.padding(bottom = 8.dp)) }
                Button(onClick = vm::launch, enabled = ui.canLaunch, modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp).testTag("launch")) {
                    Text(if (ui.launching) "Starting…" else "Start session")
                }
            }
        }
    }
}
