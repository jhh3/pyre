package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/devlogin"
)

// openURL opens a URL in this machine's browser (tests replace it).
var openURL = func(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "linux":
		return exec.Command("xdg-open", u).Start()
	}
	return fmt.Errorf("no browser opener on %s", runtime.GOOS)
}

// DeviceLoginResult is `pyre accounts login --json` output.
type DeviceLoginResult struct {
	Login         devlogin.View          `json:"login"`
	Registration  *devlogin.Registration `json:"registration,omitempty"`
	NotRegistered string                 `json:"not_registered,omitempty"`
}

func (a *App) addAccountLogin(accounts *cobra.Command) {
	var provider, node string
	var register, noOpen bool
	var poll time.Duration
	cmd := &cobra.Command{
		Use:   "login --node MACHINE [--provider openai] [--register]",
		Short: "Log a machine into Codex with a device code (no ssh); saved there as a rota account",
		Long: `Runs ` + "`codex login --device-auth`" + ` on MACHINE in a private temporary home and
shows the link and one-time code here (the link is opened in this machine's
browser when possible). Approve it in any browser; the machine then saves
the login as the rota account named by its email
(~/.codex/accounts/<email>/auth.json), keeping a backup of an earlier copy.
The machine's own ~/.codex/auth.json is never touched and the credential
never leaves the machine.

--register also makes it a Pyre account on that machine (label: the email
before @, per-machine independent). A login that is the machine's own
default login is saved but not registered: Pyre already uses it.

Claude Code has no device-code login, so --provider anthropic is refused.
Ctrl-C cancels the login on the machine.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if node == "" {
				return usagef("--node is required (the machine to log in)")
			}
			prov := c.Provider(strings.ToLower(strings.TrimSpace(provider)))
			if prov == "" {
				prov = c.ProviderOpenAI
			}
			if prov != c.ProviderOpenAI && prov != c.ProviderAnthropic {
				return usagef("--provider must be openai or anthropic")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			nodeID, err := ops.NodeID(cmd.Context(), node)
			if err != nil {
				return err
			}
			return a.runDeviceLogin(cmd.Context(), ops, nodeID, node, prov, register, !noOpen, poll)
		}),
	}
	f := cmd.Flags()
	f.StringVar(&node, "node", "", "machine (node ID or name) to log in")
	f.StringVar(&provider, "provider", "openai", "openai (anthropic has no device-code login)")
	f.BoolVar(&register, "register", false, "also register the login as a Pyre account on that machine")
	f.BoolVar(&noOpen, "no-open", false, "do not open the link in this machine's browser")
	f.DurationVar(&poll, "poll", 8*time.Second, "status long-poll per round trip (max 10s)")
	_ = f.MarkHidden("poll")
	accounts.AddCommand(cmd)
}

func (a *App) runDeviceLogin(ctx context.Context, ops *Ops, nodeID, nodeName string, prov c.Provider, register, open bool, poll time.Duration) error {
	diag := a.Stdout
	if a.jsonOut {
		diag = a.Stderr
	}
	v, err := devlogin.Start(ctx, ops.API, nodeID, prov)
	if err != nil {
		return err
	}
	shown := false
	show := func(v devlogin.View) {
		if shown || v.VerificationURL == "" {
			return
		}
		shown = true
		fmt.Fprintf(diag, "Log %s into %s:\n\n  1. Open   %s\n  2. Enter  %s\n\n", nodeName, providerName(v.Provider), v.VerificationURL, v.UserCode)
		if open {
			if err := openURL(v.VerificationURL); err == nil {
				fmt.Fprintln(diag, "  (opened in your browser)")
			}
		}
		fmt.Fprintln(diag, "  "+devlogin.Describe(v)+" — Ctrl-C cancels")
	}
	show(v)
	wait := min(max(int(poll/time.Second), 0), devlogin.MaxWaitSeconds)
	for !v.Done() {
		start := time.Now()
		next, err := devlogin.Status(ctx, ops.API, nodeID, v.LoginID, wait)
		if ctx.Err() != nil {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			cv, cerr := devlogin.Cancel(cctx, ops.API, nodeID, v.LoginID)
			cancel()
			if cerr == nil {
				fmt.Fprintln(diag, "  "+devlogin.Describe(cv))
			}
			return ctx.Err()
		}
		if err != nil {
			if c.IsCode(err, c.ErrUnavailable) {
				continue // a slow round trip; the login keeps running on the node
			}
			return err
		}
		if d := a.loginPace() - time.Since(start); d > 0 && !next.Done() {
			time.Sleep(d) // the node answered at once (it ignores wait_seconds)
		}
		v = next
		show(v)
	}
	out := DeviceLoginResult{Login: v}
	switch {
	case v.State != devlogin.StateCompleted:
	case v.LiveLogin:
		out.NotRegistered = devlogin.LiveLoginRefusal
	case register:
		r, err := devlogin.Register(ctx, ops.API, v)
		if err != nil {
			if !c.IsCode(err, c.ErrForbidden) {
				return err
			}
			out.NotRegistered = err.(*c.Error).Message
		} else {
			out.Registration = &r
		}
	}
	err = a.emit(out, func(w io.Writer) {
		fmt.Fprintln(w, "  "+devlogin.Describe(v))
		switch {
		case out.NotRegistered != "":
			fmt.Fprintln(w, "  "+out.NotRegistered)
		case out.Registration != nil:
			fmt.Fprintf(w, "  registered as Pyre account %s (%s) on %s; verified=%v\n", out.Registration.Account.Label, out.Registration.Account.AccountID, nodeName, out.Registration.Verified)
		case v.State == devlogin.StateCompleted:
			fmt.Fprintf(w, "  to use it from Pyre: pyre accounts register --node %s --provider openai --rota %s --independent\n", nodeName, v.RotaName)
		}
	})
	if err == nil && v.State != devlogin.StateCompleted {
		return withStatus(ExitError, fmt.Errorf("%s", devlogin.Describe(v)))
	}
	return err
}

func providerName(p c.Provider) string {
	if p == c.ProviderAnthropic {
		return "Claude"
	}
	return "Codex (ChatGPT)"
}

// loginPace is the minimum time between status reads.
func (a *App) loginPace() time.Duration {
	if a.PollInterval > 0 && a.PollInterval < time.Second {
		return a.PollInterval
	}
	return time.Second
}
