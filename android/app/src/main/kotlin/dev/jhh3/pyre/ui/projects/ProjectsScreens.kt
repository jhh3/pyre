package dev.jhh3.pyre.ui.projects

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.size
import androidx.compose.material3.TextButton
import dev.jhh3.pyre.R
import dev.jhh3.pyre.ui.common.CardBlock
import dev.jhh3.pyre.ui.common.EmptyState
import dev.jhh3.pyre.ui.common.ListRow
import dev.jhh3.pyre.ui.common.OnRetry
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.ScreenTitle
import dev.jhh3.pyre.ui.common.Supporting
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.model.Artifact
import dev.jhh3.pyre.model.Document
import dev.jhh3.pyre.model.DocumentWithRevision
import dev.jhh3.pyre.model.Project
import dev.jhh3.pyre.ui.common.LoadView
import dev.jhh3.pyre.ui.common.SectionHeader
import dev.jhh3.pyre.ui.viewer.MarkdownView
import java.time.Instant
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

// ---------------- project list ----------------

class ProjectsViewModel(private val repo: Repository) : ViewModel() {
    private val _ui = MutableStateFlow<Load<List<Project>>>(Load.Loading)
    val ui: StateFlow<Load<List<Project>>> = _ui.asStateFlow()
    val now: Instant get() = repo.clock.now()

    init { refresh() }

    fun refresh() = viewModelScope.launch { _ui.value = repo.projects() }
}

@Composable
fun ProjectsScreen(vm: ProjectsViewModel, onOpen: (String) -> Unit, onAllArtifacts: () -> Unit) {
    val load by vm.ui.collectAsStateWithLifecycle()
    OnRetry { vm.refresh() }
    Column(Modifier.fillMaxSize()) {
        LoadView(load, vm.now, { vm.refresh() }) { list ->
            val live = list.filterNot { it.archived }
            LazyColumn(Modifier.fillMaxSize().testTag("projects-list")) {
                items(live, key = { it.projectId }) { p ->
                    ListRow(
                        p.name,
                        supporting = listOf(p.description, "updated ${Times.ago(p.updatedAt, vm.now)}").filter { it.isNotEmpty() }.joinToString(" · "),
                        leading = { PyreIcon(R.drawable.ic_folder, null, tint = MaterialTheme.colorScheme.onSurfaceVariant) },
                        onClick = { onOpen(p.projectId) },
                    )
                }
                if (live.isEmpty()) {
                    item(key = "empty") {
                        EmptyState(
                            title = "No projects",
                            body = "Projects are optional. Sessions work fine without them.",
                            icon = R.drawable.ic_folder,
                        )
                    }
                }
                item(key = "all-artifacts") {
                    SectionHeader("Everything published")
                    ListRow(
                        "All artifacts",
                        supporting = "Reports, files and previews from every session, with or without a project",
                        leading = { PyreIcon(R.drawable.ic_image, null, tint = MaterialTheme.colorScheme.onSurfaceVariant) },
                        trailing = { PyreIcon(R.drawable.ic_chevron_right, null, tint = MaterialTheme.colorScheme.onSurfaceVariant) },
                        onClick = onAllArtifacts,
                        modifier = Modifier.testTag("all-artifacts"),
                    )
                }
            }
        }
    }
}

// ---------------- project home ----------------

data class ProjectUi(
    val project: Load<Project> = Load.Loading,
    val brief: Load<DocumentWithRevision>? = null,
    val docs: Load<List<Document>> = Load.Loading,
    val artifacts: Load<List<Artifact>> = Load.Loading,
    val now: Instant = Instant.EPOCH,
)

class ProjectViewModel(private val repo: Repository, val projectId: String) : ViewModel() {
    private val _ui = MutableStateFlow(ProjectUi(now = repo.clock.now()))
    val ui: StateFlow<ProjectUi> = _ui.asStateFlow()

    init { refresh() }

    fun refresh() = viewModelScope.launch {
        val p = repo.project(projectId)
        _ui.value = _ui.value.copy(project = p, now = repo.clock.now())
        val briefId = (p as? Load.Ready)?.value?.briefDocumentId.orEmpty()
        val brief = if (briefId.isNotEmpty()) repo.document(briefId) else repo.brief(projectId)
        _ui.value = _ui.value.copy(brief = brief, docs = repo.documents(projectId), artifacts = repo.artifacts(projectId))
    }
}

@Composable
fun ProjectScreen(vm: ProjectViewModel, onOpenDoc: (String) -> Unit, onOpenArtifact: (String) -> Unit) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    OnRetry { vm.refresh() }
    ScreenTitle((ui.project as? Load.Ready)?.value?.name)
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).testTag("project-screen")) {
        LoadView(ui.project, ui.now, { vm.refresh() }) { p ->
            if (p.description.isNotEmpty()) Text(p.description, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp))
            p.repos.forEach { Supporting("Repository ${it.name} · ${it.url}", Modifier.padding(horizontal = 16.dp, vertical = 2.dp)) }
            p.links.forEach { Supporting("${it.title}: ${it.url}", Modifier.padding(horizontal = 16.dp, vertical = 2.dp)) }
        }
        when (val b = ui.brief) {
            null -> Unit
            else -> LoadView(b, ui.now, { vm.refresh() }) { d ->
                SectionHeader("Brief") {
                    TextButton(onClick = { onOpenDoc(d.document.documentId) }, modifier = Modifier.heightIn(min = 48.dp)) { Text("Open") }
                }
                CardBlock(onClick = { onOpenDoc(d.document.documentId) }) {
                    Supporting("Revision ${d.revision.seq} · ${d.revision.author?.name?.ifEmpty { d.revision.author.id } ?: "unknown author"} · ${Times.ago(d.revision.createdAt, ui.now)}")
                    MarkdownView(d.revision.content, Modifier.padding(top = 4.dp), inset = false)
                }
            }
        }
        SectionHeader("Documents")
        LoadView(ui.docs, ui.now, { vm.refresh() }) { docs ->
            if (docs.isEmpty()) Supporting("No documents yet.", Modifier.padding(horizontal = 16.dp))
            docs.forEach { d ->
                ListRow(
                    d.title.ifEmpty { d.slug },
                    supporting = "Revision ${d.headSeq} · updated ${Times.ago(d.updatedAt, ui.now)}",
                    leading = { PyreIcon(R.drawable.ic_description, null, tint = MaterialTheme.colorScheme.onSurfaceVariant) },
                    onClick = { onOpenDoc(d.documentId) },
                )
            }
        }
        SectionHeader("Artifacts")
        LoadView(ui.artifacts, ui.now, { vm.refresh() }) { arts -> ArtifactList(arts, ui.now, onOpenArtifact) }
        Spacer(Modifier.size(24.dp))
    }
}

@Composable
fun ArtifactList(arts: List<Artifact>, now: Instant, onOpen: (String) -> Unit) {
    if (arts.isEmpty()) Supporting("Nothing published yet.", Modifier.padding(horizontal = 16.dp))
    arts.forEach { a ->
        ListRow(
            a.title,
            supporting = "${a.versionCount} version${if (a.versionCount == 1) "" else "s"} · updated ${Times.ago(a.updatedAt, now)}" + if (a.projectId == null) " · no project" else "",
            leading = { PyreIcon(R.drawable.ic_image, null, tint = MaterialTheme.colorScheme.onSurfaceVariant) },
            onClick = { onOpen(a.artifactId) },
            modifier = Modifier.testTag("artifact-${a.artifactId}"),
        )
    }
}

// ---------------- all artifacts ----------------

class AllArtifactsViewModel(private val repo: Repository) : ViewModel() {
    private val _ui = MutableStateFlow<Load<List<Artifact>>>(Load.Loading)
    val ui: StateFlow<Load<List<Artifact>>> = _ui.asStateFlow()
    val now: Instant get() = repo.clock.now()

    init { refresh() }

    fun refresh() = viewModelScope.launch { _ui.value = repo.artifacts(null) }
}

@Composable
fun AllArtifactsScreen(vm: AllArtifactsViewModel, onOpen: (String) -> Unit) {
    val load by vm.ui.collectAsStateWithLifecycle()
    OnRetry { vm.refresh() }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
        LoadView(load, vm.now, { vm.refresh() }) {
            if (it.isEmpty()) EmptyState("Nothing published yet", "Sessions publish reports, files and previews here.", R.drawable.ic_image)
            else ArtifactList(it, vm.now, onOpen)
        }
    }
}
