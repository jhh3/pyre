package dev.jhh3.pyre.core

import java.security.SecureRandom

/**
 * Generates `<prefix>_<ULID>` IDs compatible with internal/contracts/ids.go.
 * Monotonic within one millisecond in this process.
 */
object Ids {
    private const val CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
    private val random = SecureRandom()
    private var lastMillis = -1L
    private val lastRand = ByteArray(10)

    fun operation(): String = new("op")
    fun message(): String = new("msg")

    @Synchronized
    fun new(prefix: String, nowMillis: Long = System.currentTimeMillis()): String {
        val r = ByteArray(10)
        if (nowMillis == lastMillis) {
            System.arraycopy(lastRand, 0, r, 0, 10)
            for (i in 9 downTo 0) {
                r[i] = (r[i] + 1).toByte()
                if (r[i].toInt() != 0) break
            }
        } else {
            random.nextBytes(r)
            lastMillis = nowMillis
        }
        System.arraycopy(r, 0, lastRand, 0, 10)
        val b = ByteArray(16)
        for (i in 0 until 6) b[i] = (nowMillis ushr (8 * (5 - i))).toByte()
        System.arraycopy(r, 0, b, 6, 10)
        return prefix + "_" + encode(b)
    }

    private fun encode(b: ByteArray): String {
        val out = CharArray(26)
        fun bit(pos: Int): Int {
            val p = pos - 2
            if (p < 0) return 0
            return (b[p / 8].toInt() ushr (7 - p % 8)) and 1
        }
        for (i in 0 until 26) {
            var v = 0
            for (j in 0 until 5) v = (v shl 1) or bit(i * 5 + j)
            out[i] = CROCKFORD[v]
        }
        return String(out)
    }

    fun prefixOf(id: String): String = id.substringBefore('_', "")
}
