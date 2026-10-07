package connections

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jhh3/pyre/integrations/outfit"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
)

// Handler is the node action handler signature the daemon worker registers.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// OutfitClient is the subset of *outfit.Adapter the node service uses.
type OutfitClient interface {
	Connections(ctx context.Context) ([]outfit.Connection, error)
	Check(ctx context.Context, name string) (outfit.Check, error)
	Invoke(ctx context.Context, req outfit.InvokeRequest) (outfit.InvokeResult, error)
}

// NodeConfig wires a NodeService.
type NodeConfig struct {
	NodeID string
	// Outfit is the local Outfit adapter, or nil when Outfit is absent.
	Outfit OutfitClient
	// Stores and Index hold Pyre-provisioned credentials on this node.
	Stores credentials.Stores
	Index  *credentials.Index
	// DefaultBackend is the store backend for new provisions (e.g. "keychain").
	DefaultBackend string
	// Grants is the authoritative use-time grant check (asks the hub).
	Grants GrantChecker
	Clock  contracts.Clock
	// Logger receives metadata-only audit lines (never values or outputs).
	Logger *slog.Logger
	// MaxOutput bounds connection.invoke output (default 256 KiB).
	MaxOutput int
	// OnAvailabilityChange, when set, is called (without locks held) after a
	// connection.check stored a result or a credential was provisioned or
	// deprovisioned, so the node can push a fresh connections report instead
	// of waiting for the periodic one.
	OnAvailabilityChange func()
}

// NodeService implements the connection.* node actions.
type NodeService struct {
	cfg       NodeConfig
	recipient *credentials.Recipient

	mu     sync.Mutex
	mirror map[grantKey]bool // hub-pushed grants: true=active, false=revoked
	// checks holds the last connection.check result per connection and
	// mode. Outfit builds without `inventory` report every connection as
	// "unknown"; the report overlays these so the hub catalog shows what a
	// check actually verified.
	checks map[string]contracts.ConnectionAvailability
}

type grantKey struct{ conn, node, project, session string }

// NewNodeService validates cfg and returns the service.
func NewNodeService(cfg NodeConfig) (*NodeService, error) {
	if !contracts.ValidID(cfg.NodeID, contracts.PrefixNode) {
		return nil, contracts.Errorf(contracts.ErrInvalid, "invalid node id %q", cfg.NodeID)
	}
	if cfg.Grants == nil {
		return nil, contracts.Errorf(contracts.ErrInvalid, "a GrantChecker is required: every connection use re-checks the current grant")
	}
	if cfg.Clock == nil {
		cfg.Clock = contracts.SystemClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 256 << 10
	}
	if cfg.Stores == nil {
		cfg.Stores = credentials.Stores{}
	}
	if cfg.Index == nil {
		cfg.Index = credentials.OpenIndex("")
	}
	return &NodeService{cfg: cfg, recipient: credentials.NewRecipient(cfg.NodeID, cfg.Clock), mirror: map[grantKey]bool{},
		checks: map[string]contracts.ConnectionAvailability{}}, nil
}

// Handlers returns the frozen v1 connection actions keyed by action name.
func (s *NodeService) Handlers() map[string]Handler {
	return map[string]Handler{
		contracts.ActionConnCheck:  s.HandleCheck,
		contracts.ActionConnInvoke: s.HandleInvoke,
		contracts.ActionConnGrant:  s.HandleGrant,
		contracts.ActionConnRevoke: s.HandleRevoke,
		contracts.ActionConnList:   s.HandleList,
	}
}

func decode(params json.RawMessage, v any) error {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	if err := json.Unmarshal(params, v); err != nil {
		return contracts.Errorf(contracts.ErrInvalid, "invalid params: %v", err)
	}
	return nil
}

// toContract converts any error into a wire error whose text is redacted.
func toContract(err error) error {
	if err == nil {
		return nil
	}
	var se *credentials.StoreError
	if errors.As(err, &se) {
		return se.Contract()
	}
	ce := contracts.AsError(err)
	out := *ce
	out.Message = credentials.Diagnostic(nil, []byte(ce.Message))
	return &out
}

// localView merges Outfit connections and Pyre provisions for this node.
func (s *NodeService) localView(ctx context.Context) ([]contracts.Connection, error) {
	now := s.cfg.Clock.Now()
	byID := map[string]*contracts.Connection{}
	var order []string
	add := func(c contracts.Connection) {
		if cur, ok := byID[c.ConnectionID]; ok {
			cur.Modes = sortedUnique(append(cur.Modes, c.Modes...))
			cur.Availability = append(cur.Availability, c.Availability...)
			return
		}
		cc := c
		byID[c.ConnectionID] = &cc
		order = append(order, c.ConnectionID)
	}
	if s.cfg.Outfit != nil {
		list, err := s.cfg.Outfit.Connections(ctx)
		switch {
		case outfit.IsNotInstalled(err):
			// No Outfit on this node: report Pyre-provisioned credentials
			// alone. An installed Outfit that fails still fails the report,
			// so the hub keeps the last good view instead of dropping it.
		case err != nil:
			return nil, err
		}
		for _, oc := range list {
			add(oc.Contract(s.cfg.NodeID, now))
		}
	}
	provs, err := s.cfg.Index.List()
	if err != nil {
		return nil, err
	}
	for _, p := range provs {
		name, _ := outfit.NameForConnectionID(p.ConnectionID)
		add(contracts.Connection{ConnectionID: p.ConnectionID, Name: name, Modes: []string{outfit.ModeLocalCredential},
			Availability: []contracts.ConnectionAvailability{{NodeID: s.cfg.NodeID, Mode: outfit.ModeLocalCredential, State: "unknown", VerifiedAt: p.ProvisionedAt, Detail: "provisioned by Pyre as $" + p.EnvVar + "; run connection.check to verify"}}})
	}
	sort.Strings(order)
	out := make([]contracts.Connection, 0, len(order))
	for _, id := range order {
		c := *byID[id]
		c.Availability = s.overlayChecks(id, c.Availability)
		c.Scope = s.mirrorScope(id)
		out = append(out, c)
	}
	return out, nil
}

func checkKey(connID, mode string) string { return connID + "|" + mode }

// overlayChecks replaces reported availability with the last
// connection.check result for the same mode when the report knows less
// ("unknown") or is older than the check.
func (s *NodeService) overlayChecks(connID string, avs []contracts.ConnectionAvailability) []contracts.ConnectionAvailability {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]contracts.ConnectionAvailability, len(avs))
	for i, a := range avs {
		out[i] = a
		if ck, ok := s.checks[checkKey(connID, a.Mode)]; ok && (a.State == "unknown" || ck.VerifiedAt.After(a.VerifiedAt)) {
			out[i] = ck
		}
	}
	return out
}

// recordCheck stores a check result and notifies OnAvailabilityChange.
func (s *NodeService) recordCheck(connID string, av contracts.ConnectionAvailability) {
	if av.Mode == "" {
		return
	}
	if av.NodeID == "" {
		av.NodeID = s.cfg.NodeID
	}
	if av.State == "verified" && av.Detail == "" {
		av.Detail = "verified by connection.check"
	}
	s.mu.Lock()
	s.checks[checkKey(connID, av.Mode)] = av
	s.mu.Unlock()
	s.availabilityChanged()
}

// availabilityChanged asks the node to push a fresh connections report
// (after a check, a provision or a deprovision) instead of waiting for the
// periodic one.
func (s *NodeService) availabilityChanged() {
	if s.cfg.OnAvailabilityChange != nil {
		s.cfg.OnAvailabilityChange()
	}
}

// Report returns this node's snapshot for a ReportConnections report.
func (s *NodeService) Report(ctx context.Context) ([]contracts.Connection, error) {
	out, err := s.localView(ctx)
	return out, toContract(err)
}

// HandleList implements connection.list: {} -> []Connection (this node).
func (s *NodeService) HandleList(ctx context.Context, _ json.RawMessage) (any, error) {
	return s.Report(ctx)
}

// HandleCheck implements connection.check: ConnectionParams ->
// ConnectionAvailability for this node. Pyre-provisioned credentials are
// probed by reading the store (the value is discarded immediately); Outfit
// connections are re-verified by Outfit.
func (s *NodeService) HandleCheck(ctx context.Context, params json.RawMessage) (any, error) {
	var p contracts.ConnectionParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if !contracts.ValidID(p.ConnectionID, contracts.PrefixConnection) {
		return nil, contracts.Errorf(contracts.ErrInvalid, "invalid connection_id %q", p.ConnectionID)
	}
	av, err := s.Check(ctx, p.ConnectionID)
	if err == nil {
		s.recordCheck(p.ConnectionID, av)
	}
	return av, toContract(err)
}

// Check probes one connection on this node.
func (s *NodeService) Check(ctx context.Context, connectionID string) (contracts.ConnectionAvailability, error) {
	now := s.cfg.Clock.Now()
	var local *contracts.ConnectionAvailability
	provs, err := s.cfg.Index.ForConnection(connectionID)
	if err != nil {
		return contracts.ConnectionAvailability{}, err
	}
	if len(provs) > 0 {
		av := contracts.ConnectionAvailability{NodeID: s.cfg.NodeID, Mode: outfit.ModeLocalCredential, State: "verified", VerifiedAt: now}
		for _, p := range provs {
			if st := s.probe(ctx, p); st.State != "verified" {
				av = st
				break
			}
		}
		if av.State == "verified" {
			return av, nil
		}
		local = &av
	}
	if s.cfg.Outfit != nil {
		if name, ok := outfit.NameForConnectionID(connectionID); ok && outfit.ValidName(name) {
			c, err := s.cfg.Outfit.Check(ctx, name)
			if err == nil && (c.State == "verified" || local == nil) {
				av := contracts.ConnectionAvailability{NodeID: s.cfg.NodeID, Mode: outfit.ModeRemoteInvoke, State: c.State, VerifiedAt: c.CheckedAt, Detail: credentials.Diagnostic(nil, []byte(c.Detail))}
				return av, nil
			}
			if err != nil && local == nil {
				return contracts.ConnectionAvailability{NodeID: s.cfg.NodeID, Mode: outfit.ModeRemoteInvoke, State: "unknown", VerifiedAt: now, Detail: credentials.Diagnostic(nil, []byte(contracts.AsError(err).Message))}, nil
			}
		}
	}
	if local != nil {
		return *local, nil
	}
	return contracts.ConnectionAvailability{NodeID: s.cfg.NodeID, State: "missing", VerifiedAt: now, Detail: "neither Outfit nor Pyre holds this connection on this node"}, nil
}

func (s *NodeService) probe(ctx context.Context, p credentials.Provision) contracts.ConnectionAvailability {
	av := contracts.ConnectionAvailability{NodeID: s.cfg.NodeID, Mode: outfit.ModeLocalCredential, VerifiedAt: s.cfg.Clock.Now()}
	ref, err := p.ParsedRef()
	if err == nil {
		var st credentials.Store
		if st, err = s.cfg.Stores.For(ref); err == nil {
			var v credentials.Secret
			v, err = st.Get(ctx, ref)
			v.Wipe()
		}
	}
	av.State = credentials.AvailabilityState(err)
	if err != nil {
		av.Detail = credentials.Bounded("$"+p.EnvVar+": "+err.Error(), 300)
	}
	return av
}

func (s *NodeService) mirrorScope(connID string) contracts.ConnectionScope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var grants []GrantRecord
	for k, active := range s.mirror {
		if k.conn == connID && active {
			grants = append(grants, GrantRecord{NodeID: k.node, ProjectID: k.project, SessionID: k.session})
		}
	}
	return scopeOf(grants)
}

// setMirror records a hub-pushed grant or revocation. A pushed grant clears
// the connection's local revocation entries: after a deliberate owner grant,
// the authoritative hub check governs again.
func (s *NodeService) setMirror(p contracts.ConnectionGrantParams, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active {
		for k, v := range s.mirror {
			if k.conn == p.ConnectionID && !v {
				delete(s.mirror, k)
			}
		}
	}
	s.mirror[grantKey{p.ConnectionID, p.NodeID, p.ProjectID, p.SessionID}] = active
}

// locallyRevoked reports a hub-pushed revocation that matches the session
// exactly or is connection-wide. It is a fast local deny that does not
// depend on the hub being reachable; the authoritative check still runs.
func (s *NodeService) locallyRevoked(connID, sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, active := range s.mirror {
		if k.conn != connID || active {
			continue
		}
		if k.session == sessionID || (k.session == "" && k.project == "" && k.node == "") {
			return true
		}
	}
	return false
}

func (s *NodeService) connectionView(ctx context.Context, id string) contracts.Connection {
	if list, err := s.localView(ctx); err == nil {
		for _, c := range list {
			if c.ConnectionID == id {
				return c
			}
		}
	}
	name, _ := outfit.NameForConnectionID(id)
	return contracts.Connection{ConnectionID: id, Name: name, Modes: []string{}, Availability: []contracts.ConnectionAvailability{}, Scope: s.mirrorScope(id)}
}

// HandleGrant implements connection.grant on the node: the hub informs the
// node of a grant it recorded. Returns this node's view of the connection.
func (s *NodeService) HandleGrant(ctx context.Context, params json.RawMessage) (any, error) {
	var p contracts.ConnectionGrantParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := validateGrantParams(p); err != nil {
		return nil, err
	}
	s.setMirror(p, true)
	s.cfg.Logger.Info("connection.grant", "connection", p.ConnectionID, "node", p.NodeID, "project", p.ProjectID, "session", p.SessionID)
	return s.connectionView(ctx, p.ConnectionID), nil
}

// HandleRevoke implements connection.revoke on the node: an immediate local
// deny for subsequent invocations, independent of hub reachability. It does
// not delete provisioned credentials (see Deprovision) and cannot remove an
// environment variable from an already-running process.
func (s *NodeService) HandleRevoke(ctx context.Context, params json.RawMessage) (any, error) {
	var p contracts.ConnectionGrantParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := validateGrantParams(p); err != nil {
		return nil, err
	}
	s.setMirror(p, false)
	s.cfg.Logger.Info("connection.revoke", "connection", p.ConnectionID, "node", p.NodeID, "project", p.ProjectID, "session", p.SessionID)
	return s.connectionView(ctx, p.ConnectionID), nil
}

// authorize re-checks the session's CURRENT grant. Any failure denies.
func (s *NodeService) authorize(ctx context.Context, connID, sessionID string) (GrantDecision, error) {
	if s.locallyRevoked(connID, sessionID) {
		return GrantDecision{}, contracts.Errorf(contracts.ErrForbidden, "connection %s was revoked for this session", connID).WithRef(contracts.KindConnection, connID)
	}
	d, err := s.cfg.Grants.CheckGrant(ctx, connID, sessionID)
	if err != nil {
		return d, contracts.Errorf(contracts.ErrUnavailable, "cannot verify the current grant for %s (refusing): %s", connID, credentials.Diagnostic(nil, []byte(contracts.AsError(err).Message))).WithRef(contracts.KindConnection, connID)
	}
	if !d.Allowed || d.ConnectionID != connID || d.SessionID != sessionID {
		reason := d.Reason
		if reason == "" {
			reason = "not granted"
		}
		return d, contracts.Errorf(contracts.ErrForbidden, "session %s may not use %s: %s", sessionID, connID, reason).WithRef(contracts.KindConnection, connID)
	}
	return d, nil
}

// HandleInvoke implements connection.invoke: ConnectionInvokeParams ->
// ConnectionInvokeResult. It re-checks the session's current grant, executes
// the read operation through Outfit on this node, and returns bounded output
// that never contains the provider credential.
func (s *NodeService) HandleInvoke(ctx context.Context, params json.RawMessage) (any, error) {
	var p contracts.ConnectionInvokeParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	res, err := s.Invoke(ctx, p)
	if err != nil {
		return nil, toContract(err)
	}
	return res, nil
}

// Invoke is HandleInvoke with typed parameters.
func (s *NodeService) Invoke(ctx context.Context, p contracts.ConnectionInvokeParams) (contracts.ConnectionInvokeResult, error) {
	if !contracts.ValidID(p.ConnectionID, contracts.PrefixConnection) || !contracts.ValidID(p.SessionID, contracts.PrefixSession) || p.Operation == "" {
		return contracts.ConnectionInvokeResult{}, contracts.Errorf(contracts.ErrInvalid, "connection.invoke needs connection_id, session_id, and operation")
	}
	start := s.cfg.Clock.Now()
	d, err := s.authorize(ctx, p.ConnectionID, p.SessionID)
	if err != nil {
		s.cfg.Logger.Warn("connection.invoke denied", "connection", p.ConnectionID, "session", p.SessionID, "operation", p.Operation, "code", contracts.CodeOf(err))
		return contracts.ConnectionInvokeResult{}, err
	}
	if s.cfg.Outfit == nil {
		return contracts.ConnectionInvokeResult{}, contracts.Errorf(contracts.ErrUnavailable, "outfit is not installed on node %s", s.cfg.NodeID)
	}
	name, ok := outfit.NameForConnectionID(p.ConnectionID)
	if !ok || !outfit.ValidName(name) {
		return contracts.ConnectionInvokeResult{}, contracts.Errorf(contracts.ErrInvalid, "connection %s does not map to an outfit connection name", p.ConnectionID)
	}
	res, err := s.cfg.Outfit.Invoke(ctx, outfit.InvokeRequest{Connection: name, Operation: p.Operation, Args: p.Args})
	if err != nil {
		s.cfg.Logger.Warn("connection.invoke failed", "connection", p.ConnectionID, "session", p.SessionID, "operation", p.Operation, "code", contracts.CodeOf(err))
		return contracts.ConnectionInvokeResult{}, err
	}
	if e := res.Err(); e != nil {
		s.cfg.Logger.Warn("connection.invoke failed", "connection", p.ConnectionID, "session", p.SessionID, "operation", p.Operation, "code", e.Code)
		return contracts.ConnectionInvokeResult{}, e
	}
	out := contracts.ConnectionInvokeResult{ConnectionID: p.ConnectionID, Output: res.Envelope, Truncated: res.Truncated}
	if len(out.Output) > s.cfg.MaxOutput {
		marker, _ := json.Marshal(map[string]any{"truncated": true, "bytes": len(out.Output), "detail": "result exceeded the connection.invoke output bound; narrow the request"})
		out.Output, out.Truncated = marker, true
	}
	s.cfg.Logger.Info("connection.invoke", "connection", p.ConnectionID, "session", p.SessionID, "operation", p.Operation, "grant", d.GrantID, "generation", d.Generation, "bytes", len(out.Output), "truncated", out.Truncated, "ms", s.cfg.Clock.Now().Sub(start).Milliseconds())
	return out, nil
}

// DeniedConnection explains why a granted-at-launch connection was omitted.
type DeniedConnection struct {
	ConnectionID string `json:"connection_id"`
	Reason       string `json:"reason"`
}

// LaunchPlan is the connection part of a run launch on this node.
type LaunchPlan struct {
	// Env holds provisioned values for ResolvedLaunch.Env; never serialized.
	Env map[string]string `json:"-"`
	// Manifest lists provisioned variable names and refs only.
	Manifest credentials.Manifest `json:"manifest"`
	// Remote lists granted connections the session reaches via connection.invoke.
	Remote []string `json:"remote,omitempty"`
	// Denied lists requested connections without a current grant.
	Denied []DeniedConnection `json:"denied,omitempty"`
}

// PrepareLaunch resolves the environment-generation connection grants for a
// session about to launch on this node. Each connection's grant is re-checked
// now; denied connections are omitted (and listed). Locally provisioned
// credentials are read from the node's store into Env; a locked or missing
// store fails the launch with an actionable error rather than starting the
// session without a credential it was promised.
func (s *NodeService) PrepareLaunch(ctx context.Context, sessionID string, connectionIDs []string) (LaunchPlan, error) {
	var plan LaunchPlan
	var local []string
	for _, id := range sortedUnique(connectionIDs) {
		if _, err := s.authorize(ctx, id, sessionID); err != nil {
			plan.Denied = append(plan.Denied, DeniedConnection{ConnectionID: id, Reason: contracts.AsError(err).Message})
			continue
		}
		local = append(local, id)
	}
	grants, remote, err := credentials.GrantsFor(s.cfg.Index, local)
	if err != nil {
		return LaunchPlan{}, err
	}
	plan.Remote = remote
	env, manifest, err := (&credentials.Resolver{Stores: s.cfg.Stores}).Resolve(ctx, grants)
	if err != nil {
		s.cfg.Logger.Warn("connection launch provisioning failed", "session", sessionID, "code", contracts.CodeOf(toContract(err)))
		var se *credentials.StoreError
		if errors.As(err, &se) {
			ce := se.Contract()
			ce.Message = err.Error()
			return LaunchPlan{}, ce
		}
		return LaunchPlan{}, contracts.Errorf(contracts.ErrInvalid, "%s", err.Error())
	}
	plan.Env, plan.Manifest = env, manifest
	s.cfg.Logger.Info("connection launch provisioning", "session", sessionID, "env", manifest.EnvNames(), "remote", remote, "denied", len(plan.Denied))
	return plan, nil
}

// ---- explicit cross-node provisioning (proposed actions) ----

// Proposed action names (not yet in contracts/actions.go).
const (
	ActionConnProvisionOffer  = "connection.provision_offer"  // ProvisionOfferParams -> credentials.ProvisionOffer (recipient node)
	ActionConnProvisionSeal   = "connection.provision_seal"   // ProvisionSealParams -> credentials.SealedCredential (source node)
	ActionConnProvisionAccept = "connection.provision_accept" // credentials.SealedCredential -> credentials.Provision (recipient node)
	ActionConnDeprovision     = "connection.deprovision"      // ConnectionParams -> []credentials.Provision removed
)

// ProvisionOfferParams asks a recipient node to prepare for a credential.
type ProvisionOfferParams struct {
	ConnectionID string `json:"connection_id"`
	EnvVar       string `json:"env_var"`
	Backend      string `json:"backend,omitempty"` // default NodeConfig.DefaultBackend
	TTLSeconds   int    `json:"ttl_seconds,omitempty"`
}

// ProvisionSealParams asks the source node to seal a value it holds.
type ProvisionSealParams struct {
	Offer     credentials.ProvisionOffer `json:"offer"`
	SourceRef string                     `json:"source_ref"` // Ref.String() in the source node's store
}

// ProvisioningHandlers returns the proposed provisioning actions. The daemon
// should register them only once the action names are accepted into the
// contract; they are owner-initiated and never carry plaintext.
func (s *NodeService) ProvisioningHandlers() map[string]Handler {
	return map[string]Handler{
		ActionConnProvisionOffer:  s.HandleProvisionOffer,
		ActionConnProvisionSeal:   s.HandleProvisionSeal,
		ActionConnProvisionAccept: s.HandleProvisionAccept,
		ActionConnDeprovision:     s.HandleDeprovision,
	}
}

// HandleProvisionOffer runs on the recipient node.
func (s *NodeService) HandleProvisionOffer(_ context.Context, params json.RawMessage) (any, error) {
	var p ProvisionOfferParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if !contracts.ValidID(p.ConnectionID, contracts.PrefixConnection) {
		return nil, contracts.Errorf(contracts.ErrInvalid, "invalid connection_id %q", p.ConnectionID)
	}
	backend := p.Backend
	if backend == "" {
		backend = s.cfg.DefaultBackend
	}
	ref := credentials.NewRef(backend, p.ConnectionID, p.EnvVar)
	if _, err := s.cfg.Stores.For(ref); err != nil {
		return nil, toContract(err)
	}
	offer, err := s.recipient.NewOffer(p.ConnectionID, p.EnvVar, ref, time.Duration(p.TTLSeconds)*time.Second)
	if err != nil {
		return nil, contracts.Errorf(contracts.ErrInvalid, "%s", err.Error())
	}
	s.cfg.Logger.Info("connection.provision_offer", "connection", p.ConnectionID, "env", p.EnvVar, "ref", offer.Ref, "offer", offer.OfferID)
	return offer, nil
}

// HandleProvisionSeal runs on the node that holds the value.
func (s *NodeService) HandleProvisionSeal(ctx context.Context, params json.RawMessage) (any, error) {
	var p ProvisionSealParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	ref, err := credentials.ParseRef(p.SourceRef)
	if err != nil {
		return nil, contracts.Errorf(contracts.ErrInvalid, "%s", err.Error())
	}
	st, err := s.cfg.Stores.For(ref)
	if err != nil {
		return nil, toContract(err)
	}
	v, err := st.Get(ctx, ref)
	if err != nil {
		return nil, toContract(err)
	}
	defer v.Wipe()
	sealed, err := credentials.Seal(p.Offer, s.cfg.NodeID, v, s.cfg.Clock.Now())
	if err != nil {
		return nil, toContract(err)
	}
	s.cfg.Logger.Info("connection.provision_seal", "connection", p.Offer.ConnectionID, "offer", p.Offer.OfferID, "recipient", p.Offer.RecipientNode, "source_ref", p.SourceRef)
	return sealed, nil
}

// HandleProvisionAccept runs on the recipient node.
func (s *NodeService) HandleProvisionAccept(ctx context.Context, params json.RawMessage) (any, error) {
	var sealed credentials.SealedCredential
	if err := decode(params, &sealed); err != nil {
		return nil, err
	}
	prov, err := s.recipient.Accept(ctx, sealed, s.cfg.Stores, s.cfg.Index)
	if err != nil {
		return nil, toContract(err)
	}
	s.cfg.Logger.Info("connection.provision_accept", "connection", prov.ConnectionID, "env", prov.EnvVar, "ref", prov.Ref, "source", prov.SourceNode)
	s.availabilityChanged()
	return prov, nil
}

// HandleDeprovision deletes this node's Pyre-provisioned values for a
// connection and their index entries.
func (s *NodeService) HandleDeprovision(ctx context.Context, params json.RawMessage) (any, error) {
	var p contracts.ConnectionParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	removed, err := s.Deprovision(ctx, p.ConnectionID)
	return removed, toContract(err)
}

// Deprovision removes local provisions for connectionID: store values first,
// then index entries, so a store failure never orphans a value.
func (s *NodeService) Deprovision(ctx context.Context, connectionID string) ([]credentials.Provision, error) {
	provs, err := s.cfg.Index.ForConnection(connectionID)
	if err != nil {
		return nil, err
	}
	for _, p := range provs {
		ref, err := p.ParsedRef()
		if err != nil {
			continue
		}
		st, err := s.cfg.Stores.For(ref)
		if err != nil {
			return nil, err
		}
		if err := st.Delete(ctx, ref); err != nil {
			return nil, err
		}
	}
	removed, err := s.cfg.Index.Remove(connectionID, "")
	if err != nil {
		return nil, err
	}
	s.cfg.Logger.Info("connection.deprovision", "connection", connectionID, "removed", len(removed))
	if len(removed) > 0 {
		s.availabilityChanged()
	}
	if removed == nil {
		removed = []credentials.Provision{}
	}
	return removed, nil
}
