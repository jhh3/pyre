package mock

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

// actionsAllowedForSessions may be invoked by agent-session principals;
// everything else is owner-only.
var actionsAllowedForSessions = map[string]bool{
	c.ActionConnInvoke: true, c.ActionConnCheck: true, c.ActionConnList: true,
	c.ActionMemorySearch: true, c.ActionMemoryGet: true, c.ActionMemoryStatus: true, c.ActionDirList: true,
}

func (b *Backend) InvokeNodeAction(ctx context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	return mutate(b, ctx, c.OpInvokeNodeAction, req.Mutation, req, func(p c.Principal) (c.NodeActionResult, error) {
		if p.Kind != c.PrincipalOwner && !(p.Kind == c.PrincipalSession && actionsAllowedForSessions[req.Action]) {
			return c.NodeActionResult{}, c.Errorf(c.ErrForbidden, "%s principals may not invoke %s", p.Kind, req.Action)
		}
		if req.NodeID == "" && req.Action == c.ActionConnInvoke {
			req.NodeID = b.routeInvoke(req.Params) // like the hub: route to the verified holder
		}
		n, ok := b.nodes[req.NodeID]
		if !ok {
			return c.NodeActionResult{}, notFound(c.KindNode, req.NodeID)
		}
		if n.Connection.State != c.ConnConnected {
			return c.NodeActionResult{}, c.Errorf(c.ErrUnavailable, "node %s is not connected; action not sent", n.Name).WithRef(c.KindNode, n.NodeID)
		}
		result, err := b.runAction(p, n, req.Action, req.Params)
		res := c.NodeActionResult{CommandID: c.NewID(c.PrefixCommand)}
		if err != nil {
			ce, ok := err.(*c.Error)
			if !ok {
				return c.NodeActionResult{}, err
			}
			if ce.Code == c.ErrNotFound && strings.HasPrefix(ce.Message, "unknown action") {
				return c.NodeActionResult{}, ce
			}
			res.Outcome = c.OutcomeFailed
			if ce.Code == c.ErrUnsupported {
				res.Outcome = c.OutcomeUnsupported
			}
			res.Error = ce
		} else {
			res.Outcome = c.OutcomeSucceeded
			raw, _ := json.Marshal(result)
			res.Result = raw
		}
		b.emitFrom("node:"+n.NodeID, c.EvCommandResult, ref(c.KindCommand, res.CommandID), "", nil,
			c.CommandResult{CommandID: res.CommandID, Kind: c.CmdAction, Outcome: res.Outcome, Result: res.Result, Error: res.Error, At: b.now()})
		b.cmdResults[res.CommandID] = cmdResult{res: res, actor: p, nodeID: n.NodeID}
		return res, nil
	})
}

// routeInvoke picks the first node with verified remote_invoke availability
// for the connection ("" when none).
func (b *Backend) routeInvoke(params c.RawJSON) string {
	var ip c.ConnectionInvokeParams
	if decodeParams(params, &ip) != nil {
		return ""
	}
	if cn, ok := b.connections[ip.ConnectionID]; ok {
		for _, av := range cn.Availability {
			if av.Mode == "remote_invoke" && av.State == "verified" {
				return av.NodeID
			}
		}
	}
	return ""
}

func decodeParams(raw c.RawJSON, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return c.Errorf(c.ErrInvalid, "invalid action params: %v", err)
	}
	return nil
}

func (b *Backend) nodeTools(nodeID string) []c.ToolVersion {
	if t, ok := b.tools[nodeID]; ok {
		return t
	}
	return b.tools["*"]
}

func (b *Backend) runAction(p c.Principal, n *c.Node, action string, params c.RawJSON) (any, error) {
	now := b.now()
	if v, ok, err := b.deviceLoginAction(n, action, params); ok { // devlogin.go
		return v, err
	}
	switch action {
	case c.ActionInventory:
		inv := c.NodeInventory{NodeID: n.NodeID, CollectedAt: now, Harnesses: n.Harnesses, Tools: b.nodeTools(n.NodeID), Extensions: []c.ExtensionRef{},
			ConfigRoots: []c.ConfigRoot{{Harness: c.HarnessClaude, Path: "~/.claude", Exists: true}, {Harness: c.HarnessCodex, Path: "~/.codex", Exists: true}},
			Drift:       b.drift, CurrentGeneration: b.currentEnvID(n.NodeID),
			Integrations: map[string]c.IntegrationStatus{
				"hive": {Present: true, Version: "mock", Health: "mock"}, "outfit": {Present: true, Version: "mock", Health: "mock"}, "rota": {Present: true, Version: "mock", Health: "mock"},
			}}
		if gens := b.envs[n.NodeID]; len(gens) > 0 {
			g := gens[len(gens)-1]
			inv.Extensions = append(append(append(inv.Extensions, g.Skills...), g.Plugins...), g.MCPServers...)
		}
		if inv.Tools == nil {
			inv.Tools = []c.ToolVersion{}
		}
		if inv.Drift == nil {
			inv.Drift = []c.DriftItem{}
		}
		return inv, nil
	case c.ActionEnvDrift:
		if b.drift == nil {
			return []c.DriftItem{}, nil
		}
		return b.drift, nil
	case c.ActionEnvPlan:
		var pp c.EnvPlanParams
		if err := decodeParams(params, &pp); err != nil {
			return nil, err
		}
		for _, id := range sortedKeys(b.plans) {
			pl := b.plans[id]
			if pl.NodeID == n.NodeID && (pp.Scope == "" || pl.Scope == pp.Scope) {
				return pl, nil
			}
		}
		pl := c.ChangePlan{PlanID: c.NewID("plan"), NodeID: n.NodeID, Scope: pp.Scope, Changes: []c.PlanChange{}, Conflicts: []c.DriftItem{}, RestartRequired: []string{}}
		pl.ResultDigest = c.Digest("sha256:" + digestOf(pl))
		b.plans[pl.PlanID] = pl
		return pl, nil
	case c.ActionEnvApply:
		var ap c.EnvApplyParams
		if err := decodeParams(params, &ap); err != nil {
			return nil, err
		}
		pl, ok := b.plans[ap.PlanID]
		if !ok || pl.NodeID != n.NodeID {
			return nil, notFound("plan", ap.PlanID)
		}
		if ap.ResultDigest != pl.ResultDigest {
			return nil, c.Errorf(c.ErrConflict, "plan result digest changed since review; re-plan")
		}
		if len(pl.Conflicts) > 0 {
			return nil, c.Errorf(c.ErrConflict, "plan has %d unresolved conflicts", len(pl.Conflicts))
		}
		prev := b.currentEnvID(n.NodeID)
		g := c.EnvironmentGeneration{GenerationID: c.NewID(c.PrefixEnvironment), NodeID: n.NodeID, Profile: "default", CreatedAt: now,
			RuntimeVersions: map[string]string{}, ConfigDigests: map[string]c.Digest{}, Skills: []c.ExtensionRef{}, Plugins: []c.ExtensionRef{},
			MCPServers: []c.ExtensionRef{}, ConnectionGrants: []string{}, Digest: pl.ResultDigest, PreviousID: prev}
		if gens := b.envs[n.NodeID]; len(gens) > 0 {
			last := clone(gens[len(gens)-1])
			g.RuntimeVersions, g.ConfigDigests, g.Skills, g.Plugins, g.MCPServers, g.ConnectionGrants, g.ProjectID = last.RuntimeVersions, last.ConfigDigests, last.Skills, last.Plugins, last.MCPServers, last.ConnectionGrants, last.ProjectID
		}
		b.envs[n.NodeID] = append(b.envs[n.NodeID], g)
		b.emitFrom("node:"+n.NodeID, c.EvEnvironmentApplied, ref(c.KindEnvironment, g.GenerationID), "", nil, g)
		return g, nil
	case c.ActionEnvRollback:
		var rp c.EnvRollbackParams
		if err := decodeParams(params, &rp); err != nil {
			return nil, err
		}
		for _, g := range b.envs[n.NodeID] {
			if g.GenerationID == rp.GenerationID {
				ng := clone(g)
				ng.PreviousID = b.currentEnvID(n.NodeID)
				ng.CreatedAt = now
				b.envs[n.NodeID] = append(b.envs[n.NodeID], ng)
				b.emitFrom("node:"+n.NodeID, c.EvEnvironmentApplied, ref(c.KindEnvironment, ng.GenerationID), "", nil, ng)
				return ng, nil
			}
		}
		return nil, notFound(c.KindEnvironment, rp.GenerationID)
	case c.ActionEnvResolve:
		if gens := b.envs[n.NodeID]; len(gens) > 0 {
			return gens[len(gens)-1], nil
		}
		return nil, c.Errorf(c.ErrNotFound, "no environment generation on %s", n.Name)
	case c.ActionUpdateList:
		t := b.nodeTools(n.NodeID)
		if t == nil {
			t = []c.ToolVersion{}
		}
		return t, nil
	case c.ActionUpdateStage, c.ActionUpdateActivate, c.ActionUpdateRollback:
		return b.updateAction(n, action, params)
	case c.ActionConnList:
		var out []c.Connection
		for _, id := range sortedKeys(b.connections) {
			out = append(out, *b.connections[id])
		}
		if out == nil {
			out = []c.Connection{}
		}
		return out, nil
	case c.ActionConnCheck:
		var cp c.ConnectionParams
		if err := decodeParams(params, &cp); err != nil {
			return nil, err
		}
		cn, ok := b.connections[cp.ConnectionID]
		if !ok {
			return nil, notFound(c.KindConnection, cp.ConnectionID)
		}
		for _, av := range cn.Availability {
			if av.NodeID == n.NodeID {
				return av, nil
			}
		}
		return c.ConnectionAvailability{NodeID: n.NodeID, State: "unknown", Detail: "mock: never checked on this node", VerifiedAt: now}, nil
	case c.ActionConnInvoke:
		var ip c.ConnectionInvokeParams
		if err := decodeParams(params, &ip); err != nil {
			return nil, err
		}
		cn, ok := b.connections[ip.ConnectionID]
		if !ok {
			return nil, notFound(c.KindConnection, ip.ConnectionID)
		}
		sid := ip.SessionID
		if p.Kind == c.PrincipalSession {
			sid = p.ID // a session can only invoke as itself
		}
		if !b.grantAllows(cn, sid) {
			return nil, c.Errorf(c.ErrForbidden, "session %s has no grant for connection %s", sid, cn.Name)
		}
		out, _ := json.Marshal(map[string]any{"mock": true, "operation": ip.Operation, "detail": "mock backend: no external call was made"})
		return c.ConnectionInvokeResult{ConnectionID: cn.ConnectionID, Output: out}, nil
	case c.ActionConnGrant, c.ActionConnRevoke:
		var gp c.ConnectionGrantParams
		if err := decodeParams(params, &gp); err != nil {
			return nil, err
		}
		cn, ok := b.connections[gp.ConnectionID]
		if !ok {
			return nil, notFound(c.KindConnection, gp.ConnectionID)
		}
		edit := func(xs []string, v string) []string {
			if v == "" {
				return xs
			}
			xs = removeString(xs, v)
			if action == c.ActionConnGrant {
				xs = append(xs, v)
			}
			return xs
		}
		cn.Scope.Nodes = edit(cn.Scope.Nodes, gp.NodeID)
		cn.Scope.Projects = edit(cn.Scope.Projects, gp.ProjectID)
		cn.Scope.Sessions = edit(cn.Scope.Sessions, gp.SessionID)
		switch {
		case len(cn.Scope.Nodes)+len(cn.Scope.Projects)+len(cn.Scope.Sessions) > 0:
			cn.Scope.Access = "scoped"
		case action == c.ActionConnGrant && gp.NodeID == "" && gp.ProjectID == "" && gp.SessionID == "":
			cn.Scope.Access = "broad"
		case cn.Scope.Access != "broad" && cn.Scope.Access != "":
			cn.Scope.Access = "none"
		}
		cn.Generation++ // grants/revokes bump the generation (contract)
		return *cn, nil
	case c.ActionAccountList:
		var out []c.AccountView
		for _, id := range sortedKeys(b.accounts) {
			out = append(out, *b.accounts[id])
		}
		if out == nil {
			out = []c.AccountView{}
		}
		return out, nil
	case c.ActionAccountRegister:
		var rp c.AccountRegisterParams
		if err := decodeParams(params, &rp); err != nil {
			return nil, err
		}
		av, ok := b.accounts[rp.Account.AccountID]
		if !ok {
			return nil, notFound(c.KindAccount, rp.Account.AccountID)
		}
		if rp.Source != "rota" && rp.Source != "login" && rp.Source != "" {
			return nil, c.Errorf(c.ErrInvalid, "source must be rota or login")
		}
		ref := ""
		kind := c.CredClaudeProfileStore
		if rp.Harness == c.HarnessCodex {
			kind = c.CredCodexHomeFile
		}
		if rp.Source == "rota" {
			if rp.Account.RotaRef == "" {
				return nil, c.Errorf(c.ErrInvalid, "a rota account needs rota_ref")
			}
			kind = c.CredRotaCheckout
			if rp.BindRotaIdentity {
				ref = "synthetic-identity-" + rp.Account.RotaRef
			}
		}
		g := c.DeviceGrant{GrantID: c.NewID(c.PrefixGrant), AccountID: av.Account.AccountID, NodeID: n.NodeID, Harness: rp.Harness, Kind: kind,
			ProfileRef: "mock-profile", State: c.GrantValid, RefreshOwner: "harness", Generation: 1, ObservedAt: b.now(), Detail: "mock: no credential moved"}
		av.Grants = append(av.Grants, g)
		return map[string]any{"provider_ref": ref, "grant": g, "verified": true, "detail": "mock backend: profile prepared, nothing checked out"}, nil
	case c.ActionAccountCheckouts:
		return []map[string]any{}, nil
	case c.ActionAccountCheckin:
		var cp struct {
			AccountID string `json:"account_id"`
		}
		if err := decodeParams(params, &cp); err != nil {
			return nil, err
		}
		if _, ok := b.accounts[cp.AccountID]; !ok {
			return nil, notFound(c.KindAccount, cp.AccountID)
		}
		return map[string]any{"account_id": cp.AccountID, "state": "released", "detail": "mock: nothing was checked out"}, nil
	case c.ActionAccountObserve:
		var out []c.AccountObservation
		for _, id := range sortedKeys(b.accounts) {
			if a := b.accounts[id]; a.Latest != nil {
				out = append(out, *a.Latest)
			}
		}
		if out == nil {
			out = []c.AccountObservation{}
		}
		return out, nil
	case c.ActionAccountSelect:
		var sp c.AccountSelectParams
		if err := decodeParams(params, &sp); err != nil {
			return nil, err
		}
		h := c.HarnessClaude
		if sp.Provider == c.ProviderOpenAI {
			h = c.HarnessCodex
		}
		id, reason, err := b.chooseAccount(h, sp.Policy)
		sel := c.AccountSelection{AccountID: id, Provider: sp.Provider, Reason: reason, Freshness: mockFreshness(now)}
		if err != nil {
			ce, _ := err.(*c.Error)
			if ce == nil || ce.Code != c.ErrCapacityExhausted {
				return nil, err
			}
			sel.Outcome = c.FallbackWaiting
			sel.Reason = ce.Message
		}
		if a, ok := b.accounts[id]; ok {
			for _, g := range a.Grants {
				if g.NodeID == n.NodeID {
					sel.GrantID = g.GrantID
				}
			}
		}
		return sel, nil
	case c.ActionAccountInjectLimit:
		var lp c.InjectLimitParams
		if err := decodeParams(params, &lp); err != nil {
			return nil, err
		}
		return b.injectLimitLocked(lp)
	case c.ActionMemorySearch:
		var mp c.MemorySearchParams
		if err := decodeParams(params, &mp); err != nil {
			return nil, err
		}
		return b.memorySearch(mp), nil
	case c.ActionMemoryGet:
		var gp struct {
			ID string `json:"id"`
		}
		if err := decodeParams(params, &gp); err != nil {
			return nil, err
		}
		for _, h := range b.memorySearch(c.MemorySearchParams{Limit: 1000}).Hits {
			if h.ID == gp.ID {
				return h, nil
			}
		}
		return nil, c.Errorf(c.ErrNotFound, "memory item %s not found", gp.ID)
	case c.ActionMemoryStatus:
		return c.MemoryStatus{Backend: "mock", Version: "mock", PendingItems: 0, Detail: "mock backend: notes/documents only; no Hive"}, nil
	case c.ActionDirList:
		var dp c.DirListParams
		if err := decodeParams(params, &dp); err != nil {
			return nil, err
		}
		base := dp.Path
		if base == "" {
			base = "/home/synth"
			if n.Platform == "darwin" {
				base = "/Users/synth"
			}
		}
		return c.DirListResult{Path: base, Dirs: append([]string{"scratch", "src", "work"}, b.mockMadeDirs(n.NodeID, base)...), IsGit: strings.Contains(base, "/src/")}, nil
	case c.ActionDiagnostics:
		return c.Diagnostics{NodeID: n.NodeID, Uptime: 3600, Queues: map[string]int{"outbox": 0, "inbox": 0, "reports_pending": 0},
			Limits: map[string]int{"max_runs": n.Resources.MaxRuns}, Resources: n.Resources, Errors: []string{}}, nil
	}
	if v, ok, err := b.fsAction(n, action, params); ok { // W-qlpath
		return v, err
	}
	if v, ok, err := b.repoAction(n, action, params); ok { // W-quicklaunch
		return v, err
	}
	return nil, c.Errorf(c.ErrNotFound, "unknown action %q", action)
}

func (b *Backend) grantAllows(cn *c.Connection, sessionID string) bool {
	if cn.Scope.Access == "none" {
		return false
	}
	if len(cn.Scope.Sessions) > 0 {
		for _, s := range cn.Scope.Sessions {
			if s == sessionID {
				return true
			}
		}
		return false
	}
	if len(cn.Scope.Projects) > 0 {
		s, ok := b.sessions[sessionID]
		if !ok || s.ProjectID == nil {
			return false
		}
		for _, p := range cn.Scope.Projects {
			if p == *s.ProjectID {
				return true
			}
		}
		return false
	}
	return true
}

func (b *Backend) updateAction(n *c.Node, action string, params c.RawJSON) (any, error) {
	var up c.UpdateParams
	if err := decodeParams(params, &up); err != nil {
		return nil, err
	}
	if up.Tool == "" {
		return nil, c.Errorf(c.ErrInvalid, "tool is required")
	}
	tools := clone(b.nodeTools(n.NodeID))
	now := b.now()
	find := func(state string) int {
		for i, t := range tools {
			if t.Tool == up.Tool && t.State == state {
				return i
			}
		}
		return -1
	}
	var out c.ToolVersion
	switch action {
	case c.ActionUpdateStage:
		if up.Version == "" {
			return nil, c.Errorf(c.ErrInvalid, "version is required")
		}
		out = c.ToolVersion{Tool: up.Tool, Version: up.Version, Digest: up.Digest, State: "staged", InstalledAt: now, Health: "unchecked"}
		tools = append(tools, out)
		b.emitFrom("node:"+n.NodeID, c.EvUpdateStaged, ref(c.KindNode, n.NodeID), "", nil, c.UpdateResult{ToolVersion: out, NodeID: n.NodeID, Outcome: "staged", Detail: "mock"})
	case c.ActionUpdateActivate:
		i := find("staged")
		if i < 0 {
			return nil, c.Errorf(c.ErrConflict, "no staged %s version", up.Tool)
		}
		if up.SimulateHealthFailure {
			tools[i].State = "failed"
			tools[i].Health = "failed: simulated health check failure (mock)"
			out = tools[i]
			b.tools[n.NodeID] = tools
			b.emitFrom("node:"+n.NodeID, c.EvUpdateResult, ref(c.KindNode, n.NodeID), "", nil, c.UpdateResult{ToolVersion: out, NodeID: n.NodeID, Outcome: "failed", RolledBack: true, Detail: "simulated health check failure (mock)"})
			b.raiseAttentionLocked(c.AttnUpdate, "normal", "update:"+up.Tool+":"+n.NodeID, ref(c.KindNode, n.NodeID), "", nil,
				up.Tool+" "+out.Version+" failed health check; previous version kept", "mock")
			return nil, c.Errorf(c.ErrConflict, "health check failed for %s %s; active version unchanged", up.Tool, out.Version)
		}
		if j := find("active"); j >= 0 {
			tools[j].State = "previous"
		}
		tools[i].State, tools[i].Health = "active", "ok"
		out = tools[i]
		b.emitFrom("node:"+n.NodeID, c.EvUpdateResult, ref(c.KindNode, n.NodeID), "", nil, c.UpdateResult{ToolVersion: out, NodeID: n.NodeID, Outcome: "activated", Detail: "mock"})
	case c.ActionUpdateRollback:
		j := find("previous")
		if j < 0 {
			return nil, c.Errorf(c.ErrConflict, "no previous %s version to roll back to", up.Tool)
		}
		if i := find("active"); i >= 0 {
			tools[i].State = "previous"
		}
		tools[j].State = "active"
		out = tools[j]
		b.emitFrom("node:"+n.NodeID, c.EvUpdateResult, ref(c.KindNode, n.NodeID), "", nil, c.UpdateResult{ToolVersion: out, NodeID: n.NodeID, Outcome: "rolled_back", RolledBack: true, Detail: "mock"})
	}
	b.tools[n.NodeID] = tools
	return out, nil
}

func (b *Backend) memorySearch(mp c.MemorySearchParams) c.MemorySearchResult {
	q := strings.ToLower(mp.Query)
	limit := mp.Limit
	if limit <= 0 || limit > 1000 {
		limit = 20
	}
	var hits []c.MemoryHit
	for _, n := range b.notes {
		if mp.ProjectID != "" && (n.ProjectID == nil || *n.ProjectID != mp.ProjectID) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(n.Title+"\n"+n.Body), q) {
			continue
		}
		pid := ""
		if n.ProjectID != nil {
			pid = *n.ProjectID
		}
		hits = append(hits, c.MemoryHit{ID: n.NoteID, Kind: string(n.Kind), Title: n.Title, Snippet: truncate(n.Body, 200), Source: "mock:note", SessionRef: n.SessionID, ProjectID: pid, At: n.CreatedAt, Authoritative: false})
	}
	for _, d := range b.documents {
		if mp.ProjectID != "" && (d.ProjectID == nil || *d.ProjectID != mp.ProjectID) {
			continue
		}
		r := b.revisions[d.HeadRevisionID]
		if r == nil || (q != "" && !strings.Contains(strings.ToLower(d.Title+"\n"+r.Content), q)) {
			continue
		}
		pid := ""
		if d.ProjectID != nil {
			pid = *d.ProjectID
		}
		hits = append(hits, c.MemoryHit{ID: r.RevisionID, Kind: "document", Title: d.Title, Snippet: truncate(r.Content, 200), Source: "mock:document", ProjectID: pid, At: r.CreatedAt, Authoritative: true})
	}
	sort.Slice(hits, func(i, j int) bool {
		if !hits[i].At.Equal(hits[j].At) {
			return hits[i].At.After(hits[j].At)
		}
		return hits[i].ID < hits[j].ID
	})
	res := c.MemorySearchResult{Hits: []c.MemoryHit{}, Freshness: c.Freshness{ObservedAt: b.now(), Source: "mock"}}
	if len(hits) > limit {
		hits, res.Truncated = hits[:limit], true
	}
	res.Hits = append(res.Hits, hits...)
	return res
}

// injectLimitLocked simulates a usage-limit/auth failure on a session's run
// and plans a same-provider fallback (never crossing providers).
func (b *Backend) injectLimitLocked(lp c.InjectLimitParams) (c.FallbackPlan, error) {
	s, ok := b.sessions[lp.SessionID]
	if !ok {
		return c.FallbackPlan{}, notFound(c.KindSession, lp.SessionID)
	}
	r := b.runs[s.CurrentRunID]
	if r == nil {
		return c.FallbackPlan{}, c.Errorf(c.ErrConflict, "session has no run")
	}
	now := b.now()
	plan := c.FallbackPlan{SessionID: s.SessionID, FromRunID: r.RunID, FromAccountID: r.AccountID, PlannedAt: now, UncertainWork: r.State == c.RunRunning}
	if a, ok := b.accounts[r.AccountID]; ok {
		el := c.EligibleExhausted
		if lp.Kind == "auth_invalid" {
			el = c.EligibleUnusable
		}
		a.Latest = &c.AccountObservation{AccountID: a.Account.AccountID, Provider: a.Account.Provider, ReportingNode: r.NodeID, ObservedAt: now, Source: "mock:limit-event", Confidence: "high", Eligibility: el, Detail: "injected " + lp.Kind}
		b.emit(c.EvAccountObserved, ref(c.KindAccount, a.Account.AccountID), s.SessionID, s.ProjectID, a.Latest)
	}
	to, reason, err := b.chooseAccount(s.Harness, c.AccountPolicy{Mode: c.AccountEligible})
	switch {
	case err != nil || to == "" || to == r.AccountID:
		plan.Outcome = c.FallbackWaiting
		plan.Reason = "no other eligible " + string(c.ProviderFor(s.Harness)) + " subscription; work preserved"
		b.raiseAttentionLocked(c.AttnAccountPool, "urgent", "pool:"+string(c.ProviderFor(s.Harness)), ref(c.KindSession, s.SessionID), s.SessionID, s.ProjectID,
			"No eligible "+string(c.ProviderFor(s.Harness))+" account", "Session paused and resumable (mock).")
	case c.HasCap(r.Capabilities, c.CapAccountCrossResume) || s.Harness == c.HarnessClaude || s.Harness == c.HarnessMock:
		plan.Outcome, plan.ToAccountID, plan.Reason = c.FallbackNativeResume, to, "usage limit; "+reason
	default:
		plan.Outcome, plan.ToAccountID, plan.Reason = c.FallbackHandoff, to, "cross-account native resume unproven; handoff successor planned ("+reason+")"
	}
	plan.Provider = c.ProviderFor(s.Harness)
	b.fallbacks = append(b.fallbacks, plan)
	if plan.Outcome == c.FallbackWaiting {
		b.emit(c.EvAccountExhausted, ref(c.KindSession, s.SessionID), s.SessionID, s.ProjectID, plan)
	}
	b.emit(c.EvFallbackPlanned, ref(c.KindSession, s.SessionID), s.SessionID, s.ProjectID, plan)
	return plan, nil
}

func (b *Backend) ListConnections(ctx context.Context, req c.ListConnectionsRequest) (c.Page[c.Connection], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.Connection], error) {
		var out []c.Connection
		for _, id := range sortedKeys(b.connections) {
			cn := b.connections[id]
			if req.NodeID != "" {
				found := len(cn.Scope.Nodes) == 0
				for _, nid := range cn.Scope.Nodes {
					if nid == req.NodeID {
						found = true
					}
				}
				for _, av := range cn.Availability {
					if av.NodeID == req.NodeID {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			if req.ProjectID != "" && len(cn.Scope.Projects) > 0 {
				found := false
				for _, pid := range cn.Scope.Projects {
					if pid == req.ProjectID {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			out = append(out, *cn)
		}
		return paginate(out, req.PageRequest, b.now())
	})
}
