package dev.jhh3.pyre.ui.manage

import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.model.Account
import dev.jhh3.pyre.model.AccountObservation
import dev.jhh3.pyre.model.AccountView
import dev.jhh3.pyre.model.CredentialKinds
import dev.jhh3.pyre.model.DeviceGrant
import dev.jhh3.pyre.model.Eligibility
import dev.jhh3.pyre.model.Harness
import dev.jhh3.pyre.model.HarnessLogin
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.UsageWindow
import java.time.Duration
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.Locale

/*
 * Pure derivation for the Accounts screen: one card per account per machine
 * that holds a login for it (a token account is one card for all its machines), its state, its two usage bars and the provider's
 * "3 ready · 5 near limit" count. Mirrors internal/usage (Assess, Judge,
 * Pools) so the phone, the TUI, the CLI and the selector count the same. No Compose here, so it is unit-tested on the JVM.
 */

/** Bar colour band: green < 70, amber < 90, red >= 90. */
enum class UsageLevel { LOW, HIGH, CRITICAL }

fun usageLevel(pct: Double): UsageLevel = when {
    pct < 70 -> UsageLevel.LOW
    pct < 90 -> UsageLevel.HIGH
    else -> UsageLevel.CRITICAL
}

enum class CardState(val label: String) {
    NEEDS_REAUTH("Needs re-auth"),
    AT_LIMIT("At limit"),
    UNKNOWN("Unknown"),
    NEAR_LIMIT("Near limit"),
    HEALTHY("Healthy"),

    /** Works, but usage is read only at use (token expired at rest, or Codex not checked out); last-known windows have room. */
    READY_ON_USE("Ready · refreshes on use"),
    DISABLED("Disabled"),

    /** The machine's own harness login, which Pyre neither manages nor observes. */
    AMBIENT("Not managed"),
    ;

    val ready: Boolean get() = this == HEALTHY || this == NEAR_LIMIT || this == READY_ON_USE
}

data class UsageBar(
    val label: String,
    val usedPercent: Double,
    val level: UsageLevel,
    /** "resets 5:00 PM · in 2h 14m"; null when the provider gave no reset. */
    val resetText: String?,
)

data class AccountCard(
    val key: String,
    val accountId: String,
    val title: String,
    /** "" when the account has no login on any machine (or the card is the disabled summary). */
    val nodeId: String,
    val state: CardState,
    /** Why (re-auth detail, "until Thu 3:00 PM (in 3d 14h)", "token refreshes on next use"…). */
    val detail: String?,
    val bars: List<UsageBar> = emptyList(),
    /** Bars come from a stale observation: draw them dimmed and say "last known". */
    val lastKnown: Boolean = false,
    /** Model-scoped weekly windows and extra usage, e.g. "Fable week 37% · extra usage 89%". */
    val secondary: String? = null,
    val inUse: List<String> = emptyList(),
    /** "Observed 2m ago". */
    val observed: String? = null,
    /** How the machine holds the login ("isolated Claude profile"); never a credential. */
    val login: String? = null,
    /** Token account: the machines its token covers ("all machines" or their names); null otherwise. */
    val machines: String? = null,
)

data class NodeGroupCards(val nodeId: String, val title: String, val cards: List<AccountCard>)

data class ProviderSection(
    val provider: String,
    val title: String,
    /** Accounts available with room (each account counted once, by its best login). */
    val ready: Int,
    val total: Int,
    /** Amber header: accounts are available but every one is near its limit. */
    val warn: Boolean,
    val groups: List<NodeGroupCards>,
    val near: Int = 0,
    val atLimit: Int = 0,
    val reauth: Int = 0,
    val unknown: Int = 0,
) {
    /** Red header: no account is available. */
    val bad: Boolean get() = total > 0 && ready + near == 0

    /** "3 ready · 5 near limit · 1 needs re-auth" (same words as usage.Pool.Summary). */
    val summary: String get() {
        val parts = mutableListOf("$ready ready")
        when {
            near > 0 && atLimit > 0 -> parts += "${near + atLimit} near or at limit"
            near > 0 -> parts += "$near near limit"
            atLimit > 0 -> parts += "$atLimit at limit"
        }
        if (reauth > 0) parts += "$reauth needs re-auth"
        if (unknown > 0) parts += "$unknown unknown"
        return parts.joinToString(" · ")
    }

    /** "Anthropic · 3 ready · 5 near limit"; "OpenAI · not managed" when only the machine's own login exists. */
    val header: String get() = when {
        total == 0 -> "$title · not managed"
        bad -> "$title · $summary · nothing available"
        else -> "$title · $summary"
    }
}

object AccountUsage {
    const val FIVE_HOUR = "five_hour"
    const val SEVEN_DAY = "seven_day"
    const val EXTRA_USAGE = "extra_usage"
    private const val MODEL_WEEK_PREFIX = "seven_day_"
    private val REAUTH_GRANT_STATES = setOf("expired", "dead", "revoked")
    private val KNOWN_PROVIDERS = listOf("anthropic", "openai")

    const val AMBIENT_TITLE = "Your own login (not managed)"
    /** contracts.CredOAuthToken: a hub-held token usable on every machine at once. */
    const val TOKEN_KIND = "oauth-token"
    const val TOKEN_LOGIN = "token held by the hub (every machine at once)"
    const val ALL_MACHINES = "all machines"
    private val USABLE_GRANT_STATES = setOf("valid", "expiring")
    const val AMBIENT_DETAIL = "usage unknown — Pyre does not observe your own login"
    const val OWN_LOGIN = "own login"
    const val OWN_NO_USAGE = "own login · usage shows after a Pyre run on it"

    fun providerTitle(p: String): String = when (p) {
        "anthropic" -> "Anthropic"
        "openai" -> "OpenAI"
        "" -> "Other"
        else -> p.replaceFirstChar { it.uppercase() }
    }

    fun providerHarness(p: String): String? = when (p) {
        "anthropic" -> Harness.CLAUDE
        "openai" -> Harness.CODEX
        else -> null
    }

    /** The node's own observation, else the newest from any node. */
    fun observationFor(a: AccountView, nodeId: String): AccountObservation? =
        a.byNode.firstOrNull { it.reportingNode == nodeId && nodeId.isNotEmpty() } ?: a.latest

    private fun window(o: AccountObservation?, name: String): UsageWindow? = o?.windows?.firstOrNull { it.name == name }

    /** Humanized reason an observation is unknown. */
    fun unknownReason(o: AccountObservation?): String {
        val d = o?.detail.orEmpty()
        return when {
            d.contains("access token expired", ignoreCase = true) -> "token refreshes on next use"
            d.contains("not checked out", ignoreCase = true) -> "usage visible once checked out"
            d.isNotBlank() -> d
            else -> "no usage observation"
        }
    }

    /** Why a working login's usage is read only at use (null otherwise); mirrors usage.OnUseOf. */
    fun onUse(o: AccountObservation?): String? {
        if (o == null || o.eligibility != Eligibility.UNKNOWN) return null
        return when {
            o.detail.contains("access token expired", ignoreCase = true) -> "refreshes on use"
            o.detail.contains("not checked out", ignoreCase = true) -> "usage visible once checked out"
            else -> null
        }
    }

    /** The card state, in the documented precedence. Returns state and its secondary text. */
    fun state(grant: DeviceGrant?, o: AccountObservation?, now: Instant, zone: ZoneId = ZoneId.systemDefault()): Pair<CardState, String?> {
        val e = o?.eligibility
        if ((grant != null && grant.state in REAUTH_GRANT_STATES) || e == Eligibility.UNUSABLE) {
            val why = o?.detail?.takeIf { it.isNotBlank() } ?: grant?.detail?.takeIf { it.isNotBlank() } ?: "login ${grant?.state ?: "unusable"}"
            return CardState.NEEDS_REAUTH to why
        }
        onUse(o)?.let { why -> return onUseState(o!!, why, now, zone) }
        val fresh = o != null && !o.stale
        val fiveHour = window(o, FIVE_HOUR)?.usedPercent ?: 0.0
        val week = window(o, SEVEN_DAY)?.usedPercent ?: 0.0
        val limited = e == Eligibility.EXHAUSTED || e == Eligibility.COOLDOWN ||
            (fresh && e == Eligibility.ELIGIBLE && (fiveHour >= 100 || week >= 100))
        if (limited) { // a limited eligibility implies an observation
            val reset = Times.parse(o.resetAt) ?: o.windows
                .filter { it.usedPercent >= 100 }
                .mapNotNull { Times.parse(it.resetAt) }
                .maxOrNull()
            return CardState.AT_LIMIT to (reset?.let { untilText(it, now, zone) } ?: o.detail.takeIf { it.isNotBlank() })
        }
        if (o == null || o.stale || e != Eligibility.ELIGIBLE) return CardState.UNKNOWN to unknownReason(o)
        if (fiveHour >= 80 || week >= 80) return CardState.NEAR_LIMIT to null
        return CardState.HEALTHY to null
    }

    /**
     * A login available on use, judged by its last-known windows (usage.Judge):
     * a window whose reset passed counts as empty; at 100 % with the reset ahead
     * it is at limit; at 80 % near limit; otherwise ready.
     */
    private fun onUseState(o: AccountObservation, why: String, now: Instant, zone: ZoneId): Pair<CardState, String?> {
        val limits = o.windows.filter { it.name == FIVE_HOUR || it.name == SEVEN_DAY }.map { w ->
            val reset = Times.parse(w.resetAt)
            (if (reset != null && !reset.isAfter(now)) 0.0 else w.usedPercent) to reset
        }
        val full = limits.filter { it.first >= 100 }
        if (full.isNotEmpty()) {
            val until = full.mapNotNull { it.second }.maxOrNull()
            return CardState.AT_LIMIT to (until?.let { untilText(it, now, zone) + " (last known)" } ?: "limit reached (last known)")
        }
        val detail = if (why == "refreshes on use") null else why
        if (limits.any { it.first >= 80 }) return CardState.NEAR_LIMIT to ("refreshes on use" + (detail?.let { " · $it" } ?: ""))
        return CardState.READY_ON_USE to detail
    }

    fun bars(o: AccountObservation?, now: Instant, zone: ZoneId = ZoneId.systemDefault()): List<UsageBar> =
        listOf(FIVE_HOUR to "5h", SEVEN_DAY to "Week").mapNotNull { (name, label) ->
            window(o, name)?.let { w -> UsageBar(label, w.usedPercent, usageLevel(w.usedPercent), Times.parse(w.resetAt)?.let { resetText(it, now, zone) }) }
        }

    /** "Fable week 37% · extra usage 89%": windows that are never bars and never count toward state. */
    fun secondaryLine(o: AccountObservation?): String? {
        val parts = o?.windows.orEmpty().filter { it.name != FIVE_HOUR && it.name != SEVEN_DAY }.map { w ->
            val label = when {
                w.name == EXTRA_USAGE -> "extra usage"
                w.name.startsWith(MODEL_WEEK_PREFIX) ->
                    w.name.removePrefix(MODEL_WEEK_PREFIX).replace('_', ' ').replaceFirstChar { it.uppercase() } + " week"
                else -> w.name.replace('_', ' ')
            }
            "$label ${pct(w.usedPercent)}"
        }
        return parts.takeIf { it.isNotEmpty() }?.joinToString(" · ")
    }

    fun pct(v: Double): String = String.format(Locale.ROOT, "%.0f%%", v)

    // ---- time text ----

    private fun clockText(t: Instant, now: Instant, zone: ZoneId): String {
        val time = DateTimeFormatter.ofPattern("h:mm a", Locale.US).withZone(zone).format(t)
        return if (Duration.between(now, t) >= Duration.ofHours(24)) DateTimeFormatter.ofPattern("EEE", Locale.US).withZone(zone).format(t) + " " + time else time
    }

    /** "14m", "2h 14m", "2d 3h". */
    fun span(d: Duration): String {
        val m = d.toMinutes()
        return when {
            m < 1 -> "<1m"
            m < 60 -> "${m}m"
            m < 24 * 60 -> if (m % 60 == 0L) "${m / 60}h" else "${m / 60}h ${m % 60}m"
            else -> {
                val h = m / 60
                if (h % 24 == 0L) "${h / 24}d" else "${h / 24}d ${h % 24}h"
            }
        }
    }

    private fun inText(t: Instant, now: Instant): String = if (t.isAfter(now)) "in " + span(Duration.between(now, t)) else "due now"

    /** "resets 5:00 PM · in 2h 14m"; more than 24h away: "resets Thu 5:00 PM · in 2d 3h". */
    fun resetText(t: Instant, now: Instant, zone: ZoneId = ZoneId.systemDefault()): String =
        "resets ${clockText(t, now, zone)} · ${inText(t, now)}"

    /** "until Thu 3:00 PM (in 3d 14h)". */
    fun untilText(t: Instant, now: Instant, zone: ZoneId = ZoneId.systemDefault()): String =
        "until ${clockText(t, now, zone)} (${inText(t, now)})"

    // ---- screen model ----

    fun card(
        a: AccountView,
        nodeId: String,
        grant: DeviceGrant?,
        nodeNames: Map<String, String>,
        sessionTitles: Map<String, String>,
        now: Instant,
        zone: ZoneId = ZoneId.systemDefault(),
        o: AccountObservation? = observationFor(a, nodeId),
    ): AccountCard {
        val (state, detail) = state(grant, o, now, zone)
        val inUse = a.activeAssignments
            .filter { it.releasedAt == null && (nodeId.isEmpty() || it.nodeId == nodeId) }
            .map { asg ->
                val title = sessionTitles[asg.sessionId]?.takeIf { it.isNotBlank() } ?: asg.sessionId
                val node = nodeNames[asg.nodeId]?.takeIf { it.isNotBlank() } ?: asg.nodeId
                if (node.isEmpty()) "In use by $title" else "In use by $title on $node"
            }
        return AccountCard(
            key = "${a.account.accountId}@$nodeId",
            accountId = a.account.accountId,
            title = a.account.label.ifEmpty { a.account.accountId },
            nodeId = nodeId,
            state = state,
            detail = detail,
            bars = bars(o, now, zone),
            lastKnown = o != null && (o.stale || state == CardState.NEEDS_REAUTH),
            secondary = secondaryLine(o),
            inUse = inUse,
            observed = o?.observedAt?.let { Times.parse(it) }?.let { "Observed " + Times.ago(it, now) },
            login = grant?.kind?.takeIf { it.isNotEmpty() }?.let { CredentialKinds.label(it) },
        )
    }

    /**
     * The single card of a token account (mirrors usage.tokenCard): the healthiest
     * token grant represents it, the newest reading from a machine holding the
     * token decides its state, and every assignment shows. Other grants of the
     * account (a rota login kept as fallback) fold into it.
     */
    fun tokenCard(
        a: AccountView,
        nodes: List<Node>,
        nodeNames: Map<String, String>,
        sessionTitles: Map<String, String>,
        now: Instant,
        zone: ZoneId = ZoneId.systemDefault(),
    ): AccountCard {
        val tok = a.grants.filter { it.kind == TOKEN_KIND }
        val covered = tok.map { it.nodeId }.toSortedSet()
        val g = tok.sortedWith(
            compareByDescending<DeviceGrant> { it.state in USABLE_GRANT_STATES }
                .thenByDescending { Times.parse(it.observedAt) ?: Instant.EPOCH },
        ).firstOrNull()
        val newest = { l: List<AccountObservation> -> l.maxByOrNull { Times.parse(it.observedAt) ?: Instant.EPOCH } }
        val o = newest(a.byNode.filter { it.reportingNode in covered }) ?: newest(a.byNode) ?: a.latest
        val machines = machinesText(covered.toList(), nodes, nodeNames, a.account.provider)
        return card(a, "", g, nodeNames, sessionTitles, now, zone, o).copy(
            key = "${a.account.accountId}@token",
            login = TOKEN_LOGIN,
            machines = machines,
        )
    }

    /** usage.MatchesLogin: the account's rota_ref is the login's email, or the same provider ref. */
    fun matchesLogin(a: Account, l: HarnessLogin?): Boolean {
        if (l == null) return false
        if (l.email.isNotEmpty() && a.rotaRef.equals(l.email, ignoreCase = true)) return true
        return l.providerRef.isNotEmpty() && a.providerRef == l.providerRef
    }

    /** usage.ownLogins: enabled accounts without any Pyre login that are the machines' own login → those machines. */
    fun ownLogins(provider: String, active: List<AccountView>, nodes: List<Node>): Map<String, List<String>> {
        val h = providerHarness(provider) ?: return emptyMap()
        val out = linkedMapOf<String, MutableList<String>>()
        nodes.forEach { n ->
            val login = n.harnesses.firstOrNull { it.harness == h && it.available }?.login ?: return@forEach
            active.firstOrNull { it.grants.isEmpty() && matchesLogin(it.account, login) }?.let { a ->
                out.getOrPut(a.account.accountId) { mutableListOf() } += n.nodeId
            }
        }
        return out.mapValues { (_, v) -> v.sorted() }
    }

    /**
     * The single card of an account that is the machines' own login (usage.ownCard):
     * ready without a reading (runs use it), else judged by the newest reading a
     * Pyre run on it reported; an aged reading uses its last-known windows.
     */
    fun ownCard(
        a: AccountView,
        ids: List<String>,
        nodes: List<Node>,
        nodeNames: Map<String, String>,
        sessionTitles: Map<String, String>,
        now: Instant,
        zone: ZoneId = ZoneId.systemDefault(),
    ): AccountCard {
        val o = a.byNode.filter { it.reportingNode in ids && it.windows.isNotEmpty() }
            .maxByOrNull { Times.parse(it.observedAt) ?: Instant.EPOCH }
        val base = card(a, "", null, nodeNames, sessionTitles, now, zone, o)
        val fresh = o != null && !o.stale && (Times.parse(o.observedAt)?.let { Duration.between(it, now).toMinutes() < 15 } ?: false)
        val (state, detail) = when {
            o == null -> CardState.READY_ON_USE to OWN_NO_USAGE
            fresh -> base.state to base.detail
            else -> onUseState(o, OWN_LOGIN, now, zone)
        }
        return base.copy(
            key = "${a.account.accountId}@own",
            state = state,
            detail = detail,
            login = OWN_LOGIN,
            machines = machinesText(ids, nodes, nodeNames, a.account.provider),
        )
    }

    /** "all machines" when the token covers every machine with the provider's harness, else their names. */
    fun machinesText(covered: List<String>, nodes: List<Node>, nodeNames: Map<String, String>, provider: String): String {
        if (nodes.isEmpty()) return if (covered.size == 1) "1 machine" else "${covered.size} machines"
        val harness = nodes.filter { hasHarness(it, provider) }
        if (harness.isNotEmpty() && harness.all { it.nodeId in covered }) return ALL_MACHINES
        return covered.map { nodeNames[it]?.takeIf { n -> n.isNotBlank() } ?: it }.sortedBy { it.lowercase() }.joinToString(", ")
    }

    /**
     * Provider → machine → card. Accounts with a login on several machines get
     * one card per machine; a token account gets one card in an "All machines"
     * (or "<names>") group; disabled accounts get one muted card; machines that
     * run the provider's harness without any Pyre-managed login get an
     * "own login" card.
     */
    fun sections(
        accounts: List<AccountView>,
        nodes: List<Node>,
        sessionTitles: Map<String, String>,
        now: Instant,
        zone: ZoneId = ZoneId.systemDefault(),
    ): List<ProviderSection> {
        val nodeNames = nodes.associate { it.nodeId to it.name.ifEmpty { it.nodeId } }
        val byProvider = accounts.groupBy { it.account.provider }
        val providers = (KNOWN_PROVIDERS.filter { p -> byProvider.containsKey(p) || nodes.any { hasHarness(it, p) } } +
            byProvider.keys.filterNot { it in KNOWN_PROVIDERS }.sorted())
        return providers.mapNotNull { p -> section(p, byProvider[p].orEmpty(), nodes, nodeNames, sessionTitles, now, zone) }
    }

    private fun hasHarness(n: Node, provider: String): Boolean {
        val h = providerHarness(provider) ?: return false
        return n.harnesses.any { it.harness == h && it.available }
    }

    private fun section(
        provider: String,
        list: List<AccountView>,
        nodes: List<Node>,
        nodeNames: Map<String, String>,
        sessionTitles: Map<String, String>,
        now: Instant,
        zone: ZoneId,
    ): ProviderSection? {
        val active = list.filterNot { it.account.disabled }
        val cards = mutableListOf<AccountCard>()
        val own = ownLogins(provider, active, nodes)
        active.sortedBy { it.account.label.ifEmpty { it.account.accountId } }.forEach { a ->
            if (a.grants.any { it.kind == TOKEN_KIND }) {
                cards += tokenCard(a, nodes, nodeNames, sessionTitles, now, zone)
                return@forEach
            }
            own[a.account.accountId]?.let { ids ->
                cards += ownCard(a, ids, nodes, nodeNames, sessionTitles, now, zone)
                return@forEach
            }
            val grants = a.grants.groupBy { it.nodeId }
            if (grants.isEmpty()) cards += card(a, "", null, nodeNames, sessionTitles, now, zone)
            grants.forEach { (nodeId, gs) ->
                // Several grants on one node (rare): the healthiest one represents it.
                val g = gs.firstOrNull { it.state !in REAUTH_GRANT_STATES } ?: gs.first()
                cards += card(a, nodeId, g, nodeNames, sessionTitles, now, zone)
            }
        }
        // Each account counts once, by its best login (usage.Pools).
        val best = active.map { a -> cards.filter { it.accountId == a.account.accountId }.minOfOrNull { rank(it.state) } ?: 3 }
        val ready = best.count { it == 0 }
        val near = best.count { it == 1 }
        val warn = ready == 0 && near > 0

        val granted = list.flatMap { it.grants }.map { it.nodeId }.toSet() + own.values.flatten()
        nodes.filter { hasHarness(it, provider) && it.nodeId !in granted }.forEach { n ->
            cards += AccountCard(
                key = "ambient-$provider@${n.nodeId}",
                accountId = "",
                title = AMBIENT_TITLE,
                nodeId = n.nodeId,
                state = CardState.AMBIENT,
                detail = AMBIENT_DETAIL,
            )
        }

        val tokenPrefix = "token:"
        val groups = cards.groupBy { c -> c.machines?.let { tokenPrefix + it } ?: c.nodeId }
            .map { (key, cs) ->
                val title = when {
                    key.startsWith(tokenPrefix) -> key.removePrefix(tokenPrefix).replaceFirstChar { it.uppercase() }
                    key.isEmpty() -> "Not signed in on any machine"
                    else -> nodeNames[key] ?: key
                }
                NodeGroupCards(key, title, cs)
            }
            // Token accounts (every machine) first, then machines by name, then no machine.
            .sortedWith(compareBy<NodeGroupCards> { !it.nodeId.startsWith(tokenPrefix) }.thenBy { it.nodeId.isEmpty() }.thenBy { it.title.lowercase() })
            .toMutableList()
        val disabled = list.filter { it.account.disabled }
        if (disabled.isNotEmpty()) {
            groups += NodeGroupCards(
                "disabled",
                "Disabled",
                disabled.map { a ->
                    AccountCard("${a.account.accountId}@disabled", a.account.accountId, a.account.label.ifEmpty { a.account.accountId }, "", CardState.DISABLED, "disabled")
                },
            )
        }
        if (groups.isEmpty()) return null
        return ProviderSection(
            provider, providerTitle(provider), ready, active.size, warn, groups,
            near = near, atLimit = best.count { it == 2 }, reauth = best.count { it == 4 }, unknown = best.count { it == 3 },
        )
    }

    private fun rank(s: CardState): Int = when (s) {
        CardState.HEALTHY, CardState.READY_ON_USE -> 0
        CardState.NEAR_LIMIT -> 1
        CardState.AT_LIMIT -> 2
        CardState.NEEDS_REAUTH -> 4
        else -> 3
    }
}
