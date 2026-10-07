package dev.jhh3.pyre.domain

import dev.jhh3.pyre.data.IntentIds
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.model.AnswerApprovalRequest
import dev.jhh3.pyre.model.Approval
import dev.jhh3.pyre.model.ApprovalState
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.net.PyreApiException

sealed interface AnswerResult {
    /** The hub committed our answer (or returned the stored result of the same operation). */
    data class Answered(val approval: Approval) : AnswerResult

    /** Not pending any more (resolved elsewhere, expired, withdrawn): show the current state. */
    data class NotPending(val approval: Approval?, val explanation: String) : AnswerResult

    /** Could not even re-validate the request (offline) — nothing was sent. */
    data class NotSent(val error: PyreApiException) : AnswerResult

    /** The answer may or may not have reached the hub. Retrying reuses the same operation_id (safe). */
    data class OutcomeUnknown(val error: PyreApiException) : AnswerResult

    data class Failed(val error: PyreApiException) : AnswerResult
}

/**
 * Answers an approval by `(approval_id, run_id)` exactly as the contract
 * requires. Always re-fetches the live request first, refuses to answer a
 * request that is no longer pending or whose run was replaced, and uses one
 * stable operation_id per answer intent so double taps / repeated
 * notification actions / retries resolve at most once.
 */
class ApprovalAnswerer(private val repo: Repository) {

    suspend fun answer(approvalId: String, expectedRunId: String, optionId: String, text: String): AnswerResult {
        val current = try {
            repo.call { getApproval(approvalId) }
        } catch (e: PyreApiException) {
            return when (e.code) {
                ErrorCodes.NOT_FOUND, ErrorCodes.EXPIRED -> AnswerResult.NotPending(null, "This request no longer exists (${e.message}).")
                else -> AnswerResult.NotSent(e)
            }
        }
        if (current.state != ApprovalState.PENDING) return AnswerResult.NotPending(current, explain(current))
        if (expectedRunId.isNotEmpty() && current.runId != expectedRunId) {
            return AnswerResult.NotPending(current, "The run that asked this was replaced; this request is not current.")
        }
        val key = IntentIds.approvalAnswer(approvalId, optionId, text)
        val op = repo.intents.forIntent(key)
        return try {
            val result = repo.call {
                answerApproval(AnswerApprovalRequest(operationId = op, approvalId = approvalId, runId = current.runId, optionId = optionId, text = text))
            }
            repo.intents.complete(key)
            AnswerResult.Answered(result)
        } catch (e: PyreApiException) {
            when {
                e.code == ErrorCodes.CONFLICT || e.code == ErrorCodes.EXPIRED -> {
                    repo.intents.complete(key)
                    val latest = runCatching { repo.call { getApproval(approvalId) } }.getOrNull()
                    AnswerResult.NotPending(latest, latest?.let { explain(it) } ?: e.message)
                }
                e.transport || e.code == ErrorCodes.OUTCOME_INDETERMINATE || e.code == ErrorCodes.UNAVAILABLE ->
                    AnswerResult.OutcomeUnknown(e)
                else -> AnswerResult.Failed(e)
            }
        }
    }

    companion object {
        fun explain(a: Approval): String = when (a.state) {
            ApprovalState.RESOLVED -> {
                val r = a.resolution
                val choice = r?.let { res -> a.options.firstOrNull { it.id == res.optionId }?.label ?: res.optionId.ifEmpty { res.text } }.orEmpty()
                val where = when (r?.source) {
                    "native" -> "in the native terminal"
                    "pyre" -> "via Pyre" + (r.actor?.name?.takeIf { it.isNotEmpty() }?.let { " by $it" } ?: "")
                    else -> ""
                }
                val effect = when (r?.effect) {
                    "submitted" -> " Submitted to the runtime; not yet confirmed effective."
                    "effective" -> " Runtime confirmed it took effect."
                    "superseded" -> " Superseded: it was resolved elsewhere first."
                    null, "" -> ""
                    else -> " (${r.effect})"
                }
                val reason = when (r?.reason) {
                    null, "" -> ""
                    "answered_in_terminal" -> " Answered in the terminal."
                    "interrupted" -> " The run was interrupted."
                    "run_replaced" -> " The run was replaced."
                    "timeout" -> " Timed out."
                    else -> " (${r.reason})"
                }
                "Resolved" + (if (choice.isNotEmpty()) ": $choice" else "") + (if (where.isNotEmpty()) " $where" else "") + "." + effect + reason
            }
            ApprovalState.EXPIRED -> "This request expired."
            ApprovalState.WITHDRAWN -> "Withdrawn — the runtime moved on or its run was replaced."
            ApprovalState.PENDING -> "Pending."
            else -> "State: ${a.state}"
        }

        /** Notification approve/deny is offered only for plain permission requests with allow/deny options. */
        fun quickActions(a: Approval) = if (a.kind == "permission" && a.state == ApprovalState.PENDING && !a.allowText) {
            a.options.filter { it.kind == "allow" || it.kind == "deny" }
        } else emptyList()
    }
}
