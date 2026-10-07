package dev.jhh3.pyre.ui.launch

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.AccountPolicy
import dev.jhh3.pyre.model.AccountPolicyMode
import dev.jhh3.pyre.model.CommandOutcome
import dev.jhh3.pyre.model.ConnectionState
import dev.jhh3.pyre.model.ListReposRequest
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.NodeActionRequest
import dev.jhh3.pyre.model.RepoEntry
import dev.jhh3.pyre.model.RepoOps
import dev.jhh3.pyre.model.StartSessionRequest
import dev.jhh3.pyre.model.WorktreeCreateParams
import dev.jhh3.pyre.model.WorktreeCreateResult
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.ListRow
import dev.jhh3.pyre.ui.common.SectionHeader
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.common.ToneText
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

/**
 * Quick launch (same grammar as `pyre new` and the TUI bar):
 * `repo[+repo] [@machine] [claude|codex|shell] first prompt…`. Repository
 * names complete against the fleet's repo index; the machine defaults to
 * where the repository exists (most recently used first). One repository
 * starts in its main checkout (the agent decides about worktrees); "Fresh
 * worktree" or several repositories create worktrees first.
 */
object QuickLine {
    data class Parsed(val repos: List<String>, val machine: String, val harness: String, val prompt: String)

    fun parse(line: String): Parsed {
        val f = line.trim().split(Regex("\\s+")).filter { it.isNotEmpty() }
        if (f.isEmpty()) return Parsed(emptyList(), "", "", "")
        val repos = f[0].split('+').map { it.trim() }
        var machine = ""
        var harness = ""
        var i = 1
        while (i < f.size && i <= 2) {
            val a = f[i]
            val lower = a.lowercase()
            if (a.startsWith("@") && a.length > 1 && machine.isEmpty()) { machine = a.substring(1); i++; continue }
            if (harness.isEmpty() && lower in setOf("claude", "codex", "shell")) { harness = lower; i++; continue }
            break
        }
        return Parsed(repos, machine, harness, f.drop(i).joinToString(" "))
    }

    private fun subseq(s: String, t: String): Boolean {
        var i = 0
        for (c in s) if (i < t.length && c == t[i]) i++
        return i == t.length
    }

    /** Score mirrors repoindex.Score: name matches far above remote/path matches. */
    fun score(query: String, r: RepoEntry): Int? {
        val terms = query.lowercase().split(Regex("\\s+")).filter { it.isNotEmpty() }
        if (terms.isEmpty()) return 0
        val name = r.name.lowercase()
        val remote = r.remoteUrl.lowercase().removeSuffix(".git")
        val path = r.path.lowercase()
        var total = 0
        for (t in terms) {
            var best = when {
                name == t -> 1000
                name.startsWith(t) -> 600 - minOf(name.length - t.length, 100)
                name.contains(t) -> 400 - minOf(name.length - t.length, 100)
                name.split('-', '_', '.', ' ').filter { it.isNotEmpty() }.let { w -> w.size > 1 && w.joinToString("") { it.take(1) }.startsWith(t) } -> 300
                subseq(name, t) -> 150 - minOf(name.length - t.length, 100)
                else -> 0
            }
            if (best == 0) best = when {
                remote.endsWith("/$t") || remote.endsWith(":$t") -> 500
                remote.contains(t) -> 120
                path.contains(t) -> 80
                else -> 0
            }
            if (best == 0) return null
            total += best
        }
        return total
    }

    fun key(r: RepoEntry): String {
        var u = r.remoteUrl.trim().lowercase().removeSuffix(".git")
        if (u.isEmpty()) return "name:" + r.name.lowercase()
        u.indexOf("://").takeIf { it >= 0 }?.let { u = u.substring(it + 3) }
        u.indexOf('@').takeIf { it >= 0 }?.let { u = u.substring(it + 1) }
        return "remote:" + u.replaceFirst(':', '/')
    }

    /** Ranked groups (one repository on several machines is one choice). */
    fun suggest(query: String, all: List<RepoEntry>, limit: Int = 6): List<List<RepoEntry>> {
        val scored = all.mapNotNull { e -> score(query, e)?.let { e.copy(score = it) } }
            .sortedWith(compareByDescending<RepoEntry> { it.score }.thenByDescending { it.lastUsedAt != null }
                .thenByDescending { maxOf(it.lastUsedAt.orEmpty(), it.lastActivity) }.thenBy { it.name })
        val groups = LinkedHashMap<String, MutableList<RepoEntry>>()
        for (e in scored) groups.getOrPut(key(e)) { mutableListOf() }.add(e)
        return groups.values.take(limit)
    }

    /** Machines holding every chosen repository: connected, then most recently used. */
    fun machines(chosen: List<List<RepoEntry>>, connected: Set<String>): List<String> {
        if (chosen.isEmpty()) return emptyList()
        val common = chosen.map { g -> g.map { it.nodeId }.toSet() }.reduce { a, b -> a intersect b }
        fun used(id: String) = chosen.flatten().filter { it.nodeId == id }.maxOfOrNull { it.lastUsedAt.orEmpty() }.orEmpty()
        fun active(id: String) = chosen.flatten().filter { it.nodeId == id }.maxOfOrNull { it.lastActivity }.orEmpty()
        return common.sortedWith(compareByDescending<String> { it in connected }.thenByDescending { used(it) }.thenByDescending { active(it) }.thenBy { it })
    }
}

data class QuickUi(
    val line: String = "",
    val repos: List<RepoEntry> = emptyList(),
    val indexStatus: String? = "Loading repositories…",
    val selected: Int = 0,
    val machine: String = "",
    val fresh: Boolean = false,
    val launching: Boolean = false,
    val error: String? = null,
)

/** Plan derived from [QuickUi] (what Start would do). */
data class QuickPlan(
    val suggestions: List<List<RepoEntry>>,
    val chosen: List<List<RepoEntry>>,
    val machines: List<String>,
    val nodeId: String,
    val harness: String,
    val prompt: String,
    val fresh: Boolean,
    val problem: String?,
)

class QuickLaunch(
    private val repo: Repository,
    private val scope: CoroutineScope,
    private val nodes: () -> List<Node>,
    private val onLaunched: (String) -> Unit,
) {
    private val _ui = MutableStateFlow(QuickUi())
    val ui: StateFlow<QuickUi> = _ui.asStateFlow()
    private var draftKey = "quicklaunch:" + Ids.new("draft")

    init {
        scope.launch {
            try {
                val list = repo.call { listRepos(ListReposRequest(limit = 500)) }
                _ui.value = _ui.value.copy(repos = list.items, indexStatus = if (list.items.isEmpty()) "No repositories indexed yet (machines scan ~/code, ~/src, ~/Documents/code and ~/projects every 10 minutes)." else null)
            } catch (e: Exception) {
                _ui.value = _ui.value.copy(indexStatus = "Repository search unavailable (${e.message}); choose a machine and folder below.")
            }
        }
    }

    fun setLine(s: String) {
        val old = QuickLine.parse(_ui.value.line).repos.lastOrNull()
        val new = QuickLine.parse(s).repos.lastOrNull()
        _ui.value = _ui.value.copy(line = s, selected = if (old == new) _ui.value.selected else 0, error = null)
    }

    /** Tapping a suggestion completes the repository being typed. */
    fun pick(index: Int) {
        val p = plan()
        val g = p.suggestions.getOrNull(index) ?: return
        val line = _ui.value.line
        val head = line.substringBefore(' ', line)
        val rest = if (line.contains(' ')) line.substringAfter(' ') else ""
        val prefix = head.substringBeforeLast('+', "").let { if (it.isEmpty()) "" else "$it+" }
        _ui.value = _ui.value.copy(line = (prefix + g[0].name + " " + rest).trimEnd() + " ", selected = 0, machine = "")
    }

    fun selectMachine(id: String) { _ui.value = _ui.value.copy(machine = id) }
    fun toggleFresh() { _ui.value = _ui.value.copy(fresh = !_ui.value.fresh) }

    fun plan(u: QuickUi = _ui.value): QuickPlan {
        val parsed = QuickLine.parse(u.line)
        val tokens = parsed.repos.ifEmpty { listOf("") }
        val active = tokens.last()
        val sugg = QuickLine.suggest(active, u.repos)
        val chosen = tokens.mapIndexedNotNull { i, t ->
            if (i == tokens.size - 1) sugg.getOrNull(u.selected) ?: sugg.firstOrNull() else QuickLine.suggest(t, u.repos, 1).firstOrNull()
        }
        val connected = nodes().filter { it.connection.state == ConnectionState.CONNECTED }.map { it.nodeId }.toSet()
        val ms = QuickLine.machines(chosen, connected)
        val typed = parsed.machine.takeIf { it.isNotEmpty() }?.let { at -> ms.firstOrNull { id -> nodeName(id).lowercase().startsWith(at.lowercase()) } }
        val node = typed ?: ms.firstOrNull { it == u.machine } ?: ms.firstOrNull().orEmpty()
        val problem = when {
            u.repos.isEmpty() -> u.indexStatus ?: "No repositories indexed yet."
            chosen.size < tokens.size -> "No repository matches “${tokens.first { t -> QuickLine.suggest(t, u.repos, 1).isEmpty() }}”."
            ms.isEmpty() -> "No single machine has all of these repositories."
            parsed.machine.isNotEmpty() && typed == null -> "Not on a machine called “${parsed.machine}”."
            node !in connected -> "${nodeName(node)} is offline."
            else -> null
        }
        return QuickPlan(sugg, chosen, ms, node, parsed.harness.ifEmpty { "claude" }, parsed.prompt, u.fresh || chosen.size > 1, problem)
    }

    fun nodeName(id: String) = nodes().firstOrNull { it.nodeId == id }?.name ?: id

    fun launch() {
        val u = _ui.value
        val p = plan(u)
        if (u.launching) return
        if (p.problem != null) { _ui.value = u.copy(error = p.problem); return }
        val paths = p.chosen.map { g -> g.first { it.nodeId == p.nodeId }.path }
        _ui.value = u.copy(launching = true, error = null)
        scope.launch {
            try {
                var cwd = paths.first()
                if (p.fresh) {
                    val res = repo.call {
                        nodeAction(NodeActionRequest(operationId = repo.intents.forIntent("$draftKey:wt"), nodeId = p.nodeId, action = RepoOps.ACTION_WORKTREE_CREATE,
                            params = PyreJson.wire.encodeToJsonElement(WorktreeCreateParams.serializer(), WorktreeCreateParams(paths, slug = p.prompt)), timeoutSeconds = 30))
                    }
                    res.error?.let { throw IllegalStateException(it.message.ifEmpty { it.code }) }
                    if (res.outcome != CommandOutcome.SUCCEEDED || res.result == null) throw IllegalStateException("worktree creation ${res.outcome}")
                    cwd = PyreJson.wire.decodeFromJsonElement(WorktreeCreateResult.serializer(), res.result).cwd
                }
                val r = repo.call {
                    startSession(StartSessionRequest(operationId = repo.intents.forIntent(draftKey), nodeId = p.nodeId, harness = p.harness, cwd = cwd,
                        accountPolicy = AccountPolicy(AccountPolicyMode.ELIGIBLE), initialPrompt = p.prompt))
                }
                repo.intents.complete("$draftKey:wt")
                repo.intents.complete(draftKey)
                draftKey = "quicklaunch:" + Ids.new("draft")
                _ui.value = _ui.value.copy(launching = false)
                onLaunched(r.session.session.sessionId)
            } catch (e: PyreApiException) {
                _ui.value = _ui.value.copy(launching = false, error = e.describe() + if (e.transport || e.code == "outcome_indeterminate") " — retry is safe (same operation id)." else "")
            } catch (e: Exception) {
                _ui.value = _ui.value.copy(launching = false, error = "Not started: ${e.message}")
            }
        }
    }
}

private fun shortPath(p: String): String = Regex("^(/Users/[^/]+|/home/[^/]+)(/|$)").find(p)?.let { "~" + p.substring(it.groups[1]!!.range.last + 1) } ?: p

/** The one-line field with repository suggestions, at the top of the launch screen. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun QuickLaunchSection(q: QuickLaunch) {
    val ui by q.ui.collectAsStateWithLifecycle()
    val p = q.plan(ui)
    Column(Modifier.testTag("quick-launch")) {
        SectionHeader("Start in a repository")
        OutlinedTextField(
            ui.line,
            q::setLine,
            Modifier.fillMaxWidth().padding(horizontal = 16.dp).testTag("quick-line"),
            label = { Text("repo [@machine] [claude|codex|shell] first prompt…") },
            singleLine = true,
        )
        if (ui.repos.isEmpty()) {
            ui.indexStatus?.let { Supporting(it, Modifier.padding(horizontal = 16.dp, vertical = 4.dp)) }
            return@Column
        }
        p.suggestions.forEachIndexed { i, g ->
            val machines = g.joinToString(" · ") { it.nodeName }
            ListRow(
                (if (p.chosen.lastOrNull() == g) "› " else "") + g[0].name,
                supporting = "$machines · ${shortPath(g[0].path)}",
                onClick = { q.pick(i) },
                modifier = Modifier.testTag("repo-${g[0].name}"),
            )
        }
        if (p.machines.isNotEmpty()) {
            FlowRow(Modifier.padding(horizontal = 16.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                p.machines.forEach { id ->
                    FilterChip(selected = id == p.nodeId, onClick = { q.selectMachine(id) }, label = { Text(q.nodeName(id)) }, modifier = Modifier.heightIn(min = 48.dp))
                }
                FilterChip(selected = p.fresh, onClick = q::toggleFresh, enabled = p.chosen.size < 2, label = { Text("Fresh worktree") }, modifier = Modifier.heightIn(min = 48.dp).testTag("quick-fresh"))
            }
            val where = when {
                p.chosen.size > 1 -> "Fresh worktrees of ${p.chosen.joinToString(" + ") { it[0].name }} in ~/pyre-work/"
                p.fresh -> "A fresh worktree and branch of ${p.chosen[0][0].name}"
                else -> "Main checkout ${shortPath(p.chosen[0].first { it.nodeId == p.nodeId }.path)} · the agent decides about worktrees"
            }
            Supporting("${p.harness} on ${q.nodeName(p.nodeId)} · $where", Modifier.padding(horizontal = 16.dp, vertical = 4.dp))
        }
        ui.error?.let { ToneText(it, Tone.ERROR, Modifier.padding(horizontal = 16.dp, vertical = 4.dp)) }
        Button(onClick = q::launch, enabled = !ui.launching && p.problem == null, modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp).heightIn(min = 48.dp).testTag("quick-start")) {
            Text(if (ui.launching) "Starting…" else p.chosen.takeIf { it.isNotEmpty() }?.let { "Start in ${it.joinToString("+") { g -> g[0].name }}" } ?: "Start")
        }
    }
}
