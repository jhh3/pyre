package dev.jhh3.pyre.data

import android.content.Context
import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.testing.FakeAndroidKeyStore
import java.io.File
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/**
 * S-G: the pairing moves from EncryptedSharedPreferences to a Keystore
 * AES-GCM key + DataStore on first read. The old-prefs fixture is written by
 * the real old code path (security-crypto/Tink) on top of a fake
 * AndroidKeyStore provider.
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = android.app.Application::class)
class CredentialMigrationTest {
    private val ctx: Context = ApplicationProvider.getApplicationContext()
    private val creds = HubCredentials(HubEndpoint("https://hub.synthetic:7443", "ab".repeat(32)), "cli_synth-phone", "tok-synthetic-SECRET", "test phone", 1_791_000_000_000)

    @Before fun setUp() {
        FakeAndroidKeyStore.install()
        FakeAndroidKeyStore.reset()
    }

    private val seq = AtomicInteger()

    private fun dataFile(name: String) = File(ctx.filesDir, "datastore/$name-${seq.incrementAndGet()}.preferences_pb")

    // DataStore allows one instance per file per process; a new DataStoreCredentialStore
    // over the same DataStore has an empty cache, which models an app restart.
    private val stores = HashMap<File, androidx.datastore.core.DataStore<androidx.datastore.preferences.core.Preferences>>()

    private fun newStore(file: File, legacy: LegacyCredentialSource? = LegacyEncryptedPrefsStore(ctx)) =
        DataStoreCredentialStore(
            stores.getOrPut(file) { PreferenceDataStoreFactory.create(scope = CoroutineScope(Dispatchers.IO + SupervisorJob()), produceFile = { file }) },
            KeystoreAesGcmCipher(),
            legacy,
        )

    private fun oldPrefsFile() = File(ctx.applicationInfo.dataDir, "shared_prefs/${LegacyEncryptedPrefsStore.FILE}.xml")

    @Test
    fun migratesOldEncryptedPrefsThenDeletesThem() {
        LegacyEncryptedPrefsStore(ctx).save(creds)
        assertTrue("fixture written in the old format", oldPrefsFile().exists())
        assertFalse("old file is encrypted", oldPrefsFile().readText().contains("tok-synthetic-SECRET"))

        val file = dataFile("migrate")
        val store = newStore(file)
        assertEquals(creds, store.load())
        assertFalse("old prefs deleted after verified copy", oldPrefsFile().exists())
        assertFalse("old Tink master key deleted", FakeAndroidKeyStore.keys.containsKey("_androidx_security_master_key_"))
        assertTrue(FakeAndroidKeyStore.keys.containsKey(KeystoreAesGcmCipher.ALIAS))
        assertFalse("token never stored in clear", String(file.readBytes(), Charsets.ISO_8859_1).contains("tok-synthetic-SECRET"))

        // A fresh process reads the new store (no legacy source involved).
        val again = newStore(file, legacy = null)
        assertEquals(creds, again.load())
    }

    @Test
    fun nothingToMigrateMeansNotPaired() {
        assertNull(newStore(dataFile("empty")).load())
    }

    @Test
    fun saveClearAndRestart() {
        val file = dataFile("roundtrip")
        val s = newStore(file)
        s.save(creds)
        assertEquals(creds, newStore(file).load())
        s.clear()
        assertNull(newStore(file).load())
    }

    @Test
    fun lostKeyDiscardsBlobInsteadOfLeakingOrCrashing() {
        val file = dataFile("lostkey")
        newStore(file).save(creds)
        FakeAndroidKeyStore.reset() // e.g. Keystore wiped
        assertNull(newStore(file).load())
        assertNull("blob removed", newStore(file).load())
    }

    @Test
    fun interruptedMigrationFinishesOnNextStart() {
        val file = dataFile("interrupted")
        newStore(file, legacy = null).save(creds) // new store written ...
        LegacyEncryptedPrefsStore(ctx).save(creds) // ... but the old file survived a crash
        assertEquals(creds, newStore(file).load())
        assertFalse(oldPrefsFile().exists())
    }
}
