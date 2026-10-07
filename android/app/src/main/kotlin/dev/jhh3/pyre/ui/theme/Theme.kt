package dev.jhh3.pyre.ui.theme

import android.os.Build
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.ColorScheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.ReadOnlyComposable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import dev.jhh3.pyre.domain.Tone

/** Theme preference values stored in LocalPrefs.themeMode. */
object ThemeMode {
    const val SYSTEM = "system"
    const val LIGHT = "light"
    const val DARK = "dark"
}

/**
 * Pyre's own palette, used when wallpaper colours are off or unavailable
 * (Android < 12). One warm accent (flame); neutral, slightly warm surfaces.
 * Pairs are checked for WCAG contrast in ThemeContrastTest.
 */
val PyreLight: ColorScheme = lightColorScheme(
    primary = Color(0xFF9A4521),
    onPrimary = Color(0xFFFFFFFF),
    primaryContainer = Color(0xFFFFDBCC),
    onPrimaryContainer = Color(0xFF3A0B00),
    secondary = Color(0xFF6F5B52),
    onSecondary = Color(0xFFFFFFFF),
    secondaryContainer = Color(0xFFF6DED3),
    onSecondaryContainer = Color(0xFF281813),
    tertiary = Color(0xFF5E6135),
    onTertiary = Color(0xFFFFFFFF),
    tertiaryContainer = Color(0xFFE4E6AE),
    onTertiaryContainer = Color(0xFF1B1D00),
    error = Color(0xFFB3261E),
    onError = Color(0xFFFFFFFF),
    errorContainer = Color(0xFFF9DEDC),
    onErrorContainer = Color(0xFF410E0B),
    background = Color(0xFFFFF8F6),
    onBackground = Color(0xFF221A17),
    surface = Color(0xFFFFF8F6),
    onSurface = Color(0xFF221A17),
    surfaceVariant = Color(0xFFF2DFD8),
    onSurfaceVariant = Color(0xFF52443E),
    outline = Color(0xFF85736C),
    outlineVariant = Color(0xFFD8C2BA),
    surfaceContainerLowest = Color(0xFFFFFFFF),
    surfaceContainerLow = Color(0xFFFFF1EC),
    surfaceContainer = Color(0xFFFAEBE5),
    surfaceContainerHigh = Color(0xFFF4E5DF),
    surfaceContainerHighest = Color(0xFFEEDFD9),
    inverseSurface = Color(0xFF382E2B),
    inverseOnSurface = Color(0xFFFFEDE7),
    inversePrimary = Color(0xFFFFB68F),
)

/** Tuned dark: near-black neutral surfaces (OLED friendly), soft flame accent. */
val PyreDark: ColorScheme = darkColorScheme(
    primary = Color(0xFFFFB68F),
    onPrimary = Color(0xFF532200),
    primaryContainer = Color(0xFF763300),
    onPrimaryContainer = Color(0xFFFFDBCC),
    secondary = Color(0xFFE2C2B5),
    onSecondary = Color(0xFF412C24),
    secondaryContainer = Color(0xFF3A2E29),
    onSecondaryContainer = Color(0xFFFFDBCF),
    tertiary = Color(0xFFC8CA94),
    onTertiary = Color(0xFF30320B),
    tertiaryContainer = Color(0xFF46491F),
    onTertiaryContainer = Color(0xFFE4E6AE),
    error = Color(0xFFFFB4AB),
    onError = Color(0xFF690005),
    errorContainer = Color(0xFF93000A),
    onErrorContainer = Color(0xFFFFDAD6),
    background = Color(0xFF131110),
    onBackground = Color(0xFFEDE0DB),
    surface = Color(0xFF131110),
    onSurface = Color(0xFFEDE0DB),
    surfaceVariant = Color(0xFF3B3330),
    onSurfaceVariant = Color(0xFFD3C3BD),
    outline = Color(0xFF9C8D87),
    outlineVariant = Color(0xFF4A403C),
    surfaceContainerLowest = Color(0xFF0E0C0B),
    surfaceContainerLow = Color(0xFF1B1817),
    surfaceContainer = Color(0xFF201C1B),
    surfaceContainerHigh = Color(0xFF2A2624),
    surfaceContainerHighest = Color(0xFF35302E),
    inverseSurface = Color(0xFFEDE0DB),
    inverseOnSurface = Color(0xFF362F2C),
    inversePrimary = Color(0xFF9A4521),
)

@Composable
fun PyreTheme(
    themeMode: String = ThemeMode.SYSTEM,
    dynamicColor: Boolean = true,
    content: @Composable () -> Unit,
) {
    val dark = when (themeMode) {
        ThemeMode.LIGHT -> false
        ThemeMode.DARK -> true
        else -> isSystemInDarkTheme()
    }
    val ctx = LocalContext.current
    val scheme = when {
        dynamicColor && Build.VERSION.SDK_INT >= 31 -> if (dark) dynamicDarkColorScheme(ctx) else dynamicLightColorScheme(ctx)
        dark -> PyreDark
        else -> PyreLight
    }
    MaterialTheme(colorScheme = scheme, content = content)
}

/** True when the active scheme is dark (follows the app's theme choice, not only the system). */
@Composable
@ReadOnlyComposable
fun isDarkScheme(): Boolean = MaterialTheme.colorScheme.surface.luminance() < 0.5f

private fun Color.luminance(): Float = 0.2126f * red + 0.7152f * green + 0.0722f * blue

/**
 * Colour for a status. One accent: only what needs the person (attention)
 * uses the primary colour and only failures use the error colour. Working is
 * plain text colour; idle, offline, stale and ended are muted. The word next
 * to the colour always carries the meaning.
 */
@Composable
@ReadOnlyComposable
fun toneColor(t: Tone): Color {
    val c = MaterialTheme.colorScheme
    return when (t) {
        Tone.ATTENTION -> c.primary
        Tone.ERROR -> c.error
        Tone.ACTIVE -> c.onSurface
        Tone.OK -> c.onSurfaceVariant
        Tone.WARN -> c.onSurfaceVariant
        Tone.MUTED -> c.onSurfaceVariant
    }
}

/** Whether a tone deserves emphasis (bold word, filled dot). */
fun Tone.emphasized(): Boolean = this == Tone.ATTENTION || this == Tone.ERROR || this == Tone.ACTIVE
