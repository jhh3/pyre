package dev.jhh3.pyre.domain

import dev.jhh3.pyre.testing.Fixtures
import java.time.Instant
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class PinsTest {
    private val t = Instant.ofEpochSecond(1_790_000_000)

    @Test
    fun pinAddsReservedTagsAndKeepsUserTags() {
        val tags = Pins.pin(listOf("title:auto", "synth-user"), t)
        assertEquals(listOf("title:auto", "synth-user", "pinned", "pinned_at:1790000000"), tags)
        assertTrue(Pins.isPinned(tags))
        assertEquals(1_790_000_000L, Pins.pinnedAt(tags))
    }

    @Test
    fun repinReplacesTheOldTimeAndUnpinRemovesBoth() {
        val once = Pins.pin(listOf("synth-user"), t)
        val again = Pins.pin(once, t.plusSeconds(5))
        assertEquals(listOf("synth-user", "pinned", "pinned_at:1790000005"), again)
        assertEquals(listOf("synth-user"), Pins.unpin(again))
        assertEquals(listOf("synth-user"), Pins.toggle(again, t))
        assertFalse(Pins.isPinned(Pins.unpin(again)))
        assertTrue(Pins.isReserved("pinned") && Pins.isReserved("pinned_at:1") && !Pins.isReserved("pin"))
    }

    @Test
    fun orderedByPinTimeWithMissingTimeLast() {
        val base = Fixtures.bundle("03").sessions.first()
        fun s(id: String, vararg tags: String) = base.copy(session = base.session.copy(sessionId = id, tags = tags.toList()))
        val list = listOf(
            s("ses_synth-none"),
            s("ses_synth-late", "pinned", "pinned_at:300"),
            s("ses_synth-notime", "pinned"),
            s("ses_synth-early", "pinned", "pinned_at:100"),
        )
        assertEquals(listOf("ses_synth-early", "ses_synth-late", "ses_synth-notime"), Pins.ordered(list).map { it.session.sessionId })
    }
}
