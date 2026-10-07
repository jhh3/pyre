package dev.jhh3.pyre.ui.projects

import android.content.ContentResolver
import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.material3.RadioButton
import androidx.compose.ui.Alignment
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.semantics
import dev.jhh3.pyre.R
import dev.jhh3.pyre.ui.common.Expandable
import dev.jhh3.pyre.ui.common.OnRetry
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.ScreenTitle
import dev.jhh3.pyre.ui.common.Supporting
import androidx.compose.foundation.layout.Row
import androidx.compose.material3.TextButton
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.data.valueOrNull
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.files.ArtifactContent
import dev.jhh3.pyre.files.ArtifactFiles
import dev.jhh3.pyre.files.Verified
import dev.jhh3.pyre.model.Artifact
import dev.jhh3.pyre.model.ArtifactVersion
import dev.jhh3.pyre.model.ObjectRef
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.comments.CommentsController
import dev.jhh3.pyre.ui.comments.CommentsSection
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.ErrorBox
import dev.jhh3.pyre.ui.common.KeyValue
import dev.jhh3.pyre.ui.common.LoadView
import dev.jhh3.pyre.ui.common.LoadingBox
import dev.jhh3.pyre.ui.common.SectionHeader
import dev.jhh3.pyre.ui.common.shortId
import dev.jhh3.pyre.ui.viewer.MarkdownView
import java.io.File
import java.time.Instant
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

sealed interface OpenState {
    data object Idle : OpenState
    data object Downloading : OpenState
    data class Text(val text: String, val markdown: Boolean, val note: String?, val verified: String? = null) : OpenState
    data class External(val file: File, val mediaType: String, val note: String?, val verified: String? = null) : OpenState
    data class Html(val versionId: String) : OpenState
    data class Error(val message: String) : OpenState
}

data class ArtifactUi(
    val artifact: Load<Artifact> = Load.Loading,
    val versions: List<ArtifactVersion> = emptyList(),
    val selected: ArtifactVersion? = null,
    val open: OpenState = OpenState.Idle,
    val saveNotice: String? = null,
    val now: Instant = Instant.EPOCH,
)

class ArtifactViewModel(
    private val repo: Repository,
    private val content: ArtifactContent,
    private val cacheDir: File,
    private var artifactId: String,
    private val initialVersionId: String,
) : ViewModel() {
    private val _ui = MutableStateFlow(ArtifactUi(now = repo.clock.now()))
    val ui: StateFlow<ArtifactUi> = _ui.asStateFlow()
    val comments = CommentsController(repo, viewModelScope)

    init { refresh() }

    fun refresh() = viewModelScope.launch {
        if (artifactId.isEmpty() && initialVersionId.isNotEmpty()) {
            try {
                artifactId = repo.call { getVersion(initialVersionId) }.artifactId
            } catch (e: PyreApiException) {
                _ui.value = _ui.value.copy(artifact = Load.Failed(e))
                return@launch
            }
        }
        val a = repo.artifact(artifactId)
        val versions = repo.versions(artifactId).valueOrNull().orEmpty().sortedByDescending { it.seq }
        val sel = versions.firstOrNull { it.versionId == initialVersionId }
            ?: versions.firstOrNull { it.versionId == a.valueOrNull()?.latestVersionId } ?: versions.firstOrNull()
        _ui.value = _ui.value.copy(artifact = a, versions = versions, now = repo.clock.now())
        sel?.let { select(it) }
    }

    fun select(v: ArtifactVersion) {
        _ui.value = _ui.value.copy(selected = v, open = OpenState.Idle, saveNotice = null)
        comments.load(ObjectRef("artifact_version", v.versionId), "version ${v.seq}", v.provenance.sessionId)
    }

    /** Text/Markdown render in-app; HTML only through the isolated preview; everything else opens externally. */
    fun open() {
        val v = _ui.value.selected ?: return
        if (ArtifactFiles.isHtml(v.mediaType)) {
            _ui.value = _ui.value.copy(open = OpenState.Html(v.versionId))
            return
        }
        _ui.value = _ui.value.copy(open = OpenState.Downloading)
        viewModelScope.launch {
            _ui.value = _ui.value.copy(
                open = try {
                    when (val r = ArtifactFiles.fetch(cacheDir, v, content)) {
                        is Verified.Mismatch -> OpenState.Error("Downloaded bytes do not match the published digest (expected ${r.expected.take(16)}…, got ${r.actual.take(16)}…). Not opened.")
                        is Verified.Ok -> show(v, r.file, null, "Verified: these ${r.file.length()} bytes match the published SHA-256 ${v.digest.removePrefix("sha256:").take(16)}…")
                        is Verified.Unverifiable -> show(v, r.file, "Integrity not verified: ${r.reason}.")
                    }
                } catch (e: PyreApiException) {
                    OpenState.Error(e.describe())
                } catch (e: java.io.IOException) {
                    OpenState.Error("Download failed: ${e.message}")
                },
            )
        }
    }

    private suspend fun show(v: ArtifactVersion, f: File, note: String?, verified: String? = null): OpenState =
        if (ArtifactFiles.isText(v.mediaType) && f.length() <= 2_000_000) {
            OpenState.Text(withContext(Dispatchers.IO) { f.readText() }, ArtifactFiles.isMarkdown(v), note, verified)
        } else {
            OpenState.External(f, v.mediaType, note, verified)
        }

    fun consumeOpen() { _ui.value = _ui.value.copy(open = OpenState.Idle) }

    /**
     * Copies the verified bytes to a document the user picked (Storage Access
     * Framework). The only way to keep an arbitrary file no installed app can
     * open; the copy is byte-for-byte the digest-checked download.
     */
    fun saveCopy(resolver: ContentResolver, target: Uri) {
        val o = _ui.value.open as? OpenState.External ?: return
        viewModelScope.launch {
            val notice = try {
                val n = withContext(Dispatchers.IO) {
                    resolver.openOutputStream(target, "wt")?.use { out -> o.file.inputStream().use { it.copyTo(out) } }
                        ?: throw java.io.IOException("the document provider returned no stream")
                }
                "Saved a copy: $n bytes, identical to the verified download."
            } catch (e: Exception) {
                "Could not save a copy: ${e.message}"
            }
            _ui.value = _ui.value.copy(saveNotice = notice)
        }
    }
}

@Composable
fun ArtifactScreen(vm: ArtifactViewModel, onOpenHtml: (String) -> Unit) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    val cui by vm.comments.ui.collectAsStateWithLifecycle()
    val ctx = LocalContext.current
    val external = ui.open as? OpenState.External
    val saver = rememberLauncherForActivityResult(ActivityResultContracts.CreateDocument(external?.mediaType?.substringBefore(';')?.ifEmpty { null } ?: "application/octet-stream")) { uri ->
        uri?.let { vm.saveCopy(ctx.contentResolver, it) }
    }
    LaunchedEffect(ui.open) {
        when (val o = ui.open) {
            is OpenState.Html -> { vm.consumeOpen(); onOpenHtml(o.versionId) }
            is OpenState.External -> ArtifactFiles.openExternally(ctx, o.file, o.mediaType)
            else -> Unit
        }
    }
    OnRetry { vm.refresh() }
    ScreenTitle((ui.artifact as? Load.Ready)?.value?.title)
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).testTag("artifact-screen")) {
        LoadView(ui.artifact, ui.now, { vm.refresh() }) { a ->
            if (a.description.isNotEmpty()) Text(a.description, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp))
            Supporting(
                listOfNotNull(if (a.projectId == null) "No project" else "In a project", a.sessionId.ifEmpty { null }?.let { "from session ${shortId(it)}" }).joinToString(" · "),
                Modifier.padding(horizontal = 16.dp),
            )
        }
        val latestId = (ui.artifact as? Load.Ready)?.value?.latestVersionId
        if (ui.versions.size > 1) {
            SectionHeader("Versions")
            Column(Modifier.selectableGroup()) {
                ui.versions.forEach { v ->
                    val sel = ui.selected?.versionId == v.versionId
                    Row(
                        Modifier.fillMaxWidth().selectable(selected = sel, role = Role.RadioButton) { vm.select(v) }
                            .heightIn(min = 56.dp).padding(horizontal = 16.dp).testTag("version-${v.versionId}"),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        RadioButton(selected = sel, onClick = null)
                        Spacer(Modifier.width(16.dp))
                        Column(Modifier.weight(1f)) {
                            Text("Version ${v.seq}" + if (v.versionId == latestId) " · latest" else "", style = MaterialTheme.typography.bodyLarge)
                            Supporting("${v.filename} · ${humanSize(v.size)} · ${Times.ago(v.createdAt, ui.now)}")
                        }
                    }
                }
            }
        }
        ui.selected?.let { v ->
            SectionHeader(if (ui.versions.size > 1) "Version ${v.seq}" else "Version ${v.seq}" + if (v.versionId == latestId) " (latest)" else "")
            Row(Modifier.padding(horizontal = 16.dp), verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text(v.filename, style = MaterialTheme.typography.bodyLarge)
                    Supporting("${v.mediaType} · ${humanSize(v.size)} · ${v.author?.name?.ifEmpty { v.author.id } ?: "unknown author"}")
                }
            }
            if (v.note.isNotEmpty()) Text(v.note, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp))
            Button(onClick = vm::open, modifier = Modifier.padding(16.dp).heightIn(min = 48.dp).testTag("open-version")) {
                Text(if (ArtifactFiles.isHtml(v.mediaType)) "Open safe preview" else "Open")
            }
            when (val o = ui.open) {
                OpenState.Downloading -> LoadingBox()
                is OpenState.Error -> ErrorBox(o.message, vm::open)
                is OpenState.Text -> {
                    o.verified?.let { Verified(it) }
                    o.note?.let { Banner(it, Tone.MUTED) }
                    if (o.markdown) MarkdownView(o.text, Modifier.padding(vertical = 8.dp).testTag("artifact-text"))
                    else Text(o.text, fontFamily = FontFamily.Monospace, modifier = Modifier.padding(16.dp).horizontalScroll(rememberScrollState()).testTag("artifact-text"))
                }
                is OpenState.External -> {
                    o.verified?.let { Verified(it) }
                    o.note?.let { Banner(it, Tone.MUTED) }
                    Supporting(
                        "Opened in another app for ${o.mediaType}. If nothing can show this type, save a copy instead.",
                        Modifier.padding(horizontal = 16.dp),
                    )
                    Row(Modifier.padding(horizontal = 8.dp)) {
                        TextButton(onClick = { ArtifactFiles.openExternally(ctx, o.file, o.mediaType) }, modifier = Modifier.heightIn(min = 48.dp)) { Text("Open with…") }
                        TextButton(onClick = { saver.launch(o.file.name) }, modifier = Modifier.heightIn(min = 48.dp).testTag("save-copy")) { Text("Save a copy…") }
                    }
                    ui.saveNotice?.let { Banner(it, if (it.startsWith("Saved")) Tone.MUTED else Tone.ERROR, Modifier.testTag("save-notice")) }
                }
                else -> Unit
            }
            Expandable("Version details") {
                KeyValue("Version ID", shortId(v.versionId), mono = true)
                KeyValue("Fingerprint (SHA-256)", v.digest, mono = true)
                if (v.previousVersionId.isNotEmpty()) KeyValue("Follows", shortId(v.previousVersionId), mono = true)
                val p = v.provenance
                KeyValue("Made by", listOf(p.sessionId, p.runId, p.nodeId).filter { it.isNotEmpty() }.joinToString(" / ").ifEmpty { "unknown" })
            }
        }
        if (cui.target != null) CommentsSection(cui, vm.comments)
        Spacer(Modifier.size(24.dp))
    }
}

/** Integrity confirmation, worded for people: the bytes on this phone are exactly what was published. */
@Composable
private fun Verified(text: String) {
    Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp).testTag("artifact-verified").semantics(mergeDescendants = true) {}, verticalAlignment = Alignment.CenterVertically) {
        PyreIcon(R.drawable.ic_check, null, Modifier.size(18.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Spacer(Modifier.width(8.dp))
        Supporting(text)
    }
}

fun humanSize(bytes: Long): String = when {
    bytes < 1024 -> "$bytes B"
    bytes < 1024 * 1024 -> "%.1f KB".format(bytes / 1024.0)
    else -> "%.1f MB".format(bytes / (1024.0 * 1024.0))
}
