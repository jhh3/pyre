package dev.jhh3.pyre.ui

import android.app.Application
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.model.AuditActor
import dev.jhh3.pyre.model.AuditEntry
import dev.jhh3.pyre.model.AuditOps
import dev.jhh3.pyre.model.ListAuditRequest
import dev.jhh3.pyre.model.Page
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.manage.Activity
import dev.jhh3.pyre.ui.manage.ActivityScreen
import dev.jhh3.pyre.ui.manage.ActivityViewModel
import dev.jhh3.pyre.ui.theme.PyreTheme
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/** Activity screen (audit log) on Robolectric with a fake hub. Synthetic data. */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = Application::class)
class ActivityScreenUiTest {
    @get:Rule val rule = createComposeRule()

    private val entries = listOf(
        AuditEntry(
            auditId = 7, at = "2026-10-04T11:50:00Z", kind = "account", action = AuditOps.ACCOUNT_SWITCHED, automatic = true,
            actor = AuditActor("pyre"), nodeId = "node_synth-mac", nodeName = "synth-mac",
            reason = "synthetic limit reached", summary = "switched 'synthetic task' from synth-a to synth-b",
        ),
        AuditEntry(
            auditId = 6, at = "2026-10-04T11:00:00Z", kind = "session", action = "session.started",
            actor = AuditActor("owner"), nodeId = "node_synth-linux", summary = "started 'synthetic task'",
        ),
    )

    private inner class AuditHub : FakePyreApi() {
        val requests = mutableListOf<ListAuditRequest>()
        override suspend fun listAudit(req: ListAuditRequest): Page<AuditEntry> {
            requests += req
            return Page(entries.filter { req.kinds.isEmpty() || it.kind in req.kinds })
        }
    }

    @Test
    fun listsEntriesWithAutoChipAndFiltersByKind() {
        val hub = AuditHub()
        val vm = ActivityViewModel(testRepo(hub))
        rule.setContent { PyreTheme { ActivityScreen(vm) } }
        rule.onNodeWithText("switched 'synthetic task' from synth-a to synth-b").assertExists()
        rule.onNodeWithText("started 'synthetic task'").assertExists()
        rule.onNodeWithText("synth-linux", substring = true).assertExists() // node id without "node_"
        assertEquals(1, rule.onAllNodesWithTag("activity-auto", useUnmergedTree = true).fetchSemanticsNodes().size)

        rule.onNodeWithTag("activity-row-audit-7").performClick()
        rule.onNodeWithText("Why: synthetic limit reached").assertExists()

        rule.onNodeWithTag("activity-filter-accounts").performClick()
        rule.waitForIdle()
        assertEquals(listOf("account"), hub.requests.last().kinds)
        rule.onNodeWithText("started 'synthetic task'").assertDoesNotExist()
    }

    @Test
    fun oldHubShowsUpgradeMessage() {
        val api = object : FakePyreApi() {
            override suspend fun listAudit(req: ListAuditRequest): Page<AuditEntry> = throw PyreApiException.fromStatus(404)
        }
        val vm = ActivityViewModel(testRepo(api))
        rule.setContent { PyreTheme { ActivityScreen(vm) } }
        rule.onNodeWithText(Activity.UNSUPPORTED_TEXT).assertExists()
    }
}
