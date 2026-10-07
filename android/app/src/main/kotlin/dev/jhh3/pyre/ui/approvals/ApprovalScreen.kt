package dev.jhh3.pyre.ui.approvals

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.FilledTonalButton
import androidx.compose.foundation.layout.heightIn
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
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
import dev.jhh3.pyre.domain.AnswerResult
import dev.jhh3.pyre.domain.ApprovalAnswerer
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.Approval
import dev.jhh3.pyre.model.ApprovalState
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.ErrorBox
import dev.jhh3.pyre.ui.common.KeyValue
import dev.jhh3.pyre.ui.common.LoadingBox
import dev.jhh3.pyre.ui.common.Expandable
import dev.jhh3.pyre.ui.common.OnRetry
import dev.jhh3.pyre.ui.common.ScreenTitle
import dev.jhh3.pyre.ui.common.Supporting
import java.time.Instant
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

data class ApprovalUi(
    val approval: Load<Approval> = Load.Loading,
    val text: String = "",
    /** Non-null while an answer is in flight; further taps are ignored. */
    val submitting: String? = null,
    val result: String? = null,
    val resultTone: Tone = Tone.MUTED,
    /** True when the last attempt's outcome is unknown; Retry reuses the same operation_id. */
    val canRetry: Boolean = false,
    val lastChoice: Pair<String, String>? = null,
    val now: Instant = Instant.EPOCH,
) {
    val current: Approval? get() = (approval as? Load.Ready)?.value
    /** Controls only for a live, freshly fetched pending request. */
    val answerable: Boolean get() = (approval as? Load.Ready)?.let { it.cachedAt == null && it.value.state == ApprovalState.PENDING } == true
}

class ApprovalViewModel(
    private val repo: Repository,
    val approvalId: String,
    private val answerer: ApprovalAnswerer = ApprovalAnswerer(repo),
) : ViewModel() {
    private val _ui = MutableStateFlow(ApprovalUi(now = repo.clock.now()))
    val ui: StateFlow<ApprovalUi> = _ui.asStateFlow()
    var answerCalls = 0
        private set

    init { refresh() }

    fun refresh() {
        viewModelScope.launch {
            val a = try {
                Load.Ready(repo.call { getApproval(approvalId) })
            } catch (e: PyreApiException) {
                Load.Failed(e)
            }
            _ui.value = _ui.value.copy(approval = a, now = repo.clock.now())
        }
    }

    fun setText(s: String) { _ui.value = _ui.value.copy(text = s) }

    fun answer(optionId: String, text: String = "") {
        if (_ui.value.submitting != null) return // double-tap guard (the stable operation_id is the second guard)
        val a = _ui.value.current ?: return
        _ui.value = _ui.value.copy(submitting = optionId.ifEmpty { "text" }, result = null, canRetry = false, lastChoice = optionId to text)
        answerCalls++
        viewModelScope.launch {
            val r = answerer.answer(approvalId, a.runId, optionId, text)
            _ui.value = when (r) {
                is AnswerResult.Answered -> _ui.value.copy(approval = Load.Ready(r.approval), result = "Answer recorded by the hub. " + ApprovalAnswerer.explain(r.approval), resultTone = Tone.OK)
                is AnswerResult.NotPending -> _ui.value.copy(approval = r.approval?.let { Load.Ready(it) } ?: _ui.value.approval, result = r.explanation, resultTone = Tone.WARN)
                is AnswerResult.NotSent -> _ui.value.copy(result = "Not sent — ${r.error.describe()}", resultTone = Tone.ERROR, canRetry = true)
                is AnswerResult.OutcomeUnknown -> _ui.value.copy(result = "Outcome unknown — the answer may have reached the hub. Retrying is safe: it reuses the same operation id.", resultTone = Tone.ATTENTION, canRetry = true)
                is AnswerResult.Failed -> _ui.value.copy(result = r.error.describe(), resultTone = Tone.ERROR)
            }.copy(submitting = null, now = repo.clock.now())
        }
    }

    fun retry() {
        val (opt, text) = _ui.value.lastChoice ?: return
        answer(opt, text)
    }
}

@Composable
fun ApprovalScreen(vm: ApprovalViewModel, onOpenSession: (String) -> Unit) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    OnRetry { vm.refresh() }
    ScreenTitle(ui.current?.let { if (it.kind == "question") "Question" else "Approval" })
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).testTag("approval-screen")) {
        when (val l = ui.approval) {
            Load.Loading -> LoadingBox()
            is Load.Failed -> ErrorBox(
                if (l.error.code == "not_found") "This request no longer exists." else l.error.describe(),
                vm::refresh,
            )
            is Load.Ready -> {
                val a = l.value
                val pending = a.state == ApprovalState.PENDING
                Column(Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) {
                    Text(
                        (if (a.kind == "question") "Question" else "Asks permission") + if (pending) " · waiting for you" else " · " + a.state,
                        style = MaterialTheme.typography.labelLarge,
                        color = if (pending) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    Text(a.title, style = MaterialTheme.typography.headlineSmall, modifier = Modifier.padding(top = 4.dp, bottom = 8.dp))
                    if (a.body.isNotEmpty()) Text(a.body, style = MaterialTheme.typography.bodyLarge)
                    Supporting(
                        listOfNotNull("Asked " + Times.ago(a.createdAt, ui.now), a.expiresAt?.let { "expires " + Times.relative(it, ui.now) }).joinToString(" · "),
                        Modifier.padding(top = 8.dp),
                    )
                }
                ui.result?.let { msg ->
                    Banner(msg, ui.resultTone.let { if (it == Tone.OK) Tone.MUTED else it }, Modifier.testTag("approval-result")) {
                        if (ui.canRetry) TextButton(onClick = vm::retry) { Text("Retry") }
                    }
                }
                if (ui.answerable) {
                    Column(Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) {
                        val enabled = ui.submitting == null
                        a.options.forEach { o ->
                            val mod = Modifier.fillMaxWidth().padding(vertical = 4.dp).heightIn(min = 56.dp).testTag("option-${o.id}")
                            val onClick = { vm.answer(o.id) }
                            when (o.kind) {
                                "allow" -> Button(onClick = onClick, enabled = enabled, modifier = mod) { Text(o.label.ifEmpty { o.id }) }
                                "choice" -> FilledTonalButton(onClick = onClick, enabled = enabled, modifier = mod) { Text(o.label.ifEmpty { o.id }) }
                                else -> OutlinedButton(onClick = onClick, enabled = enabled, modifier = mod) { Text(o.label.ifEmpty { o.id }) }
                            }
                        }
                        if (a.allowText) {
                            OutlinedTextField(
                                ui.text,
                                vm::setText,
                                Modifier.fillMaxWidth().padding(top = 12.dp).testTag("answer-text"),
                                label = { Text(if (a.options.isEmpty()) "Your answer" else "Or type an answer") },
                                minLines = 2,
                            )
                            Button(
                                onClick = { vm.answer("", ui.text) },
                                enabled = ui.submitting == null && ui.text.isNotBlank(),
                                modifier = Modifier.padding(top = 8.dp).heightIn(min = 48.dp).testTag("send-answer"),
                            ) { Text("Send answer") }
                        }
                        if (ui.submitting != null) Supporting("Sending…", Modifier.padding(top = 8.dp))
                    }
                } else {
                    Banner(ApprovalAnswerer.explain(a), Tone.MUTED, Modifier.testTag("approval-state"))
                    if (pending && l.cachedAt != null) {
                        Supporting("This is a saved copy. Reconnect to your hub to answer.", Modifier.padding(16.dp))
                    }
                }
                TextButton(onClick = { onOpenSession(a.sessionId) }, modifier = Modifier.padding(horizontal = 8.dp).heightIn(min = 48.dp)) { Text("Open the session") }
                Expandable("Details") {
                    KeyValue("Run", a.runId, mono = true)
                    if (a.requestId.isNotEmpty()) KeyValue("Request", a.requestId, mono = true)
                    KeyValue("Request ID", a.approvalId, mono = true)
                }
            }
        }
    }
}
