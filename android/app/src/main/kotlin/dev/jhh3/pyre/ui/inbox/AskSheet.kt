package dev.jhh3.pyre.ui.inbox

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.domain.AnswerWindow
import dev.jhh3.pyre.domain.Asks
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.Ask
import dev.jhh3.pyre.model.AskOption
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.Expandable
import dev.jhh3.pyre.ui.common.KeyValue
import dev.jhh3.pyre.ui.common.LoadingBox
import dev.jhh3.pyre.ui.common.Pill
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.session.Markdown
import dev.jhh3.pyre.ui.theme.toneColor

/**
 * An ask in a bottom sheet: who asks and why (context, merged updates),
 * the deadline and default, one button per option (the default marked),
 * free text. Tapping an option answers at once: the sheet closes and the
 * inbox shows an Undo snackbar for 5 s; every answer given in that window
 * goes out in one asks.answer call. "Open the chat" jumps to the asking
 * session.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AskSheet(ui: InboxUi, vm: InboxViewModel, onOpenSession: (String) -> Unit) {
    val sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    ModalBottomSheet(onDismissRequest = vm::closeAsk, sheetState = sheetState, modifier = Modifier.testTag("ask-sheet")) {
        val ask = ui.openAsk
        Column(Modifier.fillMaxWidth().verticalScroll(rememberScrollState()).padding(bottom = 24.dp)) {
            when {
                ask != null -> AskContent(ask, ui, vm, onOpenSession)
                ui.sheetError != null -> Banner(ui.sheetError, Tone.ERROR, Modifier.testTag("ask-error"))
                else -> LoadingBox()
            }
        }
    }
}

@Composable
private fun AskContent(ask: Ask, ui: InboxUi, vm: InboxViewModel, onOpenSession: (String) -> Unit) {
    val now = ui.now
    val held = ui.held[ask.askId]
    var text by rememberSaveable(ask.askId) { mutableStateOf(held?.text.orEmpty()) }
    val answerable = !ask.decided && !ui.sending

    Column(Modifier.padding(horizontal = 24.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Pill("Ask", if (ask.decided) Tone.MUTED else Tone.ATTENTION)
            if (ask.urgent && !ask.decided) Pill("Urgent", Tone.ERROR)
            Supporting(ui.sessionTitles[ask.sessionId] ?: ask.sessionId.ifEmpty { "you" }, maxLines = 1)
        }
        Text(ask.title, style = MaterialTheme.typography.headlineSmall, modifier = Modifier.padding(top = 8.dp, bottom = 4.dp).testTag("ask-title"))
        val meta = listOfNotNull(
            "Asked " + Times.ago(ask.createdAt, now),
            if (ask.count > 1) "asked ${ask.count}×" else null,
            Asks.deadline(ask, now).ifEmpty { null },
        ).joinToString(" · ")
        Text(
            meta,
            style = MaterialTheme.typography.bodyMedium,
            color = if (Asks.soon(ask, now)) toneColor(Tone.ATTENTION) else MaterialTheme.colorScheme.onSurfaceVariant,
            fontWeight = if (Asks.soon(ask, now)) FontWeight.SemiBold else FontWeight.Normal,
            modifier = Modifier.testTag("ask-deadline"),
        )
        if (ask.deadlineRule == "morning" && !ask.decided) {
            Supporting("The deadline fell in quiet hours, so it moved to the morning.", Modifier.padding(top = 2.dp))
        }
        if (ask.context.isNotEmpty()) Markdown(ask.context, Modifier.padding(top = 12.dp).testTag("ask-context"))
        ask.updates.forEach { u ->
            Column(Modifier.padding(top = 12.dp)) {
                Supporting("Update · " + Times.ago(u.at, now))
                Text(u.text, style = MaterialTheme.typography.bodyMedium)
            }
        }
    }

    if (ask.decided) {
        Banner(Asks.explain(ask), Tone.MUTED, Modifier.padding(top = 12.dp).testTag("ask-outcome"))
    } else {
        Column(Modifier.padding(horizontal = 24.dp, vertical = 12.dp)) {
            ask.options.forEach { o ->
                OptionButton(
                    o,
                    isDefault = o.id == ask.defaultOption,
                    isHeld = held != null && held.optionId == o.id,
                    enabled = answerable,
                ) { vm.choose(ask.askId, o.id) }
            }
            OutlinedTextField(
                text,
                { text = it },
                Modifier.fillMaxWidth().padding(top = 8.dp).testTag("ask-text"),
                label = { Text(if (ask.options.isEmpty()) "Your answer" else "Or answer in your own words") },
                minLines = 2,
            )
            Button(
                onClick = { vm.choose(ask.askId, "", text) },
                enabled = answerable && text.isNotBlank(),
                modifier = Modifier.padding(top = 8.dp).heightIn(min = 48.dp).testTag("ask-send-text"),
            ) { Text("Send this answer") }
            Supporting("Sent after ${AnswerWindow.WINDOW.seconds} s; Undo takes it back until then.", Modifier.padding(top = 4.dp))
            if (held != null) {
                Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(top = 4.dp)) {
                    Supporting(
                        "Your answer: " + (held.optionId.takeIf { it.isNotEmpty() }?.let { Asks.optionLabel(ask, it) } ?: "“${held.text}”") + " · sending…",
                        Modifier.weight(1f),
                    )
                    TextButton(onClick = { vm.undo(ask.askId) }, modifier = Modifier.heightIn(min = 48.dp).testTag("ask-undo")) { Text("Undo") }
                }
            }
        }
    }

    ui.notice?.let { Banner(it, ui.noticeTone, Modifier.testTag("ask-notice")) }

    if (ask.sessionId.isNotEmpty()) {
        TextButton(
            onClick = { onOpenSession(ask.sessionId) },
            modifier = Modifier.padding(horizontal = 16.dp).heightIn(min = 48.dp).testTag("ask-open-chat"),
        ) { Text("Open the chat") }
    }
    Expandable("Details") {
        KeyValue("Topic", ask.topicKey, mono = true)
        KeyValue("Ask ID", ask.askId, mono = true)
        if (ask.deadlineAt != null) KeyValue("Deadline", Times.clock(ask.deadlineAt) + if (ask.deadlineRule.isNotEmpty()) " (${ask.deadlineRule})" else "")
        if (ask.supersededBy.isNotEmpty()) KeyValue("Replaced by", ask.supersededBy, mono = true)
    }
}

/** A full-width option: label, one line of consequence; the default is filled and says so. */
@Composable
private fun OptionButton(o: AskOption, isDefault: Boolean, isHeld: Boolean, enabled: Boolean, onClick: () -> Unit) {
    val mod = Modifier.fillMaxWidth().padding(vertical = 4.dp).heightIn(min = 56.dp).testTag("ask-option-" + o.id)
    val content: @Composable () -> Unit = {
        Column(Modifier.fillMaxWidth().padding(vertical = 4.dp)) {
            Text(
                (if (isHeld) "✓ " else "") + o.label.ifEmpty { o.id } + if (isDefault) "  · default" else "",
                style = MaterialTheme.typography.labelLarge,
            )
            if (o.detail.isNotEmpty()) Text(o.detail, style = MaterialTheme.typography.bodySmall)
        }
    }
    when {
        isHeld -> FilledTonalButton(onClick = onClick, enabled = enabled, modifier = mod) { content() }
        isDefault -> Button(onClick = onClick, enabled = enabled, modifier = mod) { content() }
        else -> OutlinedButton(onClick = onClick, enabled = enabled, modifier = mod) { content() }
    }
}
