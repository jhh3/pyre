package dev.jhh3.pyre.ui.session

import dev.jhh3.pyre.model.TranscriptEntry
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** Grouping and classification of a synthetic transcript (Claude and Codex shapes). */
class ChatTest {
    private var seq = 0L
    private fun e(kind: String, text: String, at: String = "2026-10-06T10:00:00Z", messageId: String = "") =
        TranscriptEntry(seq = ++seq, runId = "run_synth", at = at, kind = kind, text = text, messageId = messageId)

    private val transcript = listOf(
        e("user", "Fix the **proxy** redirect"),
        e("assistant", "Looking at the proxy."),
        e("tool", "Bash: git status"),
        e("tool", "tool result"),
        e("tool", "Bash: rg -n redirect src; Read: /synth/src/proxy.ts"),
        e("tool", "tool result"),
        e("system", "[synth hook notice] something"), // noise: hidden, does not split the group
        e("tool", "Bash: go test ./... ; date"),
        e("tool", "Edit: /synth/src/proxy.ts"),
        e("tool", "Edit: /synth/src/proxy.ts"),
        e("assistant", "Done. Tests pass."),
        e("system", "turn ended (1m3.25s)"),
        e("pyre_message", "[pyre-msg:msg_synth1] from session:ses_synthpeer (peer) (reply to msg_synth0):\nCan you also check the **docs**?", messageId = "msg_synth1"),
        // Codex shapes, after a long gap
        e("tool", "command: make lint (exit 2)", at = "2026-10-06T11:30:00Z"),
        e("tool", "file change: a.go, b.go"),
        e("tool", "mcp: pyre.send_message (completed)"),
        e("system", "subagent explorer: thr_synth"),
        e("tool", "WebSearch"),
        e("screen", "terminal junk"),
    )

    @Test
    fun groupsAndClassifies() {
        val items = Chat.items(transcript, mapOf("ses_synthpeer" to "Synth peer"))
        val kinds = items.map { it::class.simpleName }
        assertEquals(
            listOf("TimeMark", "Bubble", "Bubble", "ToolGroup", "Bubble", "TurnEnd", "PyreMessageChip", "TimeMark", "ToolGroup"),
            kinds,
        )
        val user = items[1] as Bubble
        assertTrue(user.mine)
        assertEquals("Fix the **proxy** redirect", user.text)

        val g1 = items[3] as ToolGroup
        // "; date" inside a shell command is not split into a fake tool.
        assertEquals(listOf("Shell", "Shell", "Read", "Shell", "Edit", "Edit"), g1.calls.map { it.name })
        assertEquals("go test ./... ; date", g1.calls[3].arg)
        assertEquals("Ran 3 shell commands, read proxy.ts, edited proxy.ts", g1.summary)
        assertFalse(g1.failed)
        assertEquals("g_run_synth_3", g1.key)

        assertEquals("Worked for 1m 3s", (items[5] as TurnEnd).label)

        val chip = items[6] as PyreMessageChip
        assertEquals("Synth peer", chip.from)
        assertEquals("reply", chip.context)
        assertEquals("Can you also check the **docs**?", chip.body)
        assertEquals("msg_synth1", chip.messageId)

        val g2 = items[8] as ToolGroup
        assertTrue(g2.failed)
        assertEquals("Ran make lint, edited 2 files, used send_message, ran a subagent, searched the web", g2.summary)
    }

    @Test
    fun keysAreStableWhenEntriesAppend() {
        val before = Chat.items(transcript.take(9))
        val after = Chat.items(transcript)
        // The open tool group keeps its key while calls are appended to it.
        assertEquals(before.last().key, after[3].key)
        assertEquals(before.map { it.key }, after.take(before.size).map { it.key })
        assertEquals(after.size, after.map { it.key }.toSet().size)
    }

    @Test
    fun summaries() {
        fun calls(vararg s: String) = s.flatMap { Chat.toolCalls(TranscriptEntry(1, kind = "tool", text = it)) }
        assertEquals("Ran 4 shell commands", Chat.summarize(calls("Bash: a", "Bash: b", "Bash: c", "Bash: d")))
        assertEquals("Edited proxy.ts", Chat.summarize(calls("Edit: /x/proxy.ts", "Write: /x/proxy.ts")))
        assertEquals("Read 3 files", Chat.summarize(calls("Read: /a", "Read: /b", "Read: /c")))
        assertEquals("Searched the web", Chat.summarize(calls("WebSearch")))
        assertEquals("Fetched example.com", Chat.summarize(calls("WebFetch: https://example.com/a")))
        assertEquals("Used progress", Chat.summarize(calls("mcp__synth-mods__progress")))
        assertEquals("Updated the plan", Chat.summarize(calls("TodoWrite")))
    }

    @Test
    fun pyreMessageFromOwnerAndBare() {
        val owner = Chat.parsePyreMessage("[pyre-msg:msg_synth2] from owner:cli_synth (owner (phone)): please stop")!!
        assertEquals("you", owner.from)
        val bare = Chat.parsePyreMessage("[pyre-msg:msg_synth3] hello: there\nmore")!!
        assertEquals("hello: there\nmore", bare.body)
        assertEquals(null, Chat.parsePyreMessage("no marker here"))
        // A user prompt that is really a delivered message becomes a chip.
        assertTrue(Chat.items(listOf(e("user", "[pyre-msg:msg_synth4] from owner:cli_x:\nhi"))).filterIsInstance<PyreMessageChip>().size == 1)
    }

    @Test
    fun timestampsOnlyAfterLongGaps() {
        val items = Chat.items(
            listOf(
                e("user", "a", at = "2026-10-06T10:00:00Z"),
                e("assistant", "b", at = "2026-10-06T10:05:00Z"),
                e("user", "c", at = "2026-10-06T12:00:00Z"),
            ),
        )
        assertEquals(listOf("TimeMark", "Bubble", "Bubble", "TimeMark", "Bubble"), items.map { it::class.simpleName })
    }

    @Test
    fun durations() {
        assertEquals("45s", Chat.prettyDuration("45.1s"))
        assertEquals("1h 2m", Chat.prettyDuration("1h2m0s"))
        assertEquals("2m", Chat.prettyDuration("2m0s"))
    }
}
