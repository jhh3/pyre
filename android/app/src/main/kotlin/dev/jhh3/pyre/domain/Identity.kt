package dev.jhh3.pyre.domain

import dev.jhh3.pyre.model.Address
import dev.jhh3.pyre.model.Attachment
import dev.jhh3.pyre.model.MessageEnvelope
import dev.jhh3.pyre.model.Principal

/**
 * Sender / recipient / thread identity of a message, worded like the CLI
 * (`pyre msg show`) and TUI message detail so the three surfaces name the
 * same principal the same way: `<title> (session:<id>)`, `owner (<client>)`.
 * Session titles come from the caller's session list when known; the id is
 * always shown so identity never depends on a mutable title.
 */
object Identity {
    fun principal(p: Principal?, titles: Map<String, String> = emptyMap()): String {
        if (p == null) return "owner (this device, not yet stored)"
        val base = when (p.kind) {
            "owner" -> if (p.name.isNotEmpty()) "owner (${p.name})" else "owner"
            "session" -> titles[p.id]?.takeIf { it.isNotEmpty() }?.let { "$it (session:${p.id})" } ?: "session:${p.id}"
            else -> "${p.kind}:${p.id}" + if (p.name.isNotEmpty()) " (${p.name})" else ""
        }
        return base + (p.onBehalfOf?.let { " on behalf of " + principal(it, titles) } ?: "")
    }

    fun address(a: Address, titles: Map<String, String> = emptyMap()): String = when (a.kind) {
        "session" -> titles[a.id]?.takeIf { it.isNotEmpty() }?.let { "$it (session:${a.id})" } ?: "session:${a.id}"
        "owner" -> "owner inbox"
        else -> "${a.kind}:${a.id}"
    }

    /** "thread T · reply to M · reissues R", empty when the message has none. */
    fun threadLine(e: MessageEnvelope): String = listOfNotNull(
        e.threadId.takeIf { it.isNotEmpty() }?.let { "thread $it" },
        e.inReplyTo.takeIf { it.isNotEmpty() }?.let { "reply to $it" },
        e.reissues.takeIf { it.isNotEmpty() }?.let { "reissues $it" },
    ).joinToString(" · ")

    /** One attachment as text plus the deep link that opens it (null when the app cannot open it directly). */
    fun attachment(a: Attachment): Pair<String, String?> = when {
        a.artifactVersionId.isNotEmpty() -> "artifact version ${a.artifactVersionId}" to "pyre://artifact_version/${a.artifactVersionId}"
        a.documentRevisionId.isNotEmpty() -> "document revision ${a.documentRevisionId}" to null
        a.noteId.isNotEmpty() -> "note ${a.noteId}" to null
        else -> "empty attachment" to null
    }

    /** Whether [e] was sent by the session [sessionId] (outgoing from that session's point of view). */
    fun sentBy(e: MessageEnvelope, sessionId: String) = e.sender?.kind == "session" && e.sender.id == sessionId
}
