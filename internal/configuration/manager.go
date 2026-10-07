package configuration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

// RunRef describes a live run as recorded by the node daemon. Configuration
// uses it to compute restart-required sessions; updates uses it to report
// InUseByRuns and to protect pinned executables from pruning.
type RunRef struct {
	RunID        string            `json:"run_id"`
	SessionID    string            `json:"session_id"`
	ProjectID    string            `json:"project_id,omitempty"`
	Harness      contracts.Harness `json:"harness"`
	GenerationID string            `json:"generation_id,omitempty"` // ResolvedLaunch.EnvGeneration
	Executable   string            `json:"executable,omitempty"`    // ResolvedLaunch.Executable (pinned path)
}

// RunRegistry lists runs the daemon currently supervises.
type RunRegistry interface {
	ActiveRuns(ctx context.Context) ([]RunRef, error)
}

// GrantScope is the scope a connection grant is checked against.
type GrantScope struct {
	NodeID    string
	ProjectID string
	SessionID string
}

// GrantSource is the WP12 (Outfit connections) integration point: it returns
// the subset of requested connection IDs granted for the scope. Never secrets.
type GrantSource interface {
	GrantedConnections(ctx context.Context, scope GrantScope, requested []string) ([]string, error)
}

// Pin is a tool executable/directory pinned for new runs.
type Pin struct {
	Tool    string           `json:"tool"`
	Version string           `json:"version"`
	Path    string           `json:"path"` // resolved real path inside the version directory
	Digest  contracts.Digest `json:"digest,omitempty"`
}

// VersionSource is implemented by the updates manager.
type VersionSource interface {
	ActiveVersions(ctx context.Context) map[string]string
	Pinned(ctx context.Context, tool string) (Pin, bool)
}

// Reporter sends a durable node report (the daemon's outbox).
type Reporter interface {
	Report(ctx context.Context, kind contracts.ReportKind, payload any) error
}

// Options configures a Manager.
type Options struct {
	StateDir string // Pyre node state dir (e.g. ~/.local/state/pyre); env data lives in <StateDir>/env
	NodeID   string
	// Homes are the default harness config homes. Zero value = DefaultHomes($HOME).
	Homes extensions.Homes
	// ProjectDirs maps a project ID to its local roots on this node (WP11).
	ProjectDirs func(ctx context.Context, projectID string) []string
	Runs        RunRegistry
	Grants      GrantSource
	Versions    VersionSource
	Managed     extensions.ManagedSource
	Reporter    Reporter
	Clock       contracts.Clock
	// ProviderDefaults overrides the built-in provider_default layer.
	ProviderDefaults map[string]any
	// HotReloadKeys are keys a running harness picks up without restart.
	HotReloadKeys map[string]bool
	// ClaudeSettingsUnsupported / CodexOverlayUnsupported, when set, give
	// the reason this node's launcher cannot apply a Claude --settings
	// overlay (managed claude.* values or hooks) or Codex -c overlays. A
	// plan that needs one then carries a blocking conflict, so the change is
	// refused at review time instead of every later launch failing.
	ClaudeSettingsUnsupported string
	CodexOverlayUnsupported   string
}

// builtinProviderDefaults are documented harness defaults shown for
// provenance only; they are never written anywhere.
var builtinProviderDefaults = map[string]any{
	"claude.permissions.defaultMode": "default",
	"codex.approval_policy":          "on-request",
	"codex.model_reasoning_effort":   "medium",
}

// adoptSkip lists settings subtrees that are not adopted as plain values:
// they are inventoried as extensions (hooks, MCP, plugins) or are harness
// internal state that Pyre must neither report as configuration nor touch.
var adoptSkip = map[contracts.Harness][]string{
	contracts.HarnessClaude: {"hooks", "enabledPlugins", "extraKnownMarketplaces", "feedbackSurveyState", "mcpServers"},
	contracts.HarnessCodex:  {"hooks", "mcp_servers", "plugins", "marketplaces", "projects", "tui", "notice", "desktop"},
}

// Manager is the single configuration merge/materialization implementation.
type Manager struct {
	opt      Options
	profiles ProfileStore // active profiles: what launches resolve
	desired  ProfileStore // pending desired profiles from env.profile.set
	mu       sync.Mutex

	lastDrift contracts.Digest
	// failBefore is a test hook for partial-failure injection.
	failBefore func(op fileOp) error
}

// New creates a Manager.
func New(opt Options) (*Manager, error) {
	if opt.StateDir == "" {
		return nil, fmt.Errorf("configuration: StateDir required")
	}
	if opt.Homes.Home == "" && opt.Homes.ClaudeHome == "" && opt.Homes.CodexHome == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		opt.Homes = extensions.DefaultHomes(h)
	}
	if opt.Clock == nil {
		opt.Clock = contracts.SystemClock{}
	}
	if opt.ProviderDefaults == nil {
		opt.ProviderDefaults = builtinProviderDefaults
	}
	m := &Manager{opt: opt, profiles: ProfileStore{dir: filepath.Join(opt.StateDir, "env", "profiles")},
		desired: ProfileStore{dir: filepath.Join(opt.StateDir, "env", "desired")}}
	if err := os.MkdirAll(m.envDir(), 0o700); err != nil {
		return nil, err
	}
	return m, nil
}

// Profiles returns the profile store (desired managed layers).
func (m *Manager) Profiles() ProfileStore { return m.profiles }

// Homes returns the default config homes.
func (m *Manager) Homes() extensions.Homes { return m.opt.Homes }

func (m *Manager) envDir() string          { return filepath.Join(m.opt.StateDir, "env") }
func (m *Manager) genDir(id string) string { return filepath.Join(m.envDir(), "generations", id) }
func (m *Manager) planPath(id string) string {
	return filepath.Join(m.envDir(), "plans", id+".json")
}
func (m *Manager) backupDir(planID string) string {
	return filepath.Join(m.envDir(), "backups", planID)
}
func (m *Manager) display(p string) string { return m.opt.Homes.Display(p) }

// --- generation records ----------------------------------------------------

// genRecord is the node-local record of a generation: the contract record
// plus everything needed to verify, re-materialize or roll back to it.
type genRecord struct {
	Generation contracts.EnvironmentGeneration `json:"generation"`
	Scope      string                          `json:"scope"`
	Source     string                          `json:"source"` // "apply" | "launch" | "rollback"
	Applied    bool                            `json:"applied"`
	AppliedAt  *time.Time                      `json:"applied_at,omitempty"`
	Overlay    overlayRecord                   `json:"overlay"`
	Home       []targetRecord                  `json:"home,omitempty"`
	Baseline   baseline                        `json:"baseline"`
	// Profile is the scope's own managed profile as reviewed in the plan
	// (global or project scope). Apply and rollback write it into the active
	// profile store, which is what new launches resolve; a pending desired
	// profile (env.profile.set) therefore reaches new runs only through a
	// reviewed apply, and a rollback brings new runs back to the earlier
	// profile. Records written before this field existed have none (apply and
	// rollback then leave the store alone).
	Profile *profileSnapshot `json:"profile_snapshot,omitempty"`
}

// profileSnapshot records a scope's managed profile; Present=false records
// an empty layer (no profile defined).
type profileSnapshot struct {
	Name    string   `json:"name"`
	Present bool     `json:"present"`
	Profile *Profile `json:"profile,omitempty"`
}

// overlayRecord lists the Pyre-owned per-generation files (relative to the
// generation dir) and the launch arguments that reference them.
type overlayRecord struct {
	Files            map[string]contracts.Digest `json:"files"`
	ClaudeSettings   string                      `json:"claude_settings,omitempty"`
	ClaudeMCP        string                      `json:"claude_mcp,omitempty"`
	ClaudePluginDirs []string                    `json:"claude_plugin_dirs,omitempty"` // absolute; generation bundle and pinned plugin dirs
	CodexArgs        []string                    `json:"codex_args,omitempty"`
	Env              map[string]string           `json:"env,omitempty"` // non-secret
}

// targetRecord is a managed config-home target as last materialized. Dir
// content is stored under <generation>/home/<Index>/ for rollback.
type targetRecord struct {
	Kind    string           `json:"kind"` // "dir" | "json_keys"
	Path    string           `json:"path"`
	Display string           `json:"display"`
	Name    string           `json:"name,omitempty"`
	Index   int              `json:"index"`
	Digest  contracts.Digest `json:"digest"`
	Keys    map[string]any   `json:"keys,omitempty"` // json_keys: in-file key -> managed value
}

// baseline records what existed (unmanaged) when the generation was made, so
// later additions show up as unmanaged_addition drift.
type baseline struct {
	Identities []string          `json:"identities"`
	Settings   map[string]string `json:"settings"` // adopted key -> redacted rendered value
}

func (m *Manager) loadRecord(id string) (*genRecord, error) {
	if !contracts.ValidID(id, contracts.PrefixEnvironment) {
		return nil, contracts.Errorf(contracts.ErrInvalid, "invalid generation id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(m.genDir(id), "record.json"))
	if os.IsNotExist(err) {
		return nil, contracts.Errorf(contracts.ErrNotFound, "environment generation %s not found (never created on this node, or pruned)", id).WithRef(contracts.KindEnvironment, id)
	}
	if err != nil {
		return nil, err
	}
	var r genRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("generation %s: %w", id, err)
	}
	return &r, nil
}

func (m *Manager) saveRecord(r *genRecord) error {
	return writeFileAtomic(filepath.Join(m.genDir(r.Generation.GenerationID), "record.json"), prettyJSON(r), 0o600)
}

func scopeKeyFor(scope string) string {
	return strings.NewReplacer(":", "-", "/", "_").Replace(scope)
}

func (m *Manager) currentID(scope string) string {
	b, err := os.ReadFile(filepath.Join(m.envDir(), "current", scopeKeyFor(scope)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (m *Manager) setCurrent(scope, id string) error {
	return writeFileAtomic(filepath.Join(m.envDir(), "current", scopeKeyFor(scope)), []byte(id+"\n"), 0o600)
}

// currentScopes returns scope -> generation ID for every applied scope.
func (m *Manager) currentScopes() map[string]string {
	out := map[string]string{}
	ents, _ := os.ReadDir(filepath.Join(m.envDir(), "current"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.envDir(), "current", e.Name()))
		if err == nil {
			out[e.Name()] = strings.TrimSpace(string(b))
		}
	}
	return out
}

// CurrentGenerationID returns the applied global generation ID, if any.
func (m *Manager) CurrentGenerationID(ctx context.Context) string { return m.currentID("global") }

// Generation returns a stored generation by ID.
func (m *Manager) Generation(ctx context.Context, id string) (contracts.EnvironmentGeneration, error) {
	r, err := m.loadRecord(id)
	if err != nil {
		return contracts.EnvironmentGeneration{}, err
	}
	return r.Generation, nil
}

// index maps generation digest -> generation ID so identical resolutions
// reuse one immutable generation.
func (m *Manager) indexPath() string { return filepath.Join(m.envDir(), "index.json") }

func (m *Manager) loadIndex() map[contracts.Digest]string {
	idx := map[contracts.Digest]string{}
	if b, err := os.ReadFile(m.indexPath()); err == nil {
		_ = json.Unmarshal(b, &idx)
	}
	return idx
}

func (m *Manager) indexGeneration(d contracts.Digest, id string) error {
	idx := m.loadIndex()
	idx[d] = id
	return writeFileAtomic(m.indexPath(), prettyJSON(idx), 0o600)
}

// ConfigRoots reports the harness config roots and whether Pyre manages
// anything inside them.
func (m *Manager) ConfigRoots(ctx context.Context) []contracts.ConfigRoot {
	managed := map[string]bool{}
	if id := m.currentID("global"); id != "" {
		if r, err := m.loadRecord(id); err == nil {
			for _, t := range r.Home {
				managed[t.Path] = true
			}
		}
	}
	isManaged := func(root string) bool {
		for p := range managed {
			if strings.HasPrefix(p, root+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	var out []contracts.ConfigRoot
	for _, r := range []struct {
		h contracts.Harness
		p string
	}{{contracts.HarnessClaude, m.opt.Homes.ClaudeHome}, {contracts.HarnessCodex, m.opt.Homes.CodexHome}} {
		if r.p == "" {
			continue
		}
		_, err := os.Stat(r.p)
		out = append(out, contracts.ConfigRoot{Harness: r.h, Path: m.display(r.p), Exists: err == nil, Managed: isManaged(r.p)})
	}
	return out
}

// Prune removes generation directories that are not current, not pinned by
// an active run, and not among the newest keep generations. It never removes
// anything inside harness config homes.
func (m *Manager) Prune(ctx context.Context, keep int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if keep < 1 {
		keep = 1
	}
	protected := map[string]bool{}
	for _, id := range m.currentScopes() {
		protected[id] = true
	}
	if m.opt.Runs != nil {
		runs, err := m.opt.Runs.ActiveRuns(ctx)
		if err != nil {
			return nil, fmt.Errorf("prune: list runs: %w (nothing removed)", err)
		}
		for _, r := range runs {
			protected[r.GenerationID] = true
		}
	}
	ents, err := os.ReadDir(filepath.Join(m.envDir(), "generations"))
	if err != nil {
		return nil, nil
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() && contracts.ValidID(e.Name(), contracts.PrefixEnvironment) {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids))) // ULIDs: newest first
	var removed []string
	for i, id := range ids {
		if i < keep || protected[id] {
			continue
		}
		if err := os.RemoveAll(m.genDir(id)); err != nil {
			return removed, err
		}
		removed = append(removed, id)
	}
	return removed, nil
}

func (m *Manager) report(ctx context.Context, kind contracts.ReportKind, payload any) {
	if m.opt.Reporter != nil {
		_ = m.opt.Reporter.Report(ctx, kind, payload)
	}
}

func (m *Manager) event(kind contracts.EventKind, ref contracts.ObjectRef, projectID string, payload any) contracts.Event {
	b, _ := json.Marshal(payload)
	var pid *string
	if projectID != "" {
		pid = &projectID
	}
	return contracts.Event{
		EventID: contracts.NewID(contracts.PrefixEvent), Origin: "node:" + m.opt.NodeID, Kind: kind,
		Object: ref, ProjectID: pid, PayloadVersion: 1, Payload: b, OccurredAt: m.opt.Clock.Now(),
	}
}
