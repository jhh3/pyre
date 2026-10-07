package dev.jhh3.pyre.data

import android.annotation.SuppressLint
import android.content.Context
import android.content.SharedPreferences
import android.util.Base64
import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.net.HubEndpoint
import java.io.File
import java.security.KeyStore
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.Serializable

/** A paired hub. The token is a secret: never log it or put it in a URL. */
data class HubCredentials(
    val endpoint: HubEndpoint,
    val clientId: String,
    val token: String,
    val clientName: String,
    val pairedAtMillis: Long,
) {
    override fun toString(): String =
        "HubCredentials(hub=${endpoint.baseUrl}, pinned=${endpoint.pinned}, clientId=$clientId, token=<redacted>)"
}

interface CredentialStore {
    fun load(): HubCredentials?
    fun save(c: HubCredentials)
    fun clear()
}

class InMemoryCredentialStore(private var value: HubCredentials? = null) : CredentialStore {
    override fun load() = value
    override fun save(c: HubCredentials) { value = c }
    override fun clear() { value = null }
}

@Serializable
internal data class PairingRecord(
    val version: Int = 2,
    val hubUrl: String,
    val certSha256: String = "",
    val devCleartext: Boolean = false,
    val clientId: String,
    val token: String,
    val clientName: String = "",
    val pairedAtMillis: Long = 0,
) {
    fun toCredentials() = HubCredentials(HubEndpoint(hubUrl, certSha256, devCleartext), clientId, token, clientName, pairedAtMillis)

    override fun toString(): String = "PairingRecord(hub=$hubUrl, clientId=$clientId, token=<redacted>)"

    companion object {
        fun of(c: HubCredentials) = PairingRecord(
            hubUrl = c.endpoint.baseUrl, certSha256 = c.endpoint.certSha256, devCleartext = c.endpoint.devCleartext,
            clientId = c.clientId, token = c.token, clientName = c.clientName, pairedAtMillis = c.pairedAtMillis,
        )
    }
}

/** Read-and-delete access to a pre-S-G credential store, used once for migration. */
interface LegacyCredentialSource {
    fun read(): HubCredentials?
    fun delete()
}

/**
 * Pairing record encrypted with a Keystore AES-256-GCM key ([SecretCipher])
 * and kept in a DataStore file (excluded from backup and device transfer).
 *
 * On first read, if the new store is empty and the old
 * EncryptedSharedPreferences store holds a pairing, it is copied over,
 * read back to verify, and only then the old file (and its Tink master key)
 * is deleted. A blob that no longer decrypts (key invalidated) is discarded:
 * the device must pair again; the token is never written in clear text.
 *
 * The interface is synchronous (token lookups happen per request), so the
 * decrypted record is cached in memory after the first read.
 */
class DataStoreCredentialStore(
    private val dataStore: DataStore<Preferences>,
    private val cipher: SecretCipher,
    private val legacy: LegacyCredentialSource?,
) : CredentialStore {
    @Volatile private var loaded = false
    @Volatile private var cache: HubCredentials? = null

    override fun load(): HubCredentials? {
        if (!loaded) synchronized(this) {
            if (!loaded) {
                cache = runBlocking(Dispatchers.IO) { readOrMigrate() }
                loaded = true
            }
        }
        return cache
    }

    private suspend fun readOrMigrate(): HubCredentials? {
        val blob = dataStore.data.first()[KEY]
        if (blob != null) {
            val creds = try {
                decode(blob)
            } catch (_: Exception) {
                dataStore.edit { it.remove(KEY) } // undecryptable: never fall back to plaintext
                null
            }
            if (creds != null) runCatching { legacy?.delete() } // finish an interrupted migration
            return creds
        }
        val old = try {
            legacy?.read()
        } catch (_: Exception) {
            null
        } ?: return null
        write(old)
        val verified = dataStore.data.first()[KEY]?.let { runCatching { decode(it) }.getOrNull() }
        if (verified == old) runCatching { legacy?.delete() }
        return old
    }

    private fun decode(blob: String): HubCredentials {
        val plain = cipher.decrypt(Base64.decode(blob, Base64.NO_WRAP))
        return PyreJson.local.decodeFromString(PairingRecord.serializer(), String(plain, Charsets.UTF_8)).toCredentials()
    }

    private suspend fun write(c: HubCredentials) {
        val json = PyreJson.local.encodeToString(PairingRecord.serializer(), PairingRecord.of(c)).toByteArray(Charsets.UTF_8)
        val blob = Base64.encodeToString(cipher.encrypt(json), Base64.NO_WRAP)
        dataStore.edit { it[KEY] = blob }
    }

    override fun save(c: HubCredentials) {
        synchronized(this) {
            runBlocking(Dispatchers.IO) { write(c) }
            cache = c
            loaded = true
        }
    }

    override fun clear() {
        synchronized(this) {
            runBlocking(Dispatchers.IO) { dataStore.edit { it.remove(KEY) } }
            runCatching { legacy?.delete() }
            cache = null
            loaded = true
        }
    }

    companion object {
        private val KEY = stringPreferencesKey("pairing_v2")
        const val FILE = "pyre_pairing.preferences_pb"

        fun create(context: Context): DataStoreCredentialStore {
            val app = context.applicationContext
            val ds = PreferenceDataStoreFactory.create(
                scope = CoroutineScope(Dispatchers.IO + SupervisorJob()),
                produceFile = { File(app.filesDir, "datastore/$FILE") },
            )
            return DataStoreCredentialStore(ds, KeystoreAesGcmCipher(), LegacyEncryptedPrefsStore(app))
        }
    }
}

/**
 * The pre-S-G store: EncryptedSharedPreferences (androidx.security-crypto,
 * deprecated in 1.1.0) with a Tink master key in the Android Keystore. Kept
 * only to read and then delete an existing pairing.
 */
@Suppress("DEPRECATION") // security-crypto is deprecated; this class exists only to migrate away from it
class LegacyEncryptedPrefsStore(private val context: Context) : LegacyCredentialSource, CredentialStore {
    private fun fileExists() = File(context.applicationInfo.dataDir, "shared_prefs/$FILE.xml").exists()

    private fun prefs(): SharedPreferences {
        val key = MasterKey.Builder(context).setKeyScheme(MasterKey.KeyScheme.AES256_GCM).build()
        return EncryptedSharedPreferences.create(
            context, FILE, key,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    override fun read(): HubCredentials? = if (!fileExists()) null else load()

    override fun load(): HubCredentials? {
        val p = prefs()
        val url = p.getString(K_URL, null) ?: return null
        val token = p.getString(K_TOKEN, null) ?: return null
        return HubCredentials(
            endpoint = HubEndpoint(url, p.getString(K_PIN, "").orEmpty(), p.getBoolean(K_DEV, false)),
            clientId = p.getString(K_CLIENT, "").orEmpty(),
            token = token,
            clientName = p.getString(K_NAME, "").orEmpty(),
            pairedAtMillis = p.getLong(K_AT, 0),
        )
    }

    /** Writes in the old format — used by tests to build a realistic old-prefs fixture. */
    @SuppressLint("ApplySharedPref") // synchronous on purpose: the fixture must be on disk before migration runs
    override fun save(c: HubCredentials) {
        prefs().edit()
            .putString(K_URL, c.endpoint.baseUrl)
            .putString(K_PIN, c.endpoint.certSha256)
            .putBoolean(K_DEV, c.endpoint.devCleartext)
            .putString(K_CLIENT, c.clientId)
            .putString(K_TOKEN, c.token)
            .putString(K_NAME, c.clientName)
            .putLong(K_AT, c.pairedAtMillis)
            .commit()
    }

    override fun clear() = delete()

    override fun delete() {
        context.deleteSharedPreferences(FILE)
        runCatching {
            KeyStore.getInstance("AndroidKeyStore").apply { load(null) }.deleteEntry(MasterKey.DEFAULT_MASTER_KEY_ALIAS)
        }
    }

    companion object {
        const val FILE = "pyre_credentials"
        private const val K_URL = "hub_url"
        private const val K_PIN = "hub_cert_sha256"
        private const val K_DEV = "dev_cleartext"
        private const val K_CLIENT = "client_id"
        private const val K_TOKEN = "client_token"
        private const val K_NAME = "client_name"
        private const val K_AT = "paired_at"
    }
}
