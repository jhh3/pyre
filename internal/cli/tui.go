package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/internal/clientconn"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
)

// TUIOptions is what the CLI hands the terminal UI.
type TUIOptions struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Self is the calling agent session, if `pyre` runs inside one.
	Self Identity
}

// streamWatcher is implemented by *client.Client (event-stream WebSocket).
type streamWatcher interface {
	WatchEvents(ctx context.Context, afterCursor int64, fn func(contracts.Event) error) error
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
		// Beside the client config, so an isolated PYRE_CONFIG stays isolated.
		StateFile: filepath.Join(filepath.Dir(clientconn.DefaultConfigPath(os.Getenv)), "tui-state.json"),
	}
	if w, ok := api.(streamWatcher); ok {
		o.Watch = w.WatchEvents
	} else {
		o.Watch = tui.PollWatcher(api)
	}
	if opts.Stdout != nil {
		o.Output = opts.Stdout
	}
	return tui.Run(ctx, api, o)
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
