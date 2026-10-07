package com.termux.terminal;

import android.os.Handler;
import android.os.Looper;

import java.nio.charset.StandardCharsets;
import java.util.UUID;

/**
 * A terminal session backed by a REMOTE byte stream instead of a local process.
 * <p>
 * MODIFIED for Pyre (see ../../../../../../NOTICE): the upstream Termux class
 * forked a local subprocess through JNI (forkpty). This version has no native
 * code: bytes received from the hub's web-terminal relay are fed with
 * {@link #feed(byte[], int, int)}; keyboard input and size changes go to a
 * {@link RemoteIo} (the relay WebSocket). The public surface used by
 * {@code TerminalView} (updateSize, write, writeCodePoint, getEmulator, ...)
 * is unchanged. All emulation and callbacks run on the main thread.
 */
public final class TerminalSession extends TerminalOutput {

    /** Transport to the remote PTY (implemented by the app's relay socket). */
    public interface RemoteIo {
        void write(byte[] data, int offset, int count);

        void resize(int columns, int rows);
    }

    public final String mHandle = UUID.randomUUID().toString();

    TerminalEmulator mEmulator;

    /** Buffer to write translate code points into utf8 before writing to the remote. */
    private final byte[] mUtf8InputBuffer = new byte[5];

    /** Callback which gets notified when a session finishes or changes title. */
    TerminalSessionClient mClient;

    /** Set by the application for user identification of session, not by terminal. */
    public String mSessionName;

    private final Handler mMainThreadHandler;
    private final Integer mTranscriptRows;
    private final RemoteIo mRemote;
    private volatile boolean mFinished;
    private boolean mReadOnly;

    public TerminalSession(Integer transcriptRows, TerminalSessionClient client, RemoteIo remote) {
        this(transcriptRows, client, remote, new Handler(Looper.getMainLooper()));
    }

    public TerminalSession(Integer transcriptRows, TerminalSessionClient client, RemoteIo remote, Handler mainThreadHandler) {
        this.mTranscriptRows = transcriptRows;
        this.mClient = client;
        this.mRemote = remote;
        this.mMainThreadHandler = mainThreadHandler;
    }

    public void updateTerminalSessionClient(TerminalSessionClient client) {
        mClient = client;
        if (mEmulator != null)
            mEmulator.updateTerminalSessionClient(client);
    }

    /** Inform the remote PTY of the new size and reflow or initialize the emulator. */
    public void updateSize(int columns, int rows, int cellWidthPixels, int cellHeightPixels) {
        if (mEmulator == null) {
            initializeEmulator(columns, rows, cellWidthPixels, cellHeightPixels);
        } else {
            mEmulator.resize(columns, rows, cellWidthPixels, cellHeightPixels);
        }
        if (mRemote != null && !mFinished) mRemote.resize(columns, rows);
    }

    /** The terminal title as set through escape sequences or null if none set. */
    public String getTitle() {
        return (mEmulator == null) ? null : mEmulator.getTitle();
    }

    /** Creates the emulator; no process is started. */
    public void initializeEmulator(int columns, int rows, int cellWidthPixels, int cellHeightPixels) {
        mEmulator = new TerminalEmulator(this, columns, rows, cellWidthPixels, cellHeightPixels, mTranscriptRows, mClient);
    }

    /**
     * Feeds bytes received from the remote PTY. Safe to call from any thread;
     * emulation happens on the main thread. Bytes arriving before the view
     * has a size are buffered by an 80x24 emulator created on demand.
     */
    public void feed(byte[] data, int offset, int count) {
        final byte[] copy = new byte[count];
        System.arraycopy(data, offset, copy, 0, count);
        if (Looper.myLooper() == mMainThreadHandler.getLooper()) {
            appendOnMain(copy);
        } else {
            mMainThreadHandler.post(() -> appendOnMain(copy));
        }
    }

    private void appendOnMain(byte[] bytes) {
        if (mEmulator == null) initializeEmulator(80, 24, 12, 24);
        mEmulator.append(bytes, bytes.length);
        notifyScreenUpdate();
    }

    /** Marks the remote stream closed, showing [reason] in the terminal. */
    public void markFinished(String reason) {
        mMainThreadHandler.post(() -> {
            if (mFinished) return;
            mFinished = true;
            if (reason != null && !reason.isEmpty()) {
                byte[] msg = ("\r\n\u001b[2m[" + reason + "]\u001b[0m\r\n").getBytes(StandardCharsets.UTF_8);
                appendOnMain(msg);
            }
            if (mClient != null) mClient.onSessionFinished(TerminalSession.this);
        });
    }

    /** Read-only sessions drop keyboard input. */
    public void setReadOnly(boolean readOnly) {
        mReadOnly = readOnly;
    }

    /** Write keyboard input to the remote PTY. */
    @Override
    public void write(byte[] data, int offset, int count) {
        if (mFinished || mReadOnly || mRemote == null) return;
        mRemote.write(data, offset, count);
    }

    /** Write the Unicode code point to the terminal encoded in UTF-8. */
    public void writeCodePoint(boolean prependEscape, int codePoint) {
        if (codePoint > 1114111 || (codePoint >= 0xD800 && codePoint <= 0xDFFF)) {
            // 1114111 (= 2**16 + 1024**2 - 1) is the highest code point, [0xD800,0xDFFF] is the surrogate range.
            throw new IllegalArgumentException("Invalid code point: " + codePoint);
        }

        int bufferPosition = 0;
        if (prependEscape) mUtf8InputBuffer[bufferPosition++] = 27;

        if (codePoint <= /* 7 bits */0b1111111) {
            mUtf8InputBuffer[bufferPosition++] = (byte) codePoint;
        } else if (codePoint <= /* 11 bits */0b11111111111) {
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b11000000 | (codePoint >> 6));
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | (codePoint & 0b111111));
        } else if (codePoint <= /* 16 bits */0b1111111111111111) {
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b11100000 | (codePoint >> 12));
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | ((codePoint >> 6) & 0b111111));
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | (codePoint & 0b111111));
        } else {
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b11110000 | (codePoint >> 18));
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | ((codePoint >> 12) & 0b111111));
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | ((codePoint >> 6) & 0b111111));
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | (codePoint & 0b111111));
        }
        write(mUtf8InputBuffer, 0, bufferPosition);
    }

    public TerminalEmulator getEmulator() {
        return mEmulator;
    }

    /** Notify the {@link #mClient} that the screen has changed. */
    protected void notifyScreenUpdate() {
        if (mClient != null) mClient.onTextChanged(this);
    }

    /** Reset state for terminal emulator state. */
    public void reset() {
        if (mEmulator == null) return;
        mEmulator.reset();
        notifyScreenUpdate();
    }

    /** Remote sessions are ended by closing the relay socket, not by killing a process. */
    public void finishIfRunning() {
        markFinished(null);
    }

    @Override
    public void titleChanged(String oldTitle, String newTitle) {
        if (mClient != null) mClient.onTitleChanged(this);
    }

    public synchronized boolean isRunning() {
        return !mFinished;
    }

    /** Remote sessions have no local exit status. */
    public synchronized int getExitStatus() {
        return 0;
    }

    @Override
    public void onCopyTextToClipboard(String text) {
        if (mClient != null) mClient.onCopyTextToClipboard(this, text);
    }

    @Override
    public void onPasteTextFromClipboard() {
        if (mClient != null) mClient.onPasteTextFromClipboard(this);
    }

    @Override
    public void onBell() {
        if (mClient != null) mClient.onBell(this);
    }

    @Override
    public void onColorsChanged() {
        if (mClient != null) mClient.onColorsChanged(this);
    }

    /** No local process. */
    public int getPid() {
        return -1;
    }

    /** No local working directory. */
    public String getCwd() {
        return null;
    }
}
