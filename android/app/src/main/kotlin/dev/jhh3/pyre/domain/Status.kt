package dev.jhh3.pyre.domain

import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.model.ConnectionState
import dev.jhh3.pyre.model.Node
import dev.jhh3.pyre.model.NodeLabels
import dev.jhh3.pyre.model.RunState
import dev.jhh3.pyre.model.SessionView
import java.time.Instant

/** Tone is a hint for color; [symbol] + [label] always carry the meaning (never color alone). */
enum class Tone { OK, ACTIVE, ATTENTION, WARN, ERROR, MUTED }

data class StatusLabel(val symbol: String, val label: String, val detail: String, val tone: Tone)

/** A list row's status: "Working" + "running tests", "Waiting for input" + "3m ago". */
data class RowStatus(val label: String, val phrase: String, val age: String, val tone: Tone)

object Status {
    /**
     * Honest session status. A stale observation, a disconnected node or an
     * `unknown` run state is shown as such — never as idle or failed.
     */
    fun session(view: SessionView, node: Node?, now: Instant): StatusLabel {
        val run = view.run ?: return StatusLabel("○", "No current run", "session persists; resume to start a run", Tone.MUTED)
        val observed = Times.ago(run.freshness.observedAt.ifEmpty { view.freshness.observedAt }, now)
        val nodeDown = node != null && node.connection.state != ConnectionState.CONNECTED
        val stale = run.freshness.stale || view.freshness.stale || nodeDown
        if (stale && run.state != RunState.EXITED && run.state != RunState.FAILED) {
            val why = run.freshness.reason.ifEmpty { view.freshness.reason }.ifEmpty {
                if (nodeDown) "node ${node?.connection?.state}" else "observation stale"
            }
            val last = if (run.state == RunState.UNKNOWN) "" else " · last observed ${label(run.state).lowercase()}"
            return StatusLabel("◌", "Unknown (stale)", "$why · seen $observed$last", Tone.WARN)
        }
        val reason = run.stateReason.takeIf { it.isNotEmpty() }?.let { " · $it" }.orEmpty()
        return when (run.state) {
            RunState.RUNNING -> StatusLabel("●", "Working", listOfNotNull(doing(view), "updated $observed").joinToString(" · "), Tone.ACTIVE)
            RunState.STARTING -> StatusLabel("◔", "Starting", "updated $observed", Tone.ACTIVE)
            RunState.WAITING_INPUT -> StatusLabel("○", "Waiting for input", "updated $observed", Tone.OK)
            RunState.WAITING_BACKGROUND -> StatusLabel("◎", "Idle · background work", "background tasks may start a turn · updated $observed", Tone.ACTIVE)
            RunState.WAITING_APPROVAL -> StatusLabel("?", "Needs approval", "${run.pendingApprovalIds.size} pending · updated $observed", Tone.ATTENTION)
            RunState.EXITING -> StatusLabel("◑", "Exiting", "updated $observed$reason", Tone.MUTED)
            RunState.EXITED -> StatusLabel("■", "Exited", (run.exitCode?.let { "exit $it" } ?: "exited") + reason, Tone.MUTED)
            RunState.FAILED -> StatusLabel("✕", "Failed", "updated $observed$reason", Tone.ERROR)
            RunState.UNKNOWN -> StatusLabel("◌", "Unknown", "observer lost contact · seen $observed$reason", Tone.WARN)
            else -> StatusLabel("◌", run.state, "unrecognized state from hub · seen $observed", Tone.WARN)
        }
    }

    /** The hub's doing-now phrase for a working run ("running tests"), or null. */
    fun doing(view: SessionView): String? =
        view.run?.takeIf { it.state == RunState.RUNNING }?.activity?.text?.trim()?.takeIf { it.isNotEmpty() }

    /**
     * Nothing of the session is running: no run, or it exited or failed
     * (the TUI's sessionEnded). Offline and stale runs are not ended.
     */
    fun ended(view: SessionView): Boolean =
        view.run == null || view.run.state == RunState.EXITED || view.run.state == RunState.FAILED

    /**
     * The short, plain status for a Sessions list row: never a raw
     * state_reason, error or account id (the session screen has those).
     * [phrase] is what it is doing or a hint, [age] when that was.
     */
    fun row(view: SessionView, node: Node?, now: Instant): RowStatus {
        val run = view.run ?: return RowStatus("Ended", "", Times.agoOrEmpty(view.freshness.observedAt, now), Tone.MUTED)
        val observedAt = run.freshness.observedAt.ifEmpty { view.freshness.observedAt }
        val nodeDown = node != null && node.connection.state != ConnectionState.CONNECTED
        val stale = run.freshness.stale || view.freshness.stale || nodeDown
        val endedAgo = Times.agoOrEmpty(run.endedAt ?: observedAt, now)
        if (stale && run.state != RunState.EXITED && run.state != RunState.FAILED) {
            return RowStatus("Unknown (stale)", if (nodeDown) "machine offline" else "not seen recently", Times.agoOrEmpty(observedAt, now).let { if (it.isEmpty()) "" else "seen $it" }, Tone.WARN)
        }
        return when (run.state) {
            RunState.RUNNING -> RowStatus("Working", doing(view).orEmpty(), "", Tone.ACTIVE)
            RunState.STARTING -> RowStatus("Starting", "", "", Tone.ACTIVE)
            RunState.WAITING_INPUT -> RowStatus("Waiting for input", "", Times.agoOrEmpty(observedAt, now), Tone.OK)
            RunState.WAITING_BACKGROUND -> RowStatus("Idle", "background work", Times.agoOrEmpty(observedAt, now), Tone.ACTIVE)
            RunState.WAITING_APPROVAL -> RowStatus("Needs approval", run.pendingApprovalIds.size.takeIf { it > 1 }?.let { "$it pending" }.orEmpty(), "", Tone.ATTENTION)
            RunState.EXITING -> RowStatus("Exiting", "", "", Tone.MUTED)
            RunState.EXITED -> RowStatus("Ended", run.exitCode?.takeIf { it != 0 }?.let { "exit $it" }.orEmpty(), endedAgo, Tone.MUTED)
            RunState.FAILED -> RowStatus(if (couldNotStart(run)) "Couldn't start" else "Stopped with an error", "open for details", endedAgo, Tone.ERROR)
            else -> RowStatus("Unknown", "", Times.agoOrEmpty(observedAt, now).let { if (it.isEmpty()) "" else "seen $it" }, Tone.WARN)
        }
    }

    /** A failed run that never got going: no start time, or it ended within two minutes of starting. */
    private fun couldNotStart(run: dev.jhh3.pyre.model.Run): Boolean {
        val start = Times.parse(run.startedAt) ?: return true
        val end = Times.parse(run.endedAt) ?: Times.parse(run.freshness.observedAt) ?: return false
        return java.time.Duration.between(start, end).toMinutes() < 2
    }

    fun label(state: String): String = when (state) {
        RunState.RUNNING -> "Working"
        RunState.STARTING -> "Starting"
        RunState.WAITING_INPUT -> "Waiting for input"
        RunState.WAITING_APPROVAL -> "Needs approval"
        RunState.WAITING_BACKGROUND -> "Idle · background work"
        RunState.EXITING -> "Exiting"
        RunState.EXITED -> "Exited"
        RunState.FAILED -> "Failed"
        else -> "Unknown"
    }

    fun node(node: Node, now: Instant): StatusLabel = when (node.connection.state) {
        ConnectionState.CONNECTED -> StatusLabel("●", "Online", "since ${Times.ago(node.connection.connectedSince, now).removeSuffix(" ago")}", Tone.OK)
        ConnectionState.DISCONNECTED -> StatusLabel("◌", "Offline", "last seen ${Times.ago(node.connection.lastSeenAt, now)} — runs may still be alive", Tone.WARN)
        ConnectionState.NEVER -> if (node.labels[NodeLabels.ENROLLMENT] == NodeLabels.ENROLLMENT_PENDING) {
            StatusLabel("○", "Enrollment pending", "enrollment ticket not redeemed yet; cannot run sessions", Tone.MUTED)
        } else {
            StatusLabel("○", "Never connected", "enrolled ${Times.ago(node.enrolledAt, now)}", Tone.MUTED)
        }
        ConnectionState.REVOKED -> StatusLabel("✕", "Revoked", "credential revoked", Tone.ERROR)
        else -> StatusLabel("◌", node.connection.state, "unrecognized connection state", Tone.WARN)
    }
}
