package dev.jhh3.pyre.domain

import dev.jhh3.pyre.model.AckState
import dev.jhh3.pyre.model.DeliveryState
import dev.jhh3.pyre.model.MessageView
import dev.jhh3.pyre.model.StorageState

/**
 * The independent delivery facts of one message, rendered separately:
 * hub storage, runtime delivery, recipient acknowledgement, work outcome.
 * "Accepted" means a supported input interface accepted the message — not
 * that the agent read or completed it.
 */
data class DeliveryFacts(
    val storage: Fact,
    val runtime: Fact,
    val ack: Fact,
    val outcome: Fact,
    val evidence: String?,
    /** Outcome unknown: show the resolve/reissue flow; never auto-retry. */
    val needsReconciliation: Boolean,
    /** A new message may be issued referencing this one. */
    val canReissue: Boolean,
    /** Reissuing could duplicate work that may already have executed. */
    val reissueDuplicateRisk: Boolean,
    val canCancel: Boolean,
)

data class Fact(val label: String, val value: String, val tone: Tone)

object Delivery {
    fun facts(m: MessageView): DeliveryFacts {
        val s = m.status
        val storage = when (s.storage) {
            StorageState.STORED -> Fact("Stored", "stored on hub", Tone.OK)
            StorageState.PENDING_LOCAL -> Fact("Stored", "pending — not yet stored on hub", Tone.WARN)
            StorageState.REJECTED -> Fact("Stored", "rejected by hub", Tone.ERROR)
            else -> Fact("Stored", s.storage, Tone.WARN)
        }
        val reason = s.runtimeReason.takeIf { it.isNotEmpty() }?.let { " — $it" }.orEmpty()
        val runtime = when (s.runtime) {
            DeliveryState.NOT_APPLICABLE -> Fact("Runtime", "not applicable (not injected into a runtime)", Tone.MUTED)
            DeliveryState.QUEUED -> Fact("Runtime", "queued$reason", Tone.MUTED)
            DeliveryState.NODE_RECEIVED -> Fact("Runtime", "received by node, not yet delivered$reason", Tone.MUTED)
            DeliveryState.ATTEMPTING -> Fact("Runtime", "delivery in progress$reason", Tone.ACTIVE)
            DeliveryState.ACCEPTED -> Fact(
                "Runtime",
                (if (s.consumedAt != null) "accepted and taken into a turn" else "accepted by runtime input (not yet consumed)") + reason,
                Tone.OK,
            )
            DeliveryState.REJECTED -> Fact("Runtime", "rejected$reason", Tone.ERROR)
            DeliveryState.INDETERMINATE -> Fact("Runtime", "INDETERMINATE — may or may not have been accepted$reason", Tone.ATTENTION)
            DeliveryState.EXPIRED -> Fact("Runtime", "expired before delivery$reason", Tone.WARN)
            DeliveryState.CANCELLED -> Fact("Runtime", "cancelled before any attempt", Tone.MUTED)
            else -> Fact("Runtime", s.runtime + reason, Tone.WARN)
        }
        val ack = when (s.ack) {
            AckState.NONE -> Fact("Acknowledged", "no acknowledgement", Tone.MUTED)
            AckState.ACKNOWLEDGED -> Fact("Acknowledged", "acknowledged" + s.ackRef.ref(), Tone.OK)
            AckState.REPLIED -> Fact("Acknowledged", "replied" + s.ackRef.ref(), Tone.OK)
            else -> Fact("Acknowledged", s.ack, Tone.MUTED)
        }
        val outcome = s.outcomeRef?.let { Fact("Outcome", "${it.kind} ${it.id}", Tone.OK) }
            ?: Fact("Outcome", "no result reported", Tone.MUTED)
        val lastReceipt = s.receipts.lastOrNull()
        val evidence = lastReceipt?.let {
            "attempt ${it.attemptId} on ${it.runId}: ${it.state} (evidence: ${it.evidence}${it.evidenceRef.ref()})" + it.detail.takeIf { d -> d.isNotEmpty() }?.let { d -> " — $d" }.orEmpty()
        }
        val indeterminate = s.runtime == DeliveryState.INDETERMINATE
        return DeliveryFacts(
            storage = storage,
            runtime = runtime,
            ack = ack,
            outcome = outcome,
            evidence = evidence,
            needsReconciliation = indeterminate,
            canReissue = s.storage == StorageState.STORED && s.runtime in setOf(DeliveryState.INDETERMINATE, DeliveryState.REJECTED, DeliveryState.EXPIRED),
            reissueDuplicateRisk = indeterminate,
            canCancel = s.storage == StorageState.STORED && s.runtime == DeliveryState.QUEUED,
        )
    }

    private fun String.ref() = if (isEmpty()) "" else " ($this)"
}
