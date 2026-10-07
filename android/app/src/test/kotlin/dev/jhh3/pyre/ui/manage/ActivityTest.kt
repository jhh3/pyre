package dev.jhh3.pyre.ui.manage

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.AuditEntry
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.model.ListAuditRequest
import dev.jhh3.pyre.model.Page
import dev.jhh3.pyre.net.HttpPyreApi
import dev.jhh3.pyre.net.HubClient
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.testing.FakePyreApi
import java.time.Instant
import java.time.ZoneId
import java.time.ZoneOffset
import java.util.Locale
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import mockwebserver3.MockResponse
import mockwebserver3.MockWebServer
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

/** Activity (audit log) decoding, unsupported mapping and row formatting. All data is synthetic. */
class ActivityTest {
    private val now = Instant.parse("2026-10-04T12:00:00Z") // a Sunday
    private val utc: ZoneId = ZoneOffset.UTC

    /** Every AuditEntry field from internal/contracts/audit.go, values obviously synthetic. */
    private val pageJson = """
        {
          "items": [
            {
              "audit_id": 42,
              "at": "2026-10-04T11:58:07.123456Z",
              "kind": "account",
              "action": "account.switched",
              "automatic": true,
              "actor": {"kind": "pyre", "id": "hub", "name": "pyre-hub"},
              "node_id": "node_synth-mac",
              "node_name": "synth-mac",
              "session_id": "ses_synth-1",
              "session_title": "synthetic refactor",
              "run_id": "run_synth-1",
              "account_id": "acct_synth-b",
              "account_label": "synth-b",
              "from": "synth-a",
              "to": "synth-b",
              "from_account_id": "acct_synth-a",
              "to_account_id": "acct_synth-b",
              "from_note": "weekly 96%",
              "to_note": "weekly 0%",
              "reason": "limit reached",
              "summary": "switched 'synthetic refactor' from synth-a (weekly 96%) to synth-b (weekly 0%) — limit reached"
            },
            {
              "audit_id": 41,
              "at": "2026-10-02T09:15:00-07:00",
              "kind": "session",
              "action": "session.started",
              "automatic": false,
              "actor": {"kind": "owner"},
              "node_id": "node_synth-linux",
              "summary": "started 'synthetic refactor'"
            }
          ],
          "next_cursor": "cur_synth-2",
          "snapshot_at": "2026-10-04T12:00:00Z"
        }
    """.trimIndent()

    private fun entry(id: Long, at: String, action: String = "session.started", automatic: Boolean = false, node: String = "node_synth-mac", nodeName: String = "", summary: String = "synthetic $id") =
        AuditEntry(auditId = id, at = at, kind = action.substringBefore('.'), action = action, automatic = automatic, nodeId = node, nodeName = nodeName, summary = summary)

    @Test
    fun decodesAPageOfAuditEntriesStrictly() {
        // strict = any key the Kotlin model lacks fails, so this pins the mirror to the Go contract.
        val page = PyreJson.strict.decodeFromString(Page.serializer(AuditEntry.serializer()), pageJson)
        assertEquals(2, page.items.size)
        assertEquals("cur_synth-2", page.nextCursor)
        assertEquals("2026-10-04T12:00:00Z", page.snapshotAt)
        val sw = page.items[0]
        assertEquals(42L, sw.auditId)
        assertEquals("account.switched", sw.action)
        assertTrue(sw.automatic)
        assertEquals("pyre", sw.actor.kind)
        assertEquals("pyre-hub", sw.actor.name)
        assertEquals("synth-mac", sw.nodeName)
        assertEquals("acct_synth-a", sw.fromAccountId)
        assertEquals("acct_synth-b", sw.toAccountId)
        assertEquals("weekly 96%", sw.fromNote)
        assertEquals("limit reached", sw.reason)
        val started = page.items[1]
        assertFalse(started.automatic)
        assertEquals("owner", started.actor.kind)
        assertEquals("", started.nodeName)
    }

    @Test
    fun requestEncodesOnlySetFilters() {
        val all = PyreJson.wire.encodeToJsonElement(ListAuditRequest.serializer(), ListAuditRequest(limit = 100)).jsonObject
        assertEquals(setOf("limit"), all.keys)
        val acc = PyreJson.wire.encodeToJsonElement(
            ListAuditRequest.serializer(),
            ListAuditRequest(cursor = "cur_synth-2", limit = 100, kinds = listOf("account"), since = "2026-10-01T00:00:00Z", nodeId = "node_synth-mac"),
        ).jsonObject
        assertEquals("account", acc["kinds"]!!.jsonArray.single().jsonPrimitive.content)
        assertEquals("cur_synth-2", acc["cursor"]!!.jsonPrimitive.content)
        assertEquals("2026-10-01T00:00:00Z", acc["since"]!!.jsonPrimitive.content)
        assertEquals("node_synth-mac", acc["node_id"]!!.jsonPrimitive.content)
    }

    @Test
    fun hubWithoutAuditListAnswers404AndIsReportedUnsupported() = runBlocking {
        val server = MockWebServer().also { it.start() }
        try {
            // What a hub that predates audit.list returns for an unknown op.
            server.enqueue(
                MockResponse.Builder().code(404).setHeader("Content-Type", "application/json")
                    .body("""{"error":{"code":"not_found","message":"no such operation"}}""").build(),
            )
            val api = HttpPyreApi(HubClient(HubEndpoint(server.url("/").toString().trimEnd('/'), devCleartext = true), { "synthetic-token" }, backoffMillis = { 1 }))
            try {
                api.listAudit(ListAuditRequest(limit = 100, kinds = listOf("account")))
                fail("expected 404")
            } catch (e: PyreApiException) {
                assertEquals(404, e.httpStatus)
                assertTrue(Activity.unsupported(e))
            }
            val rec = server.takeRequest()
            assertEquals("POST", rec.method)
            assertEquals("/v1/audit.list", rec.target)
            assertEquals("Bearer synthetic-token", rec.headers["Authorization"])
            assertEquals(1, server.requestCount) // 404 is not retried
        } finally {
            runCatching { server.close() }
        }
    }

    @Test
    fun unsupportedMapping() {
        assertTrue(Activity.unsupported(PyreApiException.fromStatus(404)))
        assertTrue(Activity.unsupported(PyreApiException.fromStatus(501)))
        assertTrue(Activity.unsupported(PyreApiException(ErrorCodes.UNSUPPORTED, "synthetic")))
        assertFalse(Activity.unsupported(PyreApiException.fromStatus(500)))
        assertFalse(Activity.unsupported(PyreApiException.fromStatus(401)))
        assertFalse(Activity.unsupported(PyreApiException.transport(java.io.IOException("synthetic: down"))))
        // The PyreApi default (an API binding without audit.list) is unsupported too.
        val e = runCatching { runBlocking { object : FakePyreApi() {}.listAudit(ListAuditRequest()) } }.exceptionOrNull() as PyreApiException
        assertTrue(Activity.unsupported(e))
    }

    @Test
    fun timeIsClockTodayWeekdayThisWeekElseDate() {
        val us = Locale.US
        assertEquals("09:05", Activity.time("2026-10-04T09:05:00Z", now, utc, us))
        assertEquals("Fri 14:05", Activity.time("2026-10-02T14:05:00Z", now, utc, us))
        assertEquals("Mon 14:05", Activity.time("2026-09-28T14:05:00Z", now, utc, us)) // 6 days back
        assertEquals("Sep 27 14:05", Activity.time("2026-09-27T14:05:00Z", now, utc, us)) // a week back
        assertEquals("Oct 5 08:00", Activity.time("2026-10-05T08:00:00Z", now, utc, us)) // clock skew: future day
        assertEquals("—", Activity.time("not a time", now, utc, us))
        // Local day, not UTC day: 05:00Z is Saturday 22:00 in Los Angeles.
        assertEquals("Sat 22:00", Activity.time("2026-10-04T05:00:00Z", now, ZoneId.of("America/Los_Angeles"), us))
    }

    @Test
    fun machineIsNameElseIdWithoutPrefix() {
        assertEquals("synth-mac", Activity.machine(entry(1, "2026-10-04T09:00:00Z", nodeName = "synth-mac")))
        assertEquals("synth-linux", Activity.machine(entry(1, "2026-10-04T09:00:00Z", node = "node_synth-linux")))
        assertEquals("", Activity.machine(entry(1, "2026-10-04T09:00:00Z", node = "")))
    }

    @Test
    fun accountSwitchesAreHighlightedAndRowsAreNewestFirst() {
        val rows = Activity.rows(
            listOf(
                entry(1, "2026-10-04T08:00:00Z", summary = "started synthetic"),
                entry(3, "2026-10-04T11:00:00Z", action = "account.switched", automatic = true, nodeName = "synth-mac", summary = "switched synthetic"),
                entry(2, "2026-10-04T10:00:00Z", action = "account.waiting", automatic = true, summary = "waiting synthetic"),
                entry(4, "2026-10-04T09:00:00Z", action = "account.chosen", automatic = true, summary = "chose synthetic"),
                entry(5, "2026-10-04T07:00:00Z", action = "node.upgraded", summary = ""),
            ),
            now, utc, Locale.US,
        )
        assertEquals(listOf("audit-3", "audit-2", "audit-4", "audit-1", "audit-5"), rows.map { it.key })
        assertEquals(listOf(true, true, false, false, false), rows.map { it.highlight })
        assertEquals("11:00 · synth-mac", rows[0].header)
        assertEquals("switched synthetic", rows[0].summary)
        assertTrue(rows[0].automatic)
        assertFalse(rows[3].automatic)
        assertEquals("node.upgraded", rows[4].summary) // empty summary falls back to the action
    }

    @Test
    fun detailsNameReasonActorChangeAndIds() {
        val page = PyreJson.wire.decodeFromString(Page.serializer(AuditEntry.serializer()), pageJson)
        val d = Activity.details(page.items[0]).toMap()
        assertEquals("account.switched (automatic)", d["What"])
        assertEquals("limit reached", d["Why"])
        assertEquals("pyre · pyre-hub", d["Who"])
        assertEquals("synth-a (weekly 96%) → synth-b (weekly 0%)", d["Change"])
        assertEquals("synthetic refactor (ses_synth-1)", d["Session"])
        assertEquals("acct_synth-a → acct_synth-b", d["Account ids"])
        assertEquals("synth-mac (node_synth-mac)", d["Machine"])
        assertEquals("run_synth-1", d["Run"])
        val d2 = Activity.details(page.items[1]).toMap()
        assertEquals("owner", d2["Who"])
        assertFalse(d2.containsKey("Why"))
        assertFalse(d2.containsKey("Change"))
    }
}
