package dev.jhh3.pyre.ui

import android.app.Application
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performScrollTo
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.data.HubCredentials
import dev.jhh3.pyre.data.InMemoryCredentialStore
import dev.jhh3.pyre.model.*
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.notify.PushSupport
import dev.jhh3.pyre.pairing.PairingService
import dev.jhh3.pyre.testing.FakePyreApi
import dev.jhh3.pyre.testing.testRepo
import dev.jhh3.pyre.ui.pairing.PairingScreen
import dev.jhh3.pyre.ui.pairing.PairingViewModel
import dev.jhh3.pyre.ui.settings.DeviceControls
import dev.jhh3.pyre.ui.settings.SettingsScreen
import dev.jhh3.pyre.ui.settings.SettingsViewModel
import dev.jhh3.pyre.ui.settings.pushLabel
import dev.jhh3.pyre.ui.theme.PyreTheme
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/** The redesigned home, settings and pairing flows (Robolectric Compose, synthetic fixtures). */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = Application::class)
class RedesignUiTest {
    @get:Rule val rule = createComposeRule()

    private val creds = HubCredentials(
        endpoint = HubEndpoint("https://hub.synthetic:8443", certSha256 = "ab".repeat(32)),
        clientId = "cli_synth-phone",
        token = "tok-synthetic",
        clientName = "Pixel (synthetic)",
        pairedAtMillis = 0,
    )

    private inner class PrefsHub : FakePyreApi() {
        var prefs = NotificationPrefs(quietHoursStart = "22:00", quietHoursEnd = "07:00", tz = "America/Los_Angeles")
        override suspend fun hello() = HelloResponse(hubVersion = "synthetic-1", protocol = 1, minProtocol = 1)
        override suspend fun getNotificationPrefs() = prefs
        override suspend fun setNotificationPrefs(req: SetNotificationPrefsRequest): NotificationPrefs {
            record(Ops.SET_NOTIFICATION_PREFS, req)
            prefs = req.prefs
            return prefs
        }
    }

    private fun device() = DeviceControls(
        pushStatus = pushLabel(fcmBuild = false, unavailableReason = null, registration = ""),
        notificationsAllowed = true,
        streamEnabled = false,
        batteryExempt = false,
        onRequestNotifications = {},
        onToggleStream = {},
        onRequestBatteryExemption = {},
        onUnpair = {},
    )

    @Test
    fun settingsShowsWhatPeopleChangeAndHidesConnectionInternals() {
        val vm = SettingsViewModel(testRepo(PrefsHub()))
        rule.setContent { PyreTheme { SettingsScreen(vm, creds, device()) } }
        rule.onNodeWithText("Live alerts").assertExists()
        rule.onNodeWithText("Quiet hours").assertExists()
        rule.onNodeWithText("Unpair").assertExists()
        // Internals are collapsed until asked for.
        rule.onNodeWithText("cli_synth-phone").assertDoesNotExist()
        rule.onNodeWithText("Certificate fingerprint").assertDoesNotExist()
        rule.onNodeWithTag("about-connection").performScrollTo().performClick()
        rule.onNodeWithText("cli_synth-phone").assertExists()
        rule.onNodeWithText("ABAB ABAB ABAB ABAB", substring = true).assertExists()
        // Firebase build instructions never reach the UI.
        assertTrue(rule.onAllNodesWithText("google-services", substring = true).fetchSemanticsNodes().isEmpty())
        assertTrue(rule.onAllNodesWithText("flavor", substring = true).fetchSemanticsNodes().isEmpty())
        assertFalse(pushLabel(false, null, "").contains("google-services"))
        assertTrue(PushSupport.NOT_CONFIGURED.contains("google-services")) // the string still exists for logs/docs, just not on screen
    }

    @Test
    fun quietHoursUseATimePickerNotFreeText() {
        val hub = PrefsHub()
        val vm = SettingsViewModel(testRepo(hub))
        rule.setContent { PyreTheme { SettingsScreen(vm, creds, device()) } }
        assertTrue("no HH:MM text fields", rule.onAllNodesWithText("HH:MM", substring = true).fetchSemanticsNodes().isEmpty())
        rule.onNodeWithTag("quiet-start").performScrollTo().performClick()
        rule.onNodeWithTag("time-picker").assertExists()
        rule.onNodeWithTag("time-ok").performClick()
        rule.waitUntil(5_000) { hub.count(Ops.SET_NOTIFICATION_PREFS) == 1 }
        val sent = hub.calls.first { it.first == Ops.SET_NOTIFICATION_PREFS }.second as SetNotificationPrefsRequest
        assertEquals("22:00", sent.prefs.quietHoursStart)
        assertEquals("07:00", sent.prefs.quietHoursEnd)
    }

    @Test
    fun pairingConfirmsHubAndCertificateWithoutShowingTheCode() {
        val pin = "11".repeat(32)
        val link = "pyre://pair?hub=https%3A%2F%2Fhub.synthetic%3A8443&pin=$pin&id=pair_synth-x&code=CODE-synthetic-secret"
        val vm = PairingViewModel(PairingService(InMemoryCredentialStore()), link, devAllowed = false)
        rule.setContent { PyreTheme { PairingScreen(vm) {} } }
        rule.onNodeWithText("Connect to this hub?").assertExists()
        rule.onNodeWithText("hub.synthetic:8443").assertExists()
        rule.onNodeWithText("1111 1111 1111 1111", substring = true).assertExists()
        rule.onNodeWithTag("pair-confirm").assertExists()
        assertTrue(rule.onAllNodesWithText("CODE-synthetic-secret", substring = true).fetchSemanticsNodes().isEmpty())
        // https link: no developer switch.
        rule.onNodeWithTag("dev-cleartext").assertDoesNotExist()
    }
}
