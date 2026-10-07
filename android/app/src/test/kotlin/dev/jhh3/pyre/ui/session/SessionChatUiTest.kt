package dev.jhh3.pyre.ui.session

import android.app.Application
import android.graphics.Bitmap
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.captureToImage
import androidx.compose.ui.test.hasText
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onFirst
import androidx.compose.ui.test.onLast
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.onRoot
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performScrollToNode
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.model.ReadSessionRequest
import dev.jhh3.pyre.model.ReadSessionResponse
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.model.TranscriptEntry
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.Fixtures
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.theme.PyreTheme
import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config
import org.robolectric.annotation.GraphicsMode

/**
 * The session chat on the JVM: opens at the newest message, stays pinned
 * while at the bottom, shows the "New messages" pill when scrolled up, and
 * tool runs are one collapsed line. Also renders build/session-chat.png
 * (synthetic data) for review.
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = Application::class, qualifiers = "w411dp-h891dp")
@GraphicsMode(GraphicsMode.Mode.NATIVE)
class SessionChatUiTest {
    @get:Rule val rule = createComposeRule()

    private val session: SessionView = Fixtures.bundle("03").sessions.first { it.session.sessionId == "ses_synth-b" }

    private inner class Hub(val entries: MutableList<TranscriptEntry>) : FakePyreApi() {
        val limits = mutableListOf<Int>()
        override suspend fun getSession(id: String) = session
        override suspend fun readSession(req: ReadSessionRequest): ReadSessionResponse {
            limits += req.limit
            val after = req.cursor.toLongOrNull() ?: 0
            var sel = entries.filter { it.seq > after }
            if (req.tail && sel.size > req.limit) sel = sel.takeLast(req.limit)
            return ReadSessionResponse(entries = sel, nextCursor = sel.lastOrNull()?.seq?.toString() ?: req.cursor)
        }
    }

    private fun e(seq: Long, kind: String, text: String, min: Int = seq.toInt()) =
        TranscriptEntry(seq = seq, runId = "run_synth", at = "2026-10-06T10:%02d:00Z".format(min % 60), kind = kind, text = text)

    /** 30 turns: prompt, agent reply, a run of tools, agent wrap-up. */
    private fun turns(n: Int): MutableList<TranscriptEntry> {
        val out = mutableListOf<TranscriptEntry>()
        var s = 0L
        for (i in 0 until n) {
            out += e(++s, "user", "Synthetic prompt $i", i)
            out += e(++s, "assistant", "Synthetic reply $i", i)
            out += e(++s, "tool", "Bash: make synth-$i", i)
            out += e(++s, "tool", "tool result", i)
            out += e(++s, "tool", "Edit: /synth/file$i.kt", i)
        }
        return out
    }

    @Test
    fun opensAtTheNewestMessageAndStaysPinned() {
        val hub = Hub(turns(30))
        val vm = SessionDetailViewModel(testRepo(hub), "ses_synth-b")
        rule.setContent { PyreTheme(dynamicColor = false) { SessionDetailScreen(vm, {}, {}) } }
        rule.waitForIdle()
        rule.onNodeWithText("Synthetic reply 29").assertIsDisplayed()
        rule.onNodeWithText("Synthetic prompt 0").assertDoesNotExist() // far above, not composed
        // Tools are one collapsed line per run.
        rule.onNodeWithText("Ran make synth-29, edited file29.kt").assertIsDisplayed()
        assertEquals(0, rule.onAllNodesWithTag("tool-call").fetchSemanticsNodes().size)
        rule.onNodeWithText("Ran make synth-29, edited file29.kt").performClick()
        rule.onNodeWithText("$ make synth-29").assertIsDisplayed()

        // New activity while at the bottom keeps the newest message in view.
        hub.entries += e(151, "assistant", "Synthetic late reply")
        vm.poll()
        rule.waitForIdle()
        rule.onNodeWithText("Synthetic late reply").assertIsDisplayed()
        rule.onNodeWithTag("new-messages").assertDoesNotExist()
    }

    @Test
    fun scrolledUpShowsNewMessagesPill() {
        val hub = Hub(turns(30))
        val vm = SessionDetailViewModel(testRepo(hub), "ses_synth-b")
        rule.setContent { PyreTheme(dynamicColor = false) { SessionDetailScreen(vm, {}, {}) } }
        rule.waitForIdle()
        rule.onNodeWithTag("session-detail").performScrollToNode(hasText("Synthetic prompt 0"))
        rule.onNodeWithText("Synthetic prompt 0").assertIsDisplayed()
        hub.entries += e(151, "assistant", "Synthetic late reply")
        vm.poll()
        rule.waitForIdle()
        // The page the user is reading does not move; a pill offers the way down.
        rule.onNodeWithText("Synthetic prompt 0").assertIsDisplayed()
        rule.onNodeWithTag("new-messages").assertIsDisplayed().performClick()
        rule.waitForIdle()
        rule.onNodeWithText("Synthetic late reply").assertIsDisplayed()
        rule.onNodeWithTag("new-messages").assertDoesNotExist()
    }

    @Test
    fun scrollingToTheTopLoadsOlderActivity() {
        val hub = Hub(turns(30)) // 150 entries; the first read takes the latest 100
        val vm = SessionDetailViewModel(testRepo(hub), "ses_synth-b")
        rule.setContent { PyreTheme(dynamicColor = false) { SessionDetailScreen(vm, {}, {}) } }
        rule.waitForIdle()
        assertTrue(vm.ui.value.canLoadOlder)
        rule.onNodeWithTag("session-detail").performScrollToNode(hasText("Synthetic prompt 10"))
        rule.waitForIdle()
        rule.onNodeWithTag("session-detail").performScrollToNode(hasText("Synthetic prompt 0"))
        rule.onNodeWithText("Start of this run").assertExists()
        assertEquals(150, vm.ui.value.transcript.size)
        assertTrue(hub.limits.any { it > 100 })
    }

    @Test
    fun rendersSyntheticSession() {
        var s = 0L
        fun x(kind: String, text: String, at: String) = TranscriptEntry(seq = ++s, runId = "run_synth", at = at, kind = kind, text = text)
        val entries = mutableListOf(
            x("user", "The share link should redirect to the synth app. Can you fix it?", "2026-10-06T09:00:00Z"),
            x("assistant", "I'll look at the **proxy** first.", "2026-10-06T09:00:10Z"),
            x("tool", "Bash: git status", "2026-10-06T09:00:12Z"),
            x("tool", "Bash: rg -n redirect src/; Read: /synth/src/proxy.ts", "2026-10-06T09:00:15Z"),
            x("tool", "Bash: npm test -- proxy", "2026-10-06T09:00:20Z"),
            x("tool", "Edit: /synth/src/proxy.ts", "2026-10-06T09:00:30Z"),
            x("tool", "Bash: npm test", "2026-10-06T09:01:00Z"),
            x(
                "assistant",
                "## Fixed\nThe redirect now keeps the query string.\n\n- `proxy.ts`: forward `?share=` intact\n- added a test\n\n" +
                    "```ts\nreturn NextResponse.redirect(new URL(`/s/${'$'}{id}${'$'}{search}`, req.url), 308)\n```\n\n" +
                    "| check | result |\n|---|---|\n| unit tests | 42 passed |\n| lint | clean |",
                "2026-10-06T09:01:30Z",
            ),
            x("system", "turn ended (1m32.4s)", "2026-10-06T09:01:31Z"),
            x("pyre_message", "[pyre-msg:msg_synth9] from session:ses_synth-peer (reviewer) (reply to msg_synth8):\nLooks good. Please also update the docs.", "2026-10-06T10:15:00Z"),
            x("tool", "WebSearch", "2026-10-06T10:15:05Z"),
            x("tool", "Edit: /synth/docs/share.md", "2026-10-06T10:15:20Z"),
            x("assistant", "Docs updated.", "2026-10-06T10:15:40Z"),
        )
        val vm = SessionDetailViewModel(testRepo(Hub(entries)), "ses_synth-b")
        rule.setContent { PyreTheme(dynamicColor = false) { SessionDetailScreen(vm, {}, {}) } }
        rule.waitForIdle()
        rule.onNodeWithText("Docs updated.").assertIsDisplayed()
        rule.onNodeWithText("Ran 4 shell commands, read proxy.ts, edited proxy.ts").assertExists()
        rule.onNodeWithText("Message from reviewer · reply").assertExists()
        rule.onAllNodesWithTag("md-table").onFirst().assertExists()
        val bmp = rule.onRoot().captureToImage().asAndroidBitmap()
        val out = File(System.getProperty("pyre.screens.dir") ?: "build").resolve("session-chat.png")
        out.parentFile?.mkdirs()
        out.outputStream().use { bmp.compress(Bitmap.CompressFormat.PNG, 100, it) }
    }
}
