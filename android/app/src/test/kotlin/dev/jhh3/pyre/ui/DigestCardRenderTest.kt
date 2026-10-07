package dev.jhh3.pyre.ui

import android.app.Application
import android.graphics.Bitmap
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.test.captureToImage
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.platform.testTag
import androidx.test.ext.junit.runners.AndroidJUnit4
import dev.jhh3.pyre.core.PyreJson
import dev.jhh3.pyre.core.Times
import dev.jhh3.pyre.digest.DigestBuilder
import dev.jhh3.pyre.digest.DigestInput
import dev.jhh3.pyre.model.AttentionItem
import dev.jhh3.pyre.model.Event
import dev.jhh3.pyre.model.ReadSessionResponse
import dev.jhh3.pyre.model.SessionView
import dev.jhh3.pyre.ui.sessions.DigestCard
import dev.jhh3.pyre.ui.theme.PyreTheme
import java.io.File
import java.time.Instant
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.builtins.MapSerializer
import kotlinx.serialization.builtins.serializer
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.jsonObject
import org.junit.Assume.assumeTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config
import org.robolectric.annotation.GraphicsMode

/**
 * Renders the digest card from the shared synthetic fixture to a PNG (real
 * Skia) for review. Opt-in: -Ppyre.digest.render=<dir>, e.g.
 * `gradlew -p android testDebugUnitTest --tests '*DigestCardRenderTest' -Ppyre.digest.render=$PWD/out`.
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = Application::class, qualifiers = "w411dp-h891dp-xxhdpi")
@GraphicsMode(GraphicsMode.Mode.NATIVE)
class DigestCardRenderTest {
    @get:Rule val rule = createComposeRule()

    @Test
    fun renderCard() {
        val out = System.getProperty("pyre.digest.render")
        assumeTrue("set -Ppyre.digest.render=<dir> to render", !out.isNullOrEmpty())
        val o = PyreJson.wire.parseToJsonElement(File(System.getProperty("pyre.digest.fixtures.dir"), "input.json").readText()).jsonObject
        val j = PyreJson.wire
        val input = DigestInput(
            since = Times.parse((o["since"] as JsonPrimitive).content)!!,
            sinceCursor = (o["since_cursor"] as JsonPrimitive).content.toLong(),
            headCursor = (o["head_cursor"] as JsonPrimitive).content.toLong(),
            now = Times.parse((o["now"] as JsonPrimitive).content)!!,
            sinceLabel = "last seen",
            events = j.decodeFromJsonElement(ListSerializer(Event.serializer()), o["events"]!!),
            sessions = j.decodeFromJsonElement(ListSerializer(SessionView.serializer()), o["sessions"]!!),
            attention = j.decodeFromJsonElement(ListSerializer(AttentionItem.serializer()), o["attention"]!!),
            transcripts = j.decodeFromJsonElement(MapSerializer(String.serializer(), ReadSessionResponse.serializer()), o["transcripts"]!!),
        )
        val d = DigestBuilder.build(input)
        rule.setContent {
            PyreTheme(themeMode = "light", dynamicColor = false) {
                Surface(color = MaterialTheme.colorScheme.surface, modifier = Modifier.fillMaxWidth().testTag("shot")) {
                    Column { DigestCard(d, Instant.parse("2026-10-05T12:00:00Z"), {}, {}) }
                }
            }
        }
        val bmp = rule.onNodeWithTag("shot").captureToImage().asAndroidBitmap()
        File(out).mkdirs()
        File(out, "digest-card-light.png").outputStream().use { bmp.compress(Bitmap.CompressFormat.PNG, 100, it) }
    }
}
