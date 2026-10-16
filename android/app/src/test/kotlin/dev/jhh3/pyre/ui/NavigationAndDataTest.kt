package dev.jhh3.pyre.ui

import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.data.FileCacheStore
import dev.jhh3.pyre.data.InMemoryBlobStore
import dev.jhh3.pyre.data.IntentIds
import dev.jhh3.pyre.data.Load
import dev.jhh3.pyre.files.ArtifactFiles
import dev.jhh3.pyre.files.Verified
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.Fixtures
import dev.jhh3.pyre.testing.fixedClock
import dev.jhh3.pyre.testing.offline
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.nav.DeepLinks
import dev.jhh3.pyre.ui.nav.Screen
import dev.jhh3.pyre.ui.sessions.SessionsViewModel
import dev.jhh3.pyre.ui.viewer.HtmlPreviewViewModel
import dev.jhh3.pyre.ui.viewer.MarkdownParser
import dev.jhh3.pyre.ui.viewer.MdBlock
import java.nio.file.Files
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class NavigationAndDataTest {
    @Test
    fun deepLinksMapToScreens() {
        assertEquals(Screen.Approval("apr_synth-pending"), DeepLinks.parse("pyre://approval/apr_synth-pending"))
        assertEquals(Screen.SessionDetail("ses_1"), DeepLinks.parse("pyre://session/ses_1"))
        assertEquals(Screen.Message("msg_1"), DeepLinks.parse("pyre://message/msg_1"))
        assertEquals(Screen.Artifact(versionId = "ver_1"), DeepLinks.parse("pyre://artifact_version/ver_1"))
        assertEquals(Screen.Node("node_1"), DeepLinks.parse("pyre://node/node_1"))
        assertEquals(Screen.Comment("cmt_1"), DeepLinks.parse("pyre://comment/cmt_1"))
        assertTrue(DeepLinks.parse("pyre://pair?hub=x&id=y&code=z") is Screen.Pairing)
        assertNull(DeepLinks.parse("https://evil.example/approval/apr_1"))
        assertNull(DeepLinks.parse("pyre://approval/../../x"))
        // Inbox links open the inbox; an item link resolves (ask sheet or session); an ask link opens its sheet.
        assertEquals(Screen.AttentionLink("att_1"), DeepLinks.parse("pyre://attention/att_1"))
        assertEquals(Screen.Inbox(), DeepLinks.parse("pyre://attention/inbox"))
        assertEquals(Screen.Inbox(openAskId = "ask_1"), DeepLinks.parse("pyre://ask/ask_1"))
        // Every deep link in the fixtures resolves.
        Fixtures.files().forEach { f ->
            Fixtures.bundle(f.name.take(2)).attention.forEach { assertNotNull(it.deepLink, DeepLinks.parse(it.deepLink)) }
        }
    }

    @Test
    fun idsAreUlidShapedAndMonotonic() {
        val a = Ids.new("op", 1_000)
        val b = Ids.new("op", 1_000)
        assertTrue(Regex("^op_[0-9A-HJKMNP-TV-Z]{26}$").matches(a))
        assertTrue(a < b)
    }

    @Test
    fun intentIdsAreStableUntilCompleted() {
        val store = InMemoryBlobStore()
        val k = IntentIds.approvalAnswer("apr_1", "allow", "")
        val first = IntentIds(store).forIntent(k)
        assertEquals("survives restart", first, IntentIds(store).forIntent(k))
        IntentIds(store).complete(k)
        assertNotEquals(first, IntentIds(store).forIntent(k))
    }

    @Test
    fun offlineListsComeFromCacheWithTimestamp() = runBlocking {
        val sessions = Fixtures.bundle("03").sessions
        var down = false
        val api = object : FakePyreApi() {
            override suspend fun listSessions(req: ListSessionsRequest): Page<SessionView> = if (down) throw offline() else Page(sessions)
        }
        val repo = testRepo(api)
        assertNull((repo.sessions() as Load.Ready).cachedAt)
        down = true
        val cached = repo.sessions() as Load.Ready
        assertEquals(fixedClock.now(), cached.cachedAt)
        assertTrue(cached.error!!.transport)
        assertEquals(2, cached.value.size)
        assertTrue(!repo.health.state.value.reachable)
    }

    @Test
    fun fileCacheRoundTrips() {
        val dir = Files.createTempDirectory("pyre-cache").toFile()
        val c = FileCacheStore(dir, fixedClock)
        val p = Fixtures.bundle("06").projects
        c.write("projects", kotlinx.serialization.builtins.ListSerializer(Project.serializer()), p)
        val back = FileCacheStore(dir, fixedClock).read("projects", kotlinx.serialization.builtins.ListSerializer(Project.serializer()))!!
        assertEquals(p, back.value)
        dir.deleteRecursively()
    }

    @Test
    fun sessionsGroupedByNodeWithAttentionFirst() {
        val b = Fixtures.bundle("03")
        val groups = SessionsViewModel.group(b.sessions.map { if (it.session.sessionId == "ses_synth-a") it.copy(attention = 2) else it }, b.nodes)
        assertEquals(listOf("synth-linux", "synth-mac"), groups.map { it.title })
    }

    @Test
    fun flaggedSessionsSortFirstInEveryGrouping() {
        val b = Fixtures.bundle("03")
        val last = b.sessions.last().session.sessionId
        val flagged = b.sessions.map {
            if (it.session.sessionId == last) it.copy(flag = dev.jhh3.pyre.model.SessionFlag(level = "needs_you", reason = "approval", count = 1)) else it
        }
        val byProject = SessionsViewModel.group(flagged, b.nodes, grouping = dev.jhh3.pyre.ui.sessions.Grouping.PROJECT)
        val home = byProject.first { g -> g.sessions.any { it.session.sessionId == last } }
        assertEquals(last, home.sessions.first().session.sessionId)
        val byAttn = SessionsViewModel.group(flagged, b.nodes, grouping = dev.jhh3.pyre.ui.sessions.Grouping.NEEDS_YOU)
        assertEquals("Needs you", byAttn.first().title)
        assertEquals(listOf(last), byAttn.first().sessions.map { it.session.sessionId })
        assertEquals("! needs you · approval", dev.jhh3.pyre.domain.Flags.label(flagged.last()))
    }

    @Test
    fun artifactDigestVerification() {
        val dir = Files.createTempDirectory("pyre-art").toFile()
        val v = Fixtures.bundle("06").versions.first()
        val content = Fixtures.bundle("06").contents.getValue(v.versionId)
        val f = ArtifactFiles.target(dir, v).apply { parentFile!!.mkdirs(); writeText(content) }
        val real = v.copy(digest = "sha256:" + ArtifactFiles.sha256(f), size = f.length())
        assertTrue(ArtifactFiles.verify(f, real) is Verified.Ok)
        assertTrue(ArtifactFiles.verify(f, real.copy(digest = "sha256:" + "0".repeat(64))) is Verified.Mismatch)
        assertTrue("non-sha256 digests are reported as unverifiable", ArtifactFiles.verify(f, v.copy(digest = "sha256:synthetic")) is Verified.Unverifiable)
        // The shared fixture's inline contents match their published digests.
        Fixtures.bundle("06").versions.forEach { ver ->
            val vf = ArtifactFiles.target(dir, ver).apply { parentFile!!.mkdirs(); writeText(Fixtures.bundle("06").contents.getValue(ver.versionId)) }
            assertTrue(ver.versionId, ArtifactFiles.verify(vf, ver) is Verified.Ok)
        }
        assertEquals("report.md", ArtifactFiles.safeName(v))
        assertEquals("passwd", ArtifactFiles.safeName(v.copy(filename = "../../etc/passwd")))
        dir.deleteRecursively()
    }

    @Test
    fun htmlPreviewOnlyForConcreteHttpsPreviewUrls() {
        val ep = HubEndpoint("https://hub.synthetic:8443")
        assertNull("template token not issued", HtmlPreviewViewModel.resolve(ep, "/preview/ver_1/{token}"))
        assertNull(HtmlPreviewViewModel.resolve(ep, ""))
        assertNull("cleartext", HtmlPreviewViewModel.resolve(ep, "http://preview.synthetic/preview/ver_1/t"))
        assertNull("not a preview path", HtmlPreviewViewModel.resolve(ep, "/v1/content/ver_1"))
        assertNull("relative without slash", HtmlPreviewViewModel.resolve(ep, "preview/ver_1/t"))
        assertEquals("https://hub.synthetic:8443/preview/ver_1/tok", HtmlPreviewViewModel.resolve(ep, "/preview/ver_1/tok").toString())
        assertEquals(
            "separate preview origin allowed",
            "https://preview.synthetic/preview/ver_1/tok",
            HtmlPreviewViewModel.resolve(ep, "https://preview.synthetic/preview/ver_1/tok").toString(),
        )
    }

    @Test
    fun markdownBlocks() {
        val blocks = MarkdownParser.parse(Fixtures.bundle("06").revisions.last().content + "\n- one\n```\ncode\n```\n")
        assertEquals(MdBlock.Heading(1, "Brief"), blocks.first())
        assertTrue(blocks.any { it is MdBlock.Bullet })
        assertTrue(blocks.any { it == MdBlock.Code("code") })
    }
}
