package dev.jhh3.pyre.digest

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.model.AttentionItem
import dev.jhh3.pyre.model.Event
import dev.jhh3.pyre.model.ReadSessionResponse
import dev.jhh3.pyre.model.SessionView
import java.io.File
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.builtins.MapSerializer
import kotlinx.serialization.builtins.serializer
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.longOrNull
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The Kotlin port must produce the same digest as internal/digest from the
 * shared fixture (tests/fixtures/digest/{input,expected}.json, written by
 * `PYRE_UPDATE_GOLDEN=1 go test ./internal/digest`).
 */
class DigestBuilderTest {
    private val dir = File(System.getProperty("pyre.digest.fixtures.dir") ?: "../../tests/fixtures/digest")
    private val json = PyreJson.wire

    private fun input(): DigestInput {
        val o = json.parseToJsonElement(File(dir, "input.json").readText()).jsonObject
        fun s(k: String) = (o[k] as JsonPrimitive).contentOrNull ?: ""
        return DigestInput(
            since = Times.parse(s("since"))!!,
            sinceCursor = (o["since_cursor"] as JsonPrimitive).longOrNull ?: 0,
            headCursor = (o["head_cursor"] as JsonPrimitive).longOrNull ?: 0,
            now = Times.parse(s("now"))!!,
            sinceLabel = s("since_label"),
            events = json.decodeFromJsonElement(ListSerializer(Event.serializer()), o["events"]!!),
            sessions = json.decodeFromJsonElement(ListSerializer(SessionView.serializer()), o["sessions"]!!),
            attention = json.decodeFromJsonElement(ListSerializer(AttentionItem.serializer()), o["attention"]!!),
            transcripts = json.decodeFromJsonElement(MapSerializer(String.serializer(), ReadSessionResponse.serializer()), o["transcripts"]!!),
        )
    }

    private fun JsonObject.str(k: String) = (this[k] as? JsonPrimitive)?.contentOrNull ?: ""
    private fun JsonObject.long(k: String) = (this[k] as? JsonPrimitive)?.longOrNull ?: 0L
    private fun JsonObject.int(k: String) = (this[k] as? JsonPrimitive)?.intOrNull ?: 0
    private fun JsonObject.bool(k: String) = (this[k] as? JsonPrimitive)?.booleanOrNull ?: false
    private fun JsonObject.strs(k: String) = (this[k] as? JsonArray)?.map { (it as JsonPrimitive).content } ?: emptyList()
    private fun JsonObject.links(k: String) = (this[k] as? JsonArray)?.map { it.jsonObject.str("link") } ?: emptyList()
    private fun JsonObject.dirs(k: String) = (this[k] as? JsonArray)?.map { it.jsonObject.str("direction") } ?: emptyList()

    @Test
    fun matchesTheGoDigestOnTheSharedFixture() {
        val got = DigestBuilder.build(input())
        val want = json.parseToJsonElement(File(dir, "expected.json").readText()).jsonObject
        val wantAgents = want["agents"]!!.jsonArray.map { it.jsonObject }
        assertEquals("agent order", wantAgents.map { it.str("session_id") }, got.agents.map { it.sessionId })
        for ((w, g) in wantAgents.zip(got.agents)) {
            val id = g.sessionId
            assertEquals("$id rank", w.str("rank"), g.rank)
            assertEquals("$id summary", w.str("summary"), g.summary)
            assertEquals("$id summary_source", w.str("summary_source"), g.summarySource)
            assertEquals("$id blocked", w.str("blocked"), g.blocked)
            assertEquals("$id link", w.str("link"), g.link)
            assertEquals("$id seq", w.long("seq"), g.seq)
            assertEquals("$id turns", w.int("turns"), g.turns)
            assertEquals("$id replies", w.int("replies"), g.replies)
            assertEquals("$id tools", w.int("tools"), g.tools)
            assertEquals("$id partial", w.bool("partial"), g.partial)
            assertEquals("$id events", w.int("events"), g.events)
            assertEquals("$id files", w.strs("files"), g.files)
            assertEquals("$id needs_you", w.links("needs_you"), g.needsYou.map { it.link })
            assertEquals("$id artifacts", w.links("artifacts"), g.artifacts.map { it.link })
            assertEquals("$id docs", w.links("docs"), g.docs.map { it.link })
            assertEquals("$id messages", w.links("messages"), g.messages.map { it.link })
            assertEquals("$id message directions", w.dirs("messages"), g.messages.map { it.direction })
            assertEquals("$id last_activity", Times.parse(w.str("last_activity")), g.lastActivity)
        }
        assertEquals("quiet", want.int("quiet"), got.quiet)
        assertEquals("other", (want["other"] as? JsonArray)?.map { it.jsonObject.str("id") } ?: emptyList<String>(), got.other.map { it.id })
        assertEquals(want.long("head_cursor"), got.headCursor)
    }

    @Test
    fun factsAndGlyphs() {
        val d = DigestBuilder.build(input())
        val c = d.agents.first { it.sessionId == "ses_synthC" }
        assertEquals("2 turns · 5 tools · 2 files · 1 artifact · 1 doc · 1 message", c.facts)
        assertEquals("Claude on synth-mac", c.where)
        assertEquals(listOf("!", "✗", "✓", "✓", "●"), d.agents.map { it.glyph })
        assertEquals(2, d.count(Rank.NEEDS_YOU))
    }

    @Test
    fun extraction() {
        assertEquals("Read main.go first.", DigestBuilder.firstSentence("## Plan\n\n1. Read main.go first. Then edit.", 160))
        assertEquals("Fixed it in v1.2 today!", DigestBuilder.firstSentence("```go\nx := 1\n```\nFixed it in v1.2 today! More later", 160))
        assertEquals("Here is what changed", DigestBuilder.firstSentence("Here is what changed:\n- a\n- b", 160))
        assertEquals("Plan", DigestBuilder.firstSentence("# Plan", 160))
        val long = DigestBuilder.firstSentence("word ".repeat(60), 40)
        assertTrue(long.length <= 40 && long.endsWith("…"))
        assertEquals(3 to listOf("/b.go"), DigestBuilder.toolCalls("Read: /a.go; Edit: /b.go; Bash: go test"))
        assertEquals(1 to listOf("x.go", "y.go"), DigestBuilder.toolCalls("file change: x.go, y.go"))
        assertEquals("pyre://session/ses_x?run=run_y&seq=42", DigestBuilder.sessionLink("ses_x", "run_y", 42))
        assertEquals("pyre://session/ses_x", DigestBuilder.sessionLink("ses_x", "", 0))
    }

    @Test
    fun emptyWithoutActivity() {
        val d = DigestBuilder.build(input().copy(events = emptyList(), attention = emptyList(), transcripts = emptyMap()))
        assertTrue(d.isEmpty)
    }
}
