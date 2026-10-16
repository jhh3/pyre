// Package tui is Pyre's keyboard-driven terminal interface (R12, A17).
//
// It is a pure client of contracts.API: every load runs asynchronously
// with a timeout, so a slow hub or offline node never blocks navigation;
// selection is tracked by stable ID so refreshes and event bursts never
// move the cursor; event bursts are coalesced into one debounced refresh.
//
// Entry point for cmd/pyre (WP08):
//
//	err := tui.Run(ctx, api, tui.Options{
//		Attach: func(d contracts.AttachDescriptor) error { ... }, // runs while the TUI has released the terminal
//		Watch:  cl.WatchEvents,                                   // optional live events
//	})
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// WatchFunc streams events after afterCursor, calling fn for each in cursor
// order, until ctx ends or a non-retryable error occurs. client.Client's
// WatchEvents method has this shape; PollWatcher adapts any contracts.API.
type WatchFunc func(ctx context.Context, afterCursor int64, fn func(contracts.Event) error) error

// AttachFunc runs the attach mechanism for a run. The TUI calls it while the
// terminal is released (alt screen exited, raw mode off, input reader
// stopped), exactly like tea.ExecProcess; it should exec the descriptor's
// command with inherited stdio and return when the user detaches. When it
// returns, the TUI restores the screen with the same view, filter and
// selection. Never log d.Ticket.
type AttachFunc func(d contracts.AttachDescriptor) error

// EditFunc opens path in the user's editor and returns when the editor
// exits. Called with the terminal released, like AttachFunc.
type EditFunc func(path string) error

// Options configures the TUI. The zero value is usable.
type Options struct {
	// Attach runs an attach descriptor (see AttachFunc). Nil uses
	// DefaultAttach.
	Attach AttachFunc
	// Edit opens a document draft in an editor. Nil uses DefaultEdit
	// ($VISUAL, $EDITOR, then vi).
	Edit EditFunc
	// Watch optionally streams live events (e.g. client.WatchEvents or
	// PollWatcher(api)). Without it the TUI refreshes on RefreshInterval.
	Watch WatchFunc
	// Clock supplies "now" for ages and freshness (default system clock).
	Clock contracts.Clock

	// RefreshInterval is the periodic full refresh (default 5s; <0 disables).
	RefreshInterval time.Duration
	// Debounce coalesces bursts of events into one refresh (default 250ms).
	Debounce time.Duration
	// LoadTimeout bounds every API call (default 20s). A timed-out load keeps
	// the previous data and labels it with its age.
	LoadTimeout time.Duration
	// PreviewDelay debounces preview loads while the cursor moves (default
	// 120ms).
	PreviewDelay time.Duration
	// WatchRetry is the delay before restarting an ended event watcher
	// (default 3s). Periodic refresh continues meanwhile.
	WatchRetry time.Duration

	// NoColor forces monochrome output. The NO_COLOR environment variable
	// is also honored.
	NoColor bool
	// Output is where the program renders (default os.Stdout).
	Output io.Writer
	// ClientNodeID is the attaching client's node, if this machine is
	// enrolled (sent in AttachRequest so the hub can offer local_tmux).
	ClientNodeID string
	// LocalTmuxSocket is this machine's Pyre tmux socket (pyred's
	// <state>/tmux/pyre.sock). With ClientNodeID, sessions running here
	// are watched straight from it, without asking the hub (localterm.go).
	// Empty: learnt from the first local_tmux descriptor the hub returns.
	LocalTmuxSocket string
	// ProgramOptions are passed to tea.NewProgram after the defaults
	// (alt screen, context). Tests use tea.WithInput/WithOutput.
	ProgramOptions []tea.ProgramOption
	// ExecFunc replaces tea.Exec for attach/edit suspension. Testing hook;
	// nil means tea.Exec.
	ExecFunc func(c tea.ExecCommand, fn tea.ExecCallback) tea.Cmd

	// KeysFile is the key override file. Empty means DefaultKeysPath()
	// (~/.config/pyre/keys.toml, never created); "none" disables overrides.
	KeysFile string
	// Keymap, when set, is used as-is (tests); KeysFile is then ignored.
	Keymap *Keymap
	// Clipboard copies text (y). Nil uses OSC 52 through the terminal.
	Clipboard func(text string)
	// StartTerm starts the embedded terminal's attach command on a pty
	// (default StartPTY). Tests substitute an in-memory terminal.
	StartTerm TermStarter
	// DialRelay streams the read-only views (Sessions pane, Grid tiles) of
	// sessions on other machines through the hub's terminal relay instead
	// of running their ssh attach command (relay.go). Nil: the ssh path.
	DialRelay RelayDialer
	// SSHPreviews forces the old path for those views: the descriptor's
	// ssh attach command and, for tiles, an ssh window-size probe
	// (multiplexed per host). PYRE_TUI_PREVIEW=ssh sets it from `pyre`.
	SSHPreviews bool
	// LeaseIdle releases a focused pane's input lease after this long
	// without keys (default 60s); the next key re-takes it.
	LeaseIdle time.Duration
	// StartingRetry is the first retry delay while a run is starting
	// (default 500ms, doubling to at most 4x).
	StartingRetry time.Duration
	// CloudInfo describes a cloud node by name for the Machines view
	// ("gcp · e2-standard-8 · us-west1-b · running · ~$0.27/h running"),
	// "" for other machines. It reads a local snapshot written by the
	// `pyre node` cloud commands, never gcloud. Nil: no cloud line.
	CloudInfo func(node string) string
	// SeenDwell is how long a session stays selected before it counts as
	// looked at for its lead's "new" signal (default 2s; leads.go).
	SeenDwell time.Duration
	// WebURL is the web app (`pyre web serve`) that owns the inbox; the
	// ask signals link to it (weblink.go). Default DefaultWebURL.
	WebURL string
	// OpenURL opens a link in a browser (I). Nil uses the system opener
	// (open, xdg-open), which reports no display over ssh or without X or
	// Wayland; the TUI then copies the link (OSC 52) instead.
	OpenURL func(u string) error
	// StateFile is the small TUI state file (first-launch tips dismissed).
	// Empty or "none": no state file and no first-launch tips bar.
	StateFile string
}

func (o Options) withDefaults() Options {
	if o.Attach == nil {
		o.Attach = DefaultAttach
	}
	if o.Edit == nil {
		o.Edit = DefaultEdit
	}
	if o.Clock == nil {
		o.Clock = contracts.SystemClock{}
	}
	if o.WebURL == "" {
		o.WebURL = DefaultWebURL
	}
	if o.SeenDwell <= 0 {
		o.SeenDwell = 2 * time.Second
	}
	if o.RefreshInterval == 0 {
		o.RefreshInterval = 5 * time.Second
	}
	if o.Debounce <= 0 {
		o.Debounce = time.Second
	}
	if o.LoadTimeout <= 0 {
		o.LoadTimeout = 20 * time.Second
	}
	if o.LeaseIdle <= 0 {
		o.LeaseIdle = 60 * time.Second
	}
	if o.StartingRetry <= 0 {
		o.StartingRetry = 500 * time.Millisecond
	}
	if o.PreviewDelay <= 0 {
		o.PreviewDelay = 120 * time.Millisecond
	}
	if o.WatchRetry <= 0 {
		o.WatchRetry = 3 * time.Second
	}
	if o.Output == nil {
		o.Output = os.Stdout
	}
	if os.Getenv("NO_COLOR") != "" {
		o.NoColor = true
	}
	if o.ExecFunc == nil {
		o.ExecFunc = tea.Exec
	}
	return o
}

// renderFPS is how often Bubble Tea flushes a changed frame to the
// terminal (its maximum; the default is 60). A keystroke's echo in the
// embedded terminal waits for the next flush, so this bounds what the pane
// adds over a native terminal to ~8ms; an unchanged frame writes nothing.
const renderFPS = 120

// Run starts the TUI and blocks until the user quits or ctx ends. It does
// not own any agent lifetime: quitting never stops sessions.
func Run(ctx context.Context, api contracts.API, opts Options) error {
	if api == nil {
		return errors.New("tui: nil API")
	}
	m := New(ctx, api, opts)
	po := []tea.ProgramOption{tea.WithContext(ctx), tea.WithOutput(m.opts.Output), tea.WithFPS(renderFPS)}
	po = append(po, m.opts.ProgramOptions...)
	p := tea.NewProgram(m, po...)
	_, err := p.Run()
	m.Close()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// DefaultAttach executes terminal attach descriptors (local_tmux, ssh_tmux)
// with inherited stdio. Web-terminal and read-only descriptors cannot be
// executed here; the TUI handles them without calling Attach.
func DefaultAttach(d contracts.AttachDescriptor) error {
	switch d.Mechanism {
	case contracts.AttachLocalTmux, contracts.AttachSSHTmux:
		if len(d.Command) == 0 {
			return fmt.Errorf("attach descriptor for %s has no command", d.RunID)
		}
		argv := sshMux(d.Command)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("attach mechanism %q is not executable from the terminal", d.Mechanism)
}

// DefaultEdit opens path in $VISUAL, $EDITOR, or vi.
func DefaultEdit(path string) error {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if runtime.GOOS == "windows" {
		if ed == "" {
			ed = "notepad"
		}
		// cmd.exe parses $EDITOR (it may carry arguments, like sh -c does).
		cmd := exec.Command("cmd.exe")
		cmd.SysProcAttr = windowsRawCmdLine(`/c ` + ed + ` "` + path + `"`)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	if ed == "" {
		ed = "vi"
	}
	cmd := exec.Command("sh", "-c", ed+` "$1"`, "pyre-edit", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// PollWatcher adapts any contracts.API (mock, hub, daemon proxy) to a
// WatchFunc using long-polling ListEvents. A reset_required error is
// returned to the caller unchanged (the TUI re-snapshots).
func PollWatcher(api contracts.EventAPI) WatchFunc {
	return func(ctx context.Context, after int64, fn func(contracts.Event) error) error {
		failures := 0
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			resp, err := api.ListEvents(ctx, contracts.ListEventsRequest{AfterCursor: after, Limit: 200, WaitSeconds: 20})
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if contracts.IsCode(err, contracts.ErrResetRequired) || !retryable(err) {
					return err
				}
				failures++
				d := time.Duration(failures) * 500 * time.Millisecond
				if d > 10*time.Second {
					d = 10 * time.Second
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(d):
				}
				continue
			}
			failures = 0
			for _, ev := range resp.Events {
				if ev.Cursor <= after {
					continue
				}
				if err := fn(ev); err != nil {
					return err
				}
				after = ev.Cursor
			}
			if resp.NextCursor > after {
				after = resp.NextCursor
			}
		}
	}
}

// minSuspend is the shortest time a suspended command may take. Bubble Tea
// v1.3.10 races when an exec'd command returns almost instantly: the
// renderer's old listen goroutine can call ticker.Stop() after
// RestoreTerminal's ticker.Reset(), leaving the UI with no frame ticks (it
// stops repainting). A real attach takes seconds, but an attach that fails
// at once (ssh missing, bad command) would freeze the screen, so every
// suspended command lasts at least this long.
const minSuspend = 50 * time.Millisecond

func minDuration(fn func() error) error {
	start := time.Now()
	err := fn()
	if d := time.Since(start); d < minSuspend {
		time.Sleep(minSuspend - d)
	}
	return err
}

func retryable(err error) bool {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce.Retryable || ce.Code == contracts.ErrUnavailable
	}
	return !errors.Is(err, context.Canceled)
}
