package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/internal/osutil"
	"github.com/jhh3/pyre/internal/stablepath"

	"github.com/jhh3/pyre/internal/agentguide"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/configuration"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/extensions"
	"github.com/jhh3/pyre/internal/updates"
)

// envWiring holds the WP14 configuration and update managers.
type envWiring struct {
	cfg *configuration.Manager
	upd *updates.Manager
	inv updates.Inventory
	// pyreBin is the pyre CLI used for the agent MCP server ("" = none found).
	pyreBin string
}

// runRegistry adapts the daemon's run records to configuration.RunRegistry.
type runRegistry struct{ ctl *runController }

func (r runRegistry) ActiveRuns(ctx context.Context) ([]configuration.RunRef, error) {
	d, err := r.ctl.get()
	if err != nil {
		return nil, nil // daemon not started yet: no runs to protect
	}
	runs, err := d.ActiveRuns(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]configuration.RunRef, 0, len(runs))
	for _, a := range runs {
		out = append(out, configuration.RunRef{RunID: a.RunID, SessionID: a.SessionID, ProjectID: a.ProjectID,
			Harness: a.Harness, GenerationID: a.GenerationID, Executable: a.Executable})
	}
	return out, nil
}

// projectDirs returns this node's local roots for a project: the working
// directories of its runs here.
func (r runRegistry) projectDirs(ctx context.Context, projectID string) []string {
	d, err := r.ctl.get()
	if err != nil {
		return nil
	}
	runs, _ := d.ActiveRuns(ctx)
	seen := map[string]bool{}
	var out []string
	for _, a := range runs {
		if a.ProjectID == projectID && a.Cwd != "" && !seen[a.Cwd] {
			seen[a.Cwd] = true
			out = append(out, a.Cwd)
		}
	}
	return out
}

// GrantedConnections implements configuration.GrantSource over the
// pushed-grant mirror (deny by default).
func (g *pushedGrants) GrantedConnections(ctx context.Context, scope configuration.GrantScope, requested []string) ([]string, error) {
	var out []string
	for _, id := range requested {
		if dec, err := g.CheckGrant(ctx, id, scope.SessionID); err == nil && dec.Allowed {
			out = append(out, id)
		}
	}
	return out, nil
}

func wireEnvironment(in *integrations, cfg *daemon.Config, o runOpts, hubURL, pin, nodeToken string, log *slog.Logger) *envWiring {
	reg := runRegistry{ctl: in.ctl}
	external := map[string]string{}
	for _, tool := range []string{"claude", "codex"} {
		if p, err := exec.LookPath(tool); err == nil {
			external[tool] = p
		}
	}
	launcher, launcherVer, _ := launcherInfo()
	if filepath.Base(launcher) == "pyred" {
		external["pyred"] = launcher // the installed pyred; probed with `pyred version --json`
	}
	var hubProto func() (updates.ProtocolRange, bool)
	if hubURL != "" && nodeToken != "" {
		hc := client.New(hubURL, nodeToken, client.Options{CertSHA256: pin, UserAgent: "pyred-updates", MaxAttempts: 1})
		hubProto = func() (updates.ProtocolRange, bool) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			h, err := hc.Hello(ctx, contracts.HelloRequest{Protocol: contracts.ProtocolVersion, Client: "pyred-updates"})
			if err != nil {
				return updates.ProtocolRange{}, false
			}
			return updates.ProtocolRange{Min: h.MinProtocol, Max: h.Protocol}, true
		}
	}
	upd, err := updates.New(updates.Options{StateDir: o.state, NodeID: cfg.NodeID, Runs: reg, Reporter: in.rep,
		HubProtocol: hubProto, External: external, DaemonBase: launcherVer,
		OnDaemonSwitch: func(v string) {
			// Let the action result reach the hub first, then restart
			// through the launcher (runs live outside pyred).
			go func() {
				time.Sleep(daemonSwitchDelay)
				requestRestart(restartViaLauncher("pyred switched to " + orLauncherVersion(v)))
			}()
		}})
	if err != nil {
		log.Warn("updates integration unavailable; runs use the probed harness executables", "err", err)
		return nil
	}
	copt := configuration.Options{StateDir: o.state, NodeID: cfg.NodeID, ProjectDirs: reg.projectDirs,
		Runs: reg, Grants: in.grants, Versions: upd, Reporter: in.rep}
	if o.envHome != "" {
		copt.Homes = extensions.DefaultHomes(o.envHome)
		log.Info("environment manager uses an explicit config home", "env_home", o.envHome)
	}
	cm, err := configuration.New(copt)
	if err != nil {
		log.Warn("configuration integration unavailable; env profiles unsupported", "err", err)
		return nil
	}
	w := &envWiring{cfg: cm, upd: upd, inv: updates.Inventory{NodeID: cfg.NodeID, Tools: upd, Env: cm,
		Integrations: updates.DefaultIntegrations()}, pyreBin: findPyreCLI()}
	cfg.Environments = w
	mcp := w.pyreBin != ""
	cfg.AgentGuide = func(h contracts.Harness, s contracts.Session) string {
		// Claude (--mcp-config) and Codex (per-thread config) get the MCP tools.
		return agentguide.Text(agentguide.Options{CLI: w.pyreBin, MCP: mcp && h != contracts.HarnessShell, WorktreeRoot: cfg.WorktreeRoot})
	}
	return w
}

// daemonSwitchDelay separates a pyred activation's action result from the
// daemon's restart.
var daemonSwitchDelay = 3 * time.Second

func orLauncherVersion(v string) string {
	if v == "" {
		return "the installed pyred"
	}
	return v
}

// findPyreCLI returns the pyre CLI next to this pyred, else on PATH.
func findPyreCLI() string {
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		exe = stablepath.Of(exe)
		p := filepath.Join(filepath.Dir(exe), osutil.ExeName("pyre"))
		if st, err := os.Stat(p); err == nil && osutil.IsExecutable(p, st) {
			return p
		}
	}
	if p, err := exec.LookPath("pyre"); err == nil {
		return p
	}
	return ""
}

// Resolve implements daemon.EnvResolver: one immutable generation per
// launch, the overlay's global args (placed before any subcommand by the
// adapters: ResolvedLaunch.Args starts with them), non-secret env, the
// generation's connection grants, and the pinned harness executable.
//
// Claude: the generation's --settings file (managed claude.* values and
// hooks) is merged by the Claude adapter into the run's own single
// --settings file next to Pyre's runtime hooks; --mcp-config and
// --plugin-dir pass through. Codex: the generation's -c overlays become
// per-thread config of the run's thread on the shared app-server (the Codex
// adapter never passes them as process flags or writes a CODEX_HOME).
func (w *envWiring) Resolve(ctx context.Context, req daemon.EnvRequest) (daemon.EnvResult, error) {
	var res daemon.EnvResult
	managed := false
	if pin, ok := w.upd.Pinned(ctx, string(req.Harness)); ok {
		res.Executable, res.RuntimeVersion = pin.Path, pin.Version
		managed = pin.Digest != "" // a staged, digest-checked version (external pins carry none)
	}
	if req.Harness != contracts.HarnessClaude && req.Harness != contracts.HarnessCodex {
		if req.Profile != "" {
			return res, contracts.Errorf(contracts.ErrUnsupported, "environment profiles apply to claude/codex runs, not %s", req.Harness)
		}
		if req.Harness == contracts.HarnessShell {
			// No configuration overlay for a plain shell, but the default
			// profile's granted connections still apply: LaunchPrep places
			// credentials provisioned on this node in the shell's
			// environment and records the names-only manifest.
			sc := configuration.Scope{NodeID: req.NodeID, SessionID: req.SessionID, Harness: req.Harness}
			if req.ProjectID != nil {
				sc.ProjectID = *req.ProjectID
			}
			grants, err := w.cfg.LaunchConnections(ctx, sc)
			if err != nil {
				return res, err
			}
			res.ConnectionGrants = grants
		}
		return res, nil // shell/mock: no configuration overlay
	}
	sc := configuration.Scope{NodeID: req.NodeID, SessionID: req.SessionID, Profile: req.Profile, Harness: req.Harness, ConfigHome: req.ConfigHome}
	if req.Harness == contracts.HarnessClaude && w.pyreBin != "" {
		// Agent tools over MCP: `pyre mcp` (stdio) inherits PYRE_SOCKET /
		// PYRE_SESSION_TOKEN from the claude process, so tools act as the
		// session. Delivered as the overlay's --mcp-config file; identical
		// user registrations are deduplicated by the configuration manager.
		sc.AdapterMCP = []configuration.MCPServerSpec{{Name: "pyre", Harness: contracts.HarnessClaude, Transport: "stdio", Command: w.pyreBin, Args: []string{"mcp"}}}
	}
	if req.ProjectID != nil {
		sc.ProjectID = *req.ProjectID
	}
	gen, ov, err := w.cfg.Resolve(ctx, sc)
	if err != nil {
		return res, err
	}
	res.GenerationID, res.Args, res.Env, res.ConnectionGrants = gen.GenerationID, ov.ExtraArgs, ov.Env, gen.ConnectionGrants
	if managed && req.Harness == contracts.HarnessClaude {
		// A run pinned to a Pyre-managed Claude must not run Claude's native
		// auto-updater, which installs into and repoints the owner's own
		// Claude installation (observed on server with a run pinned to 2.1.288).
		if res.Env == nil {
			res.Env = map[string]string{}
		}
		res.Env["DISABLE_AUTOUPDATER"] = "1"
	}
	return res, nil
}

// register adds the env.*, update.* and merged inventory actions.
func (w *envWiring) register(d *daemon.Daemon) {
	for name, h := range w.cfg.Handlers() {
		d.RegisterAction(name, daemon.ActionFunc(h))
	}
	for name, h := range w.upd.Handlers() {
		d.RegisterAction(name, daemon.ActionFunc(h))
	}
	inv := w.inv
	inv.Harnesses = d.HarnessInfo
	d.RegisterAction(contracts.ActionInventory, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return mergeInventory(ctx, d.Inventory(ctx), inv)
	})
}

// mergeInventory combines the daemon's built-in inventory (pyred/tmux,
// Hive/Outfit detection) with WP14's (managed tools, extensions, config
// roots, drift, current generation).
func mergeInventory(ctx context.Context, base contracts.NodeInventory, inv updates.Inventory) (contracts.NodeInventory, error) {
	full, err := inv.Collect(ctx)
	if err != nil {
		return base, nil
	}
	have := map[string]bool{}
	for _, t := range full.Tools {
		have[t.Tool] = true
	}
	for _, t := range base.Tools {
		if !have[t.Tool] {
			full.Tools = append(full.Tools, t)
		}
	}
	if len(full.ConfigRoots) == 0 {
		full.ConfigRoots = base.ConfigRoots
	}
	ints := base.Integrations
	if ints == nil {
		ints = map[string]contracts.IntegrationStatus{}
	}
	for k, v := range full.Integrations {
		if b, ok := ints[k]; !ok || b.Health == "unchecked" {
			ints[k] = v
		}
	}
	full.Integrations = ints
	return full, nil
}
