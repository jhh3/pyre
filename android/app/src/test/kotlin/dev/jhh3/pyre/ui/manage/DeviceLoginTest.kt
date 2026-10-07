package dev.jhh3.pyre.ui.manage

import android.app.Application
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.CommandOutcome
import dev.jhh3.pyre.model.ConnectionState
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.NodeActionRequest
import dev.jhh3.pyre.model.NodeActionResult
import dev.jhh3.pyre.model.NodeConnection
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.theme.PyreTheme
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.test.TestCoroutineScheduler
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/** A fake machine: waiting with a synthetic code, completed after `approveAfter` status reads. */
private class LoginNode(private val approveAfter: Int) : FakePyreApi() {
    val actions = mutableListOf<String>()
    private var statuses = 0

    override suspend fun nodeAction(req: NodeActionRequest): NodeActionResult {
        actions += req.action
        var v = DeviceLoginView(
            loginId = "dlogin_synth", provider = "openai", nodeId = req.nodeId, state = "waiting",
            verificationUrl = "https://auth.openai.com/codex/device", userCode = "SYNT-H3TIC",
        )
        when (req.action) {
            DeviceLoginActions.STATUS -> if (++statuses >= approveAfter) {
                v = v.copy(state = "completed", userCode = "", email = "you@synthetic.invalid", rotaName = "you@synthetic.invalid")
            }
            DeviceLoginActions.CANCEL -> v = v.copy(state = "cancelled", userCode = "")
        }
        return NodeActionResult(commandId = "cmd_synth", outcome = CommandOutcome.SUCCEEDED, result = PyreJson.wire.encodeToJsonElement(DeviceLoginView.serializer(), v))
    }
}

@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = Application::class)
class DeviceLoginTest {
    @get:Rule val rule = createComposeRule()
    private val sched = TestCoroutineScheduler()
    private val scope = CoroutineScope(SupervisorJob() + UnconfinedTestDispatcher(sched))
    private val mac = Node("node_synth-mac", name = "synth-mac", connection = NodeConnection(state = ConnectionState.CONNECTED))
    private val off = Node("node_synth-off", name = "synth-off")

    @After fun tearDown() = scope.cancel()

    @Test fun showsCodeThenCompletion() {
        val api = LoginNode(approveAfter = 3)
        val c = DeviceLoginController(testRepo(api), scope, paceMillis = 1000)
        rule.setContent { PyreTheme { DeviceLoginDialog(c, listOf(mac, off)) {} } }
        rule.onNodeWithText("Add login on a machine").assertExists()
        rule.onNodeWithText("synth-off").assertDoesNotExist() // offline machines are not offered
        rule.onNodeWithText("synth-mac").performClick()
        rule.waitForIdle()
        assertEquals("waiting", c.ui.value.view?.state)
        rule.onNodeWithText("SYNT-H3TIC").assertExists()
        sched.advanceUntilIdle() // three paced status reads later the owner has approved
        rule.waitForIdle()
        assertEquals("completed", c.ui.value.view?.state)
        rule.onNodeWithText("Logged in as you@synthetic.invalid", substring = true).assertExists()
        assertEquals(listOf(DeviceLoginActions.START, DeviceLoginActions.STATUS, DeviceLoginActions.STATUS, DeviceLoginActions.STATUS), api.actions)
    }

    @Test fun waitingShowsCodeAndCancelStopsTheLogin() {
        val api = LoginNode(approveAfter = Int.MAX_VALUE)
        val c = DeviceLoginController(testRepo(api), scope, paceMillis = 1000)
        var closed = false
        rule.setContent { PyreTheme { DeviceLoginDialog(c, listOf(mac)) { closed = true } } }
        rule.onNodeWithText("synth-mac").performClick()
        rule.waitForIdle()
        sched.advanceTimeBy(2500)
        rule.waitForIdle()
        rule.onNodeWithTag("device-login-code").assertExists()
        rule.onNodeWithText("Open link").assertExists()
        rule.onNodeWithText("Cancel login").performClick()
        rule.waitForIdle()
        assertTrue(api.actions.contains(DeviceLoginActions.CANCEL))
        assertTrue(closed)
        assertEquals(null, c.ui.value.view)
    }
}
