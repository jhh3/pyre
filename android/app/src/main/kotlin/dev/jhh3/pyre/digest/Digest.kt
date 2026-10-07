package dev.jhh3.pyre.digest

import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.model.AttentionItem
import dev.jhh3.pyre.model.AttentionState
import dev.jhh3.pyre.model.Event
import dev.jhh3.pyre.model.ReadSessionResponse
import dev.jhh3.pyre.model.RunState
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.model.TranscriptEntry
import java.net.URLEncoder
import java.time.Instant
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.longOrNull

/*
 * Kotlin port of internal/digest (digest.go, extract.go): the "while you were
 * away" digest, computed client-side from existing hub reads. Build is a pure
 * function of its Input, so it produces the same ranking, summaries and deep
 * links as the CLI/TUI from the same data; tests/fixtures/digest is the shared
 * fixture both test suites check against. No model is ever called.
 */

object Rank {
    const val NEEDS_YOU = "needs_you"
    const val DONE = "done"
    const val WORKING = "working"
    const val QUIET = "quiet"

    fun order(r: String) = when (r) {
        NEEDS_YOU -> 0
        DONE -> 1
        WORKING -> 2
        else -> 3
    }
}

/** One object a line points at, with its deep link. */
data class DigestRef(
    val kind: String,
    val id: String,
    val title: String,
    val link: String,
    val at: Instant?,
    /** Messages: "sent" (by this agent) or "received". */
    val direction: String = "",
)

/** One session's line in the digest. */
data class DigestAgent(
    val sessionId: String,
    val title: String,
    val harness: String = "",
    val node: String = "",
    val projectId: String? = null,
    val runId: String = "",
    val state: String = "",
    val archived: Boolean = false,
    val rank: String = Rank.QUIET,
    val summary: String = "",
    val summarySource: String = "",
    val blocked: String = "",
    val link: String = "",
    val seq: Long = 0,
    val turns: Int = 0,
    val replies: Int = 0,
    val tools: Int = 0,
    val partial: Boolean = false,
    val files: List<String> = emptyList(),
    val needsYou: List<DigestRef> = emptyList(),
    val artifacts: List<DigestRef> = emptyList(),
    val docs: List<DigestRef> = emptyList(),
    val messages: List<DigestRef> = emptyList(),
    val notes: List<DigestRef> = emptyList(),
    val events: Int = 0,
    val lastActivity: Instant? = null,
) {
    /** ! needs you, ✗ blocked, ✓ done, ● working. */
    val glyph: String
        get() = when {
            rank == Rank.NEEDS_YOU && needsYou.isEmpty() -> "✗"
            rank == Rank.NEEDS_YOU -> "!"
            rank == Rank.DONE -> "✓"
            rank == Rank.WORKING -> "●"
            else -> "·"
        }

    val word: String
        get() = when {
            rank == Rank.NEEDS_YOU && needsYou.isEmpty() -> "blocked"
            rank == Rank.NEEDS_YOU -> "needs you"
            rank == Rank.DONE -> "done"
            rank == Rank.WORKING -> "working"
            else -> "quiet"
        }

    /** "Claude on server". */
    val where: String
        get() {
            val h = titleWord(harness)
            return when {
                h.isNotEmpty() && node.isNotEmpty() -> "$h on $node"
                h.isNotEmpty() -> h
                else -> node
            }
        }

    /** "3 turns · 14 tools · 6 files · 1 artifact"; partial counts carry a "+". */
    val facts: String
        get() {
            val out = ArrayList<String>()
            fun add(n: Int, one: String, many: String) {
                if (n <= 0) return
                var s = plural(n, one, many)
                if (partial && (one == "turn" || one == "tool")) s = if (n == 1) "1+ $one" else "$n+ $many"
                out += s
            }
            add(turns, "turn", "turns")
            add(tools, "tool", "tools")
            add(files.size, "file", "files")
            add(artifacts.size, "artifact", "artifacts")
            add(docs.size, "doc", "docs")
            add(messages.size, "message", "messages")
            add(notes.size, "note", "notes")
            return out.joinToString(" · ")
        }
}

data class Digest(
    val since: Instant,
    val sinceCursor: Long,
    val headCursor: Long,
    val generatedAt: Instant,
    val sinceLabel: String = "",
    val truncated: Boolean = false,
    val agents: List<DigestAgent> = emptyList(),
    val quiet: Int = 0,
    val other: List<DigestRef> = emptyList(),
    val warnings: List<String> = emptyList(),
) {
    val isEmpty: Boolean get() = agents.isEmpty() && other.isEmpty()

    fun count(rank: String) = agents.count { it.rank == rank }
}

data class DigestInput(
    val since: Instant,
    val sinceCursor: Long = 0,
    val headCursor: Long = 0,
    val now: Instant,
    val sinceLabel: String = "",
    val truncated: Boolean = false,
    val events: List<Event> = emptyList(),
    val sessions: List<SessionView> = emptyList(),
    val attention: List<AttentionItem> = emptyList(),
    val transcripts: Map<String, ReadSessionResponse> = emptyMap(),
    val warnings: List<String> = emptyList(),
)

object DigestBuilder {
    /** Event kinds the digest reads (observational noise is left out at the source). */
    val EVENT_KINDS = listOf(
        "session.created", "session.updated", "run.started", "run.observed", "run.ended",
        "message.stored", "approval.requested", "artifact.published", "document.revised",
        "document.conflict", "comment.created", "note.created", "attention.changed", "account.exhausted",
    )

    /** Deep link to a transcript moment; seq 0 links to the session itself. */
    fun sessionLink(sessionId: String, runId: String, seq: Long): String {
        val base = "pyre://session/$sessionId"
        if (seq <= 0) return base
        val q = buildList {
            if (runId.isNotEmpty()) add("run=" + URLEncoder.encode(runId, "UTF-8"))
            add("seq=$seq")
        }
        return base + "?" + q.joinToString("&")
    }

    private fun deepLink(kind: String, id: String) = "pyre://$kind/$id"

    private class Acc(var a: DigestAgent, val sv: SessionView?) {
        var startedNew = false
        var endedState: String? = null
        var endedExit: Int? = null
        var exhausted = ""
        var failed = ""
        val files = LinkedHashSet<String>()
        var lastAsst: TranscriptEntry? = null
        var activity = false
        val needsYou = ArrayList<DigestRef>()
        val artifacts = ArrayList<DigestRef>()
        val docs = ArrayList<DigestRef>()
        val messages = ArrayList<DigestRef>()
        val notes = ArrayList<DigestRef>()
        var events = 0
        var turns = 0
        var replies = 0
        var tools = 0
        var partial = false
        var last: Instant? = null

        fun touch(t: Instant?) {
            if (t != null && (last == null || t.isAfter(last))) last = t
        }
    }

    fun build(input: DigestInput): Digest {
        val since = input.since
        val svs = input.sessions.associateBy { it.session.sessionId }
        val byId = LinkedHashMap<String, Acc>()
        val other = ArrayList<DigestRef>()

        fun get(sid: String?): Acc? {
            if (sid.isNullOrEmpty()) return null
            byId[sid]?.let { return it }
            val sv = svs[sid]
            val a = if (sv == null) DigestAgent(sessionId = sid, title = sid) else DigestAgent(
                sessionId = sid, title = sv.session.title, harness = sv.session.harness, node = sv.nodeName,
                projectId = sv.session.projectId, archived = sv.session.archived,
                runId = sv.run?.runId ?: "", state = sv.run?.state ?: "",
            )
            return Acc(a, sv).also { byId[sid] = it }
        }

        // 1. Events after the cutoff.
        for (ev in input.events) {
            if (input.sinceCursor > 0 && ev.cursor <= input.sinceCursor) continue
            val at = Times.parse(ev.occurredAt) ?: Times.parse(ev.ingestedAt)
            val p = ev.payload as? JsonObject
            when (ev.kind) {
                "run.started", "run.observed", "run.ended" -> {
                    p ?: continue
                    val x = get(first(p.str("session_id"), ev.sessionId)) ?: continue
                    x.events++
                    val state = p.str("state")
                    when {
                        ev.kind == "run.started" -> { x.startedNew = true; x.activity = true; x.touch(at) }
                        state == RunState.EXITED || state == RunState.FAILED -> {
                            x.endedState = state
                            x.endedExit = p.int("exit_code")
                            x.activity = true
                            if (state == RunState.FAILED) x.failed = failReason(p.str("state_reason"), p.int("exit_code"))
                            x.touch(at)
                        }
                    }
                }
                "session.created", "session.updated" -> get(ev.sessionId)?.let { it.events++ }
                "message.stored" -> {
                    val e = p?.obj("envelope") ?: continue
                    val id = e.str("message_id")
                    val sender = e.obj("sender")
                    val rcpt = e.obj("recipient")
                    val ref = DigestRef("message", id, firstSentence(e.obj("body")?.str("text") ?: "", 90), deepLink("message", id), at)
                    if (sender?.str("kind") == "session") {
                        get(sender.str("id"))?.let { x ->
                            x.messages += ref.copy(direction = "sent"); x.events++; x.activity = true; x.touch(at)
                        }
                    }
                    if (rcpt?.str("kind") == "session" && rcpt.str("id") != (sender?.str("id") ?: "")) {
                        get(rcpt.str("id"))?.let { x -> x.messages += ref.copy(direction = "received"); x.events++ }
                    }
                }
                "artifact.published" -> {
                    p ?: continue
                    val art = p.obj("artifact")
                    val ver = p.obj("version")
                    val sid = first(ver?.obj("provenance")?.str("session_id"), art?.str("session_id"), ev.sessionId, sessionOf(ver?.obj("author")))
                    val x = get(sid) ?: continue
                    val seq = ver?.long("seq") ?: 0
                    var title = art?.str("title") ?: ""
                    if (seq > 1) title += " v$seq"
                    val vid = ver?.str("version_id") ?: ""
                    x.artifacts += DigestRef("artifact_version", vid, title, deepLink("artifact_version", vid), at)
                    x.events++; x.activity = true; x.touch(at)
                }
                "document.revised", "document.conflict" -> {
                    p ?: continue
                    var sid = sessionOf(p.obj("author"))
                    val prov = (p["provenance"] ?: p["Provenance"]) as? JsonObject
                    prov?.str("session_id")?.takeIf { it.isNotEmpty() }?.let { sid = it }
                    val x = get(first(sid, ev.sessionId)) ?: continue
                    val rev = p.str("revision_id")
                    val doc = p.str("document_id")
                    var ref = DigestRef("document_revision", rev, first(p.str("summary"), doc), deepLink("document_revision", rev), at)
                    if (ev.kind == "document.conflict") {
                        val pid = p.str("proposal_id")
                        ref = ref.copy(kind = "document_proposal", id = pid, title = "conflicting edit to $doc", link = deepLink("document_proposal", pid))
                    }
                    x.docs += ref
                    x.events++; x.activity = true; x.touch(at)
                }
                "note.created" -> {
                    p ?: continue
                    val x = get(first(p.str("session_id"), p.obj("provenance")?.str("session_id"), ev.sessionId, sessionOf(p.obj("author")))) ?: continue
                    val id = p.str("note_id")
                    x.notes += DigestRef("note", id, p.str("kind") + ": " + p.str("title"), deepLink("note", id), at)
                    x.events++; x.activity = true; x.touch(at)
                }
                "comment.created" -> {
                    get(sessionOf(p?.obj("author")))?.let { x -> x.events++; x.activity = true; x.touch(at) }
                }
                "account.exhausted" -> {
                    get(first(p?.str("session_id"), ev.sessionId))?.let { x ->
                        x.events++
                        val provider = p?.str("provider") ?: ""
                        x.exhausted = if (provider.isNotEmpty()) "all ${titleWord(provider)} accounts exhausted" else "accounts exhausted"
                        x.activity = true
                        x.touch(at)
                    }
                }
                else -> get(ev.sessionId)?.let { it.events++ }
            }
        }

        // 2. Open attention updated after the cutoff.
        for (it in input.attention) {
            val upd = Times.parse(it.updatedAt)
            if (it.state != AttentionState.OPEN || upd == null || !upd.isAfter(since)) continue
            val link = it.deepLink.ifEmpty { deepLink("attention", it.attentionId) }
            val ref = DigestRef("attention", it.attentionId, it.title, link, upd)
            val x = get(it.sessionId)
            if (x == null) {
                other += ref
                continue
            }
            x.needsYou += ref
            x.activity = true
            x.touch(upd)
        }

        // 3. Transcripts: turns, tools, files, the last agent message.
        for ((sid, tr) in input.transcripts) {
            var x = byId[sid]
            if (x == null) {
                if (!hasEntriesAfter(tr, since)) continue
                x = get(sid)!!
            }
            var before = false
            for (e in tr.entries) {
                val at = Times.parse(e.at)
                if (at == null || !at.isAfter(since)) {
                    before = true
                    continue
                }
                when (e.kind) {
                    "user", "pyre_message" -> x.turns++
                    "assistant" -> { x.replies++; x.lastAsst = e }
                    "tool" -> {
                        if (e.text == "tool result") continue
                        val (n, files) = toolCalls(e.text)
                        x.tools += n
                        files.forEach { f -> x.files += relTo(f, cwdOf(x.sv)) }
                    }
                    else -> continue
                }
                x.activity = true
                x.touch(at)
            }
            if ((!before && tr.entries.isNotEmpty()) || tr.gap) x.partial = true
        }

        // 4. Rank, summarize, link.
        var quiet = 0
        val agents = ArrayList<DigestAgent>()
        for (x in byId.values) {
            val a = finish(x)
            if (a.rank == Rank.QUIET) quiet++ else agents += a
        }
        val sorted = agents.sortedWith { a, b ->
            val ra = Rank.order(a.rank)
            val rb = Rank.order(b.rank)
            when {
                ra != rb -> ra.compareTo(rb)
                a.rank == Rank.NEEDS_YOU && a.needsYou.isNotEmpty() != b.needsYou.isNotEmpty() -> if (a.needsYou.isNotEmpty()) -1 else 1
                a.lastActivity != b.lastActivity -> cmpDesc(a.lastActivity, b.lastActivity)
                else -> a.sessionId.compareTo(b.sessionId)
            }
        }
        return Digest(
            since = since, sinceCursor = input.sinceCursor, headCursor = input.headCursor, generatedAt = input.now,
            sinceLabel = input.sinceLabel, truncated = input.truncated, agents = sorted, quiet = quiet,
            other = other.sortedWith { a, b -> cmpDesc(a.at, b.at) }, warnings = input.warnings,
        )
    }

    private fun cmpDesc(a: Instant?, b: Instant?): Int = when {
        a == b -> 0
        a == null -> 1
        b == null -> -1
        else -> b.compareTo(a)
    }

    private fun finish(x: Acc): DigestAgent {
        val state = x.a.state
        val terminal = state == RunState.EXITED || state == RunState.FAILED
        val needsYou = x.needsYou.sortedWith { a, b -> cmpDesc(a.at, b.at) }
        val blocked = when {
            x.exhausted.isNotEmpty() && (state.isEmpty() || terminal || state == RunState.WAITING_INPUT) -> x.exhausted
            state == RunState.FAILED -> first(x.failed, failReasonView(x.sv), "run failed")
            state == RunState.WAITING_APPROVAL && needsYou.isEmpty() -> "waiting on a prompt in its terminal"
            else -> ""
        }
        val meaningful = x.activity || blocked.isNotEmpty()
        val rank = when {
            needsYou.isNotEmpty() || blocked.isNotEmpty() -> Rank.NEEDS_YOU
            !meaningful -> Rank.QUIET
            x.a.archived -> Rank.QUIET
            state == RunState.RUNNING || state == RunState.STARTING || state == RunState.WAITING_BACKGROUND || state == RunState.EXITING -> Rank.WORKING
            state == RunState.UNKNOWN -> Rank.WORKING
            else -> Rank.DONE
        }
        var a = x.a.copy(
            rank = rank, blocked = blocked, files = x.files.sorted(), needsYou = needsYou,
            artifacts = x.artifacts.toList(), docs = x.docs.toList(), messages = x.messages.toList(), notes = x.notes.toList(),
            events = x.events, turns = x.turns, replies = x.replies, tools = x.tools, partial = x.partial, lastActivity = x.last,
        )
        if (rank == Rank.QUIET) return a
        var summary = ""
        var source = ""
        var link = sessionLink(a.sessionId, "", 0)
        var seq = 0L
        when {
            needsYou.isNotEmpty() -> { summary = needsYou[0].title; source = "attention"; link = needsYou[0].link }
            blocked.isNotEmpty() -> { summary = upperFirst(blocked); source = "blocked" }
        }
        x.lastAsst?.let { la ->
            if (summary.isEmpty()) { summary = firstSentence(la.text, 160); source = "assistant" }
            seq = la.seq
            if (source != "attention") link = sessionLink(a.sessionId, first(la.runId, a.runId), la.seq)
        }
        if (summary.isEmpty()) activitySummary(a).takeIf { it.isNotEmpty() }?.let { summary = it; source = "activity" }
        if (summary.isEmpty()) { summary = stateSummary(x); source = "state" }
        a = a.copy(summary = summary, summarySource = source, link = link, seq = seq)
        return a
    }

    private fun activitySummary(a: DigestAgent): String {
        if (a.artifacts.isNotEmpty()) {
            var s = "Published " + a.artifacts.last().title
            val n = a.artifacts.size - 1
            if (n > 0) s += " and " + plural(n, "more artifact", "more artifacts")
            return s
        }
        if (a.docs.isNotEmpty()) return "Revised " + a.docs.last().title
        if (a.notes.isNotEmpty()) return "Recorded " + a.notes.last().title
        val sent = a.messages.count { it.direction == "sent" }
        return if (sent > 0) "Sent " + plural(sent, "message", "messages") else ""
    }

    private fun stateSummary(x: Acc): String = when {
        x.endedState == RunState.EXITED -> if (x.endedExit != null && x.endedExit != 0) "Exited with code ${x.endedExit}" else "Finished"
        x.a.state == RunState.WAITING_INPUT -> "Idle at the prompt"
        x.startedNew -> "Started"
        x.a.state == RunState.RUNNING -> "Working"
        else -> "Activity"
    }

    private fun failReason(reasonIn: String, exit: Int?): String {
        val reason = reasonIn.trim().trimEnd { it in ":;,. " }
        return failReasonTrimmed(reason, exit)
    }

    private fun failReasonTrimmed(reason: String, exit: Int?): String = when {
        reason.isNotEmpty() -> "failed: $reason"
        exit != null -> "failed with exit code $exit"
        else -> "run failed"
    }

    private fun failReasonView(sv: SessionView?): String {
        val r = sv?.run ?: return ""
        if (r.state != RunState.FAILED) return ""
        return failReason(r.stateReason, r.exitCode)
    }

    private fun cwdOf(sv: SessionView?): String {
        val r = sv?.run ?: return ""
        return r.checkout?.path?.takeIf { it.isNotEmpty() } ?: r.cwd
    }

    internal fun relTo(p: String, baseIn: String): String {
        if (baseIn.isEmpty() || !p.startsWith("/")) return p
        val base = baseIn.trimEnd('/')
        return if (p.startsWith("$base/")) p.removePrefix("$base/") else p
    }

    private fun hasEntriesAfter(tr: ReadSessionResponse, since: Instant) = tr.entries.any { e ->
        val at = Times.parse(e.at)
        at != null && at.isAfter(since) && (e.kind == "assistant" || e.kind == "user" || e.kind == "tool")
    }

    private fun sessionOf(p: JsonObject?): String {
        if (p == null) return ""
        if (p.str("kind") == "session") return p.str("id")
        val o = p.obj("on_behalf_of")
        if (o?.str("kind") == "session") return o.str("id")
        return ""
    }

    // ---- extraction (extract.go) ----

    /**
     * First sentence of an agent message as plain text, at most [max] chars:
     * headings, bullets, emphasis and code fences dropped; cut at the first
     * sentence end.
     */
    fun firstSentence(text: String, max: Int): String {
        var inFence = false
        var line = ""
        var heading = ""
        for (l in text.split("\n")) {
            val t = l.trim()
            if (t.startsWith("```")) {
                inFence = !inFence
                continue
            }
            if (inFence || t.isEmpty() || isRule(t) || t.startsWith("|")) continue
            if (t.startsWith("#")) {
                if (heading.isEmpty()) heading = t
                continue
            }
            line = t
            break
        }
        if (line.isEmpty()) line = heading
        line = line.trimStart { it in "#>*-+• \t" }
        line = stripNumbering(line)
        line = line.replace("**", "").replace("__", "").replace("`", "")
        line = line.split(Regex("\\s+")).filter { it.isNotEmpty() }.joinToString(" ")
        if (line.isEmpty()) return ""
        val i = sentenceEnd(line)
        if (i > 0) line = line.substring(0, i)
        line = line.trimEnd { it in " :;," }
        return clip(line, max)
    }

    private fun isRule(t: String) = t.trim { it in "-=*_ " }.isEmpty() && t.length >= 3

    private fun stripNumbering(s: String): String {
        var i = 0
        while (i < s.length && s[i] in '0'..'9') i++
        if (i > 0 && i < s.length && (s[i] == '.' || s[i] == ')') && i + 1 < s.length && s[i + 1] == ' ') return s.substring(i + 1).trim()
        return s
    }

    private fun sentenceEnd(s: String): Int {
        for (i in s.indices) {
            val ch = s[i]
            if (ch != '.' && ch != '!' && ch != '?') continue
            if (i + 1 < s.length && s[i + 1] != ' ') continue
            if (ch == '.') {
                val w = lastWord(s.substring(0, i))
                if (w.lowercase() in setOf("e.g", "i.e", "etc", "vs", "mr", "dr", "cf", "no")) continue
                if (w.length == 1 && w[0].isUpperCase()) continue
            }
            if (i < 12 && i + 1 < s.length) continue
            return i + 1
        }
        return 0
    }

    private fun lastWord(s: String): String {
        val i = s.lastIndexOfAny(charArrayOf(' ', '('))
        return s.substring(i + 1)
    }

    internal fun clip(s: String, max: Int): String {
        if (max <= 0 || s.codePointCount(0, s.length) <= max) return s
        var cut = s.substring(0, s.offsetByCodePoints(0, max - 1))
        val i = cut.lastIndexOf(' ')
        if (i > max * 2 / 3) cut = cut.substring(0, i)
        return cut.trimEnd { it in " ,;:" } + "…"
    }

    /** Tool calls in a transcript tool entry, and the files they changed. */
    fun toolCalls(textIn: String): Pair<Int, List<String>> {
        val text = textIn.trim()
        if (text.isEmpty()) return 0 to emptyList()
        if (text.startsWith("file change: ")) {
            return 1 to text.removePrefix("file change: ").split(", ").map { it.trim() }.filter { it.isNotEmpty() }
        }
        if (text.startsWith("command: ") || text.startsWith("mcp: ")) return 1 to emptyList()
        val parts = text.split("; ")
        val files = ArrayList<String>()
        for (p in parts) {
            val idx = p.indexOf(": ")
            val name = if (idx >= 0) p.substring(0, idx) else p
            val arg = if (idx >= 0) p.substring(idx + 2) else ""
            if (name.trim() in setOf("Edit", "Write", "MultiEdit", "NotebookEdit", "apply_patch")) {
                val a = arg.trim()
                if (a.isNotEmpty() && '\n' !in a) files += a
            }
        }
        return parts.size to files
    }
}

internal fun plural(n: Int, one: String, many: String) = if (n == 1) "1 $one" else "$n $many"

internal fun titleWord(s: String) = if (s.isEmpty()) s else s.substring(0, 1).uppercase() + s.substring(1)

private fun upperFirst(s: String) = if (s.isEmpty()) s else s.substring(0, s.offsetByCodePoints(0, 1)).uppercase() + s.substring(s.offsetByCodePoints(0, 1))

private fun first(vararg ss: String?): String = ss.firstOrNull { !it.isNullOrEmpty() } ?: ""

private fun JsonObject.str(k: String): String = ((this[k] as? JsonPrimitive)?.takeIf { it.isString })?.contentOrNull ?: ""

private fun JsonObject.obj(k: String): JsonObject? = this[k] as? JsonObject

private fun JsonObject.long(k: String): Long? = (this[k] as? JsonPrimitive)?.longOrNull

private fun JsonObject.int(k: String): Int? = (this[k] as? JsonPrimitive)?.intOrNull
