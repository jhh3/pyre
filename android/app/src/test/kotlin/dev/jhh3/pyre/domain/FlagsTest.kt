package dev.jhh3.pyre.domain

import dev.jhh3.pyre.model.Run
import dev.jhh3.pyre.model.RunState
import dev.jhh3.pyre.model.Session
import dev.jhh3.pyre.model.SessionFlag
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.ui.sessions.Grouping
import dev.jhh3.pyre.ui.sessions.SessionsViewModel
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The Sessions badge counts sessions that need you now (the TUI's
 * needsYou): a failure or re-auth flag on an ended session is shown in its
 * row but not counted (seen live: 44 "needs you" from launches that never ran).
 */
class FlagsTest {
    private fun view(id: String, state: String?, reason: String? = null, archived: Boolean = false) = SessionView(
        session = Session(sessionId = id, harness = "codex", archived = archived),
        run = state?.let { Run(runId = "run_$id", sessionId = id, state = it) },
        attention = if (reason != null) 1 else 0,
        flag = reason?.let { SessionFlag(level = Flags.NEEDS_YOU, reason = it, count = 1) },
    )

    @Test
    fun endedFailuresDoNotNeedYou() {
        assertFalse(Flags.needsYou(view("a", RunState.EXITED, "failed")))
        assertFalse(Flags.needsYou(view("b", RunState.FAILED, "failed")))
        assertFalse(Flags.needsYou(view("c", null, "failed"))) // a launch that never ran
        assertFalse(Flags.needsYou(view("d", RunState.EXITED, "reauth")))
        assertFalse(Flags.needsYou(view("e", RunState.WAITING_INPUT, "approval", archived = true)))
        assertEquals("✗ failed", Flags.label(view("a", RunState.EXITED, "failed")))
        assertEquals("⊘ re-auth", Flags.label(view("d", RunState.EXITED, "reauth")))
        assertTrue(Flags.history(view("a", RunState.EXITED, "failed")))
    }

    @Test
    fun liveOrActionableSessionsNeedYou() {
        assertTrue(Flags.needsYou(view("a", RunState.WAITING_APPROVAL, "approval")))
        assertTrue(Flags.needsYou(view("b", RunState.WAITING_APPROVAL))) // waiting on a prompt, no flag yet
        assertTrue(Flags.needsYou(view("c", RunState.WAITING_INPUT, "failed"))) // live: the failure is current
        assertTrue(Flags.needsYou(view("d", RunState.RUNNING, "reauth")))
        assertTrue(Flags.needsYou(view("e", RunState.EXITED, "asked"))) // still asking you
        assertEquals("! needs you · approval", Flags.label(view("a", RunState.WAITING_APPROVAL, "approval")))
    }

    @Test
    fun badgeSummaryAndGroupAgree() {
        val sessions = listOf(
            view("ses_live", RunState.WAITING_APPROVAL, "approval"),
            view("ses_dead1", RunState.EXITED, "failed"),
            view("ses_dead2", RunState.FAILED, "failed"),
            view("ses_dead3", null, "reauth"),
        )
        assertEquals(1, sessions.count(Flags::needsYou))
        val groups = SessionsViewModel.group(sessions, emptyList(), grouping = Grouping.NEEDS_YOU, showEnded = true)
        assertEquals("Needs you", groups.first().title)
        assertEquals(listOf("ses_live"), groups.first().sessions.map { it.session.sessionId })
        // Ended failures are hidden with the other ended sessions until shown.
        val hidden = SessionsViewModel.group(sessions, emptyList(), grouping = Grouping.NEEDS_YOU, showEnded = false)
        assertEquals(listOf("ses_live"), hidden.flatMap { g -> g.sessions.map { it.session.sessionId } })
    }
}
