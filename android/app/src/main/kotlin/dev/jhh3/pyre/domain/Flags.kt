package dev.jhh3.pyre.domain

import dev.jhh3.pyre.model.AckState
import dev.jhh3.pyre.model.MessageView
import dev.jhh3.pyre.model.SessionView

/**
 * Session attention flags, as the TUI reads them (internal/tui/attention_flag.go).
 * There is no Inbox: each session carries its own flag (SessionView.flag,
 * computed by the hub). Blocking reasons read "! needs you"; a session with
 * only a message for you reads "✉ message".
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

    fun needsYou(s: SessionView): Boolean = of(s).first == NEEDS_YOU

    fun messages(s: SessionView): Int = s.flag?.messages ?: 0

    /** Sort rank: needs you first, then a message for you, then the rest. */
    fun rank(s: SessionView): Int = when (of(s).first) {
        NEEDS_YOU -> 0
        MESSAGE -> 1
        else -> 2
    }

    /** "! needs you · approval" / "✉ message", or "" when nothing is flagged. */
    fun label(s: SessionView): String {
        val (level, reason) = of(s)
        return when (level) {
            NEEDS_YOU -> "! needs you" + if (reason.isNotEmpty() && reason != "attention") " · $reason" else ""
            MESSAGE -> "✉ message"
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
