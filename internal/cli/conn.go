package cli

import cc "github.com/jhh3/pyre/internal/clientconn"

// Connection resolution lives in internal/clientconn (a leaf package, so
// pyred's helper commands do not link the CLI and TUI); these aliases keep
// the CLI's API unchanged.

const (
	EnvHub          = cc.EnvHub
	EnvHubPin       = cc.EnvHubPin
	EnvSocket       = cc.EnvSocket
	EnvSessionToken = cc.EnvSessionToken
	EnvToken        = cc.EnvToken
	EnvConfig       = cc.EnvConfig
	EnvStateDir     = cc.EnvStateDir
	EnvSessionID    = cc.EnvSessionID
	EnvRunID        = cc.EnvRunID
	EnvNodeID       = cc.EnvNodeID
	ConfigVersion   = cc.ConfigVersion
)

type (
	ClientConfig = cc.ClientConfig
	ConnOptions  = cc.ConnOptions
	Connection   = cc.Connection
)

var (
	ErrNoConfig          = cc.ErrNoConfig
	DefaultConfigPath    = cc.DefaultConfigPath
	DefaultStateDir      = cc.DefaultStateDir
	LoadConfig           = cc.LoadConfig
	SaveConfig           = cc.SaveConfig
	Resolve              = cc.Resolve
	UnixSocketHTTPClient = cc.UnixSocketHTTPClient
)
