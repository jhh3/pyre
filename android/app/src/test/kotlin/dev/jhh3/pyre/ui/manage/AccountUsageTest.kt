package dev.jhh3.pyre.ui.manage

import dev.jhh3.pyre.model.Account
import dev.jhh3.pyre.model.AccountAssignment
import dev.jhh3.pyre.model.AccountObservation
import dev.jhh3.pyre.model.AccountView
import dev.jhh3.pyre.model.DeviceGrant
import dev.jhh3.pyre.model.HarnessInfo
import dev.jhh3.pyre.model.HarnessLogin
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.UsageWindow
import java.time.Duration
import java.time.Instant
import java.time.ZoneId
import java.time.ZoneOffset
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** Pure derivation behind the Accounts screen. All data is synthetic. */
class AccountUsageTest {
    private val now = Instant.parse("2026-10-04T12:00:00Z") // a Sunday
    private val utc: ZoneId = ZoneOffset.UTC
    private val la: ZoneId = ZoneId.of("America/Los_Angeles")

    private fun obs(
        eligibility: String = "eligible",
        fiveHour: Double? = 20.0,
        week: Double? = 30.0,
        stale: Boolean = false,
        detail: String = "",
        node: String = "node_synth-mac",
        resetAt: String? = null,
        extra: List<UsageWindow> = emptyList(),
    ) = AccountObservation(
        accountId = "acct_synth-a",
        provider = "anthropic",
        reportingNode = node,
        observedAt = "2026-10-04T11:58:00Z",
        eligibility = eligibility,
        windows = listOfNotNull(
            fiveHour?.let { UsageWindow("five_hour", it, "2026-10-04T14:14:00Z") },
            week?.let { UsageWindow("seven_day", it, "2026-10-06T15:00:00Z") },
        ) + extra,
        resetAt = resetAt,
        stale = stale,
        detail = detail,
    )

    private fun grant(acct: String = "acct_synth-a", node: String = "node_synth-mac", state: String = "valid", harness: String = "claude") =
        DeviceGrant("grant_synth-$acct-$node", accountId = acct, nodeId = node, harness = harness, kind = "claude-profile-store", state = state)

    private fun view(
        id: String = "acct_synth-a",
        provider: String = "anthropic",
        latest: AccountObservation? = obs(),
        byNode: List<AccountObservation> = emptyList(),
        grants: List<DeviceGrant> = listOf(grant(id)),
        disabled: Boolean = false,
        assignments: List<AccountAssignment> = emptyList(),
    ) = AccountView(Account(id, provider = provider, label = "synth-$id", disabled = disabled), latest, byNode, grants, assignments)

    private fun node(id: String, name: String, vararg harnesses: String) =
        Node(id, name = name, harnesses = harnesses.map { HarnessInfo(it, available = true) })

    private fun state(g: DeviceGrant?, o: AccountObservation?) = AccountUsage.state(g, o, now, utc)

    // ---- states ----

    @Test
    fun healthyWhenEligibleFreshAndBelow80() {
        assertEquals(CardState.HEALTHY to null, state(grant(), obs(fiveHour = 79.0, week = 10.0)))
    }

    @Test
    fun nearLimitAt80OnEitherBar() {
        assertEquals(CardState.NEAR_LIMIT, state(grant(), obs(fiveHour = 80.0)).first)
        assertEquals(CardState.NEAR_LIMIT, state(grant(), obs(week = 95.0)).first)
    }

    @Test
    fun modelWindowsAndExtraUsageNeverCountTowardState() {
        val o = obs(extra = listOf(UsageWindow("seven_day_fable", 100.0), UsageWindow("extra_usage", 99.0)))
        assertEquals(CardState.HEALTHY, state(grant(), o).first)
        assertEquals("Fable week 100% · extra usage 99%", AccountUsage.secondaryLine(o))
        assertEquals(listOf("5h", "Week"), AccountUsage.bars(o, now, utc).map { it.label })
    }

    @Test
    fun needsReauthFromGrantStateOrUnusableTakesPrecedence() {
        for (s in listOf("expired", "dead", "revoked")) {
            assertEquals(s, CardState.NEEDS_REAUTH, state(grant(state = s), obs(eligibility = "exhausted")).first)
        }
        val (st, why) = state(grant(), obs(eligibility = "unusable", detail = "synthetic login rejected"))
        assertEquals(CardState.NEEDS_REAUTH, st)
        assertEquals("synthetic login rejected", why)
    }

    @Test
    fun atLimitFromExhaustedCooldownOrFullBar() {
        val (s1, t1) = AccountUsage.state(grant(), obs(eligibility = "exhausted", resetAt = "2026-10-07T22:00:00Z"), now, la)
        assertEquals(CardState.AT_LIMIT, s1)
        assertEquals("until Wed 3:00 PM (in 3d 10h)", t1)

        // No observation reset: the latest reset among windows at 100%.
        val (s2, t2) = state(grant(), obs(eligibility = "cooldown", fiveHour = 100.0, week = 40.0))
        assertEquals(CardState.AT_LIMIT, s2)
        assertEquals("until 2:14 PM (in 2h 14m)", t2)

        assertEquals(CardState.AT_LIMIT, state(grant(), obs(week = 100.0)).first)
        // A stale "eligible" with a full bar is not trusted: unknown.
        assertEquals(CardState.UNKNOWN, state(grant(), obs(week = 100.0, stale = true)).first)
    }

    @Test
    fun unknownExplainsWhyInPlainWords() {
        assertEquals(CardState.UNKNOWN to "no usage observation", state(grant(), null))
        assertEquals(CardState.UNKNOWN to "synthetic probe failed", state(grant(), obs(eligibility = "unknown", detail = "synthetic probe failed")))
        assertEquals(CardState.UNKNOWN to "synthetic probe timed out", state(grant(), obs(stale = true, detail = "synthetic probe timed out")))
        assertEquals(CardState.UNKNOWN to "no usage observation", state(grant(), obs(eligibility = "unknown")))
    }

    private val expired = "access token expired; usage probe never refreshes (the harness refreshes on its next use)"
    private val codex = "codex rota account not checked out; usage needs its app-server (unknown until checked out)"

    @Test
    fun onUseLoginsAreAvailableByLastKnownWindows() {
        assertEquals(CardState.READY_ON_USE to null, state(grant(), obs(eligibility = "unknown", stale = true, week = 2.0, detail = expired)))
        assertEquals("Ready · refreshes on use", CardState.READY_ON_USE.label)
        assertEquals(
            CardState.READY_ON_USE to "usage visible once checked out",
            state(grant(harness = "codex"), obs(eligibility = "unknown", fiveHour = null, week = null, detail = codex)),
        )
        assertEquals(CardState.NEAR_LIMIT to "refreshes on use", state(grant(), obs(eligibility = "unknown", stale = true, week = 97.0, detail = expired)))
        assertEquals(CardState.AT_LIMIT, state(grant(), obs(eligibility = "unknown", stale = true, week = 100.0, detail = expired)).first)
        // The 5h reset already passed: that window is empty again.
        val passed = obs(eligibility = "unknown", stale = true, fiveHour = null, week = null, detail = expired,
            extra = listOf(UsageWindow("five_hour", 100.0, "2026-10-04T11:00:00Z")))
        assertEquals(CardState.READY_ON_USE, state(grant(), passed).first)
    }

    @Test
    fun staleObservationKeepsLastKnownBars() {
        val v = view(latest = obs(eligibility = "unknown", stale = true, fiveHour = 61.0, week = 92.0, detail = "synthetic probe timed out"))
        val c = AccountUsage.card(v, "node_synth-mac", v.grants.first(), emptyMap(), emptyMap(), now, utc)
        assertEquals(CardState.UNKNOWN, c.state)
        assertTrue(c.lastKnown)
        assertEquals(listOf(UsageLevel.LOW, UsageLevel.CRITICAL), c.bars.map { it.level })
        assertEquals("Observed 2m ago", c.observed)
    }

    // ---- per node ----

    @Test
    fun eachNodeUsesItsOwnObservationThenLatest() {
        val mac = obs(node = "node_synth-mac", fiveHour = 10.0)
        val linux = obs(node = "node_synth-linux", eligibility = "exhausted", fiveHour = 100.0)
        val v = view(
            latest = linux,
            byNode = listOf(linux, mac),
            grants = listOf(grant(node = "node_synth-mac"), grant(node = "node_synth-linux"), grant(node = "node_synth-pi")),
            assignments = listOf(AccountAssignment("asg_synth-1", accountId = "acct_synth-a", sessionId = "ses_synth-1", nodeId = "node_synth-mac")),
        )
        val nodes = listOf(node("node_synth-mac", "synth-mac"), node("node_synth-linux", "synth-linux"), node("node_synth-pi", "synth-pi"))
        val s = AccountUsage.sections(listOf(v), nodes, mapOf("ses_synth-1" to "synth refactor"), now, utc).single()
        val byNode = s.groups.associate { it.title to it.cards.single() }
        assertEquals(CardState.HEALTHY, byNode["synth-mac"]!!.state)
        assertEquals(listOf("In use by synth refactor on synth-mac"), byNode["synth-mac"]!!.inUse)
        assertEquals(CardState.AT_LIMIT, byNode["synth-linux"]!!.state)
        assertTrue(byNode["synth-linux"]!!.inUse.isEmpty())
        // No observation from the pi: falls back to latest.
        assertEquals(CardState.AT_LIMIT, byNode["synth-pi"]!!.state)
        assertEquals(listOf("synth-linux", "synth-mac", "synth-pi"), s.groups.map { it.title })
    }

    @Test
    fun assignmentWithUnknownSessionFallsBackToId() {
        val v = view(assignments = listOf(AccountAssignment("asg_synth-2", sessionId = "ses_synth-gone", nodeId = "node_synth-mac")))
        val c = AccountUsage.card(v, "node_synth-mac", v.grants.first(), mapOf("node_synth-mac" to "synth-mac"), emptyMap(), now, utc)
        assertEquals(listOf("In use by ses_synth-gone on synth-mac"), c.inUse)
    }

    // ---- ambient and disabled ----

    @Test
    fun ambientCardForNodesWithHarnessButNoGrant() {
        val nodes = listOf(
            node("node_synth-mac", "synth-mac", "claude", "codex"),
            node("node_synth-linux", "synth-linux", "claude"),
            Node("node_synth-off", name = "synth-off", harnesses = listOf(HarnessInfo("claude", available = false))),
        )
        val sections = AccountUsage.sections(listOf(view()), nodes, emptyMap(), now, utc)
        val anthropic = sections.first { it.provider == "anthropic" }
        val linux = anthropic.groups.first { it.title == "synth-linux" }.cards.single()
        assertEquals(CardState.AMBIENT, linux.state)
        assertEquals(AccountUsage.AMBIENT_TITLE, linux.title)
        assertEquals("usage unknown — Pyre does not observe your own login", linux.detail)
        assertFalse("mac has a grant", anthropic.groups.first { it.title == "synth-mac" }.cards.any { it.state == CardState.AMBIENT })
        assertTrue("unavailable harness: no card", anthropic.groups.none { it.title == "synth-off" })
        // Ambient logins never count toward the pool.
        assertEquals(1 to 1, anthropic.ready to anthropic.total)

        val openai = sections.first { it.provider == "openai" }
        assertEquals("OpenAI · not managed", openai.header)
        assertEquals(CardState.AMBIENT, openai.groups.single().cards.single().state)
        assertFalse(openai.warn)
    }

    @Test
    fun disabledAccountIsOneMutedCardAndNotCounted() {
        val v = view(id = "acct_synth-off", disabled = true, grants = listOf(grant("acct_synth-off", "node_synth-mac"), grant("acct_synth-off", "node_synth-linux")))
        val s = AccountUsage.sections(listOf(view(), v), emptyList(), emptyMap(), now, utc).single()
        val disabled = s.groups.flatMap { it.cards }.filter { it.accountId == "acct_synth-off" }
        assertEquals(1, disabled.size)
        assertEquals(CardState.DISABLED, disabled.single().state)
        assertEquals("disabled", disabled.single().detail)
        assertEquals("Anthropic · 1 ready", s.header)
    }

    // ---- token accounts: one card per identity ----

    private fun tokenGrant(acct: String, node: String, state: String = "valid") =
        DeviceGrant("grant_synth-tok-$acct-$node", accountId = acct, nodeId = node, harness = "claude", kind = AccountUsage.TOKEN_KIND, state = state)

    @Test
    fun tokenAccountIsOneCardForAllMachines() {
        val nodes = listOf(node("node_synth-mac", "synth-mac", "claude"), node("node_synth-linux", "synth-linux", "claude"))
        val tok = view(
            id = "acct_synth-t",
            byNode = listOf(obs(node = "node_synth-mac", fiveHour = 12.0), obs(node = "node_synth-linux", fiveHour = 12.0)),
            // A rota login kept as fallback on the mac folds into the same card.
            grants = listOf(tokenGrant("acct_synth-t", "node_synth-mac"), tokenGrant("acct_synth-t", "node_synth-linux"), grant("acct_synth-t", "node_synth-mac")),
        )
        val rota = view(id = "acct_synth-r", grants = listOf(grant("acct_synth-r", "node_synth-mac"), grant("acct_synth-r", "node_synth-linux")))
        val s = AccountUsage.sections(listOf(tok, rota), nodes, emptyMap(), now, utc).single()
        val tokCards = s.groups.flatMap { it.cards }.filter { it.accountId == "acct_synth-t" }
        assertEquals(1, tokCards.size)
        val c = tokCards.single()
        assertEquals("acct_synth-t@token", c.key)
        assertEquals(AccountUsage.ALL_MACHINES, c.machines)
        assertEquals(AccountUsage.TOKEN_LOGIN, c.login)
        assertEquals("All machines", s.groups.first().title)
        assertEquals(2, s.groups.flatMap { it.cards }.count { it.accountId == "acct_synth-r" })
        assertTrue("no ambient card on machines with grants", s.groups.flatMap { it.cards }.none { it.state == CardState.AMBIENT })
        assertEquals(2, s.total)
    }

    @Test
    fun tokenOnSomeMachinesNamesThem() {
        val nodes = listOf(node("node_synth-mac", "synth-mac", "claude"), node("node_synth-linux", "synth-linux", "claude"))
        val tok = view(id = "acct_synth-t", grants = listOf(tokenGrant("acct_synth-t", "node_synth-mac", state = "revoked"), tokenGrant("acct_synth-t", "node_synth-mac")))
        val s = AccountUsage.sections(listOf(tok), nodes, emptyMap(), now, utc).single()
        val g = s.groups.first()
        assertEquals("Synth-mac", g.title)
        assertEquals("synth-mac", g.cards.single().machines)
        assertTrue("the valid grant represents the account", g.cards.single().state != CardState.NEEDS_REAUTH)
        assertEquals(CardState.AMBIENT, s.groups.first { it.title == "synth-linux" }.cards.single().state)
    }

    // ---- pool count ----

    @Test
    fun poolCountsAccountsWithAnyReadyCard() {
        val a = view(id = "acct_synth-a", latest = obs(fiveHour = 10.0))
        val b = view(id = "acct_synth-b", latest = obs(eligibility = "exhausted"))
        val c = view(id = "acct_synth-c", latest = null)
        // d: limited on the mac, healthy on the linux box -> ready.
        val d = view(
            id = "acct_synth-d",
            latest = obs(node = "node_synth-mac", eligibility = "exhausted"),
            byNode = listOf(obs(node = "node_synth-linux", fiveHour = 5.0), obs(node = "node_synth-mac", eligibility = "exhausted")),
            grants = listOf(grant("acct_synth-d", "node_synth-mac"), grant("acct_synth-d", "node_synth-linux")),
        )
        val s = AccountUsage.sections(listOf(a, b, c, d), emptyList(), emptyMap(), now, utc).single()
        assertEquals("Anthropic · 2 ready · 1 at limit · 1 unknown", s.header)
        assertFalse(s.warn)
        assertFalse(s.bad)
    }

    @Test
    fun headerAmberWhenAllAvailableNearLimitRedWhenNone() {
        val none = AccountUsage.sections(listOf(view(latest = obs(eligibility = "exhausted"))), emptyList(), emptyMap(), now, utc).single()
        assertEquals(0, none.ready)
        assertFalse(none.warn)
        assertTrue(none.bad)
        assertEquals("Anthropic · 0 ready · 1 at limit · nothing available", none.header)

        val near = AccountUsage.sections(
            listOf(view(id = "acct_synth-a", latest = obs(fiveHour = 85.0)), view(id = "acct_synth-b", latest = obs(week = 81.0))),
            emptyList(), emptyMap(), now, utc,
        ).single()
        assertEquals(0, near.ready)
        assertEquals(2, near.near)
        assertTrue(near.warn)
        assertFalse(near.bad)
        assertEquals("Anthropic · 0 ready · 2 near limit", near.header)

        val mixed = AccountUsage.sections(
            listOf(view(id = "acct_synth-a", latest = obs(fiveHour = 85.0)), view(id = "acct_synth-b", latest = obs(week = 10.0))),
            emptyList(), emptyMap(), now, utc,
        ).single()
        assertFalse(mixed.warn)
    }

    // ---- bars and reset text ----

    @Test
    fun barColourBands() {
        assertEquals(UsageLevel.LOW, usageLevel(69.9))
        assertEquals(UsageLevel.HIGH, usageLevel(70.0))
        assertEquals(UsageLevel.HIGH, usageLevel(89.9))
        assertEquals(UsageLevel.CRITICAL, usageLevel(90.0))
    }

    @Test
    fun resetTextSameDayAndLater() {
        assertEquals("resets 2:14 PM · in 2h 14m", AccountUsage.resetText(Instant.parse("2026-10-04T14:14:00Z"), now, utc))
        assertEquals("resets Tue 3:00 PM · in 2d 3h", AccountUsage.resetText(Instant.parse("2026-10-06T15:00:00Z"), now, utc))
        // Local zone and 12-hour clock: 00:00Z is 5:00 PM the previous day in Los Angeles.
        assertEquals("resets 5:00 PM · in 12h", AccountUsage.resetText(Instant.parse("2026-10-05T00:00:00Z"), now, la))
        // Exactly a day away names the day.
        assertEquals("resets Mon 12:00 PM · in 1d", AccountUsage.resetText(Instant.parse("2026-10-05T12:00:00Z"), now, utc))
        assertEquals("resets 11:59 AM · due now", AccountUsage.resetText(Instant.parse("2026-10-04T11:59:00Z"), now, utc))
        val bars = AccountUsage.bars(obs(), now, utc)
        assertEquals("resets 2:14 PM · in 2h 14m", bars[0].resetText)
        assertNull(AccountUsage.bars(obs().copy(windows = listOf(UsageWindow("five_hour", 3.0))), now, utc).single().resetText)
    }

    @Test
    fun spanFormatting() {
        assertEquals("<1m", AccountUsage.span(Duration.ofSeconds(20)))
        assertEquals("14m", AccountUsage.span(Duration.ofMinutes(14)))
        assertEquals("2h", AccountUsage.span(Duration.ofHours(2)))
        assertEquals("23h 59m", AccountUsage.span(Duration.ofMinutes(24 * 60 - 1)))
        assertEquals("3d 14h", AccountUsage.span(Duration.ofHours(86)))
    }

    // ---- the machines' own login (usage.ownCard) ----

    @Test
    fun ownLoginAccountIsOneReadyCardAcrossItsMachines() {
        val login = HarnessLogin(email = "synth-owner@example.invalid")
        fun codexNode(id: String, name: String, l: HarnessLogin?) =
            Node(id, name = name, harnesses = listOf(HarnessInfo("codex", available = true, login = l)))
        val nodes = listOf(
            codexNode("node_synth-a", "synth-a", login),
            codexNode("node_synth-b", "synth-b", login),
            codexNode("node_synth-c", "synth-c", HarnessLogin(email = "other@example.invalid")),
        )
        val own = AccountView(Account("acct_synth-own", provider = "openai", label = "synth-owner", rotaRef = "synth-owner@example.invalid"), null, emptyList(), emptyList(), emptyList())
        val dead = AccountView(Account("acct_synth-old", provider = "openai", label = "synth-old", disabled = true), null, emptyList(), emptyList(), emptyList())
        val sec = AccountUsage.sections(listOf(own, dead), nodes, emptyMap(), now, utc).single { it.provider == "openai" }
        assertEquals(1, sec.ready)
        assertEquals(1, sec.total)
        val cards = sec.groups.flatMap { it.cards }
        val card = cards.single { it.accountId == "acct_synth-own" }
        assertEquals(CardState.READY_ON_USE, card.state)
        assertEquals("synth-a, synth-b", card.machines)
        assertEquals("own login", card.login)
        // Only the machine with another login keeps an "own login (not managed)" card.
        assertEquals(listOf("node_synth-c"), cards.filter { it.state == CardState.AMBIENT }.map { it.nodeId })
        // The disabled account sits under Disabled and is not counted.
        assertTrue(sec.groups.any { it.nodeId == "disabled" })
    }
}
