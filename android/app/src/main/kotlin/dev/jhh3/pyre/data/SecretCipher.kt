package dev.jhh3.pyre.data

import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import java.nio.ByteBuffer
import java.security.GeneralSecurityException
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/** Authenticated encryption of small secrets. */
interface SecretCipher {
    fun encrypt(plain: ByteArray): ByteArray

    /** Throws [GeneralSecurityException] if the blob was tampered with or the key is gone. */
    fun decrypt(blob: ByteArray): ByteArray
}

/**
 * AES-256-GCM with a non-exportable key in the Android Keystore (StrongBox
 * when the device has one). The key does not require user authentication or
 * an unlocked device, because notification actions (approve/deny) must work
 * from the lock screen. Blob: `0x01 | ivLen | iv | ciphertext+tag`, with a
 * fixed AAD binding it to this purpose.
 */
class KeystoreAesGcmCipher(
    private val alias: String = ALIAS,
    private val aad: ByteArray = AAD,
) : SecretCipher {

    private fun key(): SecretKey {
        val ks = KeyStore.getInstance(PROVIDER).apply { load(null) }
        (ks.getKey(alias, null) as? SecretKey)?.let { return it }
        return generate(strongBox = Build.VERSION.SDK_INT >= 28)
    }

    private fun generate(strongBox: Boolean): SecretKey {
        val spec = KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
            .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
            .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
            .setKeySize(256)
            .setRandomizedEncryptionRequired(true)
            .apply { if (strongBox && Build.VERSION.SDK_INT >= 28) setIsStrongBoxBacked(true) }
            .build()
        return try {
            KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, PROVIDER).run {
                init(spec)
                generateKey()
            }
        } catch (e: Exception) {
            // StrongBoxUnavailableException / ProviderException on devices without StrongBox.
            if (strongBox) generate(strongBox = false) else throw e
        }
    }

    override fun encrypt(plain: ByteArray): ByteArray {
        val c = Cipher.getInstance(TRANSFORMATION)
        c.init(Cipher.ENCRYPT_MODE, key())
        c.updateAAD(aad)
        val iv = c.iv
        val ct = c.doFinal(plain)
        return ByteBuffer.allocate(2 + iv.size + ct.size).put(VERSION).put(iv.size.toByte()).put(iv).put(ct).array()
    }

    override fun decrypt(blob: ByteArray): ByteArray {
        if (blob.size < 2 || blob[0] != VERSION) throw GeneralSecurityException("unknown credential blob format")
        val ivLen = blob[1].toInt()
        if (ivLen !in 12..16 || blob.size < 2 + ivLen + 16) throw GeneralSecurityException("truncated credential blob")
        val ks = KeyStore.getInstance(PROVIDER).apply { load(null) }
        val key = ks.getKey(alias, null) as? SecretKey ?: throw GeneralSecurityException("credential key missing")
        val c = Cipher.getInstance(TRANSFORMATION)
        c.init(Cipher.DECRYPT_MODE, key, GCMParameterSpec(128, blob, 2, ivLen))
        c.updateAAD(aad)
        return c.doFinal(blob, 2 + ivLen, blob.size - 2 - ivLen)
    }

    companion object {
        const val PROVIDER = "AndroidKeyStore"
        const val ALIAS = "pyre_credentials_aes_gcm_v2"
        private const val TRANSFORMATION = "AES/GCM/NoPadding"
        private const val VERSION: Byte = 1
        private val AAD = "pyre-credentials-v2".toByteArray()
    }
}
