package dev.jhh3.pyre.ui

import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.launch.QuickLaunch
import dev.jhh3.pyre.ui.launch.QuickLine
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class QuickLaunchTest {
    private fun entry(name: String, node: String, path: String, used: String? = null, active: String = "2026-10-03T00:00:00Z") =
        RepoEntry(name = name, path = path, remoteUrl = "git@example.invalid:synth/$name.git", nodeId = "node_$node", nodeName = node, lastUsedAt = used, lastActivity = active)

    private val index = listOf(
        entry("tundra", "synth-linux", "/home/synth/code/tundra", used = "2026-10-04T10:00:00Z"),
        entry("tundra", "synth-mac", "/Users/synth/code/tundra"),
        entry("tundra-old", "synth-linux", "/home/synth/code/tundra-old"),
        entry("api", "synth-linux", "/home/synth/code/api"),
        entry("web", "synth-linux", "/home/synth/code/web"),
        entry("voyager-web", "synth-mac", "/Users/synth/code/voyager-web"),
    )

    private val nodes = listOf(
        Node(nodeId = "node_synth-linux", name = "synth-linux", connection = NodeConnection(state = ConnectionState.CONNECTED)),
        Node(nodeId = "node_synth-mac", name = "synth-mac", connection = NodeConnection(state = ConnectionState.CONNECTED)),
    )

    @Test fun parseMatchesPyreNew() {
        assertEquals(QuickLine.Parsed(listOf("api", "web"), "server", "codex", "rename the endpoint"), QuickLine.parse("api+web @server codex rename the endpoint"))
        assertEquals(QuickLine.Parsed(listOf("tundra"), "", "", "fix the claude hooks"), QuickLine.parse("tundra fix the claude hooks"))
        assertEquals(QuickLine.Parsed(emptyList(), "", "", ""), QuickLine.parse("   "))
    }

    @Test fun suggestionsRankLikeTheHub() {
        val s = QuickLine.suggest("tund", index)
        assertEquals("tundra", s[0][0].name)
        assertEquals(2, s[0].size) // one repository on two machines is one choice
        assertEquals("synth-linux", s[0][0].nodeName) // used there most recently
        assertEquals("voyager-web", QuickLine.suggest("vw", index)[0][0].name)
        assertTrue(QuickLine.suggest("zzz", index).isEmpty())
        assertEquals(listOf("node_synth-linux"), QuickLine.machines(listOf(QuickLine.suggest("api", index)[0], QuickLine.suggest("web", index)[0]), setOf("node_synth-linux", "node_synth-mac")))
    }

    private inner class Hub : FakePyreApi() {
        val starts = mutableListOf<StartSessionRequest>()
        val actions = mutableListOf<NodeActionRequest>()
        override suspend fun listRepos(req: ListReposRequest) = RepoList(index)
        override suspend fun nodeAction(req: NodeActionRequest): NodeActionResult {
            actions += req
            val p = PyreJson.wire.decodeFromJsonElement(WorktreeCreateParams.serializer(), req.params!!)
            val res = WorktreeCreateResult(cwd = p.repos[0] + "/.worktrees/fix-share-link", branch = "pyre/fix-share-link")
            return NodeActionResult(commandId = "cmd_synth", outcome = CommandOutcome.SUCCEEDED, result = PyreJson.wire.encodeToJsonElement(WorktreeCreateResult.serializer(), res))
        }
        override suspend fun startSession(req: StartSessionRequest): StartSessionResponse {
            starts += req
            return StartSessionResponse(SessionView(Session(sessionId = "ses_synth-new", harness = req.harness)))
        }
    }

    private fun TestScope.quick(hub: Hub, launched: MutableList<String>) = QuickLaunch(testRepo(hub), this, { nodes }) { launched += it }

    @Test fun startsInMainCheckoutOnMostRecentMachine() = runTest {
        val hub = Hub()
        val launched = mutableListOf<String>()
        val q = quick(hub, launched)
        advanceUntilIdle()
        q.setLine("tund fix the share link")
        val p = q.plan()
        assertNull(p.problem)
        assertEquals("node_synth-linux", p.nodeId)
        assertEquals("claude", p.harness)
        q.launch()
        advanceUntilIdle()
        assertEquals(listOf("ses_synth-new"), launched)
        val req = hub.starts.single()
        assertEquals("/home/synth/code/tundra", req.cwd)
        assertEquals("fix the share link", req.initialPrompt)
        assertTrue(hub.actions.isEmpty())
    }

    @Test fun freshCreatesWorktreeFirstAndMachineChipWins() = runTest {
        val hub = Hub()
        val q = quick(hub, mutableListOf())
        advanceUntilIdle()
        q.setLine("tundra shell fix share link")
        q.selectMachine("node_synth-mac")
        q.toggleFresh()
        q.launch()
        advanceUntilIdle()
        val a = hub.actions.single()
        assertEquals(RepoOps.ACTION_WORKTREE_CREATE, a.action)
        assertEquals("node_synth-mac", a.nodeId)
        val req = hub.starts.single()
        assertEquals("/Users/synth/code/tundra/.worktrees/fix-share-link", req.cwd)
        assertEquals("shell", req.harness)
    }

    @Test fun problemsBlockStart() = runTest {
        val hub = Hub()
        val q = quick(hub, mutableListOf())
        advanceUntilIdle()
        q.setLine("tundra-old+voyager")
        assertNotNull(q.plan().problem) // not on one machine
        q.setLine("zzz")
        assertTrue(q.plan().problem!!.contains("zzz"))
        q.launch()
        advanceUntilIdle()
        assertTrue(hub.starts.isEmpty())
        q.setLine("") ; q.pick(0)
        assertTrue(q.ui.value.line.startsWith("tundra "))
    }
}
