package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/winenv"
)

// MockBanner is printed whenever the backend reports it is the mock.
const MockBanner = "MOCK BACKEND: all data is synthetic; nothing here is runtime evidence"

// WhoAmIResult is `pyre whoami --json`.
type WhoAmIResult struct {
	Principal   c.Principal `json:"principal"`
	Mode        string      `json:"mode"`
	Endpoint    string      `json:"endpoint"`
	Source      string      `json:"source"`
	TokenSource string      `json:"token_source"`
	Self        Identity    `json:"self"`
}

// PairResult is `pyre pair/login --json` (never includes the token).
type PairResult struct {
	ClientID      string `json:"client_id,omitempty"`
	HubURL        string `json:"hub_url"`
	HubCertSHA256 string `json:"hub_cert_sha256,omitempty"`
	ConfigPath    string `json:"config_path"`
}

func (a *App) addSystemCommands(root *cobra.Command) {
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the pyre version",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			v := map[string]any{"version": buildinfo.Version, "protocol": c.ProtocolVersion}
			return a.emit(v, func(w io.Writer) { fmt.Fprintf(w, "pyre %s (protocol %d)\n", buildinfo.Version, c.ProtocolVersion) })
		}),
	})

	var keysFile string
	keysCmd := &cobra.Command{
		Use:   "keys",
		Short: "Print the interactive UI's effective key map (defaults plus keys.toml overrides)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			path := keysFile
			if path == "" {
				path = keysPath(a.Getenv)
			}
			ref, err := tui.KeymapReference(path)
			if err != nil {
				return c.Errorf(c.ErrInvalid, "key map: %v", err)
			}
			return a.emit(map[string]any{"path": path, "reference": ref}, func(w io.Writer) {
				fmt.Fprintf(w, "key map: %s\n\n%s\n", path, strings.TrimRight(ref, "\n"))
			})
		}),
	}
	keysCmd.Flags().StringVar(&keysFile, "file", "", "keys.toml to read (default ~/.config/pyre/keys.toml)")
	root.AddCommand(keysCmd)

	root.AddCommand(&cobra.Command{
		Use:   "hello",
		Short: "Check the connection and protocol compatibility with the hub or daemon",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			h, err := ops.API.Hello(cmd.Context(), c.HelloRequest{Protocol: c.ProtocolVersion, Client: "pyre-cli/" + buildinfo.Version})
			if err != nil {
				return err
			}
			if h.Mock && a.jsonOut {
				a.warnf("%s", MockBanner)
			}
			if err := a.emit(h, func(w io.Writer) {
				fmt.Fprintf(w, "hub %s %s, protocol %d (min %d), server time %s\n", h.HubID, orDash(h.HubVersion), h.Protocol, h.MinProtocol, stamp(h.ServerTime))
				fmt.Fprintf(w, "authenticated as %s %s", h.Principal.Kind, h.Principal.ID)
				if h.Principal.Name != "" {
					fmt.Fprintf(w, " (%s)", h.Principal.Name)
				}
				fmt.Fprintln(w)
				if a.conn != nil {
					fmt.Fprintf(w, "via %s\n", a.conn.Describe())
				}
				if len(h.Features) > 0 {
					fmt.Fprintf(w, "features: %s\n", strings.Join(h.Features, ", "))
				}
				if h.Mock {
					fmt.Fprintln(w, MockBanner)
				}
			}); err != nil {
				return err
			}
			if h.Protocol < c.MinProtocolVersion || h.MinProtocol > c.ProtocolVersion {
				return c.Errorf(c.ErrProtocolMismatch, "hub speaks protocol %d (min %d); this pyre speaks %d (min %d): upgrade the older side", h.Protocol, h.MinProtocol, c.ProtocolVersion, c.MinProtocolVersion)
			}
			return nil
		}),
	})

	root.AddCommand(&cobra.Command{
		Use:   "whoami",
		Short: "Show the authenticated identity and how pyre is connected",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p, err := ops.API.WhoAmI(cmd.Context())
			if err != nil {
				return err
			}
			res := WhoAmIResult{Principal: p, Self: ops.Self, Mode: "in-process"}
			if a.conn != nil {
				res.Mode, res.Source, res.TokenSource = a.conn.Mode, a.conn.Source, a.conn.TokenSource
				res.Endpoint = a.conn.HubURL
				if a.conn.Mode == "socket" {
					res.Endpoint = a.conn.Socket
				}
			}
			return a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "%s %s", p.Kind, p.ID)
				if p.Name != "" {
					fmt.Fprintf(w, " (%s)", p.Name)
				}
				if p.OnBehalfOf != nil {
					fmt.Fprintf(w, " on behalf of %s %s", p.OnBehalfOf.Kind, p.OnBehalfOf.ID)
				}
				fmt.Fprintln(w)
				if a.conn != nil {
					fmt.Fprintf(w, "via %s\n", a.conn.Describe())
				}
				if ops.Self.SessionID != "" {
					fmt.Fprintf(w, "agent session %s run %s on node %s\n", ops.Self.SessionID, orDash(ops.Self.RunID), orDash(ops.Self.NodeID))
				}
			})
		}),
	})

	var pairName string
	var pairForce bool
	pair := &cobra.Command{
		Use:   "pair [-]",
		Short: "Pair this machine's CLI with a hub using a one-time pairing link",
		Long: `Exchange a one-time pairing link (from ` + "`pyre pair-client`" + ` or the hub) for an owner
credential and write it to the client config (mode 0600).

The link carries a one-time secret, so it is read from stdin (paste it at
the prompt, or pipe it: pyre pair < link.txt), never from the command line,
where other local processes could read it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] != "-" {
				return usagef("the pairing link is a one-time secret and is not accepted as an argument (visible to other local processes); run `pyre pair` and paste it, or `pyre pair < link.txt`")
			}
			if f, ok := a.Stdin.(*os.File); ok && isTTY(f) {
				fmt.Fprint(a.Stderr, "paste the pairing link: ")
			}
			line, err := bufio.NewReader(io.LimitReader(a.Stdin, 64<<10)).ReadString('\n')
			if err != nil && line == "" {
				return usagef("no pairing link on stdin")
			}
			p, err := enrollment.ParsePairingURI(strings.TrimSpace(line))
			if err != nil {
				return err
			}
			path := a.configPath()
			if err := a.checkOverwrite(path, pairForce); err != nil {
				return err
			}
			// Without --name the hub keeps the name given to `pyre
			// pair-client --name` (the pairing's name; the hub falls back to
			// the client kind when the pairing has none).
			cred, err := enrollment.ExchangePairing(cmd.Context(), p, pairName, "cli")
			if err != nil {
				return err
			}
			cfg := ClientConfig{HubURL: p.HubURL, HubCertSHA256: p.PinSHA256, ClientID: cred.ClientID, Token: cred.Token, PairedAt: time.Now().UTC()}
			keepClientSettings(path, &cfg)
			if err := SaveConfig(path, cfg); err != nil {
				return err
			}
			res := PairResult{ClientID: cred.ClientID, HubURL: p.HubURL, HubCertSHA256: p.PinSHA256, ConfigPath: path}
			return a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "paired as client %s with %s\ncredential written to %s (0600); check with `pyre hello`\n", cred.ClientID, p.HubURL, path)
			})
		}),
	}
	pair.Flags().StringVar(&pairName, "name", "", "client name shown on the hub (default: the --name given to pyre pair-client)")
	pair.Flags().BoolVar(&pairForce, "force", false, "replace an existing client config")
	root.AddCommand(pair)

	var loginToken string
	var loginForce, loginNoVerify bool
	login := &cobra.Command{
		Use:   "login --hub URL --token -",
		Short: "Configure a hub URL and credential manually",
		Long: `Write the client config (mode 0600) from an explicit hub URL and token.
The token is read from stdin with --token - , or taken from PYRE_TOKEN; it is
never accepted as an argument (other local processes can read command
lines). The connection is verified with a hello unless --no-verify.

On stdin, the hub's owner-credential.json (written by pyre-hub init) also
works as is: its token is used, and its hub URL and TLS pin fill in --hub
and --pin when they are not given:

  pyre login --token - < ~/.local/share/pyre-hub/owner-credential.json`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if loginToken != "" && loginToken != "-" {
				return usagef("--token takes only - (read the token from stdin); a token on the command line is visible to other local processes. Use --token - or PYRE_TOKEN")
			}
			tok := loginToken
			if tok == "" {
				tok = a.Getenv(EnvToken)
			}
			if tok == "-" {
				b, err := io.ReadAll(io.LimitReader(a.Stdin, 64<<10))
				if err != nil {
					return err
				}
				tok = strings.TrimSpace(string(b))
			}
			hubURL, pin := a.hubFlag, a.pinFlag
			if strings.HasPrefix(tok, "{") {
				// The hub's owner-credential.json (pyre-hub init) piped as is:
				// its token, and its hub URL and pin unless given as flags.
				var oc struct {
					HubURL        string `json:"hub_url"`
					HubCertSHA256 string `json:"hub_cert_sha256"`
					Token         string `json:"token"`
				}
				if err := json.Unmarshal([]byte(tok), &oc); err != nil {
					return usagef("stdin looks like JSON but is not a credential file: %v", err)
				}
				tok = oc.Token
				if hubURL == "" {
					hubURL = oc.HubURL
				}
				if pin == "" {
					pin = oc.HubCertSHA256
				}
			}
			if hubURL == "" {
				return usagef("--hub is required")
			}
			if tok == "" {
				return usagef("--token (or PYRE_TOKEN, or --token - for stdin) is required")
			}
			path := a.configPath()
			if err := a.checkOverwrite(path, loginForce); err != nil {
				return err
			}
			cfg := ClientConfig{HubURL: strings.TrimRight(hubURL, "/"), HubCertSHA256: pin, Token: tok, PairedAt: time.Now().UTC()}
			cl := client.New(cfg.HubURL, tok, client.Options{CertSHA256: cfg.HubCertSHA256, UserAgent: "pyre-cli/" + buildinfo.Version, MaxAttempts: 2})
			if err := cl.Err(); err != nil {
				return err
			}
			if !loginNoVerify {
				h, err := cl.Hello(cmd.Context(), c.HelloRequest{Protocol: c.ProtocolVersion, Client: "pyre-cli/" + buildinfo.Version})
				if err != nil {
					return fmt.Errorf("verify hub (use --no-verify to skip): %w", err)
				}
				if h.Mock {
					a.warnf("%s", MockBanner)
				}
				if h.Principal.Kind == c.PrincipalOwner {
					cfg.ClientID = h.Principal.ID
				}
			}
			keepClientSettings(path, &cfg)
			if err := SaveConfig(path, cfg); err != nil {
				return err
			}
			res := PairResult{ClientID: cfg.ClientID, HubURL: cfg.HubURL, HubCertSHA256: cfg.HubCertSHA256, ConfigPath: path}
			return a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "configured %s; credential written to %s (0600)\n", cfg.HubURL, path)
			})
		}),
	}
	login.Flags().StringVar(&loginToken, "token", "", "- to read the credential from stdin (values other than - are refused)")
	login.Flags().BoolVar(&loginForce, "force", false, "replace an existing client config")
	login.Flags().BoolVar(&loginNoVerify, "no-verify", false, "do not contact the hub before saving")
	root.AddCommand(login)

	root.AddCommand(&cobra.Command{
		Use:   "exit-codes",
		Short: "Explain pyre's exit codes",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(a.Stdout, exitCodesHelp)
			return nil
		}),
	})
}

func (a *App) configPath() string {
	if a.configFlag != "" {
		return a.configFlag
	}
	return DefaultConfigPath(a.Getenv)
}

func (a *App) checkOverwrite(path string, force bool) error {
	if force {
		return nil
	}
	if _, err := os.Lstat(path); err == nil {
		return c.Errorf(c.ErrConflict, "client config %s already exists; pass --force to replace it", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// keysPath mirrors tui.DefaultKeysPath with the CLI's environment:
// $XDG_CONFIG_HOME/pyre/keys.toml, else ~/.config/pyre/keys.toml.
func keysPath(getenv func(string) string) string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "pyre", "keys.toml")
	}
	if d := winenv.ConfigDir(getenv); d != "" {
		return filepath.Join(d, "keys.toml")
	}
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".config", "pyre", "keys.toml")
}

// keepClientSettings carries settings that are not part of the credential
// (web_url) over from the config being replaced (pair/login --force).
func keepClientSettings(path string, cfg *ClientConfig) {
	if old, err := LoadConfig(path); err == nil && cfg.WebURL == "" {
		cfg.WebURL = old.WebURL
	}
}
