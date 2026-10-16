package dev.jhh3.pyre.domain

import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.model.AckState
import dev.jhh3.pyre.model.Ask
import dev.jhh3.pyre.model.AskState
import dev.jhh3.pyre.model.MessageView
import dev.jhh3.pyre.model.RunState
import dev.jhh3.pyre.model.Session
import dev.jhh3.pyre.model.SessionView
import java.time.Instant

/**
 * Session attention flags, as the TUI reads them (internal/tui/attention_flag.go).
 * Each session carries its own flag (SessionView.flag, computed by the
 * hub). Blocking reasons read "! needs you"; a session with only a message
 * for you reads "✉ message".
 */
object Flags {
    const val NEEDS_YOU = "needs_you"
    const val MESSAGE = "message"

    /** (level, reason): NEEDS_YOU, MESSAGE or "" . A hub without flags but with open items reads as needing you. */
    fun of(s: SessionView): Pair<String, String> {
        val f = s.flag
        if (f != null && f.level.isNotEmpty()) return f.level to f.reason
        if (s.attention > 0) return NEEDS_YOU to "attention"
        return "" to ""
    }

    /** Run states in which a session is live (the TUI's sessionLive). */
    private val LIVE = setOf(RunState.STARTING, RunState.RUNNING, RunState.WAITING_INPUT, RunState.WAITING_APPROVAL, RunState.WAITING_BACKGROUND)

    fun live(s: SessionView): Boolean = s.run?.state in LIVE

    /**
     * Flag reasons that only say how a run ended (a run/launch failure, an
     * exhausted account): nothing to act on once the session has ended.
     */
    fun endedOnly(reason: String): Boolean = reason == "failed" || reason == "reauth"

    /**
     * The one "needs you" rule (badge, summary line, Needs-you group, row
     * order), as the TUI's needsYou: not archived, and either a run waiting
     * on approval or a blocking flag the owner can still act on. A failure
     * or re-auth flag counts only while the session is live; on an ended
     * session the row still shows it ([label]) but nothing counts it.
     */
    fun needsYou(s: SessionView): Boolean {
        if (s.session.archived) return false
        if (s.run?.state == RunState.WAITING_APPROVAL) return true
        val (level, reason) = of(s)
        return level == NEEDS_YOU && (live(s) || !endedOnly(reason))
    }

    /** A blocking flag that no longer counts: an ended session's failure or re-auth. */
    fun history(s: SessionView): Boolean = of(s).first == NEEDS_YOU && !needsYou(s)

    fun messages(s: SessionView): Int = s.flag?.messages ?: 0

    /** Sort rank: needs you first, then a message for you, then the rest. */
    fun rank(s: SessionView): Int = when {
        needsYou(s) -> 0
        of(s).first == MESSAGE -> 1
        else -> 2
    }

    /**
     * "! needs you · approval" / "✉ message", "✗ failed" / "⊘ re-auth" for
     * an ended session's failure (shown, not counted), or "" when nothing
     * is flagged.
     */
    fun label(s: SessionView): String {
        val (level, reason) = of(s)
        return when {
            level == NEEDS_YOU && history(s) -> if (reason == "reauth") "⊘ re-auth" else "✗ failed"
            level == NEEDS_YOU -> "! needs you" + if (reason.isNotEmpty() && reason != "attention") " · $reason" else ""
            level == MESSAGE -> "✉ message"
            else -> ""
        }
    }

    /**
     * The newest unacknowledged message [s] sent the owner, from the owner's
     * listed inbox, while its flag still says there is one.
     */
    fun messageFor(s: SessionView, ownerInbox: List<MessageView>): MessageView? {
        if (messages(s) == 0) return null
        return ownerInbox
            .filter { m ->
                val sender = m.envelope.sender
                sender != null && sender.kind == "session" && sender.id == s.session.sessionId &&
                    m.status.ack != AckState.ACKNOWLEDGED && m.status.ack != AckState.REPLIED
            }
            .maxByOrNull { it.envelope.createdAt }
    }
}

/** A lead's one signal, the highest wins (the TUI's leads.go). */
enum class LeadKind { QUIET, WORKING, NEW, WAITING, ASKS }

/**
 * A lead's signal over itself and its descendants: [asks] open asks (the
 * [soonest] deadline first), [waiting] sessions that need you, [working]
 * busy ones, [newAt] the latest finish after the owner last looked.
 */
data class LeadSignal(
    val kind: LeadKind = LeadKind.QUIET,
    val asks: Int = 0,
    val soonest: Ask? = null,
    val waiting: Int = 0,
    val working: Int = 0,
    val newAt: Instant? = null,
) {
    /** "● asks 2", "◆ waiting", "• new", "◌ working"; "" when quiet. */
    val chip: String
        get() = when (kind) {
            LeadKind.ASKS -> "● asks $asks"
            LeadKind.WAITING -> "◆ waiting"
            LeadKind.NEW -> "• new"
            LeadKind.WORKING -> "◌ working"
            LeadKind.QUIET -> ""
        }
}

/**
 * Lead signals (owner request 2026-10-14: "I should know when a project
 * lead is worth clicking on"), as the TUI computes them (internal/tui/leads.go).
 * A lead is a top-level session (no listed, unarchived parent).
 *
 * "Last looked" is the latest of the session's ownerActiveAt, when this app
 * last opened it ([seen]) and [since] (the first launch with lead signals,
 * so nothing older reads new). A turn's end is the run's last report time
 * while idle (waiting_input; the hub keeps no "state since", and a node
 * reports a run on state changes), or its end time.
 */
object Leads {
    /** The session that started or adopted [s] ("" at the top), as contracts.Session.Parent. */
    fun parent(s: Session): String {
        val p = s.lineage.parentSessionId
        if (p.isNotEmpty() && p != s.sessionId) return p
        val c = s.createdBy
        if (s.lineage.reparentedAt == null && c != null && c.kind == "session" && c.id.isNotEmpty() && c.id != s.sessionId) return c.id
        return ""
    }

    /** Each unarchived session's lead: its topmost listed, unarchived ancestor (itself when none). */
    fun roots(sessions: List<SessionView>): Map<String, String> {
        val byId = sessions.associateBy { it.session.sessionId }
        val out = HashMap<String, String>()
        for (s in sessions) {
            if (s.session.archived) continue
            var id = s.session.sessionId
            val seen = hashSetOf(id)
            var p = parent(s.session)
            while (p.isNotEmpty() && seen.add(p) && seen.size < 64) {
                val a = byId[p] ?: break
                if (a.session.archived) break
                id = a.session.sessionId
                p = parent(a.session)
            }
            out[s.session.sessionId] = id
        }
        return out
    }

    private val BUSY = setOf(RunState.RUNNING, RunState.STARTING, RunState.WAITING_BACKGROUND)

    /** The run is busy (and fresh). */
    fun working(s: SessionView): Boolean {
        val r = s.run ?: return false
        return !s.session.archived && !r.freshness.stale && !s.freshness.stale && r.state in BUSY && s.session.harness != "shell"
    }

    /** When [s]'s run last finished a turn or ended; null while it works or when unknown. */
    fun finishedAt(s: SessionView): Instant? {
        val r = s.run ?: return null
        if (r.freshness.stale) return null
        return when (r.state) {
            RunState.WAITING_INPUT -> Times.parse(r.freshness.observedAt)
            RunState.EXITED, RunState.FAILED -> Times.parse(r.endedAt)
            else -> null
        }
    }

    /** When the owner last looked at [s]. */
    fun lookedAt(s: SessionView, seen: Map<String, Instant>, since: Instant?): Instant? =
        listOfNotNull(since, seen[s.session.sessionId], Times.parse(s.session.ownerActiveAt)).maxOrNull()

    /** Every lead's signal (keyed by the lead's session id). */
    fun signals(sessions: List<SessionView>, asks: List<Ask>, seen: Map<String, Instant> = emptyMap(), since: Instant? = null): Map<String, LeadSignal> {
        val roots = roots(sessions)
        val byId = sessions.associateBy { it.session.sessionId }
        val acc = LinkedHashMap<String, Acc>()
        for (s in sessions) {
            val lead = roots[s.session.sessionId] ?: continue
            val a = acc.getOrPut(lead) { Acc() }
            a.members += s
            if (Flags.needsYou(s)) a.waiting++ else if (working(s)) a.working++
        }
        for (k in asks) {
            if (k.state != AskState.OPEN || k.sessionId.isEmpty()) continue
            val a = acc[roots[k.sessionId] ?: continue] ?: continue
            a.asks++
            val cur = a.soonest
            val d = Times.parse(k.deadlineAt)
            if (cur == null || (d != null && (Times.parse(cur.deadlineAt)?.let { d.isBefore(it) } ?: true))) a.soonest = k
        }
        val out = LinkedHashMap<String, LeadSignal>()
        for ((lead, a) in acc) {
            val l = byId[lead]
            if (l != null && !working(l)) {
                val looked = lookedAt(l, seen, since)
                for (m in a.members) {
                    val lk = if (m === l) looked else listOfNotNull(looked, lookedAt(m, seen, since)).maxOrNull()
                    a.newer(finishedAt(m), lk)
                }
                val fl = l.flag
                if (fl != null && fl.level == Flags.MESSAGE) a.newer(Times.parse(fl.since), looked)
            }
            val kind = when {
                a.asks > 0 -> LeadKind.ASKS
                a.waiting > 0 -> LeadKind.WAITING
                a.newAt != null -> LeadKind.NEW
                a.working > 0 -> LeadKind.WORKING
                else -> LeadKind.QUIET
            }
            out[lead] = LeadSignal(kind, a.asks, a.soonest, a.waiting, a.working, a.newAt)
        }
        return out
    }

    /** The Sessions badge: leads that ask or wait on you ("new" does not count). */
    fun badge(signals: Map<String, LeadSignal>): Int = signals.values.count { it.kind >= LeadKind.WAITING }

    /** A project's leads, worst first: "● 2 asks · ◆ 1 waiting · • 1 new · ◌ 1 working"; "" when all quiet. */
    fun summary(leads: List<LeadSignal>): String {
        val asks = leads.filter { it.kind == LeadKind.ASKS }.sumOf { it.asks }
        val waiting = leads.count { it.kind == LeadKind.WAITING }
        val fresh = leads.count { it.kind == LeadKind.NEW }
        val working = leads.count { it.kind == LeadKind.WORKING }
        return listOfNotNull(
            asks.takeIf { it > 0 }?.let { "● $it ask" + if (it == 1) "" else "s" },
            waiting.takeIf { it > 0 }?.let { "◆ $it waiting" },
            fresh.takeIf { it > 0 }?.let { "• $it new" },
            working.takeIf { it > 0 }?.let { "◌ $it working" },
        ).joinToString(" · ")
    }

    private class Acc {
        var asks = 0
        var soonest: Ask? = null
        var waiting = 0
        var working = 0
        var newAt: Instant? = null
        val members = ArrayList<SessionView>()

        /** Keeps [t] as the latest unseen finish when it is after [looked]. */
        fun newer(t: Instant?, looked: Instant?) {
            if (t == null || (looked != null && !t.isAfter(looked))) return
            val cur = newAt
            if (cur == null || t.isAfter(cur)) newAt = t
        }
    }

    /** The bound on remembered "opened" times. */
    const val SEEN_MAX = 300
}
