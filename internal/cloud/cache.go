package cloud

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CacheFileName is the local snapshot of cloud nodes beside the client
// config, written by the cloud commands and read by the TUI's Machines
// view (which never runs gcloud itself). It holds no secret.
const CacheFileName = "cloud-nodes.json"

// CacheEntry is one node's last known cloud facts.
type CacheEntry struct {
	Cloud       string    `json:"cloud"`
	Project     string    `json:"project,omitempty"`
	OS          string    `json:"os,omitempty"`
	Zone        string    `json:"zone,omitempty"`
	MachineType string    `json:"machine_type,omitempty"`
	Status      string    `json:"status,omitempty"`
	Spot        bool      `json:"spot,omitempty"`
	GPU         string    `json:"gpu,omitempty"`
	RunningUSD  float64   `json:"est_running_usd"`
	HourlyUSD   float64   `json:"est_hourly_usd"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Cache is the snapshot file.
type Cache struct {
	Path string
	mu   sync.Mutex
}

func (c *Cache) load() map[string]CacheEntry {
	m := map[string]CacheEntry{}
	b, err := os.ReadFile(c.Path)
	if err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (c *Cache) save(m map[string]CacheEntry) {
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.MkdirAll(filepath.Dir(c.Path), 0o700)
	tmp := c.Path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, c.Path)
	}
}

func entryOf(cn CloudNode) CacheEntry {
	e := CacheEntry{Cloud: cn.Cloud, Project: cn.Project, OS: cn.OS, RunningUSD: cn.RunningUSD, HourlyUSD: cn.HourlyUSD, UpdatedAt: time.Now().UTC()}
	if in := cn.Instance; in != nil {
		e.Zone, e.MachineType, e.Status, e.Spot, e.GPU = in.Zone, in.MachineType, in.Status, in.Spot, in.GPU
	}
	return e
}

func (c *Cache) Put(cn CloudNode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.load()
	m[cn.Name] = entryOf(cn)
	c.save(m)
}

func (c *Cache) Delete(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.load()
	delete(m, name)
	c.save(m)
}

// Replace sets the snapshot to exactly cns (a full listing).
func (c *Cache) Replace(cns []CloudNode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := map[string]CacheEntry{}
	for _, cn := range cns {
		m[cn.Name] = entryOf(cn)
	}
	c.save(m)
}

func (c *Cache) Load() map[string]CacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.load()
}

// Describe is the one-line summary the TUI shows ("" when unknown).
func (e CacheEntry) Describe() string {
	parts := []string{e.Cloud}
	if e.OS == OSWindows {
		parts = append(parts, "windows")
	}
	if e.MachineType != "" {
		mt := e.MachineType
		if e.Spot {
			mt += " spot"
		}
		if e.GPU != "" {
			mt += " " + e.GPU
		}
		parts = append(parts, mt)
	}
	if e.Zone != "" {
		parts = append(parts, e.Zone)
	}
	if e.Status != "" {
		parts = append(parts, strings.ToLower(e.Status))
	}
	if e.RunningUSD > 0 {
		parts = append(parts, fmt.Sprintf("~$%.2f/h running", e.RunningUSD))
	}
	return strings.Join(parts, " · ")
}

// CacheReader returns a lookup the TUI can call on every render: it
// re-reads the file at most every 30 s.
func CacheReader(path string) func(node string) string {
	var mu sync.Mutex
	var at time.Time
	var m map[string]CacheEntry
	c := &Cache{Path: path}
	return func(node string) string {
		mu.Lock()
		defer mu.Unlock()
		if m == nil || time.Since(at) > 30*time.Second {
			m, at = c.Load(), time.Now()
		}
		e, ok := m[node]
		if !ok {
			return ""
		}
		return e.Describe()
	}
}
