package cloud

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
)

// StartupScript is the Linux first-boot script (GCE startup-script).
//
//go:embed startup.sh
var StartupScript string

// WindowsStartupScript is the Windows first-boot script
// (GCE windows-startup-script-ps1).
//
//go:embed startup.ps1
var WindowsStartupScript string

// OS of a cloud node.
const (
	OSLinux   = "linux"
	OSWindows = "windows"
)

// Windows metadata keys.
const (
	MetaWindowsStartup = "windows-startup-script-ps1"
	// MetaAutologonPassword hands a password set by `pyre node rdp
	// --reset-password` to the VM's auto-logon refresher (removed by the
	// CLI right after).
	MetaAutologonPassword = "pyre-autologon-password"
)

// BootConfig is the non-secret pyre-config metadata the boot script reads.
// It never holds a secret: those travel as separate metadata keys.
type BootConfig struct {
	Mode           string   `json:"mode"` // node | bake
	OS             string   `json:"os"`
	Name           string   `json:"name,omitempty"`
	User           string   `json:"user"`
	HubURL         string   `json:"hub_url,omitempty"`
	HubPin         string   `json:"hub_pin,omitempty"`
	NodeID         string   `json:"node_id,omitempty"`
	ReleaseURL     string   `json:"release_url"`
	ReleaseVersion string   `json:"release_version"`
	AllowedSigners string   `json:"allowed_signers,omitempty"` // public release key (allowed-signers format)
	AllowUnsigned  bool     `json:"allow_unsigned,omitempty"`
	WithTools      bool     `json:"with_tools,omitempty"`
	TSHostname     string   `json:"ts_hostname,omitempty"`
	TSSSH          bool     `json:"ts_ssh,omitempty"`
	SSHAlias       string   `json:"ssh_alias,omitempty"`
	Harnesses      []string `json:"harnesses,omitempty"`
	// Toolchains: resolved ids in install order (ResolveToolchains).
	Toolchains []string `json:"toolchains,omitempty"`
	// Windows: GPU ("nvidia-l4:1") installs the GRID driver from GPUDriver
	// (empty: newest in Google's public bucket) and turns on hardware
	// graphics for Remote Desktop sessions.
	GPU        string             `json:"gpu,omitempty"`
	GPUDriver  string             `json:"gpu_driver,omitempty"`
	Installers []WindowsInstaller `json:"installers,omitempty"`
	PyredArgs  []string           `json:"pyred_args,omitempty"`
	RDP        bool               `json:"rdp,omitempty"` // Windows: Remote Desktop, tailnet addresses only
}

// writePrivate writes data to dir/name with mode 0600.
func writePrivate(dir, name string, data []byte) (string, error) {
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, data, 0o600)
}

// bootFiles writes the startup script, the config and the given secrets
// into dir (0700, files 0600) and returns the --metadata-from-file map.
func bootFiles(dir string, bc BootConfig, secrets map[string]string) (map[string]string, error) {
	files := map[string]string{}
	key, script := MetaStartupScript, StartupScript
	if bc.OS == OSWindows {
		key, script = MetaWindowsStartup, WindowsStartupScript
	}
	p, err := writePrivate(dir, "startup", []byte(script))
	if err != nil {
		return nil, err
	}
	files[key] = p
	cb, _ := json.Marshal(bc)
	if p, err = writePrivate(dir, "config.json", cb); err != nil {
		return nil, err
	}
	files[MetaConfig] = p
	for k, v := range secrets {
		if v == "" {
			continue
		}
		if p, err = writePrivate(dir, k, []byte(v)); err != nil {
			return nil, err
		}
		files[k] = p
	}
	return files, nil
}
