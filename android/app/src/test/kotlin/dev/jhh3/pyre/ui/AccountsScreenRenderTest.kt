package dev.jhh3.pyre.ui

import android.app.Application
import android.graphics.Bitmap
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.test.assertCountEquals
import androidx.compose.ui.test.captureToImage
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.onRoot
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.model.Account
import dev.jhh3.pyre.model.AccountAssignment
import dev.jhh3.pyre.model.AccountObservation
import dev.jhh3.pyre.model.AccountView
import dev.jhh3.pyre.model.DeviceGrant
import dev.jhh3.pyre.model.HarnessInfo
import dev.jhh3.pyre.model.ListAccountsRequest
import dev.jhh3.pyre.model.ListNodesRequest
import dev.jhh3.pyre.model.ListSessionsRequest
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.Page
import dev.jhh3.pyre.model.Session
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.model.UsageWindow
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.manage.AccountsScreen
import dev.jhh3.pyre.ui.manage.AccountsViewModel
import dev.jhh3.pyre.ui.theme.PyreTheme
import java.io.File
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config
import org.robolectric.annotation.GraphicsMode

/**
 * Renders the Accounts screen with every card state (synthetic data) on real
 * Skia and writes build/accounts-screen.png for review.
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = Application::class, qualifiers = "w411dp-h2400dp")
@GraphicsMode(GraphicsMode.Mode.NATIVE)
class AccountsScreenRenderTest {
    @get:Rule val rule = createComposeRule()

    private val mac = "node_synth-mac"
    private val linux = "node_synth-linux"

    private fun w(name: String, pct: Double, reset: String? = null) = UsageWindow(name, pct, reset)

    private fun o(acct: String, node: String, elig: String, windows: List<UsageWindow>, stale: Boolean = false, detail: String = "", reset: String? = null, at: String = "2026-10-04T11:58:00Z") =
        AccountObservation(acct, reportingNode = node, observedAt = at, eligibility = elig, windows = windows, stale = stale, detail = detail, resetAt = reset)

    private fun g(acct: String, node: String, state: String = "valid", harness: String = "claude", kind: String = "claude-profile-store") =
        DeviceGrant("grant_synth-$acct-$node", acct, node, harness, kind = kind, state = state)

    private fun acct(id: String, provider: String = "anthropic", disabled: Boolean = false) = Account(id, provider, "synth-$id", disabled = disabled)

    private val accounts = listOf(
        AccountView(
            acct("alpha"),
            o("alpha", mac, "eligible", listOf(w("five_hour", 34.0, "2026-10-04T14:14:00Z"), w("seven_day", 52.0, "2026-10-08T22:00:00Z"), w("seven_day_fable", 37.0), w("extra_usage", 89.0))),
            grants = listOf(g("alpha", mac)),
            activeAssignments = listOf(AccountAssignment("asg_synth-1", "alpha", sessionId = "ses_synth-1", nodeId = mac)),
        ),
        AccountView(
            acct("bravo"),
            o("bravo", mac, "eligible", listOf(w("five_hour", 84.0, "2026-10-04T13:05:00Z"), w("seven_day", 71.0, "2026-10-07T22:00:00Z"))),
            grants = listOf(g("bravo", mac)),
        ),
        AccountView(
            acct("charlie"),
            o("charlie", mac, "exhausted", listOf(w("five_hour", 100.0, "2026-10-04T15:20:00Z"), w("seven_day", 93.0, "2026-10-07T22:00:00Z")), reset = "2026-10-04T15:20:00Z", detail = "usage limit reached"),
            grants = listOf(g("charlie", mac)),
        ),
        AccountView(
            acct("delta"),
            o("delta", linux, "unknown", listOf(w("five_hour", 61.0), w("seven_day", 44.0)), stale = true, at = "2026-10-04T09:10:00Z",
                detail = "access token expired; usage probe never refreshes (the harness refreshes on its next use)"),
            grants = listOf(g("delta", linux)),
        ),
        AccountView(
            acct("echo"),
            o("echo", mac, "unusable", emptyList(), detail = "synthetic login rejected by provider; run pyre accounts login"),
            grants = listOf(g("echo", mac, state = "dead")),
        ),
        AccountView(acct("foxtrot", disabled = true), null, grants = listOf(g("foxtrot", mac))),
        AccountView(
            acct("golf", provider = "openai"),
            o("golf", mac, "unknown", emptyList(), detail = "codex rota account not checked out; usage needs its app-server (unknown until checked out)"),
            grants = listOf(g("golf", mac, harness = "codex", kind = "codex-home-file")),
        ),
    )

    private val nodes = listOf(
        Node(mac, name = "synth-mac", harnesses = listOf(HarnessInfo("claude", available = true), HarnessInfo("codex", available = true))),
        Node(linux, name = "synth-linux", harnesses = listOf(HarnessInfo("claude", available = true), HarnessInfo("codex", available = true))),
    )

    @Test
    fun rendersEveryState() {
        val api = object : FakePyreApi() {
            override suspend fun listAccounts(req: ListAccountsRequest) = Page(accounts)
            override suspend fun listNodes(req: ListNodesRequest) = Page(nodes)
            override suspend fun listSessions(req: ListSessionsRequest) = Page(listOf(SessionView(Session("ses_synth-1", title = "synth refactor"))))
        }
        val vm = AccountsViewModel(testRepo(api))
        rule.setContent { PyreTheme(dynamicColor = false) { AccountsScreen(vm) } }
        rule.waitForIdle()
        rule.onNodeWithText("Anthropic · 2 ready · 2 near or at limit · 1 needs re-auth").assertExists()
        rule.onNodeWithText("In use by synth refactor on synth-mac").assertExists()
        rule.onNodeWithText("Fable week 37% · extra usage 89%").assertExists()
        // delta (token expired at rest) and golf (Codex not checked out) are
        // ready on use; only golf's reason is worth a detail line.
        rule.onAllNodesWithText("Ready · refreshes on use").assertCountEquals(2)
        rule.onNodeWithText("usage visible once checked out").assertExists()
        val bmp = rule.onRoot().captureToImage().asAndroidBitmap()
        val out = File(System.getProperty("pyre.screens.dir") ?: "build").resolve("accounts-screen.png")
        out.parentFile?.mkdirs()
        out.outputStream().use { bmp.compress(Bitmap.CompressFormat.PNG, 100, it) }
    }
}
