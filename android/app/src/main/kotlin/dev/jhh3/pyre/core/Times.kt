package dev.jhh3.pyre.core

import java.time.Duration
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.time.format.DateTimeParseException

/** Injectable clock for deterministic tests. */
fun interface Clock {
    fun now(): Instant

    companion object {
        val System = Clock { Instant.now() }
    }
}

object Times {
    private val GO_ZERO = Instant.parse("0001-01-01T00:00:00Z")

    fun parse(s: String?): Instant? {
        if (s.isNullOrBlank()) return null
        return try {
            Instant.parse(s).takeUnless { it == GO_ZERO }
        } catch (_: DateTimeParseException) {
            try {
                java.time.OffsetDateTime.parse(s).toInstant()
            } catch (_: DateTimeParseException) {
                null
            }
        }
    }

    /** "just now", "4m ago", "3h ago", "2d ago"; "unknown" when absent. */
    fun ago(s: String?, now: Instant): String = parse(s)?.let { ago(it, now) } ?: "unknown"

    fun ago(t: Instant, now: Instant): String {
        val d = Duration.between(t, now)
        if (d.isNegative) return "in " + span(d.negated())
        if (d.seconds < 45) return "just now"
        return span(d) + " ago"
    }

    /** Like [ago], but "" when absent. */
    fun agoOrEmpty(s: String?, now: Instant): String = parse(s)?.let { ago(it, now) }.orEmpty()

    /** "in 2h 10m" / "5m ago" for future/past instants. */
    fun relative(s: String?, now: Instant): String {
        val t = parse(s) ?: return "unknown"
        return if (t.isAfter(now)) "in " + span(Duration.between(now, t)) else ago(t, now)
    }

    fun span(d: Duration): String {
        val m = d.toMinutes()
        return when {
            m < 1 -> "${d.seconds}s"
            m < 60 -> "${m}m"
            m < 48 * 60 -> if (m % 60 == 0L || m >= 600) "${m / 60}h" else "${m / 60}h ${m % 60}m"
            else -> "${m / (60 * 24)}d"
        }
    }

    fun clock(s: String?, zone: ZoneId = ZoneId.systemDefault()): String =
        parse(s)?.let { DateTimeFormatter.ofPattern("MMM d HH:mm").withZone(zone).format(it) } ?: "—"
}
