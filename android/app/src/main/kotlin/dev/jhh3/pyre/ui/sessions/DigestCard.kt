package dev.jhh3.pyre.ui.sessions

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.digest.Digest
import dev.jhh3.pyre.digest.DigestAgent
import dev.jhh3.pyre.digest.Rank
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.ui.common.CardBlock
import dev.jhh3.pyre.ui.common.Supporting
import dev.jhh3.pyre.ui.theme.toneColor
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter

/** "since 09:12 · 5 agents, 2 need you" (the card's subtitle). */
fun digestSubtitle(d: Digest, zone: ZoneId = ZoneId.systemDefault()): String {
    val parts = ArrayList<String>()
    parts += if (d.sinceLabel == "last seen") "since " + DateTimeFormatter.ofPattern("HH:mm").withZone(zone).format(d.since) else d.sinceLabel
    var n = if (d.agents.size == 1) "1 agent" else "${d.agents.size} agents"
    val k = d.count(Rank.NEEDS_YOU)
    if (k > 0) n += ", $k need" + (if (k == 1) "s" else "") + " you"
    parts += n
    return parts.filter { it.isNotEmpty() }.joinToString(" · ")
}

private fun DigestAgent.tone(): Tone = when {
    rank == Rank.NEEDS_YOU && needsYou.isEmpty() -> Tone.ERROR
    rank == Rank.NEEDS_YOU -> Tone.ATTENTION
    rank == Rank.WORKING -> Tone.ACTIVE
    else -> Tone.MUTED
}

private const val COLLAPSED_ROWS = 5

/**
 * "While you were away": one quiet card at the top of Sessions with a line
 * per agent (needs you → done → still working). Tapping a line opens its
 * deep link (the request, or the transcript moment); Dismiss marks it seen.
 */
@Composable
fun DigestCard(d: Digest, now: Instant, onOpenLink: (String) -> Unit, onDismiss: () -> Unit) {
    var expanded by rememberSaveable { mutableStateOf(false) }
    CardBlock(modifier = Modifier.testTag("digest-card")) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text(
                    "While you were away",
                    style = MaterialTheme.typography.titleMedium,
                    fontWeight = FontWeight.SemiBold,
                    modifier = Modifier.semantics { heading() },
                )
                Supporting(digestSubtitle(d), maxLines = 1)
            }
            TextButton(onClick = onDismiss, modifier = Modifier.heightIn(min = 48.dp).testTag("digest-dismiss")) { Text("Dismiss") }
        }
        val rows = if (expanded) d.agents else d.agents.take(COLLAPSED_ROWS)
        Spacer(Modifier.padding(top = 4.dp))
        rows.forEach { a -> AgentRow(a, now, onOpenLink) }
        d.other.forEach { r ->
            DigestLine("!", Tone.ATTENTION, r.title, null, null, Modifier.testTag("digest-other-${r.id}")) { onOpenLink(r.link) }
        }
        val hidden = d.agents.size - rows.size
        if (hidden > 0) {
            TextButton(onClick = { expanded = true }, modifier = Modifier.heightIn(min = 48.dp).testTag("digest-more")) { Text("Show $hidden more") }
        }
        if (d.quiet > 0) {
            Supporting(
                if (d.quiet == 1) "1 quiet session (status changes only)" else "${d.quiet} quiet sessions (status changes only)",
                Modifier.padding(top = 6.dp).testTag("digest-quiet"),
            )
        }
    }
}

@Composable
private fun AgentRow(a: DigestAgent, now: Instant, onOpenLink: (String) -> Unit) {
    val meta = listOf(a.where, a.lastActivity?.let { Times.ago(it, now) } ?: "", a.facts).filter { it.isNotEmpty() }.joinToString(" · ")
    DigestLine(a.glyph, a.tone(), a.title, a.summary, meta, Modifier.testTag("digest-row-${a.sessionId}")) { onOpenLink(a.link) }
}

@Composable
private fun DigestLine(glyph: String, tone: Tone, title: String, summary: String?, meta: String?, modifier: Modifier, onClick: () -> Unit) {
    Row(
        modifier.fillMaxWidth()
            .clickable(role = Role.Button, onClick = onClick)
            .heightIn(min = 48.dp)
            .padding(vertical = 8.dp)
            .semantics(mergeDescendants = true) {},
        verticalAlignment = Alignment.Top,
    ) {
        Text(
            glyph,
            color = toneColor(tone),
            style = MaterialTheme.typography.titleSmall,
            fontWeight = if (tone == Tone.ATTENTION || tone == Tone.ERROR) FontWeight.Bold else FontWeight.Normal,
            modifier = Modifier.widthIn(min = 20.dp),
        )
        Spacer(Modifier.width(8.dp))
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.titleSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
            if (!summary.isNullOrEmpty()) {
                Text(summary, style = MaterialTheme.typography.bodyMedium, maxLines = 2, overflow = TextOverflow.Ellipsis)
            }
            if (!meta.isNullOrEmpty()) {
                Text(meta, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
            }
        }
    }
}
