package dev.jhh3.pyre.data

import dev.jhh3.pyre.core.Ids
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.model.AnswerAskItem
import kotlinx.serialization.builtins.MapSerializer
import kotlinx.serialization.builtins.serializer

/**
 * Stable operation_ids per user *intent* ("answer apr_x with allow",
 * "reissue msg_y", "launch draft z"). Repeated taps, notification actions,
 * process death and retries all reuse the same operation_id, so the hub's
 * idempotency returns the stored result instead of acting twice.
 * Persisted so the in-app button and a notification action share one id.
 */
class IntentIds(private val store: BlobStore, private val maxEntries: Int = 500) {
    private val ser = MapSerializer(String.serializer(), String.serializer())
    private val map: LinkedHashMap<String, String> = LinkedHashMap(
        store.get(FILE)?.let { runCatching { PyreJson.local.decodeFromString(ser, it) }.getOrNull() } ?: emptyMap(),
    )

    @Synchronized
    fun forIntent(key: String): String {
        map[key]?.let { return it }
        trim()
        val id = Ids.operation()
        map[key] = id
        persist()
        return id
    }

    /** Forget an intent after a terminal outcome so a *new* intent gets a new id. */
    @Synchronized
    fun complete(key: String) {
        if (map.remove(key) != null) persist()
    }

    private fun trim() {
        while (map.size >= maxEntries) map.remove(map.keys.first())
    }

    private fun persist() = store.put(FILE, PyreJson.local.encodeToString(ser, map))

    companion object {
        private const val FILE = "intent_ids.json"

        fun approvalAnswer(approvalId: String, optionId: String, text: String) = "approval:$approvalId:$optionId:${text.hashCode()}"
        fun reissue(messageId: String) = "reissue:$messageId"
        fun resolve(messageId: String, resolution: String) = "resolve:$messageId:$resolution"
        fun attention(attentionId: String, state: String) = "attention:$attentionId:$state"
        /** One batch of ask answers (order-independent), e.g. a notification action and the in-app button for the same choice. */
        fun asksAnswer(items: List<AnswerAskItem>) =
            "asks:" + items.sortedBy { it.askId }.joinToString(",") { "${it.askId}=${it.optionId}:${it.text.hashCode()}" }
    }
}
