package dev.jhh3.pyre.ui

import android.app.Application
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.model.ListNodesRequest
import dev.jhh3.pyre.model.ListSessionsRequest
import dev.jhh3.pyre.model.Page
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.model.UpdateSessionRequest
import dev.jhh3.pyre.testing.FIXED_NOW
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.Fixtures
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.session.SessionDetailScreen
import dev.jhh3.pyre.ui.session.SessionDetailViewModel
import dev.jhh3.pyre.ui.sessions.PINNED_GROUP
import dev.jhh3.pyre.ui.sessions.SessionsScreen
import dev.jhh3.pyre.ui.sessions.SessionsViewModel
import dev.jhh3.pyre.ui.theme.PyreTheme
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/** Pinned sessions: first on the Sessions screen, with a pin glyph; Pin / Unpin edits the reserved tags. */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = Application::class)
class PinnedSessionsTest {
    @get:Rule val rule = createComposeRule()

    private val b = Fixtures.bundle("03")

    private inner class Hub(pinned: String?) : FakePyreApi() {
        var sessions: List<SessionView> = b.sessions.map {
            if (it.session.sessionId == pinned) it.copy(session = it.session.copy(tags = it.session.tags + listOf("pinned", "pinned_at:100"))) else it
        }
        val updates = mutableListOf<UpdateSessionRequest>()
        override suspend fun listSessions(req: ListSessionsRequest) = Page(sessions)
        override suspend fun listNodes(req: ListNodesRequest) = Page(b.nodes)
        override suspend fun updateSession(req: UpdateSessionRequest): SessionView {
            updates += req
            sessions = sessions.map { if (it.session.sessionId == req.sessionId) it.copy(session = it.session.copy(tags = req.tags!!)) else it }
            return sessions.first { it.session.sessionId == req.sessionId }
        }
    }

    @Test
    fun pinnedGroupComesFirstInEveryCase() {
        val last = b.sessions.last().session.sessionId
        val groups = SessionsViewModel.group(
            b.sessions.map { if (it.session.sessionId == last) it.copy(session = it.session.copy(tags = listOf("pinned", "pinned_at:5"))) else it },
            b.nodes,
        )
        assertEquals("Pinned", groups.first().title)
        assertEquals(PINNED_GROUP, groups.first().nodeId)
        assertEquals(listOf(last), groups.first().sessions.map { it.session.sessionId })
        // A pinned session is not repeated under its machine.
        assertEquals(1, groups.flatMap { it.sessions }.count { it.session.sessionId == last })
    }

    @Test
    fun screenShowsPinGlyphAndLongPressUnpins() {
        val sid = b.sessions.first().session.sessionId
        val hub = Hub(pinned = sid)
        val vm = SessionsViewModel(testRepo(hub))
        rule.setContent { PyreTheme { SessionsScreen(vm) {} } }
        rule.waitForIdle()
        rule.onNodeWithText("Pinned").assertExists()
        rule.onNodeWithTag("pin-$sid", useUnmergedTree = true).assertExists()

        vm.togglePin(vm.ui.value.groups.first().sessions.first())
        rule.waitForIdle()
        rule.waitUntil(5_000) { hub.updates.isNotEmpty() }
        assertEquals(sid, hub.updates.single().sessionId)
        assertEquals(false, hub.updates.single().tags!!.any { it == "pinned" || it.startsWith("pinned_at:") })
        rule.waitUntil(5_000) { vm.ui.value.groups.none { it.title == "Pinned" } }
        rule.onNodeWithTag("pin-$sid", useUnmergedTree = true).assertDoesNotExist()
    }

    @Test
    fun pinSendsMarkerAndTimeTags() {
        val sid = b.sessions.last().session.sessionId
        val hub = Hub(pinned = null)
        val vm = SessionsViewModel(testRepo(hub))
        rule.setContent { PyreTheme { SessionsScreen(vm) {} } }
        rule.waitForIdle()
        vm.togglePin(hub.sessions.first { it.session.sessionId == sid })
        rule.waitUntil(5_000) { hub.updates.isNotEmpty() }
        val tags = hub.updates.single().tags!!
        assertEquals(listOf("pinned", "pinned_at:" + FIXED_NOW.epochSecond), tags.takeLast(2))
        rule.waitUntil(5_000) { vm.ui.value.groups.firstOrNull()?.title == "Pinned" }
        rule.onNodeWithTag("pin-$sid", useUnmergedTree = true).assertExists()
    }

    @Test
    fun sessionOverflowPinsAndUnpins() {
        var current = b.sessions.first { it.session.sessionId == "ses_synth-b" }
        val api = object : FakePyreApi() {
            override suspend fun getSession(id: String) = current
            override suspend fun updateSession(req: UpdateSessionRequest): SessionView {
                current = current.copy(session = current.session.copy(tags = req.tags!!))
                return current
            }
        }
        val vm = SessionDetailViewModel(testRepo(api), "ses_synth-b")
        rule.setContent { PyreTheme { SessionDetailScreen(vm, {}, {}) } }
        rule.onNodeWithTag("session-menu").performClick()
        rule.onNodeWithText("Pin").assertExists()
        rule.onNodeWithTag("menu-pin").performClick()
        rule.waitForIdle()
        assertEquals(true, "pinned" in current.session.tags)
        rule.onNodeWithTag("session-menu").performClick()
        rule.onNodeWithText("Unpin").assertExists()
        rule.onNodeWithTag("menu-pin").performClick()
        rule.waitForIdle()
        assertEquals(false, current.session.tags.any { it == "pinned" || it.startsWith("pinned_at:") })
    }
}
