package dev.jhh3.pyre.terminal

import android.os.Handler
import android.os.Looper
import android.view.KeyEvent
import com.termux.terminal.KeyHandler
import com.termux.terminal.TerminalSession
import com.termux.terminal.TerminalSessionClient
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.update

/**
 * Glue between the relay [TerminalSocket] and a Termux [TerminalSession]
 * (vendored terminal-emulator, remote-backed). The emulator gives xterm-256 /
 * truecolor, wide/combining characters, alternate screen, scrollback and
 * mouse/keypad modes; this class only moves bytes and sizes.
 *
 * [onScreenUpdated] is set by the UI to its TerminalView's
 * `onScreenUpdated()`; it is cleared when the view goes away so the
 * ViewModel never retains a View.
 */
class RemoteTerminal(
    transcriptRows: Int = 2000,
    mainHandler: Handler = Handler(Looper.getMainLooper()),
) : TerminalSession.RemoteIo {
    /** Outbound transport; null until the socket exists. */
    @Volatile var sink: Sink? = null

    interface Sink {
        fun write(data: ByteArray, offset: Int, count: Int)
        fun resize(columns: Int, rows: Int)
    }

    var onScreenUpdated: (() -> Unit)? = null
    var onFinished: (() -> Unit)? = null
    var onBell: (() -> Unit)? = null

    /** Sticky soft modifiers from the extra-keys row, consumed by the next key. */
    data class Modifiers(val ctrl: Boolean = false, val alt: Boolean = false)

    val modifiers = MutableStateFlow(Modifiers())
    val ctrlArmed: Boolean get() = modifiers.value.ctrl
    val altArmed: Boolean get() = modifiers.value.alt

    fun toggleCtrl() = modifiers.update { it.copy(ctrl = !it.ctrl) }
    fun toggleAlt() = modifiers.update { it.copy(alt = !it.alt) }

    var columns = 0; private set
    var rows = 0; private set

    val session: TerminalSession = TerminalSession(transcriptRows, Client(), this, mainHandler)

    override fun write(data: ByteArray, offset: Int, count: Int) {
        sink?.write(data, offset, count)
    }

    override fun resize(columns: Int, rows: Int) {
        this.columns = columns
        this.rows = rows
        sink?.resize(columns, rows)
    }

    /** Bytes from the relay. */
    fun feed(bytes: ByteArray) = session.feed(bytes, 0, bytes.size)

    fun finish(reason: String?) = session.markFinished(reason)

    fun setReadOnly(readOnly: Boolean) = session.setReadOnly(readOnly)

    /** Extra-keys row: special keys honour DECCKM/keypad application modes. */
    fun sendKey(keyCode: Int) {
        val emu = session.emulator
        var mod = 0
        if (ctrlArmed) mod = mod or KeyHandler.KEYMOD_CTRL
        if (altArmed) mod = mod or KeyHandler.KEYMOD_ALT
        val code = KeyHandler.getCode(keyCode, mod, emu?.isCursorKeysApplicationMode ?: false, emu?.isKeypadApplicationMode ?: false)
        clearModifiers()
        if (code != null) session.write(code)
    }

    fun sendText(text: String) {
        text.codePoints().forEach { cp -> sendCodePoint(cp) }
    }

    /** Applies armed Ctrl/Alt to a typed code point (Ctrl+letter → C0 control). */
    fun sendCodePoint(codePoint: Int) {
        var cp = codePoint
        val alt = altArmed
        if (ctrlArmed) cp = ctrlTransform(cp)
        clearModifiers()
        session.writeCodePoint(alt, cp)
    }

    fun clearModifiers() {
        modifiers.value = Modifiers()
    }

    companion object {
        val ESC = KeyEvent.KEYCODE_ESCAPE
        val TAB = KeyEvent.KEYCODE_TAB
        val UP = KeyEvent.KEYCODE_DPAD_UP
        val DOWN = KeyEvent.KEYCODE_DPAD_DOWN
        val LEFT = KeyEvent.KEYCODE_DPAD_LEFT
        val RIGHT = KeyEvent.KEYCODE_DPAD_RIGHT
        val HOME = KeyEvent.KEYCODE_MOVE_HOME
        val END = KeyEvent.KEYCODE_MOVE_END
        val PAGE_UP = KeyEvent.KEYCODE_PAGE_UP
        val PAGE_DOWN = KeyEvent.KEYCODE_PAGE_DOWN
        val ENTER = KeyEvent.KEYCODE_ENTER

        /** Same mapping as TerminalView's Ctrl handling. */
        fun ctrlTransform(cp: Int): Int = when {
            cp in 'a'.code..'z'.code -> cp - 'a'.code + 1
            cp in 'A'.code..'Z'.code -> cp - 'A'.code + 1
            cp == ' '.code || cp == '2'.code -> 0
            cp == '['.code || cp == '3'.code -> 27
            cp == '\\'.code || cp == '4'.code -> 28
            cp == ']'.code || cp == '5'.code -> 29
            cp == '^'.code || cp == '6'.code -> 30
            cp == '_'.code || cp == '7'.code || cp == '/'.code -> 31
            cp == '8'.code -> 127
            else -> cp
        }
    }

    private inner class Client : TerminalSessionClient {
        override fun onTextChanged(changedSession: TerminalSession) { onScreenUpdated?.invoke() }
        override fun onTitleChanged(changedSession: TerminalSession) {}
        override fun onSessionFinished(finishedSession: TerminalSession) { onScreenUpdated?.invoke(); onFinished?.invoke() }
        override fun onCopyTextToClipboard(session: TerminalSession, text: String?) {}
        override fun onPasteTextFromClipboard(session: TerminalSession?) {}
        override fun onBell(session: TerminalSession) { onBell?.invoke() }
        override fun onColorsChanged(session: TerminalSession) { onScreenUpdated?.invoke() }
        override fun onTerminalCursorStateChange(state: Boolean) {}
        override fun getTerminalCursorStyle(): Int? = null
        // Terminal content may contain secrets: never log it. Library logs are dropped.
        override fun logError(tag: String?, message: String?) {}
        override fun logWarn(tag: String?, message: String?) {}
        override fun logInfo(tag: String?, message: String?) {}
        override fun logDebug(tag: String?, message: String?) {}
        override fun logVerbose(tag: String?, message: String?) {}
        override fun logStackTraceWithMessage(tag: String?, message: String?, e: Exception?) {}
        override fun logStackTrace(tag: String?, e: Exception?) {}
    }
}
