package dev.jhh3.pyre.ui.common

import androidx.annotation.DrawableRes
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.Stable
import androidx.compose.runtime.compositionLocalOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.repeatOnLifecycle
import dev.jhh3.pyre.R
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.domain.StatusLabel
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.theme.emphasized
import dev.jhh3.pyre.ui.theme.toneColor
import java.time.Instant
import kotlinx.coroutines.delay

// ---------------------------------------------------------------------------
// Screen chrome and refresh plumbing
// ---------------------------------------------------------------------------

/** Lets a screen name itself in the top app bar (e.g. the session's title). */
@Stable
class ScreenChrome {
    var title by mutableStateOf<String?>(null)
}

val LocalScreenChrome = staticCompositionLocalOf<ScreenChrome?> { null }

/** Bumped by the inline "Retry" in the connection status; open screens reload. */
val LocalRetrySignal = compositionLocalOf { 0 }

@Composable
fun ScreenTitle(title: String?) {
    val chrome = LocalScreenChrome.current ?: return
    SideEffect { chrome.title = title }
}

/**
 * Re-runs [refresh] every [periodMs] while the screen is RESUMED, so open
 * screens follow run state and delivery facts without a manual reload; it
 * stops when the app is backgrounded (no polling behind the user's back).
 * Also reloads at once when the person taps Retry.
 */
@Composable
fun RefreshWhileResumed(periodMs: Long = 5_000, refresh: () -> Unit) {
    val owner = LocalLifecycleOwner.current
    val latest by rememberUpdatedState(refresh)
    LaunchedEffect(owner, periodMs) {
        owner.lifecycle.repeatOnLifecycle(Lifecycle.State.RESUMED) {
            while (true) {
                delay(periodMs)
                latest()
            }
        }
    }
    OnRetry(refresh)
}

/** Reloads when the person taps Retry (for screens that do not poll). */
@Composable
fun OnRetry(refresh: () -> Unit) {
    val signal = LocalRetrySignal.current
    val latest by rememberUpdatedState(refresh)
    LaunchedEffect(signal) { if (signal > 0) latest() }
}

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

@Composable
fun PyreIcon(@DrawableRes id: Int, contentDescription: String?, modifier: Modifier = Modifier, tint: Color = Color.Unspecified) {
    if (tint == Color.Unspecified) Icon(painterResource(id), contentDescription, modifier)
    else Icon(painterResource(id), contentDescription, modifier, tint = tint)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

/** A small dot: filled when something is live or needs you, a ring when idle, offline or unknown. */
@Composable
fun StatusDot(tone: Tone, modifier: Modifier = Modifier, size: Int = 10) {
    val c = toneColor(tone)
    val filled = tone.emphasized()
    Canvas(modifier.size(size.dp)) {
        if (filled) drawCircle(c) else drawCircle(c, radius = this.size.minDimension / 2 - 1.dp.toPx(), style = Stroke(1.5.dp.toPx()))
    }
}

/** Status = dot + word (never colour alone). */
@Composable
fun StatusChip(s: StatusLabel, modifier: Modifier = Modifier) {
    val c = toneColor(s.tone)
    Row(modifier.testTag("status").semantics(mergeDescendants = true) {}, verticalAlignment = Alignment.CenterVertically) {
        StatusDot(s.tone)
        Spacer(Modifier.width(6.dp))
        Text(
            s.label,
            color = c,
            style = MaterialTheme.typography.labelLarge,
            fontWeight = if (s.tone == Tone.ATTENTION || s.tone == Tone.ERROR) FontWeight.SemiBold else FontWeight.Normal,
        )
    }
}

@Composable
fun ToneText(text: String, tone: Tone, modifier: Modifier = Modifier) {
    Text(text, color = toneColor(tone), style = MaterialTheme.typography.bodyMedium, modifier = modifier)
}

/** Muted supporting text. */
@Composable
fun Supporting(text: String, modifier: Modifier = Modifier, maxLines: Int = Int.MAX_VALUE) {
    Text(
        text,
        style = MaterialTheme.typography.bodyMedium,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        maxLines = maxLines,
        overflow = TextOverflow.Ellipsis,
        modifier = modifier,
    )
}

// ---------------------------------------------------------------------------
// Notices, empty / loading / error states
// ---------------------------------------------------------------------------

/**
 * An inline notice. Errors and things that need the person get a tinted
 * container; everything else sits on a quiet surface. Never a stuck banner:
 * notices belong to the content they describe.
 */
@Composable
fun Banner(text: String, tone: Tone, modifier: Modifier = Modifier, action: (@Composable () -> Unit)? = null) {
    val cs = MaterialTheme.colorScheme
    val (bg, fg) = when (tone) {
        Tone.ERROR -> cs.errorContainer to cs.onErrorContainer
        Tone.ATTENTION -> cs.primaryContainer to cs.onPrimaryContainer
        else -> cs.surfaceContainerHigh to cs.onSurface
    }
    Surface(
        color = bg,
        contentColor = fg,
        shape = RoundedCornerShape(12.dp),
        modifier = modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp),
    ) {
        Row(Modifier.padding(start = 16.dp, end = 8.dp, top = 12.dp, bottom = 12.dp), verticalAlignment = Alignment.CenterVertically) {
            Text(text, modifier = Modifier.weight(1f).padding(end = 8.dp), style = MaterialTheme.typography.bodyMedium)
            action?.invoke()
        }
    }
}

/** "Saved copy · 12m ago": one quiet line when a screen shows cached data. */
@Composable
fun CachedBanner(cachedAt: Instant?, error: PyreApiException?, now: Instant) {
    if (cachedAt == null) return
    val why = when {
        error == null -> "offline"
        error.transport -> "hub unreachable"
        else -> error.code
    }
    Row(
        Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp).testTag("cached-banner").semantics(mergeDescendants = true) {},
        verticalAlignment = Alignment.CenterVertically,
    ) {
        PyreIcon(R.drawable.ic_cloud_off, null, Modifier.size(16.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Spacer(Modifier.width(8.dp))
        Supporting("Saved copy from ${Times.ago(cachedAt, now)} · $why")
    }
}

@Composable
fun EmptyState(
    title: String,
    body: String?,
    @DrawableRes icon: Int?,
    modifier: Modifier = Modifier,
    action: (@Composable () -> Unit)? = null,
) {
    Column(
        modifier.fillMaxWidth().padding(horizontal = 32.dp, vertical = 48.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        if (icon != null) {
            PyreIcon(icon, null, Modifier.size(40.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.size(16.dp))
        }
        Text(title, style = MaterialTheme.typography.titleMedium, textAlign = TextAlign.Center, modifier = Modifier.semantics { heading() })
        if (body != null) {
            Spacer(Modifier.size(8.dp))
            Text(body, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
        }
        if (action != null) {
            Spacer(Modifier.size(20.dp))
            action()
        }
    }
}

@Composable
fun ErrorBox(message: String, onRetry: (() -> Unit)?) {
    EmptyState(
        title = "Couldn't load this",
        body = message,
        icon = R.drawable.ic_cloud_off,
        action = onRetry?.let { { FilledTonalButton(onClick = it, modifier = Modifier.heightIn(min = 48.dp)) { Text("Try again") } } },
    )
}

@Composable
fun LoadingBox() {
    Box(Modifier.fillMaxWidth().padding(vertical = 64.dp).semantics { contentDescription = "Loading" }, contentAlignment = Alignment.Center) {
        CircularProgressIndicator()
    }
}

/** Renders Loading / Failed / Ready(+cached label) uniformly. */
@Composable
fun <T> LoadView(load: Load<T>, now: Instant, onRetry: () -> Unit, content: @Composable (T) -> Unit) {
    when (load) {
        Load.Loading -> LoadingBox()
        is Load.Failed -> ErrorBox(load.error.describe(), onRetry)
        is Load.Ready -> Column {
            CachedBanner(load.cachedAt, load.error, now)
            content(load.value)
        }
    }
}

// ---------------------------------------------------------------------------
// Layout primitives
// ---------------------------------------------------------------------------

/** A quiet group label (sentence case, no rule). */
@Composable
fun SectionHeader(text: String, modifier: Modifier = Modifier, trailing: (@Composable RowScope.() -> Unit)? = null) {
    Row(
        modifier.fillMaxWidth().padding(start = 16.dp, end = 8.dp, top = 24.dp, bottom = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            text,
            style = MaterialTheme.typography.titleSmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.weight(1f).semantics { heading() },
        )
        trailing?.invoke(this)
    }
}

/** Label above value: no cramped columns, wraps at any font size. */
@Composable
fun KeyValue(key: String, value: String, modifier: Modifier = Modifier, mono: Boolean = false) {
    Column(modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp).semantics(mergeDescendants = true) {}) {
        Text(key, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(value, style = MaterialTheme.typography.bodyMedium, fontFamily = if (mono) FontFamily.Monospace else null)
    }
}

@Composable
fun Pill(text: String, tone: Tone, modifier: Modifier = Modifier) {
    val cs = MaterialTheme.colorScheme
    val (bg, fg) = when (tone) {
        Tone.ATTENTION -> cs.primaryContainer to cs.onPrimaryContainer
        Tone.ERROR -> cs.errorContainer to cs.onErrorContainer
        else -> cs.surfaceContainerHighest to cs.onSurfaceVariant
    }
    Text(
        text,
        color = fg,
        style = MaterialTheme.typography.labelMedium,
        maxLines = 1,
        overflow = TextOverflow.Ellipsis,
        modifier = modifier.background(bg, RoundedCornerShape(50)).padding(horizontal = 10.dp, vertical = 3.dp),
    )
}

/** Count badge for list rows ("2"). */
@Composable
fun CountBadge(count: Int, description: String) {
    if (count <= 0) return
    Box(
        Modifier.widthIn(min = 24.dp).heightIn(min = 24.dp)
            .background(MaterialTheme.colorScheme.primary, RoundedCornerShape(50))
            .padding(horizontal = 7.dp)
            .clearAndSetSemantics { contentDescription = description },
        contentAlignment = Alignment.Center,
    ) {
        Text("$count", color = MaterialTheme.colorScheme.onPrimary, style = MaterialTheme.typography.labelMedium)
    }
}

/** A tappable list row with headline, supporting text and optional leading/trailing slots (≥ 56dp). */
@Composable
fun ListRow(
    headline: String,
    modifier: Modifier = Modifier,
    supporting: String? = null,
    supportingMaxLines: Int = 2,
    leading: (@Composable () -> Unit)? = null,
    trailing: (@Composable () -> Unit)? = null,
    onClick: (() -> Unit)? = null,
) {
    Row(
        modifier.fillMaxWidth()
            .let { if (onClick != null) it.clickable(role = Role.Button, onClick = onClick) else it }
            .heightIn(min = 56.dp)
            .padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (leading != null) {
            leading()
            Spacer(Modifier.width(16.dp))
        }
        Column(Modifier.weight(1f)) {
            Text(headline, style = MaterialTheme.typography.bodyLarge, maxLines = 2, overflow = TextOverflow.Ellipsis)
            if (!supporting.isNullOrEmpty()) Supporting(supporting, maxLines = supportingMaxLines)
        }
        if (trailing != null) {
            Spacer(Modifier.width(12.dp))
            trailing()
        }
    }
}

/** Rounded tonal card used for inbox items, requests and messages. */
@Composable
fun CardBlock(
    modifier: Modifier = Modifier,
    onClick: (() -> Unit)? = null,
    color: Color = MaterialTheme.colorScheme.surfaceContainerLow,
    content: @Composable ColumnScope.() -> Unit,
) {
    val shape = RoundedCornerShape(16.dp)
    if (onClick != null) {
        Surface(onClick = onClick, shape = shape, color = color, modifier = modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp)) {
            Column(Modifier.padding(16.dp), content = content)
        }
    } else {
        Surface(shape = shape, color = color, modifier = modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp)) {
            Column(Modifier.padding(16.dp), content = content)
        }
    }
}

/** A collapsed-by-default section ("About this connection", "Delivery details"). */
@Composable
fun Expandable(
    title: String,
    modifier: Modifier = Modifier,
    initiallyExpanded: Boolean = false,
    tag: String? = null,
    content: @Composable ColumnScope.() -> Unit,
) {
    var open by rememberSaveable { mutableStateOf(initiallyExpanded) }
    Column(modifier.fillMaxWidth()) {
        Row(
            Modifier.fillMaxWidth()
                .clickable(role = Role.Button) { open = !open }
                .heightIn(min = 48.dp)
                .padding(horizontal = 16.dp)
                .semantics { stateDescription = if (open) "Expanded" else "Collapsed" }
                .let { if (tag != null) it.testTag(tag) else it },
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(title, style = MaterialTheme.typography.titleSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.weight(1f))
            PyreIcon(if (open) R.drawable.ic_expand_less else R.drawable.ic_expand_more, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        if (open) Column(content = content)
    }
}

@Composable
fun ButtonRow(content: @Composable RowScope.() -> Unit) {
    Row(
        Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
        verticalAlignment = Alignment.CenterVertically,
        content = content,
    )
}

@Composable
fun PrimaryButton(text: String, modifier: Modifier = Modifier, enabled: Boolean = true, onClick: () -> Unit) {
    Button(onClick = onClick, enabled = enabled, modifier = modifier.heightIn(min = 48.dp)) { Text(text) }
}

/** Text link styled action with a 48dp touch target. */
@Composable
fun LinkButton(text: String, modifier: Modifier = Modifier, onClick: () -> Unit) {
    TextButton(onClick = onClick, modifier = modifier.heightIn(min = 48.dp)) { Text(text) }
}

fun shortId(id: String): String = if (id.length > 14) id.take(6) + "…" + id.takeLast(6) else id
