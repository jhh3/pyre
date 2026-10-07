package dev.jhh3.pyre.ui.terminal

import android.view.KeyEvent
import android.view.MotionEvent
import android.view.inputmethod.InputMethodManager
import com.termux.terminal.TerminalSession
import com.termux.view.TerminalView
import com.termux.view.TerminalViewClient
import dev.jhh3.pyre.terminal.RemoteTerminal

/**
 * TerminalView callbacks: tap shows the soft keyboard, pinch changes the font
 * size, and the extra-keys row's sticky Ctrl/Alt apply to the next key.
 */
class PyreTerminalViewClient(
    private val terminal: RemoteTerminal,
    private val view: () -> TerminalView?,
    private var fontPx: Int,
    private val onFontChanged: (Int) -> Unit,
) : TerminalViewClient {

    fun showKeyboard() {
        val v = view() ?: return
        v.requestFocus()
        v.context.getSystemService(InputMethodManager::class.java)?.showSoftInput(v, 0)
    }

    override fun onScale(scale: Float): Float {
        if (scale in 0.9f..1.1f) return scale
        val next = (fontPx * scale).toInt().coerceIn(MIN_FONT_PX, MAX_FONT_PX)
        if (next != fontPx) {
            fontPx = next
            view()?.setTextSize(next)
            onFontChanged(next)
        }
        return 1.0f
    }

    override fun onSingleTapUp(e: MotionEvent?) = showKeyboard()
    override fun shouldBackButtonBeMappedToEscape() = false
    override fun shouldEnforceCharBasedInput() = true
    override fun shouldUseCtrlSpaceWorkaround() = false
    override fun isTerminalViewSelected() = true
    override fun copyModeChanged(copyMode: Boolean) {}
    override fun onKeyDown(keyCode: Int, e: KeyEvent?, session: TerminalSession?) = false

    override fun onKeyUp(keyCode: Int, e: KeyEvent?): Boolean {
        terminal.clearModifiers()
        return false
    }

    override fun onLongPress(event: MotionEvent?) = false
    override fun readControlKey() = terminal.ctrlArmed
    override fun readAltKey() = terminal.altArmed
    override fun readShiftKey() = false
    override fun readFnKey() = false

    override fun onCodePoint(codePoint: Int, ctrlDown: Boolean, session: TerminalSession?): Boolean {
        // TerminalView has already read the armed modifiers for this key.
        terminal.clearModifiers()
        return false
    }

    override fun onEmulatorSet() {}

    // Terminal content may contain secrets: library logging is dropped.
    override fun logError(tag: String?, message: String?) {}
    override fun logWarn(tag: String?, message: String?) {}
    override fun logInfo(tag: String?, message: String?) {}
    override fun logDebug(tag: String?, message: String?) {}
    override fun logVerbose(tag: String?, message: String?) {}
    override fun logStackTraceWithMessage(tag: String?, message: String?, e: Exception?) {}
    override fun logStackTrace(tag: String?, e: Exception?) {}

    companion object {
        const val MIN_FONT_PX = 14
        const val MAX_FONT_PX = 64
    }
}
