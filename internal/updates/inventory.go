package updates

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/configuration"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

// Handler is the node action handler signature.
type Handler = configuration.Handler

// Handlers returns the update.* node action handlers.
func (m *Manager) Handlers() map[string]Handler {
	dec := func(raw json.RawMessage) (contracts.UpdateParams, error) {
		var p contracts.UpdateParams
		s := strings.TrimSpace(string(raw))
		if s == "" || s == "null" {
			return p, contracts.Errorf(contracts.ErrInvalid, "params required")
		}
		d := json.NewDecoder(strings.NewReader(s))
		d.DisallowUnknownFields()
		if err := d.Decode(&p); err != nil {
			return p, contracts.Errorf(contracts.ErrInvalid, "invalid params: %v", err)
		}
		return p, nil
	}
	return map[string]Handler{
		contracts.ActionUpdateStage: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := dec(raw)
			if err != nil {
				return nil, err
			}
			return m.Stage(ctx, p)
		},
		contracts.ActionUpdateActivate: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := dec(raw)
			if err != nil {
				return nil, err
			}
			return m.Activate(ctx, p)
		},
		contracts.ActionUpdateRollback: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := dec(raw)
			if err != nil {
				return nil, err
			}
			return m.Rollback(ctx, p)
		},
		contracts.ActionUpdateList: func(ctx context.Context, raw json.RawMessage) (any, error) {
			return m.List(ctx)
		},
	}
}

// IntegrationProber reports Hive/Outfit/Rota presence on the node.
type IntegrationProber interface {
	Status(ctx context.Context) map[string]contracts.IntegrationStatus
}

// PathIntegrations probes personal tools by executable and optional
// checkout directory (dirty checkouts are reported, never modified).
type PathIntegrations struct {
	Bins      map[string]string // name -> executable (absolute or on PATH)
	Checkouts map[string]string // name -> git checkout dir
	Runner    Runner
	Timeout   time.Duration
}

// DefaultIntegrations probes hive, outfit and rota on PATH.
func DefaultIntegrations() PathIntegrations {
	return PathIntegrations{Bins: map[string]string{"hive": "hive", "outfit": "outfit", "rota": "rota"}}
}

func (p PathIntegrations) Status(ctx context.Context) map[string]contracts.IntegrationStatus {
	run := p.Runner
	if run == nil {
		run = ExecRunner{}
	}
	to := p.Timeout
	if to <= 0 {
		to = 5 * time.Second
	}
	out := map[string]contracts.IntegrationStatus{}
	names := map[string]bool{}
	for n := range p.Bins {
		names[n] = true
	}
	for n := range p.Checkouts {
		names[n] = true
	}
	for n := range names {
		st := contracts.IntegrationStatus{Health: "unchecked"}
		if bin := p.Bins[n]; bin != "" {
			if path, err := exec.LookPath(bin); err == nil {
				st.Present, st.Path = true, path
				c, cancel := context.WithTimeout(ctx, to)
				stdout, _, err := run.Run(c, "", healthEnv, path, "--version")
				cancel()
				if err == nil {
					st.Version, st.Health = semverRe.FindString(stdout), "ok"
					if st.Version == "" {
						st.Version = trim(strings.SplitN(stdout, "\n", 2)[0])
					}
				} else {
					st.Health = "failed: " + trim(err.Error())
				}
			}
		}
		if dir := p.Checkouts[n]; dir != "" {
			if _, err := os.Stat(dir); err == nil {
				st.Present = true
				if st.Path == "" {
					st.Path = dir
				}
				c, cancel := context.WithTimeout(ctx, to)
				if rev, _, err := run.Run(c, dir, nil, "git", "rev-parse", "HEAD"); err == nil {
					st.Revision = strings.TrimSpace(rev)
				}
				if s, _, err := run.Run(c, dir, nil, "git", "status", "--porcelain"); err == nil {
					st.Dirty = strings.TrimSpace(s) != ""
				}
				cancel()
			}
		}
		out[n] = st
	}
	return out
}

// EnvSource is satisfied by *configuration.Manager.
type EnvSource interface {
	Drift(ctx context.Context) ([]contracts.DriftItem, error)
	CurrentGenerationID(ctx context.Context) string
	ConfigRoots(ctx context.Context) []contracts.ConfigRoot
	Homes() extensions.Homes
}

var _ EnvSource = (*configuration.Manager)(nil)

// Inventory composes contracts.NodeInventory for the "inventory" action.
type Inventory struct {
	NodeID       string
	Clock        contracts.Clock
	Harnesses    func(ctx context.Context) []contracts.HarnessInfo // adapter probes (daemon)
	Tools        *Manager
	Env          EnvSource
	Managed      extensions.ManagedSource
	Integrations IntegrationProber
}

// Collect gathers the inventory. Partial failures are reported in place
// (missing sections, drift item) instead of failing the whole call.
func (inv Inventory) Collect(ctx context.Context) (contracts.NodeInventory, error) {
	clk := inv.Clock
	if clk == nil {
		clk = contracts.SystemClock{}
	}
	out := contracts.NodeInventory{NodeID: inv.NodeID, CollectedAt: clk.Now(), Harnesses: []contracts.HarnessInfo{},
		Tools: []contracts.ToolVersion{}, Extensions: []contracts.ExtensionRef{}, ConfigRoots: []contracts.ConfigRoot{},
		Drift: []contracts.DriftItem{}, Integrations: map[string]contracts.IntegrationStatus{}}
	if inv.Harnesses != nil {
		out.Harnesses = inv.Harnesses(ctx)
	}
	if inv.Tools != nil {
		tools, err := inv.Tools.List(ctx)
		if err != nil {
			out.Drift = append(out.Drift, contracts.DriftItem{Path: "tools", Kind: "missing", Detail: "tool state unreadable: " + err.Error()})
		} else {
			out.Tools = tools
		}
	}
	if inv.Env != nil {
		scan, err := extensions.Scan(ctx, inv.Env.Homes(), extensions.ScanOptions{Managed: inv.Managed})
		if err == nil {
			out.Extensions = scan.Refs()
			out.Drift = append(out.Drift, scan.Duplicates...)
		}
		out.ConfigRoots = inv.Env.ConfigRoots(ctx)
		out.CurrentGeneration = inv.Env.CurrentGenerationID(ctx)
		if d, err := inv.Env.Drift(ctx); err == nil {
			out.Drift = append(out.Drift, d...)
		} else {
			out.Drift = append(out.Drift, contracts.DriftItem{Path: "environment", Kind: "missing", Detail: "drift check failed: " + err.Error()})
		}
	}
	if inv.Integrations != nil {
		out.Integrations = inv.Integrations.Status(ctx)
	}
	sort.SliceStable(out.Drift, func(i, j int) bool { return out.Drift[i].Path < out.Drift[j].Path })
	return out, nil
}

// Handler returns the "inventory" node action handler.
func (inv Inventory) Handler() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) { return inv.Collect(ctx) }
}
