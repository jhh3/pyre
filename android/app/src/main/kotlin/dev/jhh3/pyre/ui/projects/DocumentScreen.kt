package dev.jhh3.pyre.ui.projects

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.TextButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
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
import dev.jhh3.pyre.model.DocumentRevision
import dev.jhh3.pyre.model.DocumentWithRevision
import dev.jhh3.pyre.model.ObjectRef
import dev.jhh3.pyre.ui.comments.CommentsController
import dev.jhh3.pyre.ui.comments.CommentsSection
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.Expandable
import dev.jhh3.pyre.ui.common.KeyValue
import dev.jhh3.pyre.ui.common.ListRow
import dev.jhh3.pyre.ui.common.OnRetry
import dev.jhh3.pyre.ui.common.ScreenTitle
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.common.LoadView
import dev.jhh3.pyre.ui.common.SectionHeader
import dev.jhh3.pyre.ui.common.shortId
import dev.jhh3.pyre.ui.viewer.MarkdownView
import java.time.Instant
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

data class DocumentUi(
    val doc: Load<DocumentWithRevision> = Load.Loading,
    val revisions: List<DocumentRevision> = emptyList(),
    val now: Instant = Instant.EPOCH,
)

class DocumentViewModel(private val repo: Repository, val documentId: String, revisionId: String) : ViewModel() {
    private val _ui = MutableStateFlow(DocumentUi(now = repo.clock.now()))
    val ui: StateFlow<DocumentUi> = _ui.asStateFlow()
    val comments = CommentsController(repo, viewModelScope)

    init { open(revisionId) }

    fun open(revisionId: String) = viewModelScope.launch {
        val d = repo.document(documentId, revisionId)
        _ui.value = _ui.value.copy(doc = d, now = repo.clock.now())
        d.valueOrNull()?.let { comments.load(ObjectRef("document_revision", it.revision.revisionId), "revision ${it.revision.seq}") }
        repo.revisions(documentId).valueOrNull()?.let { _ui.value = _ui.value.copy(revisions = it.sortedByDescending { r -> r.seq }) }
    }
}

@Composable
fun DocumentScreen(vm: DocumentViewModel) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    val cui by vm.comments.ui.collectAsStateWithLifecycle()
    OnRetry { vm.open("") }
    ScreenTitle((ui.doc as? Load.Ready)?.value?.document?.let { it.title.ifEmpty { it.slug } })
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).testTag("document-screen")) {
        LoadView(ui.doc, ui.now, { vm.open("") }) { d ->
            val r = d.revision
            val isHead = r.revisionId == d.document.headRevisionId
            Supporting(
                "Revision ${r.seq}${if (isHead) " (latest)" else ""} · ${r.author?.name?.ifEmpty { r.author.id } ?: "unknown author"} · ${Times.ago(r.createdAt, ui.now)}" +
                    if (r.summary.isNotEmpty()) "\n${r.summary}" else "",
                Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
            )
            if (!isHead) Banner("You're reading revision ${r.seq}. The latest is revision ${d.document.headSeq}.", Tone.MUTED) {
                TextButton(onClick = { vm.open("") }) { Text("Latest") }
            }
            if (d.document.mediaType == "text/markdown" || d.document.mediaType.isEmpty()) MarkdownView(r.content, Modifier.padding(vertical = 8.dp))
            else Text(r.content, fontFamily = FontFamily.Monospace, modifier = Modifier.padding(16.dp))
            Supporting(
                "To edit, use your editor on a computer (pyre doc edit). Conflicting edits are kept as proposals.",
                Modifier.padding(16.dp),
            )
            Expandable("Details") {
                KeyValue("Revision ID", shortId(r.revisionId), mono = true)
                KeyValue("Fingerprint (SHA-256)", r.digest, mono = true)
                r.provenance?.let { p -> if (p.sessionId.isNotEmpty()) KeyValue("Written by session", p.sessionId, mono = true) }
                KeyValue("Format", d.document.mediaType.ifEmpty { "text/markdown" })
            }
        }
        if (ui.revisions.isNotEmpty()) {
            SectionHeader("History")
            ui.revisions.forEach { rev ->
                val current = (ui.doc as? Load.Ready)?.value?.revision?.revisionId == rev.revisionId
                ListRow(
                    "Revision ${rev.seq}" + if (current) " · showing" else "",
                    supporting = listOfNotNull(rev.author?.name?.ifEmpty { rev.author.id }, Times.ago(rev.createdAt, ui.now), rev.summary.ifEmpty { null }).joinToString(" · "),
                    onClick = { vm.open(rev.revisionId) },
                )
            }
        }
        if (cui.target != null) CommentsSection(cui, vm.comments)
    }
}
