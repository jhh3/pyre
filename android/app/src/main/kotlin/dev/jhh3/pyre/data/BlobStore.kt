package dev.jhh3.pyre.data

import java.io.File

/** Tiny named-text persistence for non-secret local state (outbox, cursors, intents). */
interface BlobStore {
    fun get(name: String): String?
    fun put(name: String, text: String?)
}

class FileBlobStore(private val dir: File) : BlobStore {
    @Synchronized
    override fun get(name: String): String? = File(dir, name).takeIf { it.isFile }?.readText()

    @Synchronized
    override fun put(name: String, text: String?) {
        val f = File(dir, name)
        if (text == null) {
            f.delete()
            return
        }
        dir.mkdirs()
        val tmp = File(dir, "$name.tmp")
        tmp.writeText(text)
        tmp.renameTo(f)
    }
}

class InMemoryBlobStore : BlobStore {
    private val map = HashMap<String, String>()
    override fun get(name: String) = map[name]
    override fun put(name: String, text: String?) {
        if (text == null) map.remove(name) else map[name] = text
    }
}
