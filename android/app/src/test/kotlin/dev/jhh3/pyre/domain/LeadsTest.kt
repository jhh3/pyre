package dev.jhh3.pyre.domain

import dev.jhh3.pyre.model.Ask
import dev.jhh3.pyre.model.AskState
import dev.jhh3.pyre.model.Freshness
import dev.jhh3.pyre.model.Lineage
import dev.jhh3.pyre.model.Run
import dev.jhh3.pyre.model.RunState
import dev.jhh3.pyre.model.Session
import dev.jhh3.pyre.model.SessionFlag
import dev.jhh3.pyre.model.SessionView
import java.time.Instant
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * Lead signals (owner request 2026-10-14), as the TUI computes them: one
 * signal per top-level session over its subtree, highest wins — asks N,
 * waiting, new (finished after you last looked), working — else quiet.
 */
class LeadsTest {
    private val t0 = Instant.parse("2026-10-14T21:00:00Z")

    private fun view(
        id: String,
        state: String,
        parent: String = "",
        observedAt: Instant = t0.minusSeconds(600),
        reason: String? = null,
        ownerActiveAt: Instant? = null,
    ) = SessionView(
        session = Session(sessionId = id, harness = "claude", lineage = Lineage(parentSessionId = parent), ownerActiveAt = ownerActiveAt?.toString()),
        run = Run(runId = "run_$id", sessionId = id, state = state, freshness = Freshness(observedAt = observedAt.toString())),
        attention = if (reason != null) 1 else 0,
        flag = reason?.let { SessionFlag(level = Flags.NEEDS_YOU, reason = it, count = 1) },
    )

    private fun ask(id: String, session: String, deadline: Instant) =
        Ask(askId = id, sessionId = session, title = "Synthetic $id", state = AskState.OPEN, deadlineAt = deadline.toString())

    private val sessions = listOf(
        view("ses_coord", RunState.RUNNING),
        view("ses_coord_child", RunState.RUNNING, parent = "ses_coord"),
        view("ses_pair", RunState.WAITING_INPUT),
        view("ses_pair_child", RunState.WAITING_INPUT, parent = "ses_pair", reason = "waiting"),
        view("ses_new", RunState.WAITING_INPUT, observedAt = t0.plusSeconds(60)),
        view("ses_scratch", RunState.WAITING_INPUT),
        view("ses_scratch_child", RunState.RUNNING, parent = "ses_scratch"),
        view("ses_quiet", RunState.WAITING_INPUT),
    )
    private val asks = listOf(
        ask("ask_1", "ses_coord", t0.plusSeconds(2700)),
        ask("ask_2", "ses_coord", t0.plusSeconds(600)),
    )

    @Test
    fun oneSignalPerLeadHighestWins() {
        val sig = Leads.signals(sessions, asks, since = t0)
        assertEquals(setOf("ses_coord", "ses_pair", "ses_new", "ses_scratch", "ses_quiet"), sig.keys)
        assertEquals(LeadKind.ASKS, sig.getValue("ses_coord").kind)
        assertEquals(2, sig.getValue("ses_coord").asks)
        assertEquals("ask_2", sig.getValue("ses_coord").soonest?.askId)
        assertEquals("● asks 2", sig.getValue("ses_coord").chip)
        assertEquals(LeadKind.WAITING, sig.getValue("ses_pair").kind)
        assertEquals(LeadKind.NEW, sig.getValue("ses_new").kind)
        assertEquals(LeadKind.WORKING, sig.getValue("ses_scratch").kind)
        assertEquals(LeadKind.QUIET, sig.getValue("ses_quiet").kind)
        assertEquals("", sig.getValue("ses_quiet").chip)
        assertNull(sig["ses_pair_child"]) // a child is not a lead
    }

    @Test
    fun badgeCountsLeadsThatAskOrWait() {
        val sig = Leads.signals(sessions, asks, since = t0)
        assertEquals(2, Leads.badge(sig)) // coordinator, pairing; not new, not working
        assertEquals("● 2 asks · ◆ 1 waiting · • 1 new · ◌ 1 working", Leads.summary(sig.values.toList()))
        assertEquals("", Leads.summary(listOf(sig.getValue("ses_quiet"))))
    }

    @Test
    fun newUntilLookedAt() {
        // Opened after the turn ended: seen.
        val opened = Leads.signals(sessions, asks, seen = mapOf("ses_new" to t0.plusSeconds(120)), since = t0)
        assertEquals(LeadKind.QUIET, opened.getValue("ses_new").kind)
        // Typed into after it ended (ownerActiveAt): seen.
        val typed = sessions.map { if (it.session.sessionId == "ses_new") view("ses_new", RunState.WAITING_INPUT, observedAt = t0.plusSeconds(60), ownerActiveAt = t0.plusSeconds(90)) else it }
        assertEquals(LeadKind.QUIET, Leads.signals(typed, asks, since = t0).getValue("ses_new").kind)
        // A finish before the baseline (first launch) is never new.
        assertEquals(LeadKind.QUIET, Leads.signals(sessions, asks, since = t0.plusSeconds(300)).getValue("ses_new").kind)
        // A child finishing after you looked at the lead makes the lead new.
        val child = sessions + view("ses_quiet_child", RunState.WAITING_INPUT, parent = "ses_quiet", observedAt = t0.plusSeconds(30))
        assertEquals(LeadKind.NEW, Leads.signals(child, asks, since = t0).getValue("ses_quiet").kind)
    }

    @Test
    fun parentFallsBackToTheCreatingSessionUnlessReparented() {
        val s = Session(sessionId = "ses_x", createdBy = dev.jhh3.pyre.model.Principal(kind = "session", id = "ses_y"))
        assertEquals("ses_y", Leads.parent(s))
        assertEquals("", Leads.parent(s.copy(lineage = Lineage(reparentedAt = t0.toString()))))
        assertFalse(Leads.roots(listOf(view("ses_a", RunState.RUNNING, parent = "ses_a"))).isEmpty()) // a self-parent is its own lead
    }
}
