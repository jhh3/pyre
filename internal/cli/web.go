package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/web"
)

// serveWeb runs the web app (tests replace it).
var serveWeb = func(ctx context.Context, opts web.Options) error {
	s, err := web.New(opts)
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

// webStateDir is the web app's default state directory:
// $XDG_STATE_HOME/pyre/web, else ~/.local/state/pyre/web.
func (a *App) webStateDir() string {
	if x := a.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "pyre", "web")
	}
	return filepath.Join(a.Getenv("HOME"), ".local", "state", "pyre", "web")
}

// addWebCommands adds `pyre web serve` (W-web-inbox).
func (a *App) addWebCommands(root *cobra.Command) {
	group := &cobra.Command{Use: "web", Short: "The web inbox and project pages (pyre web serve)"}
	var listen, stateDir string
	var origins []string
	var dev bool
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve the web inbox and project pages on loopback (ngrok puts it online)",
		Long: `Serve the owner's web app: an email-like inbox (asks and message threads,
answers now or held for a 5-minute batch) and a page per project.

It listens on loopback only (default ` + web.DefaultListen + `) and talks to the hub
with this client's owner credentials. ngrok exposes it at ` + web.DefaultOrigin + `
with Google sign-in; the server admits only requests whose X-Pyre-Email header
(set by the ngrok policy) is an @` + web.EmailDomain + ` address. --dev also admits
loopback requests without the header, for local testing; never use it behind
the tunnel. See docs/web.md.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error {
			if err := web.CheckListen(listen); err != nil {
				return usagef("%v", c.AsError(err).Message)
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p, err := ops.API.WhoAmI(cmd.Context())
			if err != nil {
				return err
			}
			if p.Kind != c.PrincipalOwner {
				return c.Errorf(c.ErrForbidden, "pyre web serve needs the owner's client credentials, but this connection is %s %s (unset PYRE_SESSION_TOKEN / PYRE_TOKEN and use the client config)", p.Kind, p.ID)
			}
			if stateDir == "" {
				stateDir = a.webStateDir()
			}
			note := ""
			if dev {
				note = " [dev: loopback requests without sign-in are allowed]"
			}
			fmt.Fprintf(a.Stderr, "pyre web: serving on http://%s (state %s)%s\n", listen, stateDir, note)
			return serveWeb(cmd.Context(), web.Options{API: ops.API, Listen: listen, Dev: dev, StateDir: stateDir, Origins: origins})
		}),
	}
	f := serve.Flags()
	f.StringVar(&listen, "listen", web.DefaultListen, "loopback address to listen on")
	f.BoolVar(&dev, "dev", false, "admit loopback requests without X-Pyre-Email (local testing only)")
	f.StringVar(&stateDir, "state-dir", "", "the web app's state directory (default $XDG_STATE_HOME/pyre/web or ~/.local/state/pyre/web)")
	f.StringSliceVar(&origins, "origin", []string{web.DefaultOrigin}, "public origin(s) accepted on POSTs besides the request's own host")
	group.AddCommand(serve)
	root.AddCommand(group)
}
