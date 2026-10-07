package dev.jhh3.pyre.testing

import android.security.keystore.KeyGenParameterSpec
import java.io.InputStream
import java.io.OutputStream
import java.security.Key
import java.security.KeyStoreSpi
import java.security.Provider
import java.security.SecureRandom
import java.security.Security
import java.security.cert.Certificate
import java.security.spec.AlgorithmParameterSpec
import java.util.Collections
import java.util.Date
import java.util.Enumeration
import java.util.concurrent.ConcurrentHashMap
import javax.crypto.KeyGeneratorSpi
import javax.crypto.SecretKey
import javax.crypto.spec.SecretKeySpec

/**
 * JVM stand-in for the "AndroidKeyStore" JCA provider (Robolectric has none):
 * an in-memory KeyStore plus an AES KeyGenerator that honours
 * KeyGenParameterSpec's alias and size. Keys are plain SecretKeySpecs, so the
 * standard AES/GCM Cipher works with them. Enough for Tink's MasterKey /
 * EncryptedSharedPreferences and for KeystoreAesGcmCipher. Test-only.
 */
object FakeAndroidKeyStore {
    val keys = ConcurrentHashMap<String, SecretKey>()

    fun install() {
        if (Security.getProvider("AndroidKeyStore") == null) Security.insertProviderAt(ProviderImpl(), 1)
    }

    fun reset() = keys.clear()

    class ProviderImpl : Provider("AndroidKeyStore", 1.0, "Pyre test fake AndroidKeyStore") {
        init {
            put("KeyStore.AndroidKeyStore", StoreSpi::class.java.name)
            put("KeyGenerator.AES", AesGenSpi::class.java.name)
        }
    }

    class StoreSpi : KeyStoreSpi() {
        override fun engineGetKey(alias: String, password: CharArray?): Key? = keys[alias]
        override fun engineGetCertificateChain(alias: String?): Array<Certificate>? = null
        override fun engineGetCertificate(alias: String?): Certificate? = null
        override fun engineGetCreationDate(alias: String?): Date = Date(0)
        override fun engineSetKeyEntry(alias: String, key: Key, password: CharArray?, chain: Array<out Certificate>?) {
            keys[alias] = key as SecretKey
        }
        override fun engineSetKeyEntry(alias: String?, key: ByteArray?, chain: Array<out Certificate>?) = throw UnsupportedOperationException()
        override fun engineSetCertificateEntry(alias: String?, cert: Certificate?) = throw UnsupportedOperationException()
        override fun engineDeleteEntry(alias: String) { keys.remove(alias) }
        override fun engineAliases(): Enumeration<String> = Collections.enumeration(keys.keys.toList())
        override fun engineContainsAlias(alias: String) = keys.containsKey(alias)
        override fun engineSize() = keys.size
        override fun engineIsKeyEntry(alias: String) = keys.containsKey(alias)
        override fun engineIsCertificateEntry(alias: String?) = false
        override fun engineGetCertificateAlias(cert: Certificate?): String? = null
        override fun engineStore(stream: OutputStream?, password: CharArray?) {}
        override fun engineLoad(stream: InputStream?, password: CharArray?) {}
    }

    class AesGenSpi : KeyGeneratorSpi() {
        private var alias: String? = null
        private var bits = 256
        private val random = SecureRandom()

        override fun engineInit(random: SecureRandom?) {}
        override fun engineInit(params: AlgorithmParameterSpec?, random: SecureRandom?) {
            val spec = params as KeyGenParameterSpec
            alias = spec.keystoreAlias
            if (spec.keySize > 0) bits = spec.keySize
        }
        override fun engineInit(keysize: Int, random: SecureRandom?) { bits = keysize }
        override fun engineGenerateKey(): SecretKey {
            val k = SecretKeySpec(ByteArray(bits / 8).also { random.nextBytes(it) }, "AES")
            alias?.let { keys[it] = k }
            return k
        }
    }
}
