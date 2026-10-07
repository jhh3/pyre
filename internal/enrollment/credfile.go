package enrollment

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/transport"
)

// CredentialFileVersion is stamped into credential files.
const CredentialFileVersion = 1

// NodeCredentialFile is the node's local credential (written by
// `pyred enroll`, read by the daemon). Mode 0600; never logged.
type NodeCredentialFile struct {
	Version       int       `json:"version"`
	HubURL        string    `json:"hub_url"`
	HubCertSHA256 string    `json:"hub_cert_sha256,omitempty"`
	NodeID        string    `json:"node_id"`
	Token         string    `json:"token"`
	EnrolledAt    time.Time `json:"enrolled_at"`
}

// ClientCredentialFile is an owner client's local credential (CLI/TUI).
type ClientCredentialFile struct {
	Version       int       `json:"version"`
	HubURL        string    `json:"hub_url"`
	HubCertSHA256 string    `json:"hub_cert_sha256,omitempty"`
	ClientID      string    `json:"client_id"`
	Token         string    `json:"token"`
	PairedAt      time.Time `json:"paired_at"`
}

// SaveNodeCredential atomically writes c to path with mode 0600, creating
// the parent directory with mode 0700 if needed.
func SaveNodeCredential(path string, c NodeCredentialFile) error {
	if c.NodeID == "" || !strings.HasPrefix(c.Token, transport.PrefixNodeToken) || c.HubURL == "" {
		return fmt.Errorf("enrollment: node credential requires node_id, hub_url and a node token")
	}
	if c.Version == 0 {
		c.Version = CredentialFileVersion
	}
	return saveSecretJSON(path, c)
}

// LoadNodeCredential reads a node credential file, refusing files that are
// group/world accessible, not owned by the current user, or symlinks.
func LoadNodeCredential(path string) (NodeCredentialFile, error) {
	var c NodeCredentialFile
	if err := loadSecretJSON(path, &c); err != nil {
		return c, err
	}
	if c.NodeID == "" || !strings.HasPrefix(c.Token, transport.PrefixNodeToken) || c.HubURL == "" {
		return c, fmt.Errorf("enrollment: %s is not a valid node credential file", path)
	}
	return c, nil
}

// SaveClientCredential atomically writes an owner client credential (0600).
func SaveClientCredential(path string, c ClientCredentialFile) error {
	if c.ClientID == "" || !strings.HasPrefix(c.Token, transport.PrefixOwnerToken) || c.HubURL == "" {
		return fmt.Errorf("enrollment: client credential requires client_id, hub_url and an owner token")
	}
	if c.Version == 0 {
		c.Version = CredentialFileVersion
	}
	return saveSecretJSON(path, c)
}

// LoadClientCredential reads an owner client credential file with the same
// permission checks as LoadNodeCredential.
func LoadClientCredential(path string) (ClientCredentialFile, error) {
	var c ClientCredentialFile
	if err := loadSecretJSON(path, &c); err != nil {
		return c, err
	}
	if c.ClientID == "" || !strings.HasPrefix(c.Token, transport.PrefixOwnerToken) || c.HubURL == "" {
		return c, fmt.Errorf("enrollment: %s is not a valid client credential file", path)
	}
	return c, nil
}

func saveSecretJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("enrollment: create credential dir: %w", err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return transport.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

func loadSecretJSON(path string, v any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("enrollment: open credential file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("enrollment: credential file %s is not a regular file", path)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("enrollment: refusing to read credential file %s with mode %04o (readable by others); run `chmod 600 %s` and consider revoking and re-enrolling", path, perm, path)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("enrollment: credential file %s is owned by uid %d, not the current user", path, sys.Uid)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("enrollment: credential file %s is malformed", path)
	}
	return nil
}
