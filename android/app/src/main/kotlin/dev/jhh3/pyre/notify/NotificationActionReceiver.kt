package dev.jhh3.pyre.notify

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import dev.jhh3.pyre.domain.AnswerResult
import dev.jhh3.pyre.graph
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/**
 * Approve/deny from a notification. Always re-validates the live approval
 * (ApprovalAnswerer re-fetches it) and uses the shared per-intent
 * operation_id, so repeated taps or a tap racing the in-app button resolve
 * at most once; a stale request shows its current resolution instead.
 */
class NotificationActionReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != ACTION_ANSWER) return
        val attentionId = intent.getStringExtra(EXTRA_ATTENTION) ?: return
        val approvalId = intent.getStringExtra(EXTRA_APPROVAL) ?: return
        val runId = intent.getStringExtra(EXTRA_RUN).orEmpty()
        val optionId = intent.getStringExtra(EXTRA_OPTION) ?: return
        val label = intent.getStringExtra(EXTRA_LABEL).orEmpty().ifEmpty { optionId }
        val link = intent.getStringExtra(EXTRA_LINK).orEmpty()
        val graph = context.graph
        val session = graph.session.value ?: run {
            Notifications.showResult(context, attentionId, "Not paired", "Open Pyre and pair with your hub.", link)
            return
        }
        val pending = goAsync()
        Notifications.showResult(context, attentionId, "Sending “$label”…", "Checking the request is still pending.", link)
        graph.scope.launch {
            try {
                val result = withTimeoutOrNull(9_000) { session.answerer.answer(approvalId, runId, optionId, "") }
                val (title, text) = when (result) {
                    is AnswerResult.Answered -> "Answered: $label" to "Recorded by the hub. The agent continues when its runtime accepts the answer."
                    is AnswerResult.NotPending -> "No longer pending" to result.explanation
                    is AnswerResult.NotSent -> "Not sent" to "Hub unreachable; nothing was sent. ${result.error.describe()}"
                    is AnswerResult.OutcomeUnknown -> "Outcome unknown" to "The answer may have reached the hub. Open Pyre to check; retrying is safe."
                    is AnswerResult.Failed -> "Could not answer" to result.error.describe()
                    null -> "Outcome unknown" to "Timed out. Open Pyre to check; retrying is safe (same operation)."
                }
                Notifications.showResult(context, attentionId, title, text, link)
            } finally {
                pending.finish()
            }
        }
    }

    companion object {
        const val ACTION_ANSWER = "dev.jhh3.pyre.action.ANSWER_APPROVAL"
        const val EXTRA_ATTENTION = "attention_id"
        const val EXTRA_APPROVAL = "approval_id"
        const val EXTRA_RUN = "run_id"
        const val EXTRA_OPTION = "option_id"
        const val EXTRA_LABEL = "option_label"
        const val EXTRA_LINK = "deep_link"
    }
}
