package dev.jhh3.pyre.digest

import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.data.LocalSettings

/** Where this client keeps its digest "last seen" mark. */
interface LastSeenStore {
    fun load(): LastSeen?

    /** Saves the mark; an older cursor never replaces a newer one. */
    fun save(mark: LastSeen)
}

class InMemoryLastSeenStore(var mark: LastSeen? = null) : LastSeenStore {
    override fun load() = mark

    override fun save(mark: LastSeen) {
        val cur = this.mark
        if (cur == null || mark.cursor >= cur.cursor) this.mark = mark
    }
}

/**
 * The mark in the app's own preferences, keyed by the paired client id, so
 * each owner client (this phone, each CLI config) keeps its own.
 */
class PrefsLastSeenStore(private val settings: LocalSettings, private val clientId: String) : LastSeenStore {
    override fun load(): LastSeen? {
        val p = settings.prefs.value
        if (p.digestClientId != clientId || p.digestCursor <= 0) return null
        val at = Times.parse(p.digestSeenAt) ?: return null
        return LastSeen(p.digestCursor, at)
    }

    override fun save(mark: LastSeen) {
        settings.update { p ->
            if (p.digestClientId == clientId && p.digestCursor > mark.cursor) p
            else p.copy(digestCursor = mark.cursor, digestSeenAt = mark.at.toString(), digestClientId = clientId)
        }
    }
}
