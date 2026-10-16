package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/clientconn"
	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
)

// cmdInboxWatch is the Claude doorbell monitor (S-D):
//
//	pyred inbox-watch [--session ses_…] [--run-dir DIR] [--min-interval 15s] [--grace 3s]
//
// Claude Code runs it as the per-run plugin's background monitor; it
// inherits the run's environment from the claude process (PYRE_SOCKET/
// PYRE_HUB, PYRE_SESSION_TOKEN, PYRE_SESSION_ID; the token is never on a
// command line, see internal/runtime/tmux/execenv.go). It waits for mail on
// pyred's local inbox.wait (no request held against the hub; W-localwait),
// falling back to a hub messages.list long-poll when the endpoint lacks it,
// lists the session's inbox, and prints one line per batch of new mail ("Pyre: N new messages … read
// them with the pyre inbox tool"). Each stdout line becomes a Claude
// notification, so stdout carries nothing else; diagnostics go to stderr.
// Message bodies are never printed. State (cursor, notified ids) lives in
// <run-dir>/doorbell.json so a restart does not ring twice.
func cmdInboxWatch(args []string) error {
	fs := flag.NewFlagSet("inbox-watch", flag.ContinueOnError)
	session := fs.String("session", os.Getenv(clientconn.EnvSessionID), "session whose inbox to watch (default $PYRE_SESSION_ID)")
	runDir := fs.String("run-dir", firstEnv("PYRE_CLAUDE_STATE_DIR", "PYRE_RUN_DIR"), "Claude adapter run state dir")
	minInterval := fs.Duration("min-interval", 15*time.Second, "minimum gap between notification lines (storm coalescing)")
	grace := fs.Duration("grace", 3*time.Second, "delay before ringing so the paste path can claim idle deliveries")
	remind := fs.Duration("remind", 10*time.Minute, "re-notify unread mail after this long (negative disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *session == "" {
		return errors.New("inbox-watch: no session (--session or $PYRE_SESSION_ID)")
	}
	conn, err := clientconn.Resolve(clientconn.ConnOptions{})
	if err != nil {
		return fmt.Errorf("inbox-watch: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	claudeplugin.WritePID(*runDir)
	defer claudeplugin.RemovePID(*runDir)
	state := ""
	if *runDir != "" {
		state = filepath.Join(*runDir, claudeplugin.StateFile)
	}
	err = claudeplugin.Watch(ctx, &claudeplugin.APISource{API: conn.Client, Session: *session}, claudeplugin.WatchOptions{
		Out: os.Stdout, Log: os.Stderr, StateFile: state, RunDir: *runDir,
		MinInterval: *minInterval, Grace: *grace, Remind: *remind,
		Alive: claudeplugin.ParentWatch(),
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, claudeplugin.ErrSessionGone) {
		return nil
	}
	return err
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
