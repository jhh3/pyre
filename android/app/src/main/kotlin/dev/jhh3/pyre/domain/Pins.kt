package dev.jhh3.pyre.domain

import dev.jhh3.pyre.model.SessionView
import java.time.Instant

/**
 * Pinned sessions, shared with the TUI through reserved session tags
 * (sessions.update replaces the whole tag list, so edits are
 * read-modify-write of the current tags):
 * - "pinned" marks a pinned session;
 * - "pinned_at:<unix seconds>" orders pins (oldest pin first).
 * Both are bookkeeping, never shown as user tags.
 */
object Pins {
    const val TAG = "pinned"
    const val AT_PREFIX = "pinned_at:"

    fun isReserved(tag: String): Boolean = tag == TAG || tag.startsWith(AT_PREFIX)

    fun isPinned(tags: List<String>): Boolean = TAG in tags
    fun isPinned(s: SessionView): Boolean = isPinned(s.session.tags)

    /** The pin time in unix seconds, or null when absent or unreadable. */
    fun pinnedAt(tags: List<String>): Long? =
        tags.firstOrNull { it.startsWith(AT_PREFIX) }?.removePrefix(AT_PREFIX)?.toLongOrNull()

    /** Tags with the pin set to [now] (any earlier pin tags replaced). */
    fun pin(tags: List<String>, now: Instant): List<String> =
        unpin(tags) + listOf(TAG, AT_PREFIX + now.epochSecond)

    /** Tags without any pin tags; other tags keep their order. */
    fun unpin(tags: List<String>): List<String> = tags.filterNot(::isReserved)

    fun toggle(tags: List<String>, now: Instant): List<String> = if (isPinned(tags)) unpin(tags) else pin(tags, now)

    /** Pinned sessions in pin order: by pin time, a missing time last, then by id (stable). */
    fun ordered(sessions: List<SessionView>): List<SessionView> =
        sessions.filter(::isPinned).sortedWith(
            compareBy<SessionView> { pinnedAt(it.session.tags) == null }
                .thenBy { pinnedAt(it.session.tags) ?: 0L }
                .thenBy { it.session.sessionId },
        )
}
