// Package clientconn resolves how a Pyre client reaches the API (hub URL,
// local daemon socket or client config) and with which credential. It is a
// leaf package shared by the CLI and by pyred's small helper commands (the
// doorbell monitor) so those do not link the whole CLI and TUI.
package clientconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/transport"
	"github.com/jhh3/pyre/internal/winenv"
)

// Environment variables understood by Pyre clients.
const (
	EnvHub          = "PYRE_HUB"           // hub base URL (overrides socket and config)
	EnvHubPin       = "PYRE_HUB_PIN"       // TLS SHA-256 pin for EnvHub
	EnvSocket       = "PYRE_SOCKET"        // local node daemon unix socket
	EnvSessionToken = "PYRE_SESSION_TOKEN" // agent-session credential (highest precedence)
	EnvToken        = "PYRE_TOKEN"         // explicit credential
	EnvConfig       = "PYRE_CONFIG"        // client config file path
	EnvStateDir     = "PYRE_STATE_DIR"     // drafts and artifact cache
	EnvSessionID    = "PYRE_SESSION_ID"    // set inside agent sessions
	EnvHarness      = "PYRE_HARNESS"       // the session's harness (claude, codex, agy, shell), set by pyred
	EnvRunID        = "PYRE_RUN_ID"
	EnvNodeID       = "PYRE_NODE_ID"
	EnvWebURL       = "PYRE_WEB_URL" // the web app the TUI links to (overrides web_url)
	// EnvSocketWait overrides how long a call through PYRE_SOCKET waits for
	// a restarting pyred (a Go duration; "0" = fail at once; default
	// client.DefaultDaemonRestartWait).
	EnvSocketWait = "PYRE_SOCKET_WAIT"
)

// ConfigVersion is stamped into client.json.
const ConfigVersion = 1

// ClientConfig is ~/.config/pyre/client.json. It holds an owner credential,
// so it must be mode 0600 and owned by the current user; looser files are
// refused. Its JSON shape matches enrollment.ClientCredentialFile.
type ClientConfig struct {
	Version       int       `json:"version"`
	HubURL        string    `json:"hub_url"`
	HubCertSHA256 string    `json:"hub_cert_sha256,omitempty"`
	ClientID      string    `json:"client_id,omitempty"`
	Token         string    `json:"token"`
	PairedAt      time.Time `json:"paired_at,omitempty"`
	// WebURL is the web app (`pyre web serve`, the inbox) the TUI links
	// to; empty: the TUI's default. PYRE_WEB_URL overrides it.
	WebURL string `json:"web_url,omitempty"`
}

// DefaultConfigPath returns $PYRE_CONFIG, else $XDG_CONFIG_HOME/pyre/client.json,
// else ~/.config/pyre/client.json.
func DefaultConfigPath(getenv func(string) string) string {
	if p := getenv(EnvConfig); p != "" {
		return p
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "pyre", "client.json")
	}
	if d := winenv.ConfigDir(getenv); d != "" {
		return filepath.Join(d, "client.json") // %APPDATA%\Pyre
	}
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".config", "pyre", "client.json")
}

// DefaultStateDir returns $PYRE_STATE_DIR, else $XDG_STATE_HOME/pyre, else
// ~/.local/state/pyre.
func DefaultStateDir(getenv func(string) string) string {
	if p := getenv(EnvStateDir); p != "" {
		return p
	}
	if x := getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "pyre")
	}
	if d := winenv.StateDir(getenv); d != "" {
		return d // %LOCALAPPDATA%\Pyre
	}
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".local", "state", "pyre")
}

// ErrNoConfig reports a missing client config.
var ErrNoConfig = errors.New("no client config")

// LoadConfig reads a client config, refusing symlinks, non-regular files,
// files readable by group/others, and files owned by another user.
func LoadConfig(path string) (ClientConfig, error) {
	var cfg ClientConfig
	f, err := fileperm.OpenNoFollow(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, fmt.Errorf("%w at %s", ErrNoConfig, path)
		}
		return cfg, contracts.Errorf(contracts.ErrInvalid, "open client config %s: %v (symlinks are refused)", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return cfg, err
	}
	if !st.Mode().IsRegular() {
		return cfg, contracts.Errorf(contracts.ErrInvalid, "client config %s is not a regular file", path)
	}
	if desc, bad := fileperm.Exposed(path, st); bad {
		return cfg, contracts.Errorf(contracts.ErrInvalid, "refusing client config %s with %s: it holds a credential; %s", path, desc, fileperm.FixHint(path, false))
	}
	if owner, bad := fileperm.ForeignOwner(path, st); bad {
		return cfg, contracts.Errorf(contracts.ErrInvalid, "client config %s is owned by %s, not the current user", path, owner)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, contracts.Errorf(contracts.ErrInvalid, "client config %s is malformed", path)
	}
	if cfg.HubURL == "" {
		return cfg, contracts.Errorf(contracts.ErrInvalid, "client config %s has no hub_url", path)
	}
	return cfg, nil
}

// SaveConfig atomically writes cfg with mode 0600 (parent dir 0700).
func SaveConfig(path string, cfg ClientConfig) error {
	if cfg.Version == 0 {
		cfg.Version = ConfigVersion
	}
	if err := fileperm.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return transport.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// ConnOptions are the inputs to connection resolution.
type ConnOptions struct {
	HubFlag    string // --hub
	PinFlag    string // --pin
	ConfigPath string // --config (default DefaultConfigPath)
	Getenv     func(string) string
	Timeout    time.Duration
}

// Connection is a resolved API endpoint.
type Connection struct {
	Client      *client.Client
	Mode        string // "hub" | "socket"
	HubURL      string // hub mode
	Socket      string // socket mode
	Source      string // where the endpoint came from: "--hub", "PYRE_HUB", "PYRE_SOCKET", "config <path>", SourceLocalPyred
	TokenSource string // "PYRE_SESSION_TOKEN", "PYRE_TOKEN", "config", "none"
	// Pin is the hub TLS pin in use (--pin, PYRE_HUB_PIN, or the config's
	// for the same hub); a public fingerprint, not a secret.
	Pin string
}

// Describe is a one-line human description (never includes the token).
func (c *Connection) Describe() string {
	if c.Mode == "socket" {
		return fmt.Sprintf("local daemon socket %s (from %s; credential: %s)", c.Socket, c.Source, c.TokenSource)
	}
	return fmt.Sprintf("hub %s (from %s; credential: %s)", c.HubURL, c.Source, c.TokenSource)
}

// Resolve picks the endpoint and credential:
//
//	endpoint:   --hub / PYRE_HUB  >  PYRE_SOCKET (local daemon)  >  client config
//	            >  this machine's pyred socket (DefaultDaemonSocket, CheckOwnSocket)
//	credential: PYRE_SESSION_TOKEN  >  PYRE_TOKEN  >  client config token
//
// The config token and TLS pin are only sent to the hub URL they were
// configured for (or to the local daemon socket), never to an arbitrary
// --hub URL.
func Resolve(o ConnOptions) (*Connection, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	cfgPath := o.ConfigPath
	if cfgPath == "" {
		cfgPath = DefaultConfigPath(getenv)
	}
	conn := &Connection{}
	hub, src := strings.TrimSpace(o.HubFlag), "--hub"
	if hub == "" {
		hub, src = strings.TrimSpace(getenv(EnvHub)), EnvHub
	}
	pin := o.PinFlag
	if pin == "" {
		pin = getenv(EnvHubPin)
	}
	sock := getenv(EnvSocket)

	var cfg ClientConfig
	var cfgErr error
	cfgLoaded := false
	loadCfg := func() {
		if !cfgLoaded {
			cfg, cfgErr = LoadConfig(cfgPath)
			cfgLoaded = true
		}
	}
	switch {
	case hub != "":
		conn.Mode, conn.HubURL, conn.Source = "hub", hub, src
	case sock != "":
		conn.Mode, conn.Socket, conn.Source = "socket", sock, EnvSocket
	default:
		loadCfg()
		switch {
		case cfgErr == nil:
			conn.Mode, conn.HubURL, conn.Source = "hub", cfg.HubURL, "config "+cfgPath
		case errors.Is(cfgErr, ErrNoConfig):
			// No hub and no config (a plain ssh shell on a node): use this
			// machine's pyred socket, as a session there does.
			local := DefaultDaemonSocket(getenv)
			why := CheckOwnSocket(local)
			if why == nil {
				why = actsForOwner(local)
			}
			if why == nil {
				conn.Mode, conn.Socket, conn.Source = "socket", local, SourceLocalPyred
				break
			}
			msg := fmt.Sprintf("no hub configured (%v): run `pyre pair` (paste the link) or `pyre login --hub URL --token -`, or set PYRE_HUB", cfgErr)
			if !errors.Is(why, fs.ErrNotExist) {
				msg += fmt.Sprintf("; this machine's pyred socket %s was not used: %v", local, why)
			}
			return nil, contracts.Errorf(contracts.ErrUnavailable, "%s", msg)
		default:
			return nil, cfgErr // a config that is there but unusable is not skipped
		}
	}
	// A config is relevant when it describes the same hub, or for the local
	// daemon socket (same user, local trust).
	cfgApplies := func() bool {
		loadCfg()
		if cfgErr != nil {
			return false
		}
		return conn.Mode == "socket" || SameURL(cfg.HubURL, conn.HubURL)
	}

	token := ""
	switch {
	case getenv(EnvSessionToken) != "":
		token, conn.TokenSource = getenv(EnvSessionToken), EnvSessionToken
	case getenv(EnvToken) != "":
		token, conn.TokenSource = getenv(EnvToken), EnvToken
	case cfgApplies() && cfg.Token != "":
		token, conn.TokenSource = cfg.Token, "config"
	default:
		conn.TokenSource = "none"
	}
	if pin == "" && conn.Mode == "hub" && cfgApplies() {
		pin = cfg.HubCertSHA256
	}

	copts := client.Options{UserAgent: "pyre-cli/" + buildinfo.Version, Timeout: o.Timeout}
	base := conn.HubURL
	if conn.Mode == "socket" {
		base = "http://pyred.local"
		copts.HTTPClient = UnixSocketHTTPClient(conn.Socket)
		copts.DaemonRestartWait = client.DefaultDaemonRestartWait // ride out a pyred restart
		if v := strings.TrimSpace(getenv(EnvSocketWait)); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return nil, contracts.Errorf(contracts.ErrInvalid, "%s=%q: want a duration such as 30s, or 0", EnvSocketWait, v)
			}
			copts.DaemonRestartWait = d
		}
	} else {
		copts.CertSHA256 = pin
		conn.Pin = pin
	}
	conn.Client = client.New(base, token, copts)
	if err := conn.Client.Err(); err != nil {
		return nil, err
	}
	return conn, nil
}

// SourceLocalPyred is Connection.Source when no hub, PYRE_SOCKET or client
// config is set and the CLI uses this machine's pyred socket instead.
const SourceLocalPyred = "this machine's pyred"

// socketForeignOwner is fileperm.ForeignOwner (a test seam: a test cannot
// create a socket another user owns without root).
var socketForeignOwner = fileperm.ForeignOwner

// DefaultDaemonSocket returns where this user's pyred, run with default
// settings, serves its local socket: <state>/pyred.sock, the state directory
// chosen as daemon.DefaultStateDir does (PYRE_STATE_DIR; macOS
// ~/Library/Application Support/pyre; Linux $XDG_STATE_HOME/pyre or
// ~/.local/state/pyre; Windows %LOCALAPPDATA%\Pyre). It reads only getenv
// (no HOME: ""), so an isolated environment never finds the user's pyred.
func DefaultDaemonSocket(getenv func(string) string) string {
	if d := getenv(EnvStateDir); d != "" {
		return filepath.Join(d, "pyred.sock")
	}
	if d := winenv.StateDir(getenv); d != "" {
		return filepath.Join(d, "pyred.sock")
	}
	home := getenv("HOME")
	if home == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "pyre", "pyred.sock")
	}
	if x := getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "pyre", "pyred.sock")
	}
	return filepath.Join(home, ".local", "state", "pyre", "pyred.sock")
}

// CheckOwnSocket reports why path is not a usable pyred socket of this
// user: missing (fs.ErrNotExist), a symlink or not a socket, owned by
// another user, open to group/others (pyred makes it 0600 in its 0700
// state directory), or nothing listening. Only a process running as this
// user can bind a socket this user owns, so using it gives no access beyond
// what any process of the user already has through it.
func CheckOwnSocket(path string) error {
	if path == "" {
		return fs.ErrNotExist
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSocket == 0 {
		return errors.New("not a unix socket (symlinks are refused)")
	}
	if owner, bad := socketForeignOwner(path, st); bad {
		return fmt.Errorf("owned by %s, not the current user", owner)
	}
	if desc, bad := fileperm.Exposed(path, st); bad {
		return fmt.Errorf("refusing a socket with %s: pyred makes it user-only (0600)", desc)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return errors.New("nothing is listening on it (is pyred running? see `pyred status`)")
	}
	c.Close()
	return nil
}

// localStatusPath mirrors daemon.PathLocalStatus (this package does not
// link the daemon).
const localStatusPath = "/v1/local/status"

// actsForOwner asks the pyred at sock whether a call without a bearer acts
// as the owner on the hub (it holds an owner credential). Without one pyred
// forwards such calls to the hub with no credential and the hub refuses
// them, so falling back to it would only turn "no hub configured" into
// "missing bearer token" (and `pyre hello` into pyred's own answer while
// the hub is down).
func actsForOwner(sock string) error {
	hc := UnixSocketHTTPClient(sock)
	hc.Timeout = 3 * time.Second
	resp, err := hc.Get("http://pyred.local" + localStatusPath)
	if err != nil {
		return fmt.Errorf("its status did not answer: %v", err)
	}
	defer resp.Body.Close()
	var st struct {
		OwnerCredential *bool `json:"owner_credential"`
		Versions        struct {
			Pyred string `json:"pyred"`
		} `json:"versions"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st) != nil {
		return fmt.Errorf("its status did not answer (HTTP %d)", resp.StatusCode)
	}
	switch {
	case st.OwnerCredential == nil:
		// An older pyred's status does not say (owner_credential is new).
		return fmt.Errorf("pyred %s is too old to say whether it holds an owner credential", st.Versions.Pyred)
	case !*st.OwnerCredential:
		return errors.New("it holds no owner credential (pyred --owner-credential), so it cannot act for you on the hub " +
			"(a Pyre session on this machine can: its PYRE_SESSION_TOKEN carries the session's identity)")
	}
	return nil
}

// UnixSocketHTTPClient returns an HTTP client whose every connection dials
// the unix socket at path (the request host is ignored). A failed dial is a
// *client.SocketDialError, so errors name the local daemon socket rather
// than the hub.
func UnixSocketHTTPClient(path string) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			c, err := d.DialContext(ctx, "unix", path)
			if err != nil {
				return nil, &client.SocketDialError{Path: path, Err: err}
			}
			return c, nil
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &http.Client{Transport: tr}
}

// SameURL reports whether two hub URLs name the same hub.
func SameURL(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}
