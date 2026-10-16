// Package cloud provisions on-demand cloud machines as Pyre nodes
// (W-gcp). Everything runs client-side: the CLI machine drives the owner's
// `gcloud` (exec, JSON output, behind Runner so tests use a fake), mints a
// one-time hub enrollment ticket and a single-use Tailscale auth key, and
// hands both to the VM's first-boot script through instance metadata. The
// VM joins the tailnet, verifies and installs the signed Pyre release,
// enrolls, starts pyred as a systemd user service, and powers itself off
// when idle. No hub or node protocol changes are involved.
package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// ConfigFileName is the cloud configuration beside the client config
// (~/.config/pyre/cloud.json, or $PYRE_CLOUD_CONFIG).
const (
	ConfigFileName = "cloud.json"
	EnvConfig      = "PYRE_CLOUD_CONFIG"
	// Secret sources (never flags: argv is visible in ps).
	EnvTailscaleAuthKey      = "PYRE_TAILSCALE_AUTHKEY"
	EnvTailscaleClientID     = "TAILSCALE_OAUTH_CLIENT_ID"
	EnvTailscaleClientSecret = "TAILSCALE_OAUTH_CLIENT_SECRET"
)

// Defaults. None of them names an account, project or zone: those come
// from the config file, flags, or the owner's gcloud configuration.
const (
	DefaultMachineType  = "e2-standard-8"
	DefaultDiskGB       = 200
	DefaultDiskType     = "pd-balanced"
	DefaultBaseImage    = "ubuntu-2404-lts-amd64"
	DefaultBaseImageARM = "ubuntu-2404-lts-arm64"
	DefaultImageProject = "ubuntu-os-cloud"
	DefaultBakedFamily  = "pyre-node"
	DefaultNetwork      = "default"
	DefaultUser         = "pyre"
	DefaultIdleMinutes  = 30
	DefaultWaitMinutes  = 15
	DefaultTicketTTLMin = 30
	DefaultTailscaleTag = "tag:pyre-node"
)

// Config is the cloud configuration file. Every field is optional; flags
// override it.
type Config struct {
	// HubURL is the hub as cloud nodes reach it: its tailnet address, e.g.
	// https://server.<tailnet>.ts.net:7443 or https://100.x.y.z:7443. The TLS
	// pin is the client's (pins ignore the hostname).
	HubURL    string          `json:"hub_url,omitempty"`
	GCP       GCPConfig       `json:"gcp"`
	Release   ReleaseConfig   `json:"release"`
	Tailscale TailscaleConfig `json:"tailscale"`
	// Prices overrides the built-in $/hour estimates, keyed by machine
	// type ("e2-standard-8"), "gpu:<accelerator>" or "disk:<type>" ($/GB-month).
	Prices map[string]float64 `json:"prices,omitempty"`
}

type GCPConfig struct {
	Project     string `json:"project,omitempty"` // default: gcloud config get-value project
	Zone        string `json:"zone,omitempty"`    // default: gcloud config get-value compute/zone
	MachineType string `json:"machine_type,omitempty"`
	DiskGB      int    `json:"disk_gb,omitempty"`
	DiskType    string `json:"disk_type,omitempty"`
	// Image is the base image family for nodes when no baked image exists
	// (ImageProject defaults to ubuntu-os-cloud).
	Image        string `json:"image,omitempty"`
	ImageProject string `json:"image_project,omitempty"`
	// BakedFamily is the image family `pyre node image bake` writes in the
	// project; create prefers it when it exists ("none" disables).
	BakedFamily string `json:"baked_family,omitempty"`
	Network     string `json:"network,omitempty"`
	// ServiceAccount for the VM. It is attached (with only the
	// devstorage.read_only scope) only when the release comes from gs://;
	// otherwise the VM has no service account at all.
	ServiceAccount string `json:"service_account,omitempty"`
	Spot           bool   `json:"spot,omitempty"`
	User           string `json:"user,omitempty"`
	// IdleStopMinutes: the node powers itself off after this long with no
	// live runs (0 disables). Default 30.
	IdleStopMinutes *int     `json:"idle_stop_minutes,omitempty"`
	PyredArgs       []string `json:"pyred_args,omitempty"`
	// Harnesses installed by the boot script when absent: claude, codex, gh.
	Harnesses []string `json:"harnesses,omitempty"`
	// Toolchains the boot script installs (ids, id@version or presets; see
	// ResolveToolchains). Empty: the "default" preset. --toolchain adds to it.
	Toolchains []string `json:"toolchains,omitempty"`
	// WindowsInstallers run silently on Windows nodes (and in Windows bakes)
	// from the project's private bucket; setting any gives the VM the
	// read-only storage scope.
	WindowsInstallers []WindowsInstaller `json:"windows_installers,omitempty"`
	// WindowsGPUDriver overrides the GRID (vWS) driver a Windows GPU node
	// installs: https://… or gs://nvidia-drivers-us-public/GRID/…/*.exe
	// (default: the newest Windows Server GRID driver in that bucket).
	WindowsGPUDriver string `json:"windows_gpu_driver,omitempty"`
	WaitMinutes      int    `json:"wait_minutes,omitempty"`
	// Firewall: manage the pyre-deny-ingress rule (default true).
	Firewall *bool `json:"firewall,omitempty"`
}

type ReleaseConfig struct {
	// URL is the directory holding pyre-<v>-linux-<arch>.tar.gz, SHA256SUMS
	// and SHA256SUMS.sig: gs://bucket/prefix/ (read with the VM's identity)
	// or https://.
	URL     string `json:"url,omitempty"`
	Version string `json:"version,omitempty"`
	// AllowedSigners is the release key file (default: the pinned
	// ~/.config/pyre/release-allowed-signers).
	AllowedSigners string `json:"allowed_signers,omitempty"`
	AllowUnsigned  bool   `json:"allow_unsigned,omitempty"`
	WithTools      bool   `json:"with_tools,omitempty"`
}

type TailscaleConfig struct {
	Tag string `json:"tag,omitempty"` // default tag:pyre-node
	// OAuthClientID / OAuthClientSecretFile mint a single-use, ephemeral,
	// pre-authorized, tagged key per create/start (env
	// TAILSCALE_OAUTH_CLIENT_ID / TAILSCALE_OAUTH_CLIENT_SECRET also work).
	OAuthClientID         string `json:"oauth_client_id,omitempty"`
	OAuthClientSecretFile string `json:"oauth_client_secret_file,omitempty"`
	// AuthKeyFile holds a pre-made auth key (single use: one create).
	AuthKeyFile string `json:"auth_key_file,omitempty"`
	// SSH enables Tailscale SSH on the node (default true) so remote attach
	// works without an open port.
	SSH *bool `json:"ssh,omitempty"`
	// KeyTTLSeconds is the minted key's lifetime (default 900).
	KeyTTLSeconds int `json:"key_ttl_seconds,omitempty"`
}

// ConfigPath returns $PYRE_CLOUD_CONFIG, else cloud.json beside the
// client config file.
func ConfigPath(getenv func(string) string, clientConfigPath string) string {
	if p := getenv(EnvConfig); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(clientConfigPath), ConfigFileName)
}

// LoadConfig reads path; a missing file is an empty config.
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, contracts.Errorf(contracts.ErrInvalid, "cloud config %s: %v", path, err)
	}
	return c, nil
}

func (c GCPConfig) machineType() string { return or(c.MachineType, DefaultMachineType) }
func (c GCPConfig) diskType() string    { return or(c.DiskType, DefaultDiskType) }
func (c GCPConfig) network() string     { return or(c.Network, DefaultNetwork) }
func (c GCPConfig) user() string        { return or(c.User, DefaultUser) }
func (c GCPConfig) bakedFamily() string { return or(c.BakedFamily, DefaultBakedFamily) }
func (c GCPConfig) diskGB() int {
	if c.DiskGB > 0 {
		return c.DiskGB
	}
	return DefaultDiskGB
}
func (c GCPConfig) idleMinutes() int {
	if c.IdleStopMinutes != nil && *c.IdleStopMinutes >= 0 {
		return *c.IdleStopMinutes
	}
	return DefaultIdleMinutes
}
func (c GCPConfig) firewall() bool { return c.Firewall == nil || *c.Firewall }
func (c GCPConfig) harnesses() []string {
	if len(c.Harnesses) > 0 {
		return c.Harnesses
	}
	return []string{"claude", "codex", "gh"}
}
func (t TailscaleConfig) tag() string { return or(t.Tag, DefaultTailscaleTag) }
func (t TailscaleConfig) ssh() bool   { return t.SSH == nil || *t.SSH }

func or(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// readSecretFile reads a small secret file (trimmed). Group/world-readable
// files are refused so a secret is never left lying around readable.
func readSecretFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by other users (mode %o): chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
