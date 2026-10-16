//go:build windows || pyre_termhost

package main

// The Pyre terminal host (`pyred __pyre_termhost serve|attach`) is pyred's
// session host on Windows. Its VT emulator has a heavy package init, so
// macOS/Linux builds (tmux) leave it out unless built with
// -tags pyre_termhost (PYRE_SESSION_HOST=termhost then works there too).
import _ "github.com/jhh3/pyre/internal/runtime/termhost/hostproc"
