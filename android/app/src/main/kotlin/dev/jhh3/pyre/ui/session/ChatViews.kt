package dev.jhh3.pyre.ui.session

import androidx.compose.animation.animateContentSize
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.platform.LocalWindowInfo
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.jhh3.pyre.R
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.ui.common.PyreIcon
import dev.jhh3.pyre.ui.theme.toneColor

/** One chat item. Each is self-contained so the LazyColumn re-composes only items whose data changed. */
@Composable
fun ChatRow(item: ChatItem) {
    when (item) {
        is Bubble -> ChatBubble(item)
        is PyreMessageChip -> MessageChip(item)
        is ToolGroup -> ToolGroupRow(item)
        is TurnEnd -> Faint(item.label, Modifier.testTag("turn-end"))
        is TimeMark -> Faint(Times.clock(item.at), Modifier.testTag("time-mark"))
    }
}

@Composable
private fun ChatBubble(b: Bubble) {
    // Bubbles take most of the width; the opposite margin shows who is speaking.
    val maxW = windowWidth() * if (b.mine) 0.82f else 0.92f
    Row(
        Modifier.fillMaxWidth().padding(start = if (b.mine) 40.dp else 12.dp, end = if (b.mine) 12.dp else 24.dp, top = 6.dp, bottom = 6.dp),
        horizontalArrangement = if (b.mine) Arrangement.End else Arrangement.Start,
    ) {
        Surface(
            color = if (b.mine) MaterialTheme.colorScheme.secondaryContainer else MaterialTheme.colorScheme.surfaceContainerHigh,
            contentColor = if (b.mine) MaterialTheme.colorScheme.onSecondaryContainer else MaterialTheme.colorScheme.onSurface,
            shape = RoundedCornerShape(topStart = 18.dp, topEnd = 18.dp, bottomStart = if (b.mine) 18.dp else 6.dp, bottomEnd = if (b.mine) 6.dp else 18.dp),
            modifier = Modifier.widthIn(max = maxW).testTag(if (b.mine) "bubble-user" else "bubble-agent"),
        ) {
            Column(Modifier.padding(horizontal = 14.dp, vertical = 10.dp)) {
                Markdown(b.text)
                if (b.truncated) Text("Shortened by Pyre — the full text is in the terminal.", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(top = 6.dp))
            }
        }
    }
}

@Composable
private fun MessageChip(m: PyreMessageChip) {
    var open by rememberSaveable(m.key) { mutableStateOf(false) }
    Row(Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 6.dp), horizontalArrangement = Arrangement.End) {
        Surface(
            onClick = { open = !open },
            color = MaterialTheme.colorScheme.surfaceContainerLow,
            shape = RoundedCornerShape(14.dp),
            modifier = Modifier.widthIn(max = windowWidth() * 0.86f).testTag("pyre-message"),
        ) {
            Column(Modifier.padding(horizontal = 12.dp, vertical = 8.dp).animateContentSize()) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    PyreIcon(R.drawable.ic_inbox, null, Modifier.size(16.dp), tint = MaterialTheme.colorScheme.primary)
                    Spacer(Modifier.width(6.dp))
                    Text(
                        "Message from ${m.from}" + if (m.context.isNotEmpty()) " · ${m.context}" else "",
                        style = MaterialTheme.typography.labelLarge,
                        color = MaterialTheme.colorScheme.primary,
                    )
                }
                if (m.body.isNotEmpty()) {
                    if (open) Markdown(m.body, Modifier.padding(top = 4.dp), style = MaterialTheme.typography.bodyMedium)
                    else Text(m.body, style = MaterialTheme.typography.bodyMedium, maxLines = 3, overflow = TextOverflow.Ellipsis, modifier = Modifier.padding(top = 4.dp))
                }
            }
        }
    }
}

/** "Ran 4 shell commands, edited proxy.ts" — one quiet line; tap for the calls. */
@Composable
private fun ToolGroupRow(g: ToolGroup) {
    var open by rememberSaveable(g.key) { mutableStateOf(false) }
    val muted = MaterialTheme.colorScheme.onSurfaceVariant
    Column(Modifier.fillMaxWidth().padding(horizontal = 12.dp).animateContentSize()) {
        Row(
            Modifier.fillMaxWidth()
                .clickable(role = Role.Button) { open = !open }
                .heightIn(min = 40.dp)
                .padding(horizontal = 4.dp)
                .semantics { stateDescription = if (open) "Expanded" else "Collapsed" }
                .testTag("tool-group"),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            PyreIcon(if (g.calls.all { it.kind == ToolKind.SHELL }) R.drawable.ic_terminal else R.drawable.ic_done_all, null, Modifier.size(16.dp), tint = muted.copy(alpha = 0.7f))
            Spacer(Modifier.width(8.dp))
            Text(g.summary, style = MaterialTheme.typography.bodyMedium, color = muted, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
            if (g.failed) {
                Spacer(Modifier.width(6.dp))
                Text("· failed", style = MaterialTheme.typography.bodySmall, color = toneColor(Tone.ERROR))
            }
            PyreIcon(if (open) R.drawable.ic_expand_less else R.drawable.ic_expand_more, null, Modifier.size(18.dp), tint = muted.copy(alpha = 0.6f))
        }
        if (open) {
            Column(Modifier.padding(start = 28.dp, end = 4.dp, bottom = 8.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                g.calls.forEachIndexed { i, c -> ToolCallDetail(c, "${g.key}_$i") }
            }
        }
    }
}

@Composable
private fun ToolCallDetail(c: ToolCall, key: String) {
    var more by rememberSaveable(key) { mutableStateOf(false) }
    val muted = MaterialTheme.colorScheme.onSurfaceVariant
    val label = when (c.kind) {
        ToolKind.SHELL -> "Shell"
        ToolKind.EDIT -> if (c.name == "Write") "Wrote" else "Edited"
        ToolKind.READ -> "Read"
        ToolKind.SEARCH -> "Searched"
        ToolKind.WEB -> "Web search"
        ToolKind.FETCH -> "Fetched"
        ToolKind.AGENT -> "Subagent"
        ToolKind.PLAN -> "Plan"
        ToolKind.MCP, ToolKind.OTHER -> c.name
    } + if (c.failed) " · failed" else ""
    Column(Modifier.fillMaxWidth().testTag("tool-call")) {
        Text(label, style = MaterialTheme.typography.labelMedium, color = if (c.failed) toneColor(Tone.ERROR) else muted)
        if (c.arg.isNotEmpty()) {
            val long = c.arg.length > 160 || c.arg.count { it == '\n' } >= 3
            val shown = if (c.kind == ToolKind.SHELL) "$ " + c.arg else c.arg
            if (c.kind == ToolKind.SHELL || c.kind == ToolKind.EDIT || c.kind == ToolKind.READ) {
                CodeBlock(if (long && !more) shown.lineSequence().take(3).joinToString("\n").take(160) + "…" else shown, Modifier.padding(top = 2.dp))
            } else {
                Text(
                    c.arg,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurface,
                    fontFamily = if (c.kind == ToolKind.SEARCH) FontFamily.Monospace else null,
                    maxLines = if (more) Int.MAX_VALUE else 3,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            if (long || c.truncated) {
                Text(
                    if (!more) "Show more" else if (c.truncated) "Show less · Pyre kept the first 2000 characters" else "Show less",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.primary,
                    modifier = Modifier.clickable(role = Role.Button) { more = !more }.heightIn(min = 32.dp).padding(top = 6.dp).testTag("show-more"),
                )
            }
        }
    }
}

/** The window width, for readable bubble widths. */
@Composable
private fun windowWidth(): Dp = with(LocalDensity.current) { LocalWindowInfo.current.containerSize.width.toDp() }

@Composable
private fun Faint(text: String, modifier: Modifier = Modifier) {
    Text(
        text,
        style = MaterialTheme.typography.labelSmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.7f),
        textAlign = TextAlign.Center,
        modifier = modifier.fillMaxWidth().padding(vertical = 6.dp),
    )
}
