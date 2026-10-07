package dev.jhh3.pyre.ui

import dev.jhh3.pyre.data.InMemoryBlobStore
import dev.jhh3.pyre.data.LocalSettings
import dev.jhh3.pyre.domain.Status
import dev.jhh3.pyre.model.Freshness
import dev.jhh3.pyre.model.Run
import dev.jhh3.pyre.model.RunActivity
import dev.jhh3.pyre.model.RunState
import dev.jhh3.pyre.model.Session
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.ui.sessions.Grouping
import dev.jhh3.pyre.ui.sessions.SessionsViewModel
import java.time.Instant
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** List rows say what a session is doing in plain words; ended sessions collapse; choices persist. */
class SessionRowStatusTest {
    private val now = Instant.parse("2026-10-06T12:00:00Z")

    private fun view(id: String, state: String?, reason: String = "", activity: String = "", started: String = "2026-10-06T11:00:00Z", ended: String? = null) =
        SessionView(
            session = Session(sessionId = id, harness = "claude"),
            run = state?.let {
                Run(
                    runId = "run_$id", sessionId = id, nodeId = "node_synth", state = it, stateReason = reason,
                    freshness = Freshness(observedAt = "2026-10-06T11:57:00Z"), startedAt = started, endedAt = ended,
                    activity = activity.takeIf(String::isNotEmpty)?.let { a -> RunActivity(text = a) },
                )
            },
            nodeName = "synth-box",
        )

    @Test
    fun workingShowsTheDoingNowPhraseNeverTheRawReason() {
        val r = Status.row(view("a", RunState.RUNNING, reason = "tool finished: Bash", activity = "running tests"), null, now)
        assertEquals("Working", r.label)
        assertEquals("running tests", r.phrase)
        val idle = Status.row(view("b", RunState.WAITING_INPUT, reason = "idle at prompt"), null, now)
        assertEquals("Waiting for input", idle.label)
        assertEquals("3m ago", idle.age)
        assertFalse(idle.phrase.contains("idle at prompt"))
        assertFalse(Status.session(view("a", RunState.RUNNING, reason = "tool finished: Bash"), null, now).detail.contains("Bash"))
    }

    @Test
    fun failedIsAPlainOneLiner() {
        val raw = "spawn failed: account acct_synth-123 unavailable"
        val r = Status.row(view("f", RunState.FAILED, reason = raw, started = "2026-10-06T11:00:00Z", ended = "2026-10-06T11:00:10Z"), null, now)
        assertEquals("Couldn't start", r.label)
        assertEquals("open for details", r.phrase)
        assertFalse((r.label + r.phrase + r.age).contains("acct_"))
        // The session screen keeps the full reason.
        assertTrue(Status.session(view("f", RunState.FAILED, reason = raw), null, now).detail.contains(raw))
    }

    @Test
    fun endedSessionsAreHiddenUntilShown() {
        val list = listOf(view("live", RunState.RUNNING), view("done", RunState.EXITED), view("bad", RunState.FAILED), view("norun", null))
        val hidden = SessionsViewModel.group(list, emptyList(), grouping = Grouping.MACHINE, showEnded = false)
        assertEquals(listOf("live"), hidden.flatMap { it.sessions }.map { it.session.sessionId })
        assertEquals(3, SessionsViewModel.hiddenEnded(list))
        assertEquals(4, SessionsViewModel.group(list, emptyList(), showEnded = true).flatMap { it.sessions }.size)
    }

    @Test
    fun groupingAndEndedChoiceSurviveARestart() {
        val store = InMemoryBlobStore()
        LocalSettings(store).update { it.copy(sessionsGrouping = Grouping.MACHINE.name, showEndedSessions = true) }
        val again = LocalSettings(store).prefs.value
        assertEquals("MACHINE", again.sessionsGrouping)
        assertTrue(again.showEndedSessions)
    }
}
