package dev.jhh3.pyre.ui.comments

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.model.ReviewComment
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.LoadView
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

/** `pyre://comment/<id>`: fetch the comment (comments.get) and open its exact target version. */
class CommentLinkViewModel(private val repo: Repository, val commentId: String) : ViewModel() {
    private val _ui = MutableStateFlow<Load<ReviewComment>>(Load.Loading)
    val ui: StateFlow<Load<ReviewComment>> = _ui.asStateFlow()

    init { refresh() }

    fun refresh() = viewModelScope.launch {
        _ui.value = try { Load.Ready(repo.call { getComment(commentId) }) } catch (e: PyreApiException) { Load.Failed(e) }
    }
}

@Composable
fun CommentLinkScreen(vm: CommentLinkViewModel, onOpenArtifactVersion: (String) -> Unit) {
    val load by vm.ui.collectAsStateWithLifecycle()
    val c = (load as? Load.Ready)?.value
    LaunchedEffect(c?.commentId) {
        if (c != null && c.target.kind == "artifact_version") onOpenArtifactVersion(c.target.id)
    }
    LoadView(load, java.time.Instant.now(), { vm.refresh() }) { cm ->
        Column(Modifier.padding(16.dp)) {
            Text("Comment on ${cm.target.kind} ${cm.target.id}" + (cm.anchor?.let { " · ${it.kind} ${it.value}" } ?: ""), style = MaterialTheme.typography.labelMedium)
            Text(cm.body, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(top = 8.dp))
        }
    }
}
