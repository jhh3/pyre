package dev.jhh3.pyre.digest

import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.model.AttentionItem
import dev.jhh3.pyre.model.AttentionState
import dev.jhh3.pyre.model.ErrorCodes
import dev.jhh3.pyre.model.Event
import dev.jhh3.pyre.model.Harness
import dev.jhh3.pyre.model.ListAttentionRequest
import dev.jhh3.pyre.model.ListEventsRequest
import dev.jhh3.pyre.model.ListSessionsRequest
import dev.jhh3.pyre.model.ReadSessionRequest
import dev.jhh3.pyre.model.ReadSessionResponse
import dev.jhh3.pyre.model.RunState
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.net.PyreApi
import dev.jhh3.pyre.net.PyreApiException
import java.time.Duration
import java.time.Instant
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.sync.Semaphore
import kotlinx.coroutines.sync.withPermit
import kotlinx.coroutines.withTimeoutOrNull

/** This client's "last looked" mark (cursor + time). */
data class LastSeen(val cursor: Long, val at: Instant)

/**
 * Reads the hub with existing operations only (events.list, sessions.list,
 * attention.list, sessions.read) and builds the digest — the same steps as
 * internal/digest.Gather. Every call is a read.
 */
class DigestFetcher(
    private val api: PyreApi,
    private val maxEvents: Int = 20_000,
    private val transcriptTail: Int = 200,
    private val parallel: Int = 4,
    private val callTimeout: Duration = Duration.ofSeconds(5),
) {
    suspend fun fetch(lastSeen: LastSeen?, now: Instant): Digest = DigestBuilder.build(gather(lastSeen, now))

    suspend fun gather(lastSeen: LastSeen?, now: Instant): DigestInput {
        val since = lastSeen?.at ?: now.minus(DEFAULT_WINDOW)
        val label = if (lastSeen != null) "last seen" else "last 12h"
        val (head, oldest) = bounds()
        var sinceCursor = lastSeen?.cursor ?: 0
        var truncated = false
        if (sinceCursor <= 0 || sinceCursor > head) sinceCursor = cursorAt(since, oldest, head)
        if (sinceCursor < oldest - 1) {
            truncated = true
            sinceCursor = oldest - 1
        }

        val events = ArrayList<Event>()
        var after = sinceCursor
        while (events.size < maxEvents && after < head) {
            val resp = try {
                api.listEvents(ListEventsRequest(afterCursor = after, limit = 1000, kinds = DigestBuilder.EVENT_KINDS))
            } catch (e: PyreApiException) {
                if (e.code == ErrorCodes.RESET_REQUIRED) { truncated = true; break }
                throw e
            }
            resp.events.filter { it.cursor > after }.let { events += it }
            var next = resp.nextCursor
            resp.events.lastOrNull()?.let { if (it.cursor > next) next = it.cursor }
            if (next <= after || resp.events.isEmpty()) break
            after = next
        }
        if (events.size >= maxEvents && after < head) truncated = true

        val sessions = pages { cur -> api.listSessions(ListSessionsRequest(cursor = cur, limit = 200, includeArchived = true)).let { it.items to it.nextCursor } }
        val attention: List<AttentionItem> = pages { cur ->
            api.listAttention(ListAttentionRequest(cursor = cur, limit = 200, states = listOf(AttentionState.OPEN))).let { it.items to it.nextCursor }
        }
        val partial = DigestInput(since = since, sinceCursor = sinceCursor, headCursor = head, now = now, sinceLabel = label,
            truncated = truncated, events = events, sessions = sessions, attention = attention)
        val (transcripts, warnings) = readTranscripts(candidates(partial))
        return partial.copy(transcripts = transcripts, warnings = warnings)
    }

    /** Sessions with events after the cutoff, live agent sessions, and runs that ended after it. */
    private fun candidates(input: DigestInput): List<SessionView> {
        val active = input.events.mapNotNull { it.sessionId.ifEmpty { null } }.toSet()
        return input.sessions.filter { sv ->
            val run = sv.run ?: return@filter false
            if (sv.session.harness == Harness.SHELL) return@filter false
            val live = run.state != RunState.EXITED && run.state != RunState.FAILED
            val ended = Times.parse(run.endedAt)?.isAfter(input.since) == true
            sv.session.sessionId in active || (live && !sv.session.archived) || ended
        }
    }

    private suspend fun readTranscripts(ss: List<SessionView>): Pair<Map<String, ReadSessionResponse>, List<String>> = coroutineScope {
        val sem = Semaphore(parallel)
        val results: List<Pair<SessionView, Any>?> = ss.map { sv ->
            async {
                sem.withPermit {
                    val r: Any = try {
                        withTimeoutOrNull(callTimeout.toMillis()) {
                            api.readSession(ReadSessionRequest(sessionId = sv.session.sessionId, tail = true, limit = transcriptTail))
                        } ?: "transcript of \"${sv.session.title}\" unavailable: no answer within ${callTimeout.seconds}s"
                    } catch (e: PyreApiException) {
                        // No run to read (e.g. a launch that failed): nothing to summarize, no warning.
                        if (e.code == ErrorCodes.NOT_FOUND) return@withPermit null
                        "transcript of \"${sv.session.title}\" unavailable: ${e.describe()}"
                    }
                    Pair<SessionView, Any>(sv, r)
                }
            }
        }.awaitAll()
        val out = LinkedHashMap<String, ReadSessionResponse>()
        val warn = ArrayList<String>()
        for ((sv, r) in results.filterNotNull()) {
            when (r) {
                is ReadSessionResponse -> out[sv.session.sessionId] = r
                is String -> warn += r
            }
        }
        out to warn
    }

    /** Head cursor and oldest retained cursor. */
    private suspend fun bounds(): Pair<Long, Long> = try {
        val r = api.listEvents(ListEventsRequest(afterCursor = 0, limit = 1))
        r.headCursor to (r.events.firstOrNull()?.cursor ?: 1L)
    } catch (e: PyreApiException) {
        if (e.code != ErrorCodes.RESET_REQUIRED) throw e
        (e.detail("snapshot_cursor")?.toLongOrNull() ?: 0L) to (e.detail("oldest_cursor")?.toLongOrNull() ?: 0L)
    }

    /** Cursor just before the first event ingested after [t] (binary search). */
    private suspend fun cursorAt(t: Instant, oldestIn: Long, head: Long): Long {
        if (head <= 0) return 0
        val oldest = if (oldestIn <= 0) 1 else oldestIn
        var lo = oldest - 1
        var hi = head
        while (lo < hi) {
            val mid = lo + (hi - lo) / 2
            val resp = try {
                api.listEvents(ListEventsRequest(afterCursor = mid, limit = 1))
            } catch (e: PyreApiException) {
                if (e.code == ErrorCodes.RESET_REQUIRED) { lo = mid + 1; continue }
                throw e
            }
            val ev = resp.events.firstOrNull()
            if (ev == null) { hi = mid; continue }
            val at = Times.parse(ev.ingestedAt)
            if (at != null && at.isAfter(t)) hi = mid else lo = ev.cursor
        }
        return lo
    }

    private suspend fun <T> pages(fetch: suspend (String) -> Pair<List<T>, String>): List<T> {
        val out = ArrayList<T>()
        var cur = ""
        repeat(50) {
            val (items, next) = fetch(cur)
            out += items
            if (next.isEmpty() || next == cur) return out
            cur = next
        }
        return out
    }

    companion object {
        val DEFAULT_WINDOW: Duration = Duration.ofHours(12)
    }
}
