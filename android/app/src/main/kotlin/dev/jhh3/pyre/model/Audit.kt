package dev.jhh3.pyre.model

import kotlinx.serialization.Serializable

/**
 * Audit log (internal/contracts/audit.go; `audit.list`, additive, read-only).
 * Hubs that predate it answer HTTP 404 for the path.
 */
object AuditOps {
    const val LIST_AUDIT = "audit.list"

    const val KIND_ACCOUNT = "account"
    const val KIND_SESSION = "session"
    const val KIND_FLEET = "fleet"

    const val ACCOUNT_SWITCHED = "account.switched"
    const val ACCOUNT_WAITING = "account.waiting"
}

/** Newest first. Since/Until are RFC 3339; Limit is clamped by the hub (default 50, max 500). */
@Serializable
data class ListAuditRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val since: String? = null,
    val until: String? = null,
    val kinds: List<String> = emptyList(),
    val sessionId: String = "",
    val nodeId: String = "",
    val accountId: String = "",
)

/** Who did it. Kind: "owner" | "session" | "node" | "pyre". */
@Serializable
data class AuditActor(val kind: String = "", val id: String = "", val name: String = "")

/** One audit record; display fields and [summary] are resolved by the hub when listed. */
@Serializable
data class AuditEntry(
    val auditId: Long = 0,
    val at: String = "",
    val kind: String = "",
    val action: String = "",
    val automatic: Boolean = false,
    val actor: AuditActor = AuditActor(),
    val nodeId: String = "",
    val nodeName: String = "",
    val sessionId: String = "",
    val sessionTitle: String = "",
    val runId: String = "",
    val accountId: String = "",
    val accountLabel: String = "",
    val from: String = "",
    val to: String = "",
    val fromAccountId: String = "",
    val toAccountId: String = "",
    val fromNote: String = "",
    val toNote: String = "",
    val reason: String = "",
    val summary: String = "",
)
