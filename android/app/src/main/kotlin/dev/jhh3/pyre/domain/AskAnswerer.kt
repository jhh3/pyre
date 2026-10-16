package dev.jhh3.pyre.domain

import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.IntentIds
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.model.AnswerAskItem
import dev.jhh3.pyre.model.AnswerAskResult
import dev.jhh3.pyre.model.AnswerAsksRequest
import dev.jhh3.pyre.model.Ask
import dev.jhh3.pyre.model.AskState
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.net.PyreApiException
import java.time.Duration
import java.time.Instant

sealed interface AsksAnswerResult {
    /** The hub answered the batch; each item says whether it committed ([AnswerAskResult.error] null). */
    data class Sent(val results: List<AnswerAskResult>) : AsksAnswerResult {
        val committed: List<AnswerAskResult> get() = results.filter { it.error == null }
        val refused: List<AnswerAskResult> get() = results.filter { it.error != null }
    }

    /** Nothing was sent: every ask is already decided (shown with its outcome), or the hub could not be reached to check. */
    data class NotOpen(val asks: List<Ask>, val explanation: String) : AsksAnswerResult

    data class NotSent(val error: PyreApiException) : AsksAnswerResult

    /** The answers may or may not have reached the hub. Retrying reuses the same operation_id (safe). */
    data class OutcomeUnknown(val error: PyreApiException) : AsksAnswerResult

    data class Failed(val error: PyreApiException) : AsksAnswerResult
}

/**
 * Answers asks (`asks.answer`, batch) like [ApprovalAnswerer] answers
 * approvals: one stable operation_id per answer intent, so a double tap, a
 * notification action racing the in-app button, or a retry after an unknown
 * outcome answers at most once. Answers are never queued offline: the owner
 * decides against the live ask.
 */
class AskAnswerer(private val repo: Repository) {

    /**
     * Answers one ask after re-fetching it, so a stale notification shows the
     * decision instead of sending (the hub refuses a decided ask anyway).
     */
    suspend fun answerOne(askId: String, optionId: String, text: String): AsksAnswerResult {
        val current = try {
            repo.call { getAsk(askId) }
        } catch (e: PyreApiException) {
            // Only reached for an ask the hub listed or notified, so the hub
            // has asks and not_found means this ask is gone.
            return when (e.code) {
                ErrorCodes.NOT_FOUND -> AsksAnswerResult.NotOpen(emptyList(), "This question no longer exists.")
                ErrorCodes.UNSUPPORTED -> AsksAnswerResult.Failed(e)
                else -> AsksAnswerResult.NotSent(e)
            }
        }
        if (current.decided) return AsksAnswerResult.NotOpen(listOf(current), Asks.explain(current))
        return answer(listOf(AnswerAskItem(askId = askId, optionId = optionId, text = text)))
    }

    /** One `asks.answer` call for all [items] (the decisions for one session reach it as one message). */
    suspend fun answer(items: List<AnswerAskItem>): AsksAnswerResult {
        if (items.isEmpty()) return AsksAnswerResult.Sent(emptyList())
        val key = IntentIds.asksAnswer(items)
        val op = repo.intents.forIntent(key)
        return try {
            val r = repo.call { answerAsks(AnswerAsksRequest(operationId = op, answers = items)) }
            repo.intents.complete(key)
            AsksAnswerResult.Sent(r.results)
        } catch (e: PyreApiException) {
            when {
                e.code == ErrorCodes.CONFLICT || e.code == ErrorCodes.EXPIRED -> {
                    repo.intents.complete(key)
                    val latest = items.mapNotNull { i -> runCatching { repo.call { getAsk(i.askId) } }.getOrNull() }
                    AsksAnswerResult.NotOpen(latest, latest.joinToString(" ") { Asks.explain(it) }.ifEmpty { e.message })
                }
                e.transport || e.code == ErrorCodes.OUTCOME_INDETERMINATE || e.code == ErrorCodes.UNAVAILABLE ->
                    AsksAnswerResult.OutcomeUnknown(e)
                else -> {
                    repo.intents.complete(key)
                    AsksAnswerResult.Failed(e)
                }
            }
        }
    }
}

/**
 * Answers given in the inbox but not sent yet. Tapping an option answers at
 * once from the owner's point of view; the answer waits [WINDOW] so Undo can
 * take it back, and every answer given inside the window (each restarts it)
 * goes out together in one [AskAnswerer.answer] call. Pure: the caller owns
 * the timer. A newer answer to the same ask replaces the held one.
 */
class AnswerWindow {
    private val held = LinkedHashMap<String, AnswerAskItem>()

    /** Held answers, oldest first. */
    val items: List<AnswerAskItem> get() = held.values.toList()

    fun isEmpty(): Boolean = held.isEmpty()

    fun hold(item: AnswerAskItem) {
        held.remove(item.askId)
        held[item.askId] = item
    }

    /** Takes back [askId]'s answer (null [askId]: the last one given); null when it is not held (sent already). */
    fun undo(askId: String? = null): AnswerAskItem? {
        val id = askId ?: held.keys.lastOrNull() ?: return null
        return held.remove(id)
    }

    /** Everything held, emptied (the window ended, or the inbox is being left). */
    fun take(): List<AnswerAskItem> = items.also { held.clear() }

    companion object {
        val WINDOW: Duration = Duration.ofSeconds(5)
    }
}

/** Pure wording for asks, shared by the inbox, the ask sheet and notifications. */
object Asks {
    /** Android shows at most three notification actions. */
    const val MAX_NOTIFICATION_ACTIONS = 3

    /** A hub that predates asks answers 404 ("no such endpoint") for a list call (unsupported treated alike). */
    fun unsupported(e: PyreApiException): Boolean = e.httpStatus == 404 || e.code == ErrorCodes.UNSUPPORTED

    fun optionLabel(a: Ask, optionId: String): String = a.option(optionId)?.label?.ifEmpty { null } ?: optionId

    /**
     * "defaults to B in 42m", "expires in 42m" (no default), "deadline
     * passed" (the hub is about to apply it); "" for a decided ask or one
     * without a deadline.
     */
    fun deadline(a: Ask, now: Instant): String {
        if (a.decided) return ""
        val at = Times.parse(a.deadlineAt) ?: return ""
        val left = Duration.between(now, at)
        val def = a.defaultOption.takeIf { it.isNotEmpty() }?.let { optionLabel(a, it) }
        if (left.isNegative || left.isZero) return if (def != null) "defaulting to $def" else "deadline passed"
        val span = Times.span(left)
        return if (def != null) "defaults to $def in $span" else "expires in $span"
    }

    /** Whether the deadline is close enough to stand out (under 15 minutes, or urgent). */
    fun soon(a: Ask, now: Instant): Boolean {
        if (a.decided) return false
        if (a.urgent) return true
        val at = Times.parse(a.deadlineAt) ?: return false
        return Duration.between(now, at) < Duration.ofMinutes(15)
    }

    /** The outcome of a decided ask in one sentence. */
    fun explain(a: Ask): String {
        val choice = a.answer?.let { ans ->
            listOfNotNull(
                ans.optionId.takeIf { it.isNotEmpty() }?.let { optionLabel(a, it) },
                ans.text.takeIf { it.isNotEmpty() }?.let { "“$it”" },
            ).joinToString(" — ")
        }.orEmpty()
        return when (a.state) {
            AskState.OPEN -> "Waiting for your answer."
            AskState.ANSWERED -> "Answered" + (if (choice.isNotEmpty()) ": $choice" else "") + (a.answer?.by?.name?.takeIf { it.isNotEmpty() }?.let { " by $it" } ?: "") + "."
            AskState.DEFAULTED -> "The deadline passed; the default applied" + (if (choice.isNotEmpty()) ": $choice" else "") + "."
            AskState.EXPIRED -> "The deadline passed with no answer and no default."
            AskState.WITHDRAWN -> "Withdrawn by the agent that asked."
            AskState.SUPERSEDED -> "Replaced by a newer question on the same topic."
            else -> "State: ${a.state}"
        }
    }

    /** The Undo snackbar: "Answered “Question” → Merge" (+ "and 1 more"). */
    fun held(a: Ask?, item: AnswerAskItem, more: Int): String {
        val choice = item.optionId.takeIf { it.isNotEmpty() }?.let { id -> a?.let { optionLabel(it, id) } ?: id }
            ?: "“" + item.text.lineSequence().first().take(40) + "”"
        val title = a?.title?.takeIf { it.isNotEmpty() }?.let { if (it.length > 48) it.take(47) + "…" else it }
        return (if (title != null) "Answered “$title” → $choice" else "Answered: $choice") +
            (if (more > 0) " and $more more" else "") + " · sending in ${AnswerWindow.WINDOW.seconds}s"
    }

    /** "1 answer sent", "2 answers sent · 1 already decided: …". */
    fun summary(r: AsksAnswerResult): String = when (r) {
        is AsksAnswerResult.Sent -> {
            val ok = r.committed.size
            val head = if (ok == 1) "1 answer sent" else "$ok answers sent"
            val refused = r.refused.joinToString(" ") { res ->
                res.ask?.takeIf { it.decided }?.let { "Already decided: “${it.title}”: ${explain(it)}" }
                    ?: ("Not sent: " + res.askId + ": " + res.error?.let { it.message.ifEmpty { it.code } }.orEmpty())
            }
            when {
                refused.isEmpty() -> "$head. The agent gets them as one message."
                ok == 0 -> refused
                else -> "$head. $refused"
            }
        }
        is AsksAnswerResult.NotOpen -> r.explanation
        is AsksAnswerResult.NotSent -> "Not sent — ${r.error.describe()}"
        is AsksAnswerResult.OutcomeUnknown -> "Outcome unknown — the answers may have reached the hub. Retrying is safe: it reuses the same operation id."
        is AsksAnswerResult.Failed -> r.error.describe()
    }
}
