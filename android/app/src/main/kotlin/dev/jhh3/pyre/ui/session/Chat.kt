package dev.jhh3.pyre.ui.session

import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.model.TranscriptEntry
import java.time.Duration

/**
 * The transcript read as a chat, like the Claude Code and Codex apps: user
 * and agent bubbles, one quiet line per run of tool calls, delivered Pyre
 * messages as "Message from …" chips, faint timestamps only after long
 * gaps. Pure and unit-tested; the screen only draws these items.
 *
 * Classification follows the TUI reader (internal/tui/reader.go): Claude's
 * bare "tool result" placeholders carry nothing and are dropped,
 * consecutive tool calls read as one block, and `[pyre-msg:…]` envelopes
 * show who sent them instead of the runtime header.
 */
sealed interface ChatItem {
    /** Stable LazyColumn key: derived from the first entry's run and seq. */
    val key: String
}

data class Bubble(override val key: String, val mine: Boolean, val text: String, val truncated: Boolean, val at: String) : ChatItem

data class PyreMessageChip(override val key: String, val messageId: String, val from: String, val context: String, val body: String, val at: String) : ChatItem

data class ToolGroup(override val key: String, val calls: List<ToolCall>, val at: String) : ChatItem {
    val summary: String get() = Chat.summarize(calls)
    val failed: Boolean get() = calls.any { it.failed }
}

/** "Worked for 4m 2s" — the end of an agent turn, drawn as a faint divider. */
data class TurnEnd(override val key: String, val label: String) : ChatItem

/** A faint time label before an entry that follows a long gap. */
data class TimeMark(override val key: String, val at: String) : ChatItem

enum class ToolKind { SHELL, EDIT, READ, SEARCH, WEB, FETCH, AGENT, PLAN, MCP, OTHER }

/** One tool call: its kind, display name and argument (command, path, pattern…). */
data class ToolCall(val kind: ToolKind, val name: String, val arg: String, val failed: Boolean = false, val truncated: Boolean = false)

object Chat {
    /** Gap after which a faint timestamp is shown. */
    val LONG_GAP: Duration = Duration.ofMinutes(20)

    private val toolSplit = Regex("""; (?=(?:[A-Z][A-Za-z]+|mcp__[\w-]+)(?::\s|;|$))""")
    private val pyreMsg = Regex("""^\s*\[pyre-msg:([^\]]+)\]\s*(?:from (\w+):([^\s:()]+)(?:\s+\(([^)]*)\))?)?""")
    private val exitCode = Regex("""\s*\(exit (-?\d+)\)$""")
    private val turnEnded = Regex("""^turn ended \((.+)\)$""")

    /**
     * Turns transcript entries (oldest first) into chat items. [names]
     * resolves session ids to titles for message senders.
     */
    fun items(entries: List<TranscriptEntry>, names: Map<String, String> = emptyMap()): List<ChatItem> {
        val out = ArrayList<ChatItem>()
        var group: MutableList<ToolCall>? = null
        var groupKey = ""
        var groupAt = ""
        var lastAt: java.time.Instant? = null
        fun flush() {
            group?.let { if (it.isNotEmpty()) out.add(ToolGroup(groupKey, it.toList(), groupAt)) }
            group = null
        }
        for (e in entries) {
            val key = "${e.runId}_${e.seq}"
            if (hidden(e)) continue
            val at = Times.parse(e.at)
            val gap = at != null && (lastAt == null || Duration.between(lastAt, at) >= LONG_GAP)
            if (at != null) lastAt = at
            if (e.kind == "tool" || isSubagent(e)) {
                // A long pause inside a run of tools still starts a new group,
                // so the time label sits where the work resumed.
                if (group == null || gap) {
                    flush()
                    if (gap) out.add(TimeMark("t_$key", e.at))
                    group = ArrayList(); groupKey = "g_$key"; groupAt = e.at
                }
                group!!.addAll(toolCalls(e))
                continue
            }
            flush()
            if (gap) out.add(TimeMark("t_$key", e.at))
            when (e.kind) {
                "user", "pyre_message" -> {
                    val pm = parsePyreMessage(e.text, names)
                    if (pm != null) {
                        out.add(PyreMessageChip("e_$key", e.messageId.ifEmpty { pm.messageId }, pm.from, pm.context, pm.body, e.at))
                    } else {
                        out.add(Bubble("e_$key", mine = true, text = e.text.trim(), truncated = e.truncated, at = e.at))
                    }
                }
                "assistant" -> out.add(Bubble("e_$key", mine = false, text = e.text.trim(), truncated = e.truncated, at = e.at))
                "system" -> turnEnded.find(e.text.trim())?.let { out.add(TurnEnd("e_$key", "Worked for " + it.groupValues[1].let(::prettyDuration))) }
            }
        }
        flush()
        return out
    }

    /**
     * Entries that carry nothing for a reader: Claude's bare "tool result"
     * placeholders, empty text, terminal screens (the terminal is one tap
     * away) and system noise other than the end of a turn (hook notices,
     * meta prompts). Hidden entries never split a tool group.
     */
    fun hidden(e: TranscriptEntry): Boolean = when (e.kind) {
        "tool" -> e.text.trim().let { it.isEmpty() || it == "tool result" }
        "system" -> !(turnEnded.matches(e.text.trim()) || isSubagent(e))
        "screen" -> true
        else -> e.text.isBlank()
    }

    private fun isSubagent(e: TranscriptEntry) = e.kind == "system" && e.text.startsWith("subagent ")

    /** Parses one tool entry: Claude joins several calls with "; ", Codex writes "command: …" / "file change: …" / "mcp: …". */
    fun toolCalls(e: TranscriptEntry): List<ToolCall> {
        if (e.kind == "system") {
            // Codex: "subagent <kind>: <thread>" — shown as an agent call.
            return listOf(ToolCall(ToolKind.AGENT, "subagent", e.text.removePrefix("subagent ").substringBefore(':').trim()))
        }
        val text = e.text.trim()
        return text.split(toolSplit).map { part -> toolCall(part.trim(), e.truncated) }
    }

    private fun toolCall(s: String, truncated: Boolean): ToolCall {
        val name = s.substringBefore(": ", s).trim()
        val arg = if (s.contains(": ")) s.substringAfter(": ").trim() else ""
        return when (name) {
            "command", "Bash", "BashOutput" -> {
                val m = exitCode.find(arg)
                val failed = m != null && m.groupValues[1] != "0"
                ToolCall(ToolKind.SHELL, "Shell", if (m != null) arg.removeRange(m.range) else arg, failed, truncated)
            }
            "file change" -> ToolCall(ToolKind.EDIT, "Edit", arg, truncated = truncated)
            "Edit", "MultiEdit", "Write", "NotebookEdit" -> ToolCall(ToolKind.EDIT, name, arg, truncated = truncated)
            "Read", "NotebookRead" -> ToolCall(ToolKind.READ, name, arg, truncated = truncated)
            "Grep", "Glob", "LS", "ToolSearch" -> ToolCall(ToolKind.SEARCH, name, arg, truncated = truncated)
            "WebSearch" -> ToolCall(ToolKind.WEB, name, arg, truncated = truncated)
            "WebFetch" -> ToolCall(ToolKind.FETCH, name, arg, truncated = truncated)
            "Task", "Agent" -> ToolCall(ToolKind.AGENT, name, arg, truncated = truncated)
            "TodoWrite", "ExitPlanMode", "TaskCreate", "TaskUpdate" -> ToolCall(ToolKind.PLAN, name, arg, truncated = truncated)
            "mcp" -> {
                // Codex: "mcp: server.tool (status)"
                val status = Regex("""\s*\(([^)]*)\)$""").find(arg)
                val tool = (if (status != null) arg.removeRange(status.range) else arg).trim()
                ToolCall(ToolKind.MCP, tool.substringAfterLast('.'), tool, failed = status?.groupValues?.get(1)?.let { it == "failed" || it == "error" } == true, truncated = truncated)
            }
            else -> if (name.startsWith("mcp__")) {
                ToolCall(ToolKind.MCP, name.substringAfterLast("__"), arg.ifEmpty { name.removePrefix("mcp__").replace("__", ".") }, truncated = truncated)
            } else {
                ToolCall(ToolKind.OTHER, name, arg, truncated = truncated)
            }
        }
    }

    /** One quiet line for a run of calls: "Ran 4 shell commands, edited proxy.ts". */
    fun summarize(calls: List<ToolCall>): String {
        val byKind = LinkedHashMap<ToolKind, MutableList<ToolCall>>()
        calls.forEach { byKind.getOrPut(it.kind) { ArrayList() }.add(it) }
        val parts = byKind.map { (k, cs) -> phrase(k, cs) }
        return parts.mapIndexed { i, p -> if (i == 0) p.replaceFirstChar { it.uppercase() } else p }.joinToString(", ")
    }

    private fun phrase(k: ToolKind, cs: List<ToolCall>): String {
        val n = cs.size
        return when (k) {
            ToolKind.SHELL -> if (n == 1) "ran " + shortCommand(cs[0].arg).ifEmpty { "a shell command" } else "ran $n shell commands"
            ToolKind.EDIT -> {
                val files = cs.flatMap { it.arg.split(", ") }.map { it.trim() }.filter { it.isNotEmpty() }.distinct()
                when {
                    files.size == 1 -> "edited " + baseName(files[0])
                    files.isEmpty() -> "edited files"
                    else -> "edited ${files.size} files"
                }
            }
            ToolKind.READ -> {
                val files = cs.map { it.arg }.distinct()
                if (files.size == 1) "read " + baseName(files[0]).ifEmpty { "a file" } else "read ${files.size} files"
            }
            ToolKind.SEARCH -> if (n == 1) "searched the code" else "searched the code $n times"
            ToolKind.WEB -> if (n == 1) "searched the web" else "searched the web $n times"
            ToolKind.FETCH -> if (n == 1) "fetched " + host(cs[0].arg) else "fetched $n pages"
            ToolKind.AGENT -> if (n == 1) "ran a subagent" else "ran $n subagents"
            ToolKind.PLAN -> "updated the plan"
            ToolKind.MCP -> cs.map { it.name }.distinct().let { if (it.size == 1) "used " + it[0] + if (n > 1) " ×$n" else "" else "used ${it.size} tools" }
            ToolKind.OTHER -> cs.map { it.name }.distinct().let { if (it.size == 1) "used " + it[0] + if (n > 1) " ×$n" else "" else "used ${it.size} tools" }
        }
    }

    /** First line of a command, at most 40 characters: "git status", "go test ./…". */
    fun shortCommand(cmd: String): String {
        val line = cmd.trim().lineSequence().firstOrNull().orEmpty().trim()
        return if (line.length > 40) line.take(39).trimEnd() + "…" else line
    }

    fun baseName(path: String): String = path.trim().trimEnd('/').substringAfterLast('/')

    private fun host(url: String): String = url.substringAfter("://").substringBefore('/').ifEmpty { "a page" }

    /** "1m3.2s" / "45.1s" (Go durations) -> "1m 3s" / "45s". */
    fun prettyDuration(go: String): String {
        val m = Regex("""^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$""").find(go.trim()) ?: return go
        val (h, mi, s) = m.destructured
        val secs = s.toDoubleOrNull()?.toInt()
        return listOfNotNull(
            h.takeIf { it.isNotEmpty() }?.let { "${it}h" },
            mi.takeIf { it.isNotEmpty() }?.let { "${it}m" },
            secs?.takeIf { it > 0 || (h.isEmpty() && mi.isEmpty()) }?.let { "${it}s" },
        ).joinToString(" ").ifEmpty { go }
    }

    data class ParsedMessage(val messageId: String, val from: String, val context: String, val body: String)

    /**
     * A delivered message as it appears in a harness transcript:
     * "[pyre-msg:<id>] from <kind>:<id> (<name>) (reply to …):\n<body>".
     * Null when the text carries no marker.
     */
    fun parsePyreMessage(text: String, names: Map<String, String> = emptyMap()): ParsedMessage? {
        val m = pyreMsg.find(text) ?: return null
        val id = m.groupValues[1]
        val kind = m.groupValues[2]
        val pid = m.groupValues[3]
        val label = m.groupValues[4]
        val from = when (kind) {
            "" -> "a peer"
            "owner" -> "you"
            "session" -> names[pid]?.takeIf { it.isNotEmpty() } ?: label.takeIf { it.isNotEmpty() } ?: "session " + pid.takeLast(6)
            else -> label.ifEmpty { "$kind ${pid.takeLast(6)}" }
        }
        var header = text
        var body = text.substring(m.range.last + 1)
        val nl = if (kind.isEmpty()) -1 else text.indexOf(":\n")
        if (nl >= 0) {
            header = text.substring(0, nl); body = text.substring(nl + 2)
        } else if (kind.isNotEmpty()) {
            val i = text.indexOf("): ", m.range.last - 1)
            if (i >= 0) { header = text.substring(0, i + 1); body = text.substring(i + 3) }
        }
        val ctx = listOf("steering" to "steering", "reply to " to "reply", "reissue of " to "reissue", "ack requested" to "ack requested")
            .filter { header.contains(it.first) }.joinToString(", ") { it.second }
        return ParsedMessage(id, from, ctx, body.trim().removePrefix(":").trim())
    }
}
