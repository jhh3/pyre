package dev.jhh3.pyre.ui.comments

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.OutboxItem
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.Anchor
import dev.jhh3.pyre.model.CreateCommentRequest
import dev.jhh3.pyre.model.ObjectRef
import dev.jhh3.pyre.model.ReviewComment
import dev.jhh3.pyre.ui.common.CardBlock
import dev.jhh3.pyre.ui.common.Expandable
import dev.jhh3.pyre.ui.common.Pill
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.common.SectionHeader
import dev.jhh3.pyre.ui.common.ToneText
import java.time.Instant
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

data class CommentsUi(
    val target: ObjectRef? = null,
    /** Human label of the exact version/revision being commented on. */
    val targetLabel: String = "",
    val comments: Load<List<ReviewComment>> = Load.Loading,
    val pending: List<OutboxItem> = emptyList(),
    val draft: String = "",
    val anchorKind: String = "",
    val anchorValue: String = "",
    val mention: String = "",
    val notice: String? = null,
    val now: Instant = Instant.EPOCH,
)

/**
 * Review comments on an exact document revision or artifact version, with
 * an optional anchor and an optional session mention (so a worker is
 * notified). Creation goes through the outbox: offline comments stay
 * visible as pending and replay with the same operation_id.
 */
class CommentsController(private val repo: Repository, private val scope: CoroutineScope) {
    private val _ui = MutableStateFlow(CommentsUi())
    val ui: StateFlow<CommentsUi> = _ui.asStateFlow()

    init {
        scope.launch {
            repo.outbox.items.collect { items ->
                val t = _ui.value.target
                _ui.value = _ui.value.copy(pending = items.filter { it.kind == OutboxItem.KIND_COMMENT && it.state != OutboxItem.DONE && it.createComment?.target == t })
            }
        }
    }

    fun load(target: ObjectRef, label: String, defaultMention: String = "") {
        _ui.value = _ui.value.copy(target = target, targetLabel = label, mention = _ui.value.mention.ifEmpty { defaultMention }, comments = Load.Loading)
        scope.launch {
            val c = repo.comments(target)
            _ui.value = _ui.value.copy(
                comments = c,
                now = repo.clock.now(),
                pending = repo.outbox.items.value.filter { it.kind == OutboxItem.KIND_COMMENT && it.state != OutboxItem.DONE && it.createComment?.target == target },
            )
        }
    }

    fun setDraft(s: String) { _ui.value = _ui.value.copy(draft = s) }
    fun setAnchorKind(s: String) { _ui.value = _ui.value.copy(anchorKind = if (_ui.value.anchorKind == s) "" else s) }
    fun setAnchorValue(s: String) { _ui.value = _ui.value.copy(anchorValue = s) }
    fun setMention(s: String) { _ui.value = _ui.value.copy(mention = s) }

    fun submit() {
        val u = _ui.value
        val target = u.target ?: return
        if (u.draft.isBlank()) return
        val req = CreateCommentRequest(
            operationId = Ids.operation(),
            target = target,
            anchor = if (u.anchorKind.isNotEmpty() && u.anchorValue.isNotBlank()) Anchor(u.anchorKind, u.anchorValue.trim()) else null,
            body = u.draft.trim(),
            mentions = u.mention.split(',', ' ').map { it.trim() }.filter { it.startsWith("ses_") },
        )
        _ui.value = u.copy(draft = "", anchorValue = "")
        scope.launch {
            repo.outbox.enqueueComment(req)
            val ok = repo.flushOutbox()
            _ui.value = _ui.value.copy(notice = if (ok) null else "Offline — comment saved locally and will be posted on reconnect.")
            if (ok) load(target, u.targetLabel)
        }
    }
}

@Composable
fun CommentsSection(ui: CommentsUi, c: CommentsController) {
    SectionHeader("Comments on ${ui.targetLabel}")
    ui.notice?.let { ToneText(it, Tone.MUTED, Modifier.padding(horizontal = 16.dp)) }
    when (val l = ui.comments) {
        Load.Loading -> Supporting("Loading comments…", Modifier.padding(horizontal = 16.dp))
        is Load.Failed -> ToneText("Comments unavailable: ${l.error.describe()}", Tone.ERROR, Modifier.padding(horizontal = 16.dp))
        is Load.Ready -> {
            if (l.cachedAt != null) Supporting("Saved copy from ${Times.ago(l.cachedAt, ui.now)}", Modifier.padding(horizontal = 16.dp))
            if (l.value.isEmpty() && ui.pending.isEmpty()) Supporting("No comments on this ${ui.targetLabel.substringBefore(' ').ifEmpty { "version" }} yet.", Modifier.padding(horizontal = 16.dp))
            l.value.forEach { cm ->
                CardBlock(modifier = Modifier.testTag("comment-${cm.commentId}")) {
                    Supporting(
                        (cm.author?.name?.ifEmpty { cm.author.id } ?: "unknown") + " · " + Times.ago(cm.createdAt, ui.now) +
                            (cm.anchor?.let { " · ${it.kind} ${it.value}" } ?: "") + if (cm.resolved) " · resolved" else "",
                    )
                    Text(cm.body, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(top = 4.dp))
                    if (cm.mentions.isNotEmpty()) Supporting("Notified: " + cm.mentions.joinToString(), Modifier.padding(top = 4.dp))
                }
            }
        }
    }
    ui.pending.forEach { p ->
        CardBlock {
            Row { Pill(if (p.state == OutboxItem.FAILED) "Not posted" else "Waiting to post", if (p.state == OutboxItem.FAILED) Tone.ERROR else Tone.MUTED) }
            Text(p.createComment?.body.orEmpty(), style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(top = 4.dp))
            if (p.lastError.isNotEmpty()) Supporting(p.lastError)
        }
    }
    Column(Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) {
        OutlinedTextField(
            ui.draft,
            c::setDraft,
            Modifier.fillMaxWidth().testTag("comment-draft"),
            label = { Text("Add a comment") },
            placeholder = { Text("What should change?") },
            minLines = 2,
            maxLines = 6,
        )
    }
    Expandable("Point to a spot or notify a session") {
        Row(Modifier.padding(horizontal = 16.dp)) {
            listOf("line" to "Lines", "quote" to "Quote", "page" to "Page").forEach { (k, label) ->
                FilterChip(selected = ui.anchorKind == k, onClick = { c.setAnchorKind(k) }, label = { Text(label) }, modifier = Modifier.padding(end = 8.dp))
            }
        }
        if (ui.anchorKind.isNotEmpty()) {
            OutlinedTextField(
                ui.anchorValue,
                c::setAnchorValue,
                Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp),
                label = { Text(when (ui.anchorKind) { "line" -> "Lines, e.g. 12-18"; "quote" -> "Quoted text"; else -> "Page number" }) },
                singleLine = true,
            )
        }
        OutlinedTextField(
            ui.mention,
            c::setMention,
            Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp),
            label = { Text("Notify session (optional)") },
            placeholder = { Text("ses_…") },
            singleLine = true,
        )
    }
    Button(
        onClick = c::submit,
        enabled = ui.draft.isNotBlank(),
        modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp).heightIn(min = 48.dp).testTag("comment-submit"),
    ) { Text("Comment on ${ui.targetLabel.ifEmpty { "this version" }}") }
}
