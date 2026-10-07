package dev.jhh3.pyre.ui

import android.app.Application
import androidx.compose.ui.test.assertIsEnabled
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.hasTestTag
import androidx.compose.ui.test.performScrollTo
import androidx.compose.ui.test.performScrollToNode
import androidx.compose.ui.test.performTextReplacement
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.data.InMemoryCredentialStore
import dev.jhh3.pyre.pairing.PairingService
import dev.jhh3.pyre.ui.pairing.PairingScreen
import dev.jhh3.pyre.ui.pairing.PairingViewModel
import java.net.URLEncoder
import mockwebserver3.MockResponse
import mockwebserver3.MockWebServer
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.Fixtures
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.approvals.ApprovalScreen
import dev.jhh3.pyre.ui.approvals.ApprovalViewModel
import dev.jhh3.pyre.ui.manage.AccountsScreen
import dev.jhh3.pyre.ui.manage.AccountsViewModel
import dev.jhh3.pyre.ui.session.SessionDetailScreen
import dev.jhh3.pyre.ui.session.SessionDetailViewModel
import dev.jhh3.pyre.ui.sessions.SessionsScreen
import dev.jhh3.pyre.ui.sessions.SessionsViewModel
import dev.jhh3.pyre.ui.theme.PyreTheme
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/** Compose UI on the JVM (Robolectric) for the main screens, driven by contract fixtures. */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = Application::class)
class ScreensUiTest {
    @get:Rule val rule = createComposeRule()

    @Test
    fun sessionsShowOfflineNodeHonestly() {
        val b2 = Fixtures.bundle("02")
        val api = object : FakePyreApi() {
            override suspend fun listSessions(req: ListSessionsRequest) = Page(b2.sessions)
            override suspend fun listNodes(req: ListNodesRequest) = Page(b2.nodes)
        }
        val vm = SessionsViewModel(testRepo(api))
        rule.setContent { PyreTheme { SessionsScreen(vm) {} } }
        rule.onNodeWithText("long refactor").assertExists()
        // The status word leads the row's supporting line ("Unknown (stale) · claude · …").
        rule.onNodeWithText("Unknown (stale)", substring = true).assertExists()
        // Machine health shows on the machine group headers (grouping toggle).
        rule.onNodeWithTag("group-machine").performClick()
        rule.onNodeWithText("Offline").assertExists()
        assertEquals(0, rule.onAllNodesWithText("Waiting for input", substring = true).fetchSemanticsNodes().size)
    }

    private val b9 = Fixtures.bundle("09")

    private inner class ApprovalHub : FakePyreApi() {
        var current = b9.approvals.first { it.approvalId == "apr_synth-pending" }
        override suspend fun getApproval(id: String): Approval = if (id == current.approvalId) current else b9.approvals.first { it.approvalId == id }
        override suspend fun answerApproval(req: AnswerApprovalRequest): Approval {
            record(Ops.ANSWER_APPROVAL, req)
            current = PyreJson.wire.decodeFromJsonElement(Approval.serializer(), b9.exchanges.first().response!!)
            return current
        }
    }

    @Test
    fun approvalDoubleTapAnswersOnce() {
        val hub = ApprovalHub()
        val vm = ApprovalViewModel(testRepo(hub), "apr_synth-pending")
        rule.setContent { PyreTheme { ApprovalScreen(vm) {} } }
        rule.onNodeWithText("Run `make deploy-staging`?").assertExists()
        rule.onNodeWithTag("option-allow").performClick()
        rule.onNodeWithTag("approval-result").assertExists()
        rule.onNodeWithTag("option-allow").assertDoesNotExist() // controls withdrawn once resolved
        assertEquals(1, hub.count(Ops.ANSWER_APPROVAL))
        rule.onNodeWithText("Answer recorded", substring = true).assertExists()
    }

    @Test
    fun staleApprovalLinkShowsResolutionWithoutControls() {
        val vm = ApprovalViewModel(testRepo(ApprovalHub()), "apr_synth-resolved")
        rule.setContent { PyreTheme { ApprovalScreen(vm) {} } }
        rule.onNodeWithTag("approval-state").assertExists()
        rule.onNodeWithText("native terminal", substring = true).assertExists()
        rule.onNodeWithTag("option-allow").assertDoesNotExist()
        rule.onNodeWithTag("option-deny").assertDoesNotExist()
    }

    @Test
    fun indeterminateDeliveryShowsReconcileFlowWithRiskGate() {
        val b3 = Fixtures.bundle("03")
        val api = object : FakePyreApi() {
            override suspend fun getSession(id: String) = b3.sessions.first { it.session.sessionId == id }
            override suspend fun listMessages(req: ListMessagesRequest) =
                Page(b3.messages.filter { it.envelope.recipient.id == "ses_synth-b" && it.status.storage == StorageState.STORED })
        }
        val vm = SessionDetailViewModel(testRepo(api), "ses_synth-b")
        rule.setContent { PyreTheme { SessionDetailScreen(vm, {}, {}) } }
        rule.onNodeWithTag("session-detail").performScrollToNode(hasTestTag("resolve-msg_synth-indeterminate"))
        rule.onNodeWithTag("resolve-msg_synth-indeterminate").performClick()
        rule.onNodeWithTag("reconcile-dialog").assertExists()
        rule.onNodeWithText("Reissue as new message").assertIsNotEnabled()
        rule.onNodeWithTag("accept-risk").performClick()
        rule.onNodeWithText("Reissue as new message").assertIsEnabled()
        rule.onNodeWithText("It was delivered (mark accepted)").assertExists()
    }

    @Test
    fun sessionOverflowRenamesThroughSessionsUpdate() {
        val b3 = Fixtures.bundle("03")
        var current = b3.sessions.first { it.session.sessionId == "ses_synth-b" }
        val api = object : FakePyreApi() {
            override suspend fun getSession(id: String) = current
            override suspend fun updateSession(req: UpdateSessionRequest): SessionView {
                record(Ops.UPDATE_SESSION, req)
                current = current.copy(session = current.session.copy(title = req.title!!))
                return current
            }
        }
        val vm = SessionDetailViewModel(testRepo(api), "ses_synth-b")
        rule.setContent { PyreTheme { SessionDetailScreen(vm, {}, {}) } }
        // Cancel leaves the name alone.
        rule.onNodeWithTag("session-menu").performClick()
        rule.onNodeWithTag("menu-rename").performClick()
        rule.onNodeWithTag("rename-cancel").performClick()
        rule.onNodeWithTag("rename-field").assertDoesNotExist()
        assertEquals(0, api.count(Ops.UPDATE_SESSION))
        // Blank cannot be saved; a real name is sent once.
        rule.onNodeWithTag("session-menu").performClick()
        rule.onNodeWithTag("menu-rename").performClick()
        rule.onNodeWithTag("rename-field").performTextReplacement("   ")
        rule.onNodeWithTag("rename-save").assertIsNotEnabled()
        rule.onNodeWithTag("rename-field").performTextReplacement("Stripe webhook fix")
        rule.onNodeWithTag("rename-save").performClick()
        rule.waitForIdle()
        rule.onNodeWithTag("rename-field").assertDoesNotExist()
        assertEquals(1, api.count(Ops.UPDATE_SESSION))
        assertEquals("Stripe webhook fix", (vm.ui.value.session as dev.jhh3.pyre.data.Load.Ready).value.session.title)
    }

    @Test
    fun sessionDetailSeparatesTerminalFromChat() {
        val b3 = Fixtures.bundle("03")
        val api = object : FakePyreApi() {
            override suspend fun getSession(id: String) = b3.sessions.first { it.session.sessionId == id }
        }
        val vm = SessionDetailViewModel(testRepo(api), "ses_synth-b")
        rule.setContent { PyreTheme { SessionDetailScreen(vm, {}, {}) } }
        rule.onNodeWithTag("open-terminal").assertExists()
        rule.onNodeWithTag("send").assertExists()
        rule.onNodeWithTag("steer").assertDoesNotExist() // no input.steer capability
    }

    @Test
    fun accountsShowStaleAndUnknownEligibility() {
        val accounts = Fixtures.bundle("07").accounts
        val b1 = Fixtures.bundle("01")
        val api = object : FakePyreApi() {
            override suspend fun listAccounts(req: ListAccountsRequest) = Page(accounts)
            override suspend fun listNodes(req: ListNodesRequest) = Page(b1.nodes)
            override suspend fun listSessions(req: ListSessionsRequest) = Page(b1.sessions)
        }
        val vm = AccountsViewModel(testRepo(api))
        rule.setContent { PyreTheme { AccountsScreen(vm) } }
        rule.onNodeWithText("claude-personal-a").assertExists()
        // Pool count (usage.Pool.Summary words): claude-a ready, claude-c at
        // limit, claude-b unknown (stale, rate-limited).
        rule.onNodeWithText("Anthropic · 1 ready · 1 at limit · 1 unknown").assertExists()
        rule.onNodeWithText("Healthy").assertExists()
        rule.onNodeWithText("At limit").assertExists()
        rule.onAllNodesWithText("Unknown").fetchSemanticsNodes().let { assert(it.size == 2) { "claude-b and codex-a are unknown: ${it.size}" } }
        // The stale observation keeps its last-known bar, labelled as such.
        rule.onNodeWithText("5h · last known").assertExists()
        rule.onNodeWithText("usage endpoint rate-limited", substring = true).assertExists()
        rule.onNodeWithText("In use by scratch (standalone) on synth-mac").assertExists()
        // synth-linux runs claude without a Pyre-managed login.
        rule.onNodeWithText("Your own login (not managed)").assertExists()
    }

    @Test
    fun pairingWithAnIncompatibleHubShowsTheRefusal() {
        val server = MockWebServer()
        server.enqueue(
            MockResponse.Builder().code(426).body(
                """{"error":{"code":"protocol_mismatch","message":"client protocol 1 not supported; hub supports 2..2","retryable":false,"details":{"min_protocol":2,"protocol":2}}}""",
            ).build(),
        )
        server.start()
        try {
            val store = InMemoryCredentialStore()
            val link = "pyre://pair?hub=" + URLEncoder.encode("http://localhost:${server.port}", "UTF-8") + "&id=pair_synth-x&code=CODE-synthetic"
            val vm = PairingViewModel(PairingService(store), link, devAllowed = true)
            var paired = false
            rule.setContent { PyreTheme { PairingScreen(vm) { paired = true } } }
            rule.onNodeWithTag("pair-confirm").performScrollTo().performClick()
            rule.waitUntil(10_000) { rule.onAllNodesWithText("hub supports 2..2", substring = true).fetchSemanticsNodes().isNotEmpty() }
            rule.onNodeWithText("The pairing code was not used", substring = true).assertExists()
            assertFalse("no navigation on a refused pairing", paired)
            assertNull("nothing stored", store.load())
        } finally {
            server.close()
        }
    }
}
