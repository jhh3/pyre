package dev.jhh3.pyre.ui

import androidx.compose.material3.ColorScheme
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import dev.jhh3.pyre.ui.nav.Navigator
import dev.jhh3.pyre.ui.nav.Screen
import dev.jhh3.pyre.ui.theme.PyreDark
import dev.jhh3.pyre.ui.theme.PyreLight
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * WCAG 2.x contrast for Pyre's own palettes (used when wallpaper colours are
 * off or on Android < 12; Material You schemes come with their own tonal
 * guarantees). Text pairs need 4.5:1; large/ornamental pairs 3:1.
 */
class ThemeContrastTest {
    private fun ratio(a: Color, b: Color): Double {
        val l1 = a.luminance() + 0.05
        val l2 = b.luminance() + 0.05
        return (maxOf(l1, l2) / minOf(l1, l2)).toDouble()
    }

    private fun check(name: String, s: ColorScheme) {
        val text = listOf(
            "onSurface/surface" to (s.onSurface to s.surface),
            "onSurfaceVariant/surface" to (s.onSurfaceVariant to s.surface),
            "onSurfaceVariant/surfaceContainerLow (cards)" to (s.onSurfaceVariant to s.surfaceContainerLow),
            "onSurfaceVariant/surfaceContainer (composer)" to (s.onSurfaceVariant to s.surfaceContainer),
            "onSurface/surfaceContainerHigh (agent bubble)" to (s.onSurface to s.surfaceContainerHigh),
            "primary/surface (needs-you labels)" to (s.primary to s.surface),
            "primary/surfaceContainerLow (needs-you labels on cards)" to (s.primary to s.surfaceContainerLow),
            "onPrimary/primary (Allow button)" to (s.onPrimary to s.primary),
            "onPrimaryContainer/primaryContainer" to (s.onPrimaryContainer to s.primaryContainer),
            "onSecondaryContainer/secondaryContainer (tonal buttons, your bubbles)" to (s.onSecondaryContainer to s.secondaryContainer),
            "error/surface (failures, Unpair)" to (s.error to s.surface),
            "onErrorContainer/errorContainer" to (s.onErrorContainer to s.errorContainer),
        )
        text.forEach { (pair, c) ->
            val r = ratio(c.first, c.second)
            assertTrue("$name $pair contrast %.2f < 4.5".format(r), r >= 4.5)
        }
        val outline = ratio(s.outline, s.surface)
        assertTrue("$name outline/surface contrast %.2f < 3".format(outline), outline >= 3.0)
    }

    @Test fun lightPaletteMeetsContrast() = check("light", PyreLight)

    @Test fun darkPaletteMeetsContrast() = check("dark", PyreDark)

    @Test
    fun homeIsSessionsAndThereAreFourDestinations() {
        assertEquals(Screen.Sessions, Navigator().current.screen)
        val roots = listOf(Screen.Sessions, Screen.Projects, Screen.Settings, Screen.Machines, Screen.Accounts, Screen.Activity).filter { it.isRoot }
        assertEquals(listOf(Screen.Sessions, Screen.Projects, Screen.Machines, Screen.Accounts), roots)
    }
}
