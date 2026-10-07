package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Provision records that a credential for a connection has been provisioned
// into a node-local store: the environment variable NAME a session receives
// and the Ref the launch wrapper reads. It never holds the value.
type Provision struct {
	ConnectionID  string    `json:"connection_id"`
	EnvVar        string    `json:"env_var"`
	Ref           string    `json:"ref"` // Ref.String()
	SourceNode    string    `json:"source_node,omitempty"`
	ProvisionedAt time.Time `json:"provisioned_at"`
}

// ParsedRef parses p.Ref.
func (p Provision) ParsedRef() (Ref, error) { return ParseRef(p.Ref) }

// Index is the node-local provisioning index (JSON file, mode 0600). Index
// content is metadata and safe to report; it is the node's answer to "which
// connections can I provision locally, under which variable names".
type Index struct {
	path string
	mu   sync.Mutex
	mem  []Provision // used when path == ""
}

// OpenIndex returns an index stored at path (created on first write). An
// empty path keeps the index in memory only (tests).
func OpenIndex(path string) *Index { return &Index{path: path} }

type indexFile struct {
	Version    int         `json:"version"`
	Provisions []Provision `json:"provisions"`
}

func (ix *Index) load() ([]Provision, error) {
	if ix.path == "" {
		return append([]Provision(nil), ix.mem...), nil
	}
	raw, err := os.ReadFile(ix.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credentials: read provisioning index: %w", err)
	}
	var f indexFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("credentials: provisioning index is corrupt: %w", err)
	}
	return f.Provisions, nil
}

func (ix *Index) save(ps []Provision) error {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].ConnectionID != ps[j].ConnectionID {
			return ps[i].ConnectionID < ps[j].ConnectionID
		}
		return ps[i].EnvVar < ps[j].EnvVar
	})
	if ix.path == "" {
		ix.mem = append([]Provision(nil), ps...)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(ix.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(indexFile{Version: 1, Provisions: ps}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(ix.path), ".provisions-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), ix.path)
}

// List returns all provisions.
func (ix *Index) List() ([]Provision, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.load()
}

// ForConnection returns the provisions for one connection.
func (ix *Index) ForConnection(connectionID string) ([]Provision, error) {
	all, err := ix.List()
	if err != nil {
		return nil, err
	}
	var out []Provision
	for _, p := range all {
		if p.ConnectionID == connectionID {
			out = append(out, p)
		}
	}
	return out, nil
}

// Upsert adds or replaces the (connection, env var) entry.
func (ix *Index) Upsert(p Provision) error {
	if _, err := p.ParsedRef(); err != nil {
		return err
	}
	if err := ValidateEnvName(p.EnvVar); err != nil {
		return err
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ps, err := ix.load()
	if err != nil {
		return err
	}
	out := ps[:0]
	for _, q := range ps {
		if q.ConnectionID != p.ConnectionID || q.EnvVar != p.EnvVar {
			out = append(out, q)
		}
	}
	return ix.save(append(out, p))
}

// Remove deletes entries for a connection (all env vars when envVar == "")
// and returns the removed provisions so callers can delete store values.
func (ix *Index) Remove(connectionID, envVar string) ([]Provision, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ps, err := ix.load()
	if err != nil {
		return nil, err
	}
	var keep, removed []Provision
	for _, q := range ps {
		if q.ConnectionID == connectionID && (envVar == "" || q.EnvVar == envVar) {
			removed = append(removed, q)
		} else {
			keep = append(keep, q)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	return removed, ix.save(keep)
}
