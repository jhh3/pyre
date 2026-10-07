package outfit

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Status is the detection result for this node.
type Status struct {
	Present  bool   `json:"present"`
	Path     string `json:"path,omitempty"`
	Version  string `json:"version,omitempty"`
	Revision string `json:"revision,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
	Health   string `json:"health"`
}

// Integration converts to the inventory contract shape.
func (s Status) Integration() contracts.IntegrationStatus {
	return contracts.IntegrationStatus{Present: s.Present, Version: s.Version, Path: s.Path, Revision: s.Revision, Dirty: s.Dirty, Health: s.Health}
}

// Detect finds Outfit and its version. It runs only `outfit version`, which
// does not open Outfit state. It never returns an error: absence and failures
// are reported in Status.Health.
func (a *Adapter) Detect(ctx context.Context) Status {
	path, err := a.binary()
	if err != nil {
		return Status{Health: "absent: outfit not found on PATH"}
	}
	st := Status{Present: true, Path: path}
	res, err := a.run(ctx, minDur(a.opt.Timeout, 5*time.Second), nil, nil, "version")
	if err != nil {
		st.Health = "failed: " + contracts.AsError(err).Message
		return st
	}
	if res.exit != 0 {
		st.Health = "failed: outfit version exited " + itoa(res.exit) + ": " + diag(res.stderr)
		return st
	}
	st.Version, st.Revision = parseVersion(string(res.stdout))
	st.Dirty = strings.Contains(st.Revision, "+working") || strings.Contains(st.Revision, "dirty")
	if st.Version == "" {
		st.Health = "failed: unrecognized version output"
		return st
	}
	st.Health = "ok"
	return st
}

// parseVersion parses "outfit 0.10.7+fix (c8a3dd7+working, 2026-09-21T18:33:02Z)".
func parseVersion(out string) (version, revision string) {
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	rest, ok := strings.CutPrefix(line, "outfit ")
	if !ok {
		return "", ""
	}
	version, paren, _ := strings.Cut(rest, " (")
	if paren != "" {
		revision, _, _ = strings.Cut(strings.TrimSuffix(paren, ")"), ",")
	}
	return strings.TrimSpace(version), strings.TrimSpace(revision)
}

// Connection is non-secret Outfit connection metadata for this node.
type Connection struct {
	ID               string     `json:"id,omitempty"` // Outfit-local ID (empty from `ls`)
	Name             string     `json:"name"`
	Connector        string     `json:"connector"`
	Display          string     `json:"display,omitempty"`
	Mode             string     `json:"mode"`
	Executor         string     `json:"executor,omitempty"`
	CredentialMethod string     `json:"credential_method,omitempty"`
	Custody          string     `json:"custody,omitempty"` // "outfit" | "oauth" | "executor" | "none"
	Status           string     `json:"status"`
	CredentialStatus string     `json:"credential_status,omitempty"`
	LastVerifiedAt   *time.Time `json:"last_verified_at,omitempty"`
	Environment      []string   `json:"environment"` // declared env var NAMES
	Operations       int        `json:"operations"`
}

// ConnectionIDFor is the fleet-wide Pyre connection ID for an Outfit
// connection name. It is deterministic so every node and the hub agree on the
// same logical connection without coordination. ('_' is not allowed in ID
// bodies, and ':' never appears in Outfit names, so the mapping is 1:1.)
func ConnectionIDFor(name string) string {
	return contracts.PrefixConnection + "_" + strings.ReplaceAll(name, "_", ":")
}

// NameForConnectionID inverts ConnectionIDFor.
func NameForConnectionID(id string) (string, bool) {
	body, ok := strings.CutPrefix(id, contracts.PrefixConnection+"_")
	if !ok || body == "" {
		return "", false
	}
	return strings.ReplaceAll(body, ":", "_"), true
}

// Contract renders the connection as hub catalog metadata observed on nodeID.
// Outfit-held connections are used through Outfit on this node, so their mode
// is "remote_invoke" (Outfit executes; the credential never leaves Outfit).
func (c Connection) Contract(nodeID string, observedAt time.Time) contracts.Connection {
	av := contracts.ConnectionAvailability{NodeID: nodeID, Mode: ModeRemoteInvoke, VerifiedAt: observedAt.UTC()}
	switch st := strings.ToLower(c.Status); {
	case st == "active" && c.LastVerifiedAt != nil:
		av.State, av.VerifiedAt, av.Detail = "verified", c.LastVerifiedAt.UTC(), "outfit last verified this connection"
	case st == "active":
		av.State, av.Detail = "unknown", "active in outfit; run connection.check to verify"
	case strings.Contains(st, "expired") || strings.Contains(st, "reauth") || strings.Contains(st, "repair"):
		av.State, av.Detail = "expired", "outfit status: "+c.Status
	default:
		av.State, av.Detail = "unknown", "outfit status: "+c.Status
	}
	return contracts.Connection{ConnectionID: ConnectionIDFor(c.Name), Name: c.Name, Service: c.Connector, Modes: []string{ModeRemoteInvoke}, Availability: []contracts.ConnectionAvailability{av}}
}

// Connection modes (contracts.Connection.Modes values).
const (
	ModeLocalCredential = "local_credential"
	ModeRemoteInvoke    = "remote_invoke"
)

// InventorySchema is the upstream inventory document schema.
const InventorySchema = "outfit.jhh3.dev/inventory/v1"

type inventoryDoc struct {
	Schema      string           `json:"schema"`
	Version     string           `json:"version"`
	Connections []Connection     `json:"connections"`
	Tools       []inventoryTool  `json:"tools"`
	Skills      []inventorySkill `json:"skills"`
}

type inventoryTool struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	Platform    string    `json:"platform"`
	Path        string    `json:"path"`
	SHA256      string    `json:"sha256"`
	Adopted     bool      `json:"adopted"`
	Current     bool      `json:"current"`
	InstalledAt time.Time `json:"installed_at"`
}

type inventorySkill struct {
	ID         string `json:"id"`
	Connection string `json:"connection"`
	Connector  string `json:"connector"`
	Host       string `json:"host"`
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	Ref        string `json:"ref"`
	Digest     string `json:"digest"`
	Path       string `json:"path"`
	Enabled    bool   `json:"enabled"`
}

// envelope is Outfit's outfit.jhh3.dev/result/v1 shape.
type envelope struct {
	Schema string          `json:"schema"`
	OK     bool            `json:"ok"`
	Data   json.RawMessage `json:"data"`
	Error  *Failure        `json:"error,omitempty"`
}

// Failure is Outfit's structured failure.
type Failure struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Hint       string `json:"hint,omitempty"`
	Correction string `json:"correction,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}

func isUnknownCommand(res runResult) bool {
	// The message appears raw on stderr or JSON-escaped in a failure envelope.
	text := strings.ReplaceAll(string(res.stdout)+string(res.stderr), `\"`, `"`)
	return strings.Contains(text, `unknown command "inventory"`)
}

// inventory runs `outfit inventory --json` when supported. ok=false means the
// installed Outfit predates the command (callers fall back to `ls --json`).
func (a *Adapter) inventory(ctx context.Context) (inventoryDoc, bool, error) {
	a.mu.Lock()
	skip := a.noInventory
	a.mu.Unlock()
	if skip {
		return inventoryDoc{}, false, nil
	}
	res, err := a.run(ctx, a.opt.Timeout, nil, nil, "inventory", "--json")
	if err != nil {
		return inventoryDoc{}, false, err
	}
	if res.exit != 0 {
		if isUnknownCommand(res) {
			a.mu.Lock()
			a.noInventory = true
			a.mu.Unlock()
			return inventoryDoc{}, false, nil
		}
		return inventoryDoc{}, false, unavailable("outfit inventory failed (exit %d): %s", res.exit, diag(res.stderr, res.stdout))
	}
	if res.truncated {
		return inventoryDoc{}, false, contracts.Errorf(contracts.ErrPayloadTooLarge, "outfit inventory exceeded %d bytes", a.opt.MaxOutput)
	}
	var doc inventoryDoc
	if err := json.Unmarshal(res.stdout, &doc); err != nil || doc.Schema != InventorySchema {
		return inventoryDoc{}, false, unavailable("outfit inventory returned an unrecognized document")
	}
	return doc, true, nil
}

// Connections lists this node's Outfit connections (metadata only). It uses
// the secret-free `inventory --json` document when available and falls back
// to `ls --json` (which lacks custody, env names, and verification times).
func (a *Adapter) Connections(ctx context.Context) ([]Connection, error) {
	doc, ok, err := a.inventory(ctx)
	if err != nil {
		return nil, err
	}
	if ok {
		for i := range doc.Connections {
			if doc.Connections[i].Environment == nil {
				doc.Connections[i].Environment = []string{}
			}
		}
		return validConnections(doc.Connections), nil
	}
	res, err := a.run(ctx, a.opt.Timeout, nil, []string{"OUTFIT_AGENT=1"}, "ls", "--json")
	if err != nil {
		return nil, err
	}
	var env envelope
	if jerr := json.Unmarshal(res.stdout, &env); jerr != nil {
		return nil, unavailable("outfit ls returned no result envelope (exit %d): %s", res.exit, diag(res.stderr))
	}
	if !env.OK || res.exit != 0 {
		msg := "unknown failure"
		if env.Error != nil {
			msg = env.Error.Message
		}
		return nil, unavailable("outfit ls failed: %s", diag([]byte(msg)))
	}
	var rows []struct {
		Name       string `json:"name"`
		Connector  string `json:"connector"`
		Display    string `json:"display"`
		Mode       string `json:"mode"`
		Status     string `json:"status"`
		Operations int    `json:"operations"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		return nil, unavailable("outfit ls returned unexpected data")
	}
	out := make([]Connection, 0, len(rows))
	for _, r := range rows {
		out = append(out, Connection{Name: r.Name, Connector: r.Connector, Display: r.Display, Mode: r.Mode, Status: r.Status, Operations: r.Operations, Environment: []string{}})
	}
	return validConnections(out), nil
}

func validConnections(in []Connection) []Connection {
	out := in[:0]
	for _, c := range in {
		if ValidName(c.Name) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Find returns the connection with the given Pyre connection ID.
func (a *Adapter) Find(ctx context.Context, connectionID string) (Connection, error) {
	name, ok := NameForConnectionID(connectionID)
	if !ok {
		return Connection{}, contracts.Errorf(contracts.ErrInvalid, "invalid connection id %q", connectionID)
	}
	list, err := a.Connections(ctx)
	if err != nil {
		return Connection{}, err
	}
	for _, c := range list {
		if c.Name == name {
			return c, nil
		}
	}
	return Connection{}, contracts.Errorf(contracts.ErrNotFound, "outfit on this node has no connection named %q", name).WithRef(contracts.KindConnection, connectionID)
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
