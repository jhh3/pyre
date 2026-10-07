package dev.jhh3.pyre.data

import dev.jhh3.pyre.core.Clock
import dev.jhh3.pyre.core.PyreJson
import java.io.File
import java.time.Instant
import kotlinx.serialization.KSerializer
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement

/**
 * Last-read snapshots of lists and documents so the app stays readable while
 * the hub is unreachable. Never contains credentials. Entries are labelled
 * with their save time so the UI can say "cached 12m ago".
 */
interface CacheStore {
    fun <T> read(key: String, ser: KSerializer<T>): Cached<T>?
    fun <T> write(key: String, ser: KSerializer<T>, value: T)
    fun clear()
}

data class Cached<T>(val value: T, val savedAt: Instant)

@Serializable
private data class CacheEnvelope(val savedAtMillis: Long, val payload: JsonElement)

class FileCacheStore(private val dir: File, private val clock: Clock = Clock.System) : CacheStore {
    private val json = PyreJson.local

    private fun file(key: String) = File(dir, key.replace(Regex("[^A-Za-z0-9._-]"), "_") + ".json")

    @Synchronized
    override fun <T> read(key: String, ser: KSerializer<T>): Cached<T>? {
        val f = file(key)
        if (!f.isFile) return null
        return try {
            val env = json.decodeFromString(CacheEnvelope.serializer(), f.readText())
            Cached(json.decodeFromJsonElement(ser, env.payload), Instant.ofEpochMilli(env.savedAtMillis))
        } catch (_: Exception) {
            f.delete()
            null
        }
    }

    @Synchronized
    override fun <T> write(key: String, ser: KSerializer<T>, value: T) {
        dir.mkdirs()
        val env = CacheEnvelope(clock.now().toEpochMilli(), json.encodeToJsonElement(ser, value))
        val tmp = File(dir, file(key).name + ".tmp")
        tmp.writeText(json.encodeToString(CacheEnvelope.serializer(), env))
        tmp.renameTo(file(key))
    }

    @Synchronized
    override fun clear() {
        dir.listFiles()?.forEach { it.delete() }
    }
}

class InMemoryCacheStore(private val clock: Clock = Clock.System) : CacheStore {
    private val map = HashMap<String, Pair<JsonElement, Instant>>()
    override fun <T> read(key: String, ser: KSerializer<T>): Cached<T>? =
        map[key]?.let { Cached(PyreJson.local.decodeFromJsonElement(ser, it.first), it.second) }

    override fun <T> write(key: String, ser: KSerializer<T>, value: T) {
        map[key] = PyreJson.local.encodeToJsonElement(ser, value) to clock.now()
    }

    override fun clear() = map.clear()
}
