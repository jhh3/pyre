package dev.jhh3.pyre.model

import kotlinx.serialization.Serializable

/**
 * Asks (internal/contracts/asks.go; additive, optional): a structured
 * question from a top-level session to the owner with 2–4 options, an
 * optional default and a deadline after which the hub applies the default.
 * A hub that offers them advertises the Hello feature [FEATURE]; one that
 * does not answers HTTP 404 for the paths.
 */
object AsksOps {
    const val FEATURE = "asks"

    const val LIST = "asks.list"
    const val GET = "asks.get"
    const val ANSWER = "asks.answer"
    const val WITHDRAW = "asks.withdraw"

    /** ObjectRef kind / deep link kind of an ask. */
    const val KIND = "ask"
}

object AskState {
    const val OPEN = "open"
    const val ANSWERED = "answered"
    const val DEFAULTED = "defaulted"
    const val EXPIRED = "expired"
    const val WITHDRAWN = "withdrawn"
    const val SUPERSEDED = "superseded"

    val DECIDED = listOf(ANSWERED, DEFAULTED, EXPIRED, WITHDRAWN, SUPERSEDED)

    /** Whether the ask has a final outcome (unknown future states count as decided: never answerable). */
    fun decided(state: String): Boolean = state != OPEN && state.isNotEmpty()
}

@Serializable
data class AskOption(
    val id: String,
    val label: String = "",
    val detail: String = "",
)

@Serializable
data class AskAnswer(
    val optionId: String = "",
    val text: String = "",
    val by: Principal? = null,
    val byDefault: Boolean = false,
    val at: String = "",
)

/** A follow-up on the same topic merged into the open ask instead of new mail. */
@Serializable
data class AskUpdate(
    val seq: Int = 0,
    val sessionId: String = "",
    val text: String = "",
    val at: String = "",
)

@Serializable
data class Ask(
    val askId: String,
    val topicKey: String = "",
    val projectId: String? = null,
    val sessionId: String = "",
    val title: String = "",
    val context: String = "",
    val options: List<AskOption> = emptyList(),
    val defaultOption: String = "",
    val urgent: Boolean = false,
    val deadlineAt: String? = null,
    /** "1h" | "custom" | "morning" | "urgent". */
    val deadlineRule: String = "",
    val state: String = AskState.OPEN,
    val answer: AskAnswer? = null,
    val updates: List<AskUpdate> = emptyList(),
    val count: Int = 1,
    val supersededBy: String = "",
    val messageId: String = "",
    val threadId: String = "",
    val attentionId: String = "",
    val decisionMessageId: String = "",
    val createdAt: String = "",
    val updatedAt: String = "",
) {
    val decided: Boolean get() = AskState.decided(state)

    fun option(id: String): AskOption? = options.firstOrNull { it.id == id }
}

@Serializable
data class ListAsksRequest(
    val cursor: String = "",
    val limit: Int = 0,
    val states: List<String> = emptyList(),
    val projectId: String? = null,
    val sessionId: String = "",
    val topicKey: String = "",
    val query: String = "",
    /** RFC 3339; limits decided asks to those decided after it (open ones always match). */
    val since: String? = null,
)

@Serializable
data class AnswerAskItem(
    val askId: String,
    val optionId: String = "",
    /** Free text; required when [optionId] is empty. */
    val text: String = "",
)

/** Answers one or more asks at once; each commits independently. */
@Serializable
data class AnswerAsksRequest(
    val operationId: String,
    val correlationId: String = "",
    val answers: List<AnswerAskItem>,
)

@Serializable
data class AnswerAskResult(
    val askId: String,
    val ask: Ask? = null,
    /** e.g. conflict: already decided. */
    val error: ApiError? = null,
)

@Serializable
data class AnswerAsksResponse(val results: List<AnswerAskResult> = emptyList())

@Serializable
data class WithdrawAskRequest(
    val operationId: String,
    val correlationId: String = "",
    val askId: String,
    val reason: String = "",
    val supersededBy: String = "",
)
