package dev.jhh3.pyre.ui.session

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.size
import androidx.compose.material3.Button
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.semantics
import dev.jhh3.pyre.R
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.ui.common.CardBlock
import dev.jhh3.pyre.ui.common.Expandable
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.common.SectionHeader
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.common.shortId
import dev.jhh3.pyre.ui.theme.toneColor
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Checkbox
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.data.IntentIds
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.Delivery
import dev.jhh3.pyre.domain.Identity
import dev.jhh3.pyre.data.valueOrNull
import dev.jhh3.pyre.ui.common.KeyValue
import dev.jhh3.pyre.ui.common.RefreshWhileResumed
import androidx.compose.foundation.clickable
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.DeliveryState
import dev.jhh3.pyre.model.MessageView
import dev.jhh3.pyre.model.ReissueMessageRequest
import dev.jhh3.pyre.model.ResolveDeliveryRequest
import dev.jhh3.pyre.net.PyreApi
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.ErrorBox
import dev.jhh3.pyre.ui.common.LoadingBox
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

data class MessageUi(
    val message: Load<MessageView> = Load.Loading,
    val acceptRisk: Boolean = false,
    val busy: Boolean = false,
    val notice: String? = null,
    val titles: Map<String, String> = emptyMap(),
)

/** Deep-link target for a message (e.g. an indeterminate-delivery inbox item). */
class MessageViewModel(private val repo: Repository, val messageId: String) : ViewModel() {
    private val _ui = MutableStateFlow(MessageUi())
    val ui: StateFlow<MessageUi> = _ui.asStateFlow()

    init { refresh() }

    fun refresh() = viewModelScope.launch {
        _ui.value = _ui.value.copy(
            message = try { Load.Ready(repo.call { getMessage(messageId) }) } catch (e: PyreApiException) { Load.Failed(e) },
        )
        repo.sessions().valueOrNull()?.let { list -> _ui.value = _ui.value.copy(titles = list.associate { it.session.sessionId to it.session.title }) }
    }

    fun setAcceptRisk(v: Boolean) { _ui.value = _ui.value.copy(acceptRisk = v) }

    fun resolve(resolution: String) = run(IntentIds.resolve(messageId, resolution)) { op ->
        resolveDelivery(ResolveDeliveryRequest(operationId = op, messageId = messageId, resolution = resolution, note = "resolved from Android"))
    }

    fun reissue() {
        val m = (_ui.value.message as? Load.Ready)?.value ?: return
        if (m.status.runtime == DeliveryState.INDETERMINATE && !_ui.value.acceptRisk) return
        run(IntentIds.reissue(messageId)) { op -> reissueMessage(ReissueMessageRequest(operationId = op, messageId = messageId, acceptDuplicateRisk = true)) }
    }

    private fun run(intent: String, block: suspend PyreApi.(String) -> MessageView) {
        if (_ui.value.busy) return
        _ui.value = _ui.value.copy(busy = true)
        viewModelScope.launch {
            try {
                // Intent kept (not completed): repeats replay the same operation.
                val r = repo.call { block(repo.intents.forIntent(intent)) }
                val reissued = r.envelope.messageId != messageId
                _ui.value = _ui.value.copy(busy = false, notice = if (reissued) "Reissued as ${r.envelope.messageId}." else "Recorded.")
                refresh()
            } catch (e: PyreApiException) {
                _ui.value = _ui.value.copy(busy = false, notice = e.describe())
            }
        }
    }
}

@Composable
fun MessageScreen(vm: MessageViewModel, onOpenLink: (String) -> Unit = {}, onOpenSession: (String) -> Unit) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    RefreshWhileResumed { vm.refresh() }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
        ui.notice?.let { Banner(it, Tone.MUTED) }
        when (val l = ui.message) {
            Load.Loading -> LoadingBox()
            is Load.Failed -> ErrorBox(l.error.describe(), { vm.refresh() })
            is Load.Ready -> {
                val m = l.value
                val f = Delivery.facts(m)
                val e = m.envelope
                Column(Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) {
                    Text(
                        "From " + Identity.principal(e.sender, ui.titles),
                        style = MaterialTheme.typography.titleMedium,
                    )
                    Supporting("To " + Identity.address(e.recipient, ui.titles) + (e.createdAt.takeIf { it.isNotEmpty() }?.let { " · " + Times.clock(it) } ?: ""))
                }
                CardBlock {
                    Text(e.body.text, style = MaterialTheme.typography.bodyLarge)
                }
                e.attachments.forEach { a ->
                    val (label, link) = Identity.attachment(a)
                    LinkRow("Attached: $label", link != null, Modifier.testTag("attachment")) { link?.let(onOpenLink) }
                }
                if (e.inReplyTo.isNotEmpty()) LinkRow("In reply to ${shortId(e.inReplyTo)}", true, Modifier.testTag("in-reply-to")) { onOpenLink("pyre://message/${e.inReplyTo}") }
                if (m.status.ackRef.isNotEmpty()) LinkRow("Acknowledged by ${shortId(m.status.ackRef)}", true) { onOpenLink("pyre://message/${m.status.ackRef}") }
                if (e.recipient.kind == "session") LinkRow("Open the session", true) { onOpenSession(e.recipient.id) }

                SectionHeader("Delivery")
                listOf(f.storage, f.runtime, f.ack, f.outcome).forEach { fact -> FactRow(fact.label, fact.value, fact.tone) }
                f.evidence?.let { FactRow("Evidence", it, Tone.MUTED) }

                if (f.needsReconciliation) {
                    SectionHeader("Did it arrive?")
                    Supporting("Pyre can't tell whether the agent received this. Check the session's terminal or activity before deciding.", Modifier.padding(horizontal = 16.dp))
                    Column(Modifier.padding(16.dp)) {
                        OutlinedButton(enabled = !ui.busy, onClick = { vm.resolve(DeliveryState.ACCEPTED) }, modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp)) { Text("It was delivered (mark accepted)") }
                        OutlinedButton(enabled = !ui.busy, onClick = { vm.resolve(DeliveryState.REJECTED) }, modifier = Modifier.fillMaxWidth().padding(top = 8.dp).heightIn(min = 48.dp)) { Text("It was not delivered (mark rejected)") }
                        Text(
                            "Sending again creates a NEW message. If the first one was received, the work may happen twice.",
                            color = MaterialTheme.colorScheme.error,
                            style = MaterialTheme.typography.bodyMedium,
                            modifier = Modifier.padding(top = 16.dp),
                        )
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Checkbox(ui.acceptRisk, vm::setAcceptRisk, Modifier.testTag("accept-risk"))
                            Text("I accept the duplication risk")
                        }
                    }
                }
                if (f.canReissue) {
                    Button(
                        enabled = !ui.busy && (!f.reissueDuplicateRisk || ui.acceptRisk),
                        onClick = vm::reissue,
                        modifier = Modifier.padding(horizontal = 16.dp).heightIn(min = 48.dp),
                    ) { Text("Reissue as new message") }
                }
                Expandable("Details", modifier = Modifier.padding(top = 8.dp)) {
                    KeyValue("Message", e.messageId, mono = true, modifier = Modifier.testTag("message-id"))
                    KeyValue("Mode", e.mode)
                    if (e.threadId.isNotEmpty()) KeyValue("Thread", e.threadId, mono = true)
                    if (e.reissues.isNotEmpty()) KeyValue("Reissues", e.reissues, mono = true)
                    if (e.createdAt.isNotEmpty()) KeyValue("Created", e.createdAt)
                }
                Spacer(Modifier.size(24.dp))
            }
        }
    }
}

@Composable
private fun LinkRow(text: String, enabled: Boolean, modifier: Modifier = Modifier, onClick: () -> Unit) {
    Row(
        modifier.fillMaxWidth().let { if (enabled) it.clickable(role = Role.Button, onClick = onClick) else it }.heightIn(min = 48.dp).padding(horizontal = 16.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(text, style = MaterialTheme.typography.bodyLarge, color = if (enabled) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurface, modifier = Modifier.weight(1f))
        if (enabled) PyreIcon(R.drawable.ic_chevron_right, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun FactRow(label: String, value: String, tone: Tone) {
    Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp).semantics(mergeDescendants = true) {}) {
        Text(label, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(value, style = MaterialTheme.typography.bodyLarge, color = if (tone == Tone.ERROR || tone == Tone.ATTENTION) toneColor(tone) else MaterialTheme.colorScheme.onSurface)
    }
}
