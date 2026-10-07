package dev.jhh3.pyre.terminal

import android.app.Activity
import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.Typeface
import android.os.Handler
import android.os.Looper
import android.view.View
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.termux.terminal.KeyHandler
import com.termux.terminal.TerminalEmulator
import com.termux.terminal.TextStyle
import com.termux.view.TerminalView
import dev.jhh3.pyre.ui.terminal.PyreTerminalViewClient
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config
import org.robolectric.annotation.GraphicsMode

/**
 * Renders byte streams captured from real TUIs through a tmux attach — exactly
 * what the hub relays — with the vendored Termux emulator. Captures were made
 * with android/scripts/capture-tui.py using isolated HOME/CLAUDE_CONFIG_DIR/
 * CODEX_HOME (onboarding/login screens; no credentials, no personal data).
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34], application = android.app.Application::class)
@GraphicsMode(GraphicsMode.Mode.NATIVE) // real Skia: font metrics and pixel output
class TerminalRenderTest {
    private fun resource(name: String) = javaClass.classLoader!!.getResourceAsStream("tui/$name")!!.readBytes()

    private fun terminal(cols: Int, rows: Int, bytes: ByteArray): RemoteTerminal {
        val t = RemoteTerminal(mainHandler = Handler(Looper.getMainLooper()))
        t.session.updateSize(cols, rows, 10, 20)
        t.feed(bytes)
        shadowOf(Looper.getMainLooper()).idle()
        return t
    }

    private fun TerminalEmulator.screenText(): String =
        screen.getSelectedText(0, 0, mColumns, mRows - 1).lines().joinToString("\n") { it.trimEnd() }

    private fun TerminalEmulator.lineOf(text: String): Int =
        (0 until mRows).first { screen.getSelectedText(0, it, mColumns, it).contains(text) }

    @Test
    fun claudeCodeOnboardingScreen() {
        val emu = terminal(100, 30, resource("claude-onboarding.bin")).session.emulator
        val text = emu.screenText()
        // The onboarding is taller than 30 rows; its header scrolled off the tmux client screen.
        assertTrue(text, text.contains("Let's get started"))
        assertTrue("theme picker rendered", text.contains("Dark mode"))
        assertTrue("box drawing rendered", text.contains("╌╌╌╌"))
        assertTrue("block glyphs", text.contains("█"))
        // Colour: some cell on the diff preview rows carries a non-default foreground/background.
        val row = emu.lineOf("Hello, Claude!")
        val styled = (0 until emu.mColumns).any {
            val st = emu.screen.getStyleAt(row, it)
            TextStyle.decodeForeColor(st) != TextStyle.COLOR_INDEX_FOREGROUND || TextStyle.decodeBackColor(st) != TextStyle.COLOR_INDEX_BACKGROUND
        }
        assertTrue("colour attributes preserved", styled)
    }

    @Test
    fun codexLoginScreen() {
        val emu = terminal(100, 30, resource("codex-login.bin")).session.emulator
        val text = emu.screenText()
        assertTrue(text, text.contains("Welcome to Codex"))
        assertTrue(text.contains("Sign in with ChatGPT"))
        assertTrue(text.contains("Press enter to continue"))
        assertTrue("alternate screen buffer active", emu.isAlternateBufferActive)
    }

    @Test
    fun truecolorWideCharsAndKeypadModes() {
        val seq = "\u001b[38;2;255;100;0mFIRE\u001b[0m 中文🔥|\u001b[?1h"
        val t = terminal(40, 5, seq.toByteArray())
        val emu = t.session.emulator
        val st = emu.screen.getStyleAt(0, 0)
        assertEquals("24-bit colour kept", 0xffff6400.toInt(), TextStyle.decodeForeColor(st))
        val line = emu.screen.getSelectedText(0, 0, 39, 0)
        assertTrue(line, line.startsWith("FIRE 中文🔥|"))
        // 中, 文 and 🔥 are double width: '|' sits at column 5 + 2*3 = 11, cursor after it at 12.
        assertEquals("|", emu.screen.getSelectedText(11, 0, 11, 0))
        assertEquals(12, emu.cursorCol)
        assertTrue("DECCKM application cursor keys", emu.isCursorKeysApplicationMode)
        assertEquals("\u001bOA", KeyHandler.getCode(RemoteTerminal.UP, 0, emu.isCursorKeysApplicationMode, emu.isKeypadApplicationMode))
    }

    @Test
    fun extraKeysAndStickyModifiersReachTheRelay() {
        val t = terminal(80, 24, "\u001b[?1h".toByteArray())
        val sent = java.io.ByteArrayOutputStream()
        val resizes = mutableListOf<Pair<Int, Int>>()
        t.sink = object : RemoteTerminal.Sink {
            override fun write(data: ByteArray, offset: Int, count: Int) = sent.write(data, offset, count)
            override fun resize(columns: Int, rows: Int) { resizes += columns to rows }
        }
        t.sendKey(RemoteTerminal.ESC)
        t.sendKey(RemoteTerminal.UP) // application mode -> ESC O A
        t.toggleCtrl()
        t.sendCodePoint('c'.code) // Ctrl-C
        assertTrue("ctrl is one-shot", !t.ctrlArmed)
        t.toggleAlt()
        t.sendText("x") // ESC x
        t.sendKey(RemoteTerminal.TAB)
        assertEquals("\u001b\u001bOA\u0003\u001bx\t", sent.toString(Charsets.UTF_8))
        t.session.updateSize(100, 40, 10, 20)
        assertEquals(listOf(100 to 40), resizes)
        t.setReadOnly(true)
        t.sendText("ignored")
        assertEquals("\u001b\u001bOA\u0003\u001bx\t", sent.toString(Charsets.UTF_8))
    }

    @Test
    fun terminalViewDrawsCapturedScreen() {
        val activity = Robolectric.buildActivity(Activity::class.java).setup().get()
        // The stream was rendered by tmux for 100x30; feed it at that size, then let the
        // view resize (reflow) the emulator to its own geometry, as on a phone.
        val t = terminal(100, 30, resource("claude-onboarding.bin"))
        val view = TerminalView(activity, null)
        view.setTerminalViewClient(PyreTerminalViewClient(t, { view }, 10) {})
        view.setTextSize(10) // creates the renderer; typeface after
        view.setTypeface(Typeface.MONOSPACE)
        view.setBackgroundColor(android.graphics.Color.BLACK)
        assertTrue(view.attachSession(t.session))
        activity.setContentView(view)
        view.measure(View.MeasureSpec.makeMeasureSpec(2200, View.MeasureSpec.EXACTLY), View.MeasureSpec.makeMeasureSpec(1600, View.MeasureSpec.EXACTLY))
        view.layout(0, 0, 2200, 1600)
        view.updateSize()
        t.onScreenUpdated = { view.onScreenUpdated() }
        shadowOf(Looper.getMainLooper()).idle()
        val emu = t.session.emulator
        assertTrue("view sized the emulator: ${emu.mColumns}x${emu.mRows}", emu.mColumns >= 40 && emu.mRows >= 20)
        val bmp = Bitmap.createBitmap(2200, 1600, Bitmap.Config.ARGB_8888)
        view.draw(Canvas(bmp)) // colours, box drawing, block glyphs
        java.io.File("build/terminal-render-claude.png").outputStream().use { bmp.compress(Bitmap.CompressFormat.PNG, 100, it) }
        assertTrue("${emu.mColumns}x${emu.mRows}:\n" + emu.screen.transcriptText, emu.screen.transcriptText.contains("Hello, Claude!"))
        // Real pixels: text was drawn, and in more than one colour.
        val colours = HashSet<Int>()
        var lit = 0
        for (y in 0 until bmp.height step 2) for (x in 0 until bmp.width step 2) {
            val p = bmp.getPixel(x, y)
            if (p != android.graphics.Color.BLACK) { lit++; colours += p and 0xF0F0F0 }
        }
        assertTrue("glyphs rendered ($lit lit samples)", lit > 1000)
        assertTrue("multiple colours rendered (${colours.size})", colours.size > 3)
    }
}
