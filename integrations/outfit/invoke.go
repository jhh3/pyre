package outfit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Check is a live availability probe of one connection on this node.
type Check struct {
	Name      string    `json:"name"`
	State     string    `json:"state"` // "verified" | "missing" | "expired" | "locked_store" | "unknown"
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// Check runs `outfit connector test NAME` ("Reverify the selected mode without
// changing it"). Outfit verifies through the provider and its own credential
// custody; Pyre sees only success or a classified, redacted failure. Note:
// Outfit records the verification time in its own state as a side effect.
func (a *Adapter) Check(ctx context.Context, name string) (Check, error) {
	if !ValidName(name) {
		return Check{}, contracts.Errorf(contracts.ErrInvalid, "invalid outfit connection name %q", name)
	}
	res, err := a.run(ctx, a.opt.CheckTimeout, nil, []string{"OUTFIT_AGENT=0"}, "connector", "test", name)
	now := time.Now().UTC()
	if err != nil {
		if res.timedOut {
			return Check{Name: name, State: "unknown", Detail: "verification timed out (a locked credential store may be waiting for an unlock prompt)", CheckedAt: now}, nil
		}
		return Check{}, err
	}
	if res.exit == 0 {
		return Check{Name: name, State: "verified", CheckedAt: now}, nil
	}
	text := strings.ToLower(string(res.stderr) + " " + string(res.stdout))
	c := Check{Name: name, CheckedAt: now, Detail: diag(res.stderr, res.stdout)}
	switch {
	case containsAny(text, "no such connection", "connection not found", "sql: no rows"):
		// Outfit has no connection of this name here (older builds leak the
		// raw store error "sql: no rows in result set").
		c.State = "missing"
		c.Detail = "connection " + strconv.Quote(name) + " is not configured in Outfit on this node"
	case containsAny(text, "secret not found in keyring", "was not found"):
		c.State = "missing"
	case containsAny(text, "user interaction is not allowed", "keyring is locked", "collection is locked", "locked", "org.freedesktop.secrets", "os keychain", "secret service"):
		c.State = "locked_store"
	case containsAny(text, "expired", "reauth", "invalid_grant", "401", "unauthorized", "bad credentials", "not logged in", "authentication"):
		c.State = "expired"
	default:
		c.State = "unknown"
	}
	return c, nil
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// InvokeRequest is one read operation executed by Outfit on this node.
type InvokeRequest struct {
	Connection string            // Outfit connection name
	Operation  string            // canonical or friendly operation name
	Args       map[string]string // passed as JSON on stdin, never argv
	MaxItems   int               // optional page item cap
}

// InvokeResult carries Outfit's result envelope. It never includes provider
// credentials (Outfit substitutes redaction markers before execution).
type InvokeResult struct {
	OK        bool            `json:"ok"`
	Envelope  json.RawMessage `json:"envelope,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	Failure   *Failure        `json:"failure,omitempty"`
}

// Err maps a failed result to the wire error shape.
func (r InvokeResult) Err() *contracts.Error {
	if r.OK {
		return nil
	}
	f := r.Failure
	if f == nil {
		f = &Failure{Code: "operation_failed", Message: "outfit operation failed"}
	}
	msg := diag([]byte(f.Message))
	var e *contracts.Error
	switch f.Code {
	case "write_confirmation_required":
		e = contracts.Errorf(contracts.ErrForbidden, "outfit refused a mutating operation over connection.invoke (reads only): %s", msg)
	case "invalid_arguments", "missing_argument":
		e = contracts.Errorf(contracts.ErrInvalid, "%s", msg)
	case "unknown_operation":
		e = contracts.Errorf(contracts.ErrNotFound, "%s", msg)
	case "provider_timeout":
		e = contracts.Errorf(contracts.ErrUnavailable, "%s", msg)
	default:
		e = contracts.Errorf(contracts.ErrUnavailable, "outfit operation failed: %s", msg)
		e.Retryable = f.Retryable
	}
	e = e.WithDetail("outfit_code", f.Code)
	if f.Hint != "" {
		e = e.WithDetail("hint", diag([]byte(f.Hint)))
	}
	return e
}

// Invoke runs a read operation: `outfit run NAME OP --input - --json` with the
// arguments as a JSON object on stdin. Mutations are not supported over this
// path (Outfit requires an explicit human --write).
func (a *Adapter) Invoke(ctx context.Context, req InvokeRequest) (InvokeResult, error) {
	if !ValidName(req.Connection) {
		return InvokeResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid outfit connection name %q", req.Connection)
	}
	if !opPattern.MatchString(req.Operation) {
		return InvokeResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid operation name %q", req.Operation)
	}
	args := map[string]string{}
	for k, v := range req.Args {
		if !argKey.MatchString(k) {
			return InvokeResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid argument name %q", k)
		}
		args[k] = v
	}
	input, _ := json.Marshal(args)
	argv := []string{"run", req.Connection, req.Operation, "--input", "-", "--json"}
	if req.MaxItems > 0 {
		argv = append(argv, "--max-items", strconv.Itoa(req.MaxItems))
	}
	spill := a.opt.MaxOutput - 1024
	if spill < 4096 {
		spill = 4096
	}
	if spill > 10<<20 {
		spill = 10 << 20
	}
	res, err := a.run(ctx, a.opt.InvokeTimeout, input, []string{"OUTFIT_AGENT=1", "OUTFIT_AGENT_MAX_BYTES=" + strconv.Itoa(spill)}, argv...)
	if err != nil {
		if res.timedOut {
			return InvokeResult{}, contracts.Errorf(contracts.ErrOutcomeIndeterminate, "outfit operation %s timed out; its provider-side outcome is unknown", req.Operation)
		}
		return InvokeResult{}, err
	}
	if res.truncated {
		return InvokeResult{}, contracts.Errorf(contracts.ErrPayloadTooLarge, "outfit result exceeded %d bytes", a.opt.MaxOutput)
	}
	var env envelope
	if err := json.Unmarshal(res.stdout, &env); err != nil {
		return InvokeResult{}, unavailable("outfit run returned no result envelope (exit %d): %s", res.exit, diag(res.stderr))
	}
	out := InvokeResult{OK: env.OK && res.exit == 0, Failure: env.Error}
	if out.Failure != nil {
		f := *out.Failure
		f.Message, f.Hint = diag([]byte(f.Message)), diag([]byte(f.Hint))
		out.Failure = &f
	}
	out.Envelope, out.Truncated = sanitizeSpill(res.stdout)
	return out, nil
}

// sanitizeSpill handles Outfit's large-result spill: the inline envelope then
// names a private temp file on THIS node, useless to a remote caller. Pyre
// replaces the pointer with a bounded marker and removes the file it caused.
func sanitizeSpill(raw []byte) (json.RawMessage, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return raw, false
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(doc["meta"], &meta) != nil || meta["spill"] == nil {
		return raw, false
	}
	var spill struct {
		Path   string `json:"path"`
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	}
	_ = json.Unmarshal(meta["spill"], &spill)
	if spill.Path != "" && strings.HasPrefix(filepath.Base(spill.Path), "outfit-result-") && filepath.Dir(filepath.Clean(spill.Path)) == filepath.Clean(os.TempDir()) {
		_ = os.Remove(spill.Path)
	}
	marker, _ := json.Marshal(map[string]any{"spilled": true, "bytes": spill.Bytes, "sha256": spill.SHA256, "detail": "result exceeded Pyre's inline bound; narrow the request or use max_items"})
	doc["data"] = marker
	delete(meta, "spill")
	meta["truncated"] = json.RawMessage("true")
	doc["meta"], _ = json.Marshal(meta)
	b, _ := json.Marshal(doc)
	return b, true
}
