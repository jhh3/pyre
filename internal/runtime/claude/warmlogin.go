package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/internal/osutil"
	"github.com/jhh3/pyre/internal/runtime/hostsel"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// WarmLoginOptions configures WarmLogin.
type WarmLoginOptions struct {
	// Bin is the claude executable (default "claude" on PATH).
	Bin string
	// TmuxBin is the tmux executable (default tmux on PATH).
	TmuxBin string
	// Dir is a private Pyre state dir for the throwaway tmux socket (short
	// path; created 0700).
	Dir string
	// Unset lists inherited variables removed from Claude's environment
	// (API keys, other logins).
	Unset []string
	// Env adds environment variables (e.g. the owner's DISABLE_AUTOUPDATER
	// opt-out); CLAUDE_CONFIG_DIR is always the profile.
	Env map[string]string
	// Poll is the credential check interval (default 250 ms).
	Poll time.Duration
}

// warmLoginArgs start the interactive TUI with no prompt, with hooks off
// (the owner's hooks must not see a throwaway start) and no MCP servers.
var warmLoginArgs = []string{"--settings", `{"disableAllHooks":true}`, "--strict-mcp-config"}

// WarmLogin lets Claude Code refresh its own login in profileDir (a Pyre
// profile that currently owns the login, e.g. a checked-out rota account
// whose access token expired at rest). Verified on Claude 2.1.289: the
// interactive TUI refreshes an expired access token at startup, before any
// prompt, while `claude auth status` does not. The TUI runs in a throwaway
// tmux server with no prompt, so no turn is sent and no quota is used; it is
// stopped as soon as fresh() reports a valid token (or ctx ends). fresh is
// the caller's read-only check of the profile's credential store.
func WarmLogin(ctx context.Context, o WarmLoginOptions, profileDir string, fresh func() bool) error {
	if o.Bin == "" {
		o.Bin = "claude"
	}
	if o.Poll <= 0 {
		o.Poll = 250 * time.Millisecond
	}
	if err := tmux.PrepareDir(o.Dir); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(o.Dir, "wl-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// A throwaway host of the node's kind (tmux with no config, as before;
	// a termhost on Windows).
	var srv sessionhost.Host
	if hostsel.Kind() == hostsel.KindTmux {
		srv, err = tmux.NewServer(o.TmuxBin, filepath.Join(dir, "s"))
	} else {
		srv, err = termhost.New(filepath.Join(dir, "s"))
	}
	if err != nil {
		return err
	}
	bg := context.WithoutCancel(ctx)
	env := map[string]string{}
	for k, v := range o.Env {
		env[k] = v
	}
	env["CLAUDE_CONFIG_DIR"] = profileDir
	if err := srv.NewSession(ctx, tmux.SessionSpec{Name: "warmlogin", Cwd: dir, Env: env,
		Unset: o.Unset, Argv: append([]string{o.Bin}, warmLoginArgs...), Width: 160, Height: 45}); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	pid := 0
	if p, err := srv.Pane(bg, "warmlogin"); err == nil {
		pid = p.PID
	}
	defer func() {
		_ = srv.KillServer(bg)
		if pid > 0 && osutil.SignalZero(pid) == nil {
			_ = osutil.Terminate(pid)
		}
	}()
	t := time.NewTicker(o.Poll)
	defer t.Stop()
	for {
		if fresh() {
			// Let Claude finish writing its own store before it is stopped.
			time.Sleep(time.Second)
			return nil
		}
		if p, err := srv.Pane(bg, "warmlogin"); err == nil && p.Dead || errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer) {
			return fmt.Errorf("claude exited before refreshing the login")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("claude did not refresh the login: %w", ctx.Err())
		case <-t.C:
		}
	}
}
