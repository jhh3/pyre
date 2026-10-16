package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/clientconn"
	"github.com/jhh3/pyre/internal/cloud"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/terminal"
	"github.com/jhh3/pyre/internal/tui"
)

// TUIOptions is what the CLI hands the terminal UI.
type TUIOptions struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Self is the calling agent session, if `pyre` runs inside one.
	Self Identity
	// Relay: the API is the hub itself, whose terminal relay streams the
	// previews and Grid tiles of other machines' sessions (no ssh).
	Relay bool
}

// streamWatcher is implemented by *client.Client (event-stream WebSocket).
type streamWatcher interface {
	WatchEvents(ctx context.Context, afterCursor int64, fn func(contracts.Event) error) error
}

// terminalDialer is implemented by *client.Client (the hub's terminal
// relay): the TUI watches sessions on other machines through it instead
// of over ssh.
type terminalDialer interface {
	DialTerminal(ctx context.Context, d contracts.AttachDescriptor, cols, rows int, fit bool) (*terminal.ClientConn, error)
}

// relayDialer adapts a terminalDialer to tui.RelayDialer.
func relayDialer(td terminalDialer) tui.RelayDialer {
	return func(ctx context.Context, d contracts.AttachDescriptor, cols, rows int, fit bool) (tui.TermConn, error) {
		c, err := td.DialTerminal(ctx, d, cols, rows, fit)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// RunTUI launches the interactive terminal UI (internal/tui). Attach runs
// the descriptor's command in the foreground with inherited stdio while the
// UI is suspended, holding and renewing the run's input lease exactly like
// `pyre attach`; detaching returns to the same list, filter and selection.
// It refuses (unsupported) unless stdin and stdout are real terminals.
var RunTUI = func(ctx context.Context, api contracts.API, opts TUIOptions) error {
	if !ttyStream(opts.Stdin, os.Stdin) || !ttyStream(opts.Stdout, os.Stdout) {
		return contracts.Errorf(contracts.ErrUnsupported, "the interactive UI needs a terminal on stdin and stdout; use subcommands (pyre sessions, pyre attach, ...) or --json")
	}
	o := tui.Options{
		Attach:       tuiAttach(ctx, api, opts),
		Edit:         tui.DefaultEdit,
		ClientNodeID: opts.Self.NodeID,
		// Sessions on this machine are watched straight from pyred's tmux
		// server, without the hub (tui/localterm.go).
		LocalTmuxSocket: localTmuxSocket(os.Getenv, opts.Self.NodeID),
		// Beside the client config, so an isolated PYRE_CONFIG stays isolated.
		StateFile: filepath.Join(filepath.Dir(clientconn.DefaultConfigPath(os.Getenv)), "tui-state.json"),
		// W-gcp: cloud facts from the snapshot the cloud commands write.
		CloudInfo: cloud.CacheReader(filepath.Join(filepath.Dir(clientconn.DefaultConfigPath(os.Getenv)), cloud.CacheFileName)),
		// The web app that owns the inbox: the ask signals link to it.
		WebURL: tuiWebURL(os.Getenv, clientconn.DefaultConfigPath(os.Getenv)),
	}
	if w, ok := api.(streamWatcher); ok {
		o.Watch = w.WatchEvents
	} else {
		o.Watch = tui.PollWatcher(api)
	}
	if td, ok := api.(terminalDialer); ok && opts.Relay {
		o.DialRelay = relayDialer(td)
	}
	// PYRE_TUI_PREVIEW=ssh: previews and Grid tiles of other machines'
	// sessions run the ssh attach again (multiplexed) instead of the relay.
	o.SSHPreviews = strings.EqualFold(os.Getenv("PYRE_TUI_PREVIEW"), "ssh")
	if opts.Stdout != nil {
		o.Output = opts.Stdout
	}
	return tui.Run(ctx, api, o)
}

// tuiWebURL is the web app the TUI links its ask signals to:
// $PYRE_WEB_URL, else the client config's web_url, else "" (the TUI's
// default, tui.DefaultWebURL). An unreadable config falls through.
func tuiWebURL(getenv func(string) string, configPath string) string {
	if u := strings.TrimSpace(getenv(clientconn.EnvWebURL)); u != "" {
		return u
	}
	if cfg, err := clientconn.LoadConfig(configPath); err == nil {
		return strings.TrimSpace(cfg.WebURL)
	}
	return ""
}

// localTmuxSocket is the tmux socket of this machine's pyred
// (<state>/tmux/pyre.sock, the state dirs discoverLocalNode tries), or ""
// when the client has no node here or no such socket exists. The TUI
// still checks each run's tmux session there before using it.
func localTmuxSocket(getenv func(string) string, nodeID string) string {
	if nodeID == "" || runtime.GOOS == "windows" {
		return ""
	}
	var dirs []string
	if d := getenv("PYRE_STATE_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	if home := getenv("HOME"); home != "" {
		if runtime.GOOS == "darwin" {
			dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "pyre"))
		} else if x := getenv("XDG_STATE_HOME"); x != "" {
			dirs = append(dirs, filepath.Join(x, "pyre"))
		} else {
			dirs = append(dirs, filepath.Join(home, ".local", "state", "pyre"))
		}
	}
	for _, d := range dirs {
		p := filepath.Join(d, "tmux", "pyre.sock")
		if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return p
		}
	}
	return ""
}

// ttyStream reports whether s (or def when s is nil) is a terminal file.
func ttyStream(s any, def *os.File) bool {
	if s == nil {
		s = def
	}
	f, ok := s.(*os.File)
	return ok && isTTY(f)
}

// tuiAttach returns the AttachFunc used by the TUI. It is called with the
// terminal released, so it may own stdio until the attach command exits.
func tuiAttach(ctx context.Context, api contracts.API, opts TUIOptions) tui.AttachFunc {
	stdin, stdout, stderr := opts.Stdin, opts.Stdout, opts.Stderr
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	return func(d contracts.AttachDescriptor) error {
		switch d.Mechanism {
		case contracts.AttachLocalTmux, contracts.AttachSSHTmux:
		default:
			return contracts.Errorf(contracts.ErrUnsupported, "attach mechanism %q cannot run in this terminal", d.Mechanism)
		}
		if len(d.Command) == 0 {
			return contracts.Errorf(contracts.ErrInternal, "attach descriptor for run %s has no command", d.RunID)
		}
		if _, err := exec.LookPath(d.Command[0]); err != nil {
			return contracts.Errorf(contracts.ErrUnsupported, "attach needs %q on this machine: %v", d.Command[0], err)
		}
		var lease *contracts.InputLease
		if !d.ReadOnly {
			l, err := api.AcquireLease(ctx, contracts.LeaseRequest{Mutation: mut(""), RunID: d.RunID, TTLSeconds: 120})
			if err != nil {
				if contracts.IsCode(err, contracts.ErrConflict) {
					return fmt.Errorf("another client holds this run's input lease (use `pyre attach --read-only` to watch or `--takeover` to take over): %w", err)
				}
				return err
			}
			lease = &l
		}
		if d.Notes != "" {
			fmt.Fprintf(stderr, "pyre: %s\n", d.Notes)
		}
		stop := make(chan struct{})
		renewed := make(chan struct{})
		go func() {
			defer close(renewed)
			if lease == nil {
				return
			}
			t := time.NewTicker(40 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					if _, err := api.AcquireLease(rctx, contracts.LeaseRequest{Mutation: mut(""), RunID: d.RunID, TTLSeconds: 120}); err != nil {
						fmt.Fprintf(stderr, "pyre: could not renew input lease: %v\n", err)
					}
					cancel()
				}
			}
		}()
		cmd := exec.Command(d.Command[0], d.Command[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		runErr := cmd.Run()
		close(stop)
		<-renewed
		if lease != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if _, err := api.ReleaseLease(rctx, contracts.LeaseReleaseRequest{Mutation: mut(""), LeaseID: lease.LeaseID}); err != nil {
				fmt.Fprintf(stderr, "pyre: could not release input lease %s (it expires on its own): %v\n", lease.LeaseID, err)
			}
			cancel()
		}
		return runErr
	}
}
