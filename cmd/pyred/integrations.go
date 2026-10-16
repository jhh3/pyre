package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhh3/pyre/integrations/hive"
	"github.com/jhh3/pyre/integrations/outfit"
	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/connections"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/runtime/claude"
	"github.com/jhh3/pyre/internal/runtime/codex"
	"github.com/jhh3/pyre/internal/transport"
)

// integrations wires WP10 (Hive), WP12 (Outfit connections/credentials)
// and WP13 (accounts) into the daemon. Every integration degrades to a
// reported status; none blocks the daemon or a launch unless the launch
// explicitly needs it.
type integrations struct {
	log    *slog.Logger
	acct   *accounts.Service
	ctl    *runController
	rep    *reportBridge
	conns  *connections.NodeService
	grants *pushedGrants
	hive   *hive.Client
	bridge *hive.Bridge
	// capturer hands Codex rollouts to Hive (see hivecapture.go).
	capturer *hive.Capturer
	env      *envWiring
	hub      *client.Client // node-credential hub client (note events)
}

func wireIntegrations(ctx context.Context, cfg *daemon.Config, o runOpts, built builtAdapters, hubURL, pin, nodeToken string, log *slog.Logger) *integrations {
	in := &integrations{log: log, ctl: &runController{}, rep: &reportBridge{}, grants: &pushedGrants{active: map[grantKey]bool{}}}
	if hubURL != "" && nodeToken != "" {
		// Node-credential hub client: note events, grant checks, fleet leases.
		in.hub = client.New(hubURL, nodeToken, client.Options{CertSHA256: pin, UserAgent: "pyred-integrations"})
	}

	// WP13 accounts: isolated profiles, selection, fallback, rota checkout.
	var lapi leaseAPI
	var tokens contracts.AccountTokenFetcher
	var fleet accounts.FleetDirectory // machine affinity: other machines' runs per account
	if in.hub != nil {
		lapi, tokens, fleet = in.hub, in.hub, in.hub
	}
	var busy accounts.HomeBusyChecker
	if built.codex != nil {
		busy = built.codex
	}
	svc, err := accounts.New(ctx, accounts.Config{NodeID: cfg.NodeID, DB: cfg.DB, ProfileRoot: profileRoot(o.state, os.Getenv, userHome()),
		Reporter: in.rep, Controller: in.ctl, Logger: log.With("component", "accounts"), CodexReader: codexReader(built),
		Rota: rotaConfig(o, lapi, busy), Tokens: tokenSource(tokens), TestMode: o.accountsTestMode, ClaudeBin: lookPathOrEmpty("claude"), DefaultAccount: o.defaultAccount,
		Fleet: fleet, NodeName: cfg.NodeName,
		LoginRefresher: loginRefresher{codex: built.codex, claudeBin: lookPathOrEmpty("claude"), stateDir: o.state}})
	if o.accountsTestMode {
		log.Warn("accounts TEST MODE is on (--accounts-test-mode): account.inject_limit is enabled; testing only")
	}
	if err != nil {
		log.Warn("accounts integration unavailable; runs use the ambient login", "err", err)
	} else {
		in.acct = svc
		// Every Claude profile shares one conversation store, so a session
		// resumes natively under another account (W-xacct; idempotent).
		svc.MigrateSharedStores(ctx)
		cfg.Accounts = accountsResolver{svc}
		// The machine's own login: identity in the harness inventory, usage
		// from the rate limits Pyre's own runs on it receive (no probes).
		cfg.OwnLogin = svc.OwnLogin
		if built.rates != nil {
			built.rates.set(func(home string, raw json.RawMessage) {
				if home == "" {
					svc.ObserveOwnRateLimits(ctx, contracts.HarnessCodex, raw)
				}
			})
		}
	}

	// W-audit: dialogs the adapters answered by themselves reach the
	// owner's audit log as node.automation events (durable outbox).
	if built.auto != nil {
		built.auto.set(func(a contracts.NodeAutomation) {
			payload, err := json.Marshal(a)
			if err != nil {
				return
			}
			ev := contracts.Event{Kind: contracts.EvNodeAutomation, Object: contracts.ObjectRef{Kind: contracts.KindRun, ID: a.RunID},
				SessionID: a.SessionID, PayloadVersion: 1, Payload: payload, OccurredAt: time.Now().UTC()}
			if err := in.rep.Report(ctx, contracts.ReportEvent, ev); err != nil {
				log.Info("audit: automation report not queued", "action", a.Action, "run", a.RunID, "err", err)
			}
		})
	}

	// WP12 connections and credential provisioning.
	stores := credentials.Stores{}
	backend := ""
	if ccfg, err := credentialConfig(o, os.Getenv); err != nil {
		log.Warn("credential store configuration rejected; connection provisioning limited to remote invocation", "err", err)
	} else if st, err := credentials.Open(ccfg); err != nil {
		log.Info("no OS credential store; connection provisioning limited to remote invocation", "err", err)
	} else {
		if ccfg.Keychain != "" {
			log.Info("connection credentials use an explicit keychain file", "keychain", ccfg.Keychain)
		}
		stores.Add(st)
		backend = st.Backend()
	}
	oa := outfit.New(outfit.Options{})
	ns, err := connections.NewNodeService(connections.NodeConfig{NodeID: cfg.NodeID, Outfit: oa, Stores: stores,
		Index: credentials.OpenIndex(filepath.Join(o.state, "provisions.json")), DefaultBackend: backend,
		Grants: in.grantChecker(), Logger: log.With("component", "connections"),
		// Push a fresh connections report after each check so the hub
		// catalog shows verified availability without waiting 5 minutes.
		OnAvailabilityChange: func() { go in.pushConnections(ctx) }})
	if err != nil {
		log.Warn("connections integration unavailable", "err", err)
	} else {
		in.conns = ns
		cfg.LaunchPrep = func(ctx context.Context, req daemon.LaunchPrepRequest) (daemon.LaunchPrepResult, error) {
			if len(req.ConnectionGrants) == 0 {
				return daemon.LaunchPrepResult{}, nil
			}
			plan, err := ns.PrepareLaunch(ctx, req.SessionID, req.ConnectionGrants)
			if err != nil {
				return daemon.LaunchPrepResult{}, err
			}
			m, _ := json.Marshal(plan) // Env is never serialized (json:"-")
			return daemon.LaunchPrepResult{Env: plan.Env, Manifest: m}, nil
		}
		cfg.Periodic = append(cfg.Periodic, daemon.PeriodicReport{Kind: contracts.ReportConnections, Interval: 5 * time.Minute,
			Collect: func(ctx context.Context) (any, error) { return ns.Report(ctx) }})
	}

	// WP10 Hive memory.
	in.hive = hive.New(hive.EnvFromOS(), hive.Options{BackgroundSlots: o.hiveBackgroundSlots})
	in.capturer = hive.NewCapturerWith(in.hive, hive.CapturerOptions{MaxQueue: o.hiveCaptureQueue,
		Logf: func(f string, a ...any) { log.Info("hive capture: " + sprintf(f, a...)) }})
	cfg.OnRunEnded = in.onRunEnded
	if in.hub != nil {
		b, err := hive.NewBridge(hive.BridgeConfig{Store: in.hub, Hive: in.hive, StateFile: filepath.Join(o.state, "hive-bridge.json"),
			Logf: func(f string, a ...any) { log.Info("hive bridge: " + sprintf(f, a...)) }})
		if err != nil {
			log.Warn("hive bridge unavailable", "err", err)
		} else {
			in.bridge = b
			in.hive.SetPendingFunc(b.Pending)
		}
	}

	cfg.BackgroundQueues = in.backgroundQueues
	in.env = wireEnvironment(in, cfg, o, hubURL, pin, nodeToken, log)
	cfg.Integrations = func(ctx context.Context) map[string]contracts.IntegrationStatus {
		out := map[string]contracts.IntegrationStatus{
			"outfit": oa.Detect(ctx).Integration(),
			"hive":   in.hive.Status(ctx).Integration,
		}
		return out
	}
	return in
}

// attach registers actions and starts background work once the daemon and
// hub link exist.
func (in *integrations) attach(ctx context.Context, d *daemon.Daemon, link *transport.NodeLink) {
	in.ctl.set(d)
	if in.env != nil {
		in.env.register(d)
	}
	if link != nil {
		in.rep.set(link)
	}
	if in.env != nil && link != nil {
		// Results recorded at boot (an automatic pyred rollback by the
		// launcher) go out once the report outbox is attached; reports
		// made before that would be dropped.
		if n := in.env.upd.FlushPendingResults(ctx); n > 0 {
			in.log.Info("reported update results recorded at boot (automatic pyred rollback)", "count", n)
		}
	}
	if in.acct != nil {
		for name, h := range in.acct.Handlers() {
			d.RegisterAction(name, daemon.ActionFunc(h))
		}
		go in.acct.RunObserver(ctx)
		if in.hub != nil {
			// Token accounts follow the hub: registered here without
			// add-token (node enrolled or harness installed later), and the
			// hub's account folds applied (accounts/tokenmerge.go).
			go in.acct.RunTokenSync(ctx, in.hub, harnessAvailable(ctx, d), 20*time.Second, 3*time.Minute)
		}
	}
	if in.conns != nil {
		in.grants.sessions = d
		in.grants.nodeID = d.NodeID()
		for name, h := range in.conns.Handlers() {
			h := daemon.ActionFunc(h)
			switch name {
			case contracts.ActionConnGrant, contracts.ActionConnRevoke:
				active := name == contracts.ActionConnGrant
				inner := h
				h = func(ctx context.Context, p json.RawMessage) (any, error) {
					v, err := inner(ctx, p)
					if err == nil {
						var gp contracts.ConnectionGrantParams
						if json.Unmarshal(p, &gp) == nil {
							in.grants.record(gp, active)
						}
					}
					return v, err
				}
			}
			d.RegisterAction(name, h)
		}
		for name, h := range in.conns.ProvisioningHandlers() {
			d.RegisterAction(name, daemon.ActionFunc(h))
		}
	}
	// Attribute transcripts Hive captured (through the harness's own hooks)
	// to the Pyre session and run that launched that native session.
	in.hive.SetNativeResolver(func(ctx context.Context, nativeID string) (hive.NativeSession, bool) {
		sid, rid, pid, ok := d.SessionForNative(ctx, nativeID)
		ns := hive.NativeSession{SessionID: sid, RunID: rid}
		if pid != nil {
			ns.ProjectID = *pid
		}
		return ns, ok
	})
	for name, h := range in.hive.Handlers() {
		d.RegisterAction(name, daemon.ActionFunc(h))
	}
	if in.capturer != nil {
		go in.capturer.Run(ctx)
		go in.captureIdleLoop(ctx, d)
	}
	if in.bridge != nil {
		// UpdateNoteSync needs a node principal in the context.
		bctx := contracts.WithPrincipal(ctx, contracts.Principal{Kind: contracts.PrincipalNode, ID: d.NodeID()})
		go in.bridge.Run(bctx)
		if in.hub != nil {
			go watchNotes(bctx, in.hub, in.bridge.Notify, in.log)
		}
	}
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

// backgroundQueues reports Hive background queue depths and limits for
// node diagnostics (A20: queue sizes stay within their caps).
func (in *integrations) backgroundQueues() (map[string]int, map[string]int) {
	q, l := map[string]int{}, map[string]int{}
	if in.hive != nil {
		bs := in.hive.BackgroundStats()
		q["hive_background_running"], q["hive_background_waiting"] = bs.Running, bs.Waiting
		l["hive_background_slots"] = bs.Slots
	}
	if in.capturer != nil {
		st := in.capturer.Stats()
		q["hive_capture_pending"], q["hive_capture_dropped"] = in.capturer.Pending(), st.Dropped
		l["hive_capture_queue"] = in.capturer.MaxQueue()
	}
	if in.bridge != nil {
		q["hive_bridge_pending"] = in.bridge.Pending()
	}
	return q, l
}

// pushConnections enqueues an out-of-cycle connections report.
func (in *integrations) pushConnections(ctx context.Context) {
	if in.conns == nil {
		return
	}
	list, err := in.conns.Report(ctx)
	if err != nil {
		in.log.Warn("connections report failed", "err", err)
		return
	}
	if err := in.rep.Report(ctx, contracts.ReportConnections, list); err != nil {
		in.log.Warn("connections report enqueue failed", "err", err)
	}
}

// ---- WP13 adapters ----

// accountsResolver adapts accounts.Service to the daemon account hooks.
type accountsResolver struct{ s *accounts.Service }

func (a accountsResolver) Resolve(ctx context.Context, h contracts.Harness, p contracts.AccountPolicy, runID string) (string, map[string]string, *contracts.AccountAssignment, error) {
	return a.s.Resolve(ctx, h, p, runID)
}

func (a accountsResolver) Bind(ctx context.Context, r daemon.AccountBindRequest) (daemon.AccountBinding, error) {
	res, err := a.s.ResolveFor(ctx, accounts.ResolveRequest{Harness: r.Harness, Policy: r.Policy, RunID: r.RunID, SessionID: r.SessionID, Exclude: r.Exclude})
	if err != nil {
		return daemon.AccountBinding{}, err
	}
	return daemon.AccountBinding{ProfileDir: res.ProfileDir, Env: res.Env, Unset: res.Unset, Assignment: res.Assignment}, nil
}

func (a accountsResolver) CheckAssignment(ctx context.Context, runID string) error {
	return a.s.CheckAssignment(ctx, runID)
}

func (a accountsResolver) Release(runID string) { a.s.Release(runID) }

// OnLimit hands limit events to the account service, which plans the
// fallback and reports it itself (so the daemon reports nothing extra).
func (a accountsResolver) OnLimit(ctx context.Context, run contracts.Run, ev contracts.LimitEvent) (*contracts.FallbackPlan, error) {
	_, err := a.s.HandleLimitEvent(ctx, run.SessionID, run.RunID, ev)
	return nil, err
}

// reportBridge implements accounts.Reporter over the hub link (dropped in
// standalone mode).
type reportBridge struct {
	mu   sync.Mutex
	link *transport.NodeLink
}

func (r *reportBridge) set(l *transport.NodeLink) { r.mu.Lock(); r.link = l; r.mu.Unlock() }

func (r *reportBridge) Report(ctx context.Context, kind contracts.ReportKind, payload any) error {
	r.mu.Lock()
	l := r.link
	r.mu.Unlock()
	if l == nil {
		return nil
	}
	_, err := l.Enqueue(ctx, kind, payload)
	return err
}

// runController implements accounts.RunController over the daemon.
type runController struct {
	mu sync.Mutex
	d  *daemon.Daemon
}

func (c *runController) set(d *daemon.Daemon) { c.mu.Lock(); c.d = d; c.mu.Unlock() }

func (c *runController) get() (*daemon.Daemon, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.d == nil {
		return nil, contracts.Errorf(contracts.ErrUnavailable, "daemon not started")
	}
	return c.d, nil
}

func (c *runController) CurrentRun(ctx context.Context, sessionID string) (accounts.RunInfo, error) {
	d, err := c.get()
	if err != nil {
		return accounts.RunInfo{}, err
	}
	r, err := d.CurrentRun(ctx, sessionID)
	return accounts.RunInfo{SessionID: r.SessionID, RunID: r.RunID, Harness: r.Harness, Capabilities: r.Capabilities, NativeRef: r.NativeRef, TurnInFlight: r.TurnInFlight}, err
}

func (c *runController) Quiesce(ctx context.Context, sessionID, runID string) (accounts.QuiesceResult, error) {
	d, err := c.get()
	if err != nil {
		return accounts.QuiesceResult{}, err
	}
	exited, inFlight, detail, err := d.QuiesceRun(ctx, sessionID, runID)
	return accounts.QuiesceResult{Exited: exited, TurnInFlight: inFlight, Detail: detail}, err
}

func (c *runController) StartContinuation(ctx context.Context, k accounts.Continuation) (accounts.ContinuationResult, error) {
	d, err := c.get()
	if err != nil {
		return accounts.ContinuationResult{}, err
	}
	id, kind, err := d.StartContinuation(ctx, daemon.ContinuationRequest{SessionID: k.SessionID, PreviousRunID: k.PreviousRunID,
		AccountID: k.AccountID, Kind: k.Kind, Reason: k.Reason, Prompt: k.Prompt})
	return accounts.ContinuationResult{NewRunID: id, Kind: kind}, err
}

// ---- WP12 grant checker ----

type grantKey struct{ conn, node, project, session string }

// pushedGrants allows only grants the hub pushed to this node via
// connection.grant (deny by default). Connection use goes through
// hubGrants (grants.go), which asks the hub and uses this mirror only when
// standalone or talking to a hub without connections.check_grant.
type pushedGrants struct {
	mu       sync.Mutex
	active   map[grantKey]bool
	nodeID   string
	sessions interface {
		SessionInfo(ctx context.Context, sessionID string) (contracts.Session, bool)
	}
}

func (g *pushedGrants) record(p contracts.ConnectionGrantParams, active bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active[grantKey{p.ConnectionID, p.NodeID, p.ProjectID, p.SessionID}] = active
}

func (g *pushedGrants) CheckGrant(ctx context.Context, connID, sessionID string) (connections.GrantDecision, error) {
	dec := connections.GrantDecision{ConnectionID: connID, SessionID: sessionID, CheckedAt: time.Now().UTC(), Reason: "no grant pushed to this node"}
	project := ""
	if g.sessions != nil {
		if s, ok := g.sessions.SessionInfo(ctx, sessionID); ok && s.ProjectID != nil {
			project = *s.ProjectID
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, on := range g.active {
		if !on || k.conn != connID || (k.node != "" && k.node != g.nodeID) {
			continue
		}
		if k.session == sessionID || (k.session == "" && (k.project == "" || k.project == project)) {
			dec.Allowed, dec.Reason = true, "granted (pushed to node)"
			return dec, nil
		}
	}
	return dec, nil
}

// codexReader returns the Codex adapter as the accounts rate-limit reader
// (it reads through the single Pyre app-server per CODEX_HOME), or nil.
// loginRefresher lets a harness refresh its own login inside a checked-out
// rota profile whose access token expired at rest (accounts.LoginRefresher):
// Claude's TUI started without a prompt (hooks off), Codex's app-server
// account/read with refreshToken. Never used for a login rota still holds.
type loginRefresher struct {
	codex     *codex.Adapter
	claudeBin string
	stateDir  string
}

func (l loginRefresher) RefreshLogin(ctx context.Context, h contracts.Harness, profileDir string, fresh func() bool) error {
	switch h {
	case contracts.HarnessClaude:
		if l.claudeBin == "" {
			return fmt.Errorf("claude not found on this node")
		}
		return claude.WarmLogin(ctx, claude.WarmLoginOptions{Bin: l.claudeBin, Dir: filepath.Join(l.stateDir, "wl"),
			Unset: accounts.UnsetFor(h), Env: accounts.ClaudeProfileEnv()}, profileDir, fresh)
	case contracts.HarnessCodex:
		if l.codex == nil {
			return fmt.Errorf("codex adapter unavailable on this node")
		}
		return l.codex.RefreshLogin(ctx, profileDir)
	}
	return fmt.Errorf("no login refresh for harness %s", h)
}

func codexReader(b builtAdapters) accounts.CodexRateLimitReader {
	if b.codex == nil {
		return nil
	}
	return b.codex
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// lookPathOrEmpty resolves a tool on the (widened) PATH, "" if absent.
func lookPathOrEmpty(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// harnessAvailable reports whether the node's probes (cached, at most one
// run per 5 minutes) find the harness usable.
func harnessAvailable(ctx context.Context, d *daemon.Daemon) func(contracts.Harness) bool {
	return func(h contracts.Harness) bool {
		for _, hi := range d.HarnessInfo(ctx) {
			if hi.Harness == h && hi.Available {
				return true
			}
		}
		return false
	}
}
