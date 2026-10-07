package dev.jhh3.pyre.ui

import android.app.Application
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.semantics.SemanticsProperties
import androidx.compose.ui.semantics.getOrNull
import androidx.compose.ui.test.SemanticsMatcher
import androidx.compose.ui.test.performScrollTo
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.core.Clock
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.data.InMemoryBlobStore
import dev.jhh3.pyre.data.InMemoryCacheStore
import dev.jhh3.pyre.data.IntentIds
import dev.jhh3.pyre.data.LocalSettings
import dev.jhh3.pyre.data.Outbox
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.digest.InMemoryLastSeenStore
import dev.jhh3.pyre.digest.LastSeen
import dev.jhh3.pyre.digest.PrefsLastSeenStore
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.ui.sessions.SessionsScreen
import dev.jhh3.pyre.ui.sessions.SessionsViewModel
import dev.jhh3.pyre.ui.nav.DeepLinks
import dev.jhh3.pyre.ui.nav.Screen
import dev.jhh3.pyre.ui.theme.PyreTheme
import java.io.File
import java.time.Instant
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.builtins.MapSerializer
import kotlinx.serialization.builtins.serializer
import kotlinx.serialization.json.jsonObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/** The "While you were away" card at the top of Sessions, fed by the shared synthetic digest fixture through a fake hub. */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = Application::class)
class DigestCardUiTest {
    @get:Rule val rule = createComposeRule()

    private val now = Instant.parse("2026-10-05T12:00:00Z")
    private val since = Instant.parse("2026-10-05T09:00:00Z")

    /** Serves tests/fixtures/digest/input.json through the existing read operations only. */
    private class FixtureHub : FakePyreApi() {
        private val o = PyreJson.wire.parseToJsonElement(
            File(System.getProperty("pyre.digest.fixtures.dir") ?: "../../tests/fixtures/digest", "input.json").readText(),
        ).jsonObject
        val events = PyreJson.wire.decodeFromJsonElement(ListSerializer(Event.serializer()), o["events"]!!).sortedBy { it.cursor }
        val sessions = PyreJson.wire.decodeFromJsonElement(ListSerializer(SessionView.serializer()), o["sessions"]!!)
        val attention = PyreJson.wire.decodeFromJsonElement(ListSerializer(AttentionItem.serializer()), o["attention"]!!)
        val transcripts = PyreJson.wire.decodeFromJsonElement(MapSerializer(String.serializer(), ReadSessionResponse.serializer()), o["transcripts"]!!)
        val head = events.maxOf { it.cursor }

        override suspend fun listEvents(req: ListEventsRequest): ListEventsResponse {
            record(Ops.LIST_EVENTS, req)
            if (req.afterCursor > head) throw PyreApiException(ErrorCodes.RESET_REQUIRED, "synthetic: ahead")
            val out = events.filter { it.cursor > req.afterCursor && (req.kinds.isEmpty() || it.kind in req.kinds) }.take(maxOf(1, minOf(req.limit, 7)))
            return ListEventsResponse(out, out.lastOrNull()?.cursor ?: req.afterCursor, head)
        }

        override suspend fun listSessions(req: ListSessionsRequest) = Page(sessions)
        override suspend fun listAttention(req: ListAttentionRequest) =
            Page(attention.filter { req.states.isEmpty() || it.state in req.states })
        override suspend fun readSession(req: ReadSessionRequest): ReadSessionResponse {
            record(Ops.READ_SESSION, req)
            if (req.sessionId == "ses_synthB") throw PyreApiException(ErrorCodes.NOT_FOUND, "synthetic: no run")
            return transcripts[req.sessionId] ?: ReadSessionResponse()
        }
        override suspend fun listApprovals(req: ListApprovalsRequest) = Page<Approval>(emptyList())
    }

    private fun repo(hub: FakePyreApi): Repository {
        val blobs = InMemoryBlobStore()
        val clock = Clock { now }
        return Repository(hub, InMemoryCacheStore(clock), Outbox(blobs, clock), IntentIds(blobs), clock = clock)
    }

    @Test
    fun cardRanksAgentsOpensLinksAndDismisses() {
        val hub = FixtureHub()
        val store = InMemoryLastSeenStore(LastSeen(100, since))
        val vm = SessionsViewModel(repo(hub), digestStore = store)
        val opened = mutableListOf<String>()
        rule.setContent { PyreTheme { SessionsScreen(vm, onOpenLink = { opened += it }) {} } }
        rule.waitUntil(5_000) { rule.onAllNodesWithTag("digest-card").fetchSemanticsNodes().isNotEmpty() }

        rule.onNodeWithText("While you were away").assertIsDisplayed()
        rule.onNodeWithText("5 agents, 2 need you", substring = true).assertExists()
        // Needs you → done → still working, top to bottom.
        val order = listOf("ses_synthA", "ses_synthB", "ses_synthH", "ses_synthC", "ses_synthD")
        val rowTags = rule.onAllNodes(SemanticsMatcher("digest row") { n ->
            n.config.getOrNull(SemanticsProperties.TestTag)?.startsWith("digest-row-") == true
        }).fetchSemanticsNodes().map { it.config[SemanticsProperties.TestTag].removePrefix("digest-row-") }
        assertEquals(order, rowTags)
        rule.onNodeWithText("Approve: go test -bench ./...").assertExists()
        rule.onNodeWithText("Updated the digest docs and added tests for ranking, e.g. needs-you first.").assertExists()
        rule.onNodeWithText("Claude on synth-mac · 1h 50m ago · 2 turns · 5 tools · 2 files", substring = true).assertExists()
        rule.onNodeWithTag("digest-quiet").assertExists()
        rule.onNodeWithTag("digest-other-att_synthNode").assertExists()
        // The fetch used only reads, with the kinds filter.
        assertTrue(hub.calls.filter { it.first == Ops.LIST_EVENTS }.any { (it.second as ListEventsRequest).kinds.isNotEmpty() })

        // A tap opens the exact transcript moment, which the app resolves to the session.
        rule.onNodeWithTag("digest-row-ses_synthC").performScrollTo().performClick()
        assertEquals("pyre://session/ses_synthC?run=run_synthC&seq=812", opened.last())
        assertEquals(Screen.SessionDetail("ses_synthC"), DeepLinks.parse(opened.last()))
        rule.onNodeWithTag("digest-row-ses_synthA").performScrollTo().performClick()
        assertEquals("pyre://approval/apr_synthA", opened.last())

        rule.onNodeWithTag("digest-dismiss").performScrollTo().performClick()
        rule.waitForIdle()
        assertTrue(rule.onAllNodesWithTag("digest-card").fetchSemanticsNodes().isEmpty())
        assertEquals(LastSeen(hub.head, now), store.mark)
    }

    @Test
    fun noCardWhenNothingHappened() {
        val hub = FixtureHub()
        val vm = SessionsViewModel(repo(hub), digestStore = InMemoryLastSeenStore(LastSeen(hub.head, now)))
        rule.setContent { PyreTheme { SessionsScreen(vm) {} } }
        rule.waitForIdle()
        rule.waitUntil(5_000) { hub.calls.any { it.first == Ops.LIST_EVENTS } }
        rule.waitForIdle()
        assertTrue(rule.onAllNodesWithTag("digest-card").fetchSemanticsNodes().isEmpty())
        assertNull(vm.ui.value.digest)
    }

    @Test
    fun lastSeenIsPerClientAndNeverMovesBack() {
        val settings = LocalSettings(InMemoryBlobStore())
        val phone = PrefsLastSeenStore(settings, "cli_synth-phone")
        assertNull(phone.load())
        phone.save(LastSeen(120, since))
        phone.save(LastSeen(110, now)) // older cursor: ignored
        assertEquals(LastSeen(120, since), phone.load())
        assertNull("a different (re-paired) client starts fresh", PrefsLastSeenStore(settings, "cli_synth-other").load())
    }

    @Test
    fun digestDeepLinksResolve() {
        assertEquals(Screen.SessionDetail("ses_1"), DeepLinks.parse("pyre://session/ses_1?run=run_2&seq=812"))
        assertEquals(Screen.Approval("apr_1"), DeepLinks.parse("pyre://approval/apr_1"))
        assertNull(DeepLinks.parse("pyre://session/ses_1?x=<script>"))
    }
}
