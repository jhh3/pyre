package dev.jhh3.pyre.data

import dev.jhh3.pyre.core.PyreJson
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.serialization.Serializable

/** Non-secret device preferences. */
@Serializable
data class LocalPrefs(
    val hubStreamEnabled: Boolean = false,
    /** Last committed /v1/events/stream cursor (re-delivery is safe; skipping is not). */
    val eventCursor: Long = 0,
    val devCleartextAllowed: Boolean = false,
    val pushStatus: String = "",
    /** Appearance: "system" | "light" | "dark". */
    val themeMode: String = "system",
    /** Use wallpaper (Material You) colours on Android 12+. */
    val dynamicColor: Boolean = true,
    /** "While you were away" digest: this client's last-seen event cursor and time (RFC 3339). */
    val digestCursor: Long = 0,
    val digestSeenAt: String = "",
    /** The paired client the mark belongs to; a new pairing starts fresh. */
    val digestClientId: String = "",
    /** Sessions list grouping (a Grouping name; "" = the default, Project). */
    val sessionsGrouping: String = "",
    /** Sessions list shows ended (exited, failed, no run) sessions, like the TUI's ".". */
    val showEndedSessions: Boolean = false,
)

class LocalSettings(private val store: BlobStore) {
    private val _prefs = MutableStateFlow(
        store.get(FILE)?.let { runCatching { PyreJson.local.decodeFromString(LocalPrefs.serializer(), it) }.getOrNull() } ?: LocalPrefs(),
    )
    val prefs: StateFlow<LocalPrefs> = _prefs.asStateFlow()

    @Synchronized
    fun update(f: (LocalPrefs) -> LocalPrefs) {
        val next = f(_prefs.value)
        _prefs.value = next
        store.put(FILE, PyreJson.local.encodeToString(LocalPrefs.serializer(), next))
    }

    private companion object {
        const val FILE = "local_prefs.json"
    }
}
