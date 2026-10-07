package notifications

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"
)

// ErrNoDesktopNotifier is returned when neither osascript (macOS) nor
// notify-send (Linux) is available.
var ErrNoDesktopNotifier = errors.New("notifications: no desktop notifier available (osascript on macOS, notify-send on Linux)")

// Indirections for tests.
var (
	desktopGOOS     = runtime.GOOS
	desktopLookPath = exec.LookPath
	desktopRun      = func(ctx context.Context, path string, args ...string) error {
		return exec.CommandContext(ctx, path, args...).Run()
	}
)

// osascriptProgram receives title and body as argv, so user text is never
// interpolated into AppleScript source.
var osascriptProgram = []string{
	"-e", "on run argv",
	"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
	"-e", "end run",
}

// Desktop shows a local desktop notification (used by `pyre watch`). It runs
// osascript on macOS or notify-send on Linux directly (no shell), passing
// text as separate arguments. It returns ErrNoDesktopNotifier when no
// notifier is installed; callers should treat that as non-fatal.
func Desktop(title, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return DesktopContext(ctx, title, body)
}

// DesktopContext is Desktop with a caller-supplied context.
func DesktopContext(ctx context.Context, title, body string) error {
	title = desktopText(title, 120)
	body = desktopText(body, 400)
	if title == "" {
		title = "Pyre"
	}
	var bin string
	var args []string
	switch desktopGOOS {
	case "darwin":
		bin = "osascript"
		args = append(append([]string{}, osascriptProgram...), title, body)
	default:
		bin = "notify-send"
		// "--" ends option parsing so text beginning with '-' is not a flag.
		args = []string{"--app-name=Pyre", "--", title, body}
	}
	path, err := desktopLookPath(bin)
	if err != nil {
		return ErrNoDesktopNotifier
	}
	return desktopRun(ctx, path, args...)
}

// desktopText strips control characters, collapses whitespace, truncates,
// and keeps a leading '-' from being parsed as an option by tools that do
// not accept "--".
func desktopText(s string, n int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if strings.HasPrefix(out, "-") {
		out = "‐" + out[1:] // typographic hyphen
	}
	return clip(out, n)
}
