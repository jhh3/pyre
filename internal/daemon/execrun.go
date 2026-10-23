package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/contracts"
)

// Headless runs the caller executes itself (pyre run, W-pyrerun).
//
// `pyre run -- claude -p ...` must behave exactly like calling claude
// directly (stdio byte for byte, the child's exit code), so the CLI runs
// the command as its own child. The node does what it does for every run
// except starting the process: it binds the best eligible account, composes
// the profile, records the session and run in the hub and reports the run's
// end. The start request carries ExecArg (the SHA-256 of a secret only the
// starting CLI knows); the CLI then claims the binding over this node's
// local socket with the secret (PathLocalExecClaim), reports the child's
// pid and its exit. The run's adapter is the harness's exec adapter
// (Config.ExecAdapters), which never starts anything.
const (
	// ExecArg marks a start as headless: "--pyre-exec=<hex sha256 of the claim secret>".
	ExecArg = contracts.ArgPyreExec
	// ExecResumeArg names the conversation the caller's command resumes
	// (claude --resume ID), so the run binds the account that reaches it.
	ExecResumeArg = contracts.ArgPyreExecResume
)

// Local-socket endpoints of headless runs (not part of the hub API).
const (
	PathLocalExec        = "/v1/local/exec"         // GET: supported (node id, version)
	PathLocalExecClaim   = "/v1/local/exec/claim"   // POST ExecClaimRequest -> ExecBinding
	PathLocalExecStarted = "/v1/local/exec/started" // POST ExecProgress
	PathLocalExecExited  = "/v1/local/exec/exited"  // POST ExecProgress
)

// handleDataExecClaim keeps the claim hash on the run's node-local handle
// (started/exited reports after a pyred restart).
const handleDataExecClaim = "exec_claim_sha256"

// ExecInfo answers GET PathLocalExec.
type ExecInfo struct {
	NodeID    string   `json:"node_id"`
	Version   string   `json:"version"`
	Harnesses []string `json:"harnesses"`
	// Start: the node records headless runs itself with its own hub
	// credential (PathLocalExecStart), for a CLI without a hub login.
	Start bool `json:"start,omitempty"`
}

// ExecClaimRequest takes a headless run's binding.
type ExecClaimRequest struct {
	RunID     string `json:"run_id"`
	Secret    string `json:"secret"`
	ClientPID int    `json:"client_pid"`
}

// ExecBinding is what the CLI runs the command with. Env holds the
// account's environment (the profile's CLAUDE_CONFIG_DIR, a token
// account's token variable): secret, returned once over the 0600 local
// socket and never logged or stored.
type ExecBinding struct {
	RunID      string            `json:"run_id"`
	SessionID  string            `json:"session_id"`
	AccountID  string            `json:"account_id,omitempty"`
	ProfileDir string            `json:"profile_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Unset      []string          `json:"unset,omitempty"`
}

// ExecProgress reports the child's start (PID) or end (ExitCode/Signal,
// NativeID: the Claude session id when the CLI saw it in the output).
type ExecProgress struct {
	RunID    string `json:"run_id"`
	Secret   string `json:"secret"`
	PID      int    `json:"pid,omitempty"`
	Group    bool   `json:"group,omitempty"` // the child leads its own process group
	ExitCode *int   `json:"exit_code,omitempty"`
	Signal   string `json:"signal,omitempty"`
	NativeID string `json:"native_id,omitempty"`
}

// execRunAdapter is a harness's headless adapter (claude.ExecAdapter).
type execRunAdapter interface {
	contracts.RuntimeAdapter
	Claimed(h contracts.RunHandle, clientPID int) error
	Started(h contracts.RunHandle, childPID int, group bool) error
	Exited(h contracts.RunHandle, code *int, signal, nativeID string) error
}

// conversationLocator tells which account reaches a native conversation
// (claude.Adapter.ResumeOwner).
type conversationLocator interface {
	ResumeOwner(cwd, sid string) (shared bool, accountID string, defaultLogin bool)
}

// execClaim is a recorded headless run waiting for its CLI.
type execClaim struct {
	hash  string
	ready chan struct{}
	once  sync.Once
	b     ExecBinding
	err   error
	taken bool // claimed: the environment was handed out and cleared
}

func (c *execClaim) finish(b ExecBinding, err error) {
	c.once.Do(func() {
		c.b, c.err = b, err
		close(c.ready)
	})
}

type execClaims struct {
	mu sync.Mutex
	m  map[string]*execClaim
}

func (d *Daemon) execClaim(runID string) *execClaim {
	d.execs.mu.Lock()
	defer d.execs.mu.Unlock()
	return d.execs.m[runID]
}

func (d *Daemon) putExecClaim(runID, hash string) *execClaim {
	d.execs.mu.Lock()
	defer d.execs.mu.Unlock()
	if d.execs.m == nil {
		d.execs.m = map[string]*execClaim{}
	}
	c := &execClaim{hash: hash, ready: make(chan struct{})}
	d.execs.m[runID] = c
	return c
}

// takeExecClaim reports whether c is still unclaimed; with take it marks
// it claimed and clears the environment it holds.
func (d *Daemon) takeExecClaim(c *execClaim, take bool) bool {
	d.execs.mu.Lock()
	defer d.execs.mu.Unlock()
	if c.taken {
		return false
	}
	if take {
		c.taken = true
		c.b = ExecBinding{}
	}
	return true
}

func (d *Daemon) dropExecClaim(runID string) {
	d.execs.mu.Lock()
	defer d.execs.mu.Unlock()
	delete(d.execs.m, runID)
}

// failExecClaim answers the claim of a headless start the node refused
// (no eligible account, harness unavailable, ...), so the waiting CLI falls
// back at once instead of at its setup timeout.
func (d *Daemon) failExecClaim(sc contracts.StartRunCommand, err error) {
	_, hash, _, perr := splitExecArgs(sc.Args)
	if perr != nil || hash == "" || d.execClaim(sc.RunID) != nil {
		return
	}
	ce := contracts.AsError(err)
	ce.Message = redact(ce.Message)
	d.putExecClaim(sc.RunID, hash).finish(ExecBinding{}, ce)
	time.AfterFunc(execClaimWait+2*time.Minute, func() { d.dropExecClaim(sc.RunID) })
}

// execAdapterFor returns the harness's headless adapter.
func (d *Daemon) execAdapterFor(h contracts.Harness) execRunAdapter {
	for _, a := range d.cfg.ExecAdapters {
		if ex, ok := a.(execRunAdapter); ok && a.Harness() == h {
			return ex
		}
	}
	return nil
}

func isExecAdapter(a contracts.RuntimeAdapter) bool {
	_, ok := a.(execRunAdapter)
	return ok
}

func hexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// splitExecArgs removes ExecArg/ExecResumeArg from args.
func splitExecArgs(args []string) (rest []string, hash, resume string, err error) {
	for i := 0; i < len(args); i++ {
		name, val, has := strings.Cut(args[i], "=")
		if name != ExecArg && name != ExecResumeArg {
			rest = append(rest, args[i])
			continue
		}
		if !has {
			if i+1 >= len(args) {
				return nil, "", "", contracts.Errorf(contracts.ErrInvalid, "%s needs a value", name)
			}
			i++
			val = args[i]
		}
		if name == ExecArg {
			if !hexSHA256(val) {
				return nil, "", "", contracts.Errorf(contracts.ErrInvalid, "%s needs the claim's hex SHA-256", ExecArg)
			}
			hash = strings.ToLower(val)
		} else {
			resume = strings.TrimSpace(val)
		}
	}
	return rest, hash, resume, nil
}

// execPolicy picks the account policy of a headless resume: the account
// whose profile holds the conversation when only it can reach it, the
// default login when only that has it; otherwise (a shared transcript
// store, nothing found, or an explicit choice) the request's own. A Codex
// thread in the shared store prefers the account a run of this node last
// ran it on (cross-account Codex resume is unproven, ResumeSpec), without
// insisting: a limited account still falls back to the best eligible one.
func (d *Daemon) execPolicy(ctx context.Context, h contracts.Harness, p contracts.AccountPolicy, cwd, resume string) contracts.AccountPolicy {
	if resume == "" || p.Mode != contracts.AccountEligible {
		return p
	}
	loc, ok := d.byHarness[h].(conversationLocator)
	if !ok {
		return p
	}
	shared, acct, def := loc.ResumeOwner(expandHome(cwd), resume)
	switch {
	case shared && h == contracts.HarnessCodex:
		if rec, ok, err := d.st.runForNative(ctx, resume); err == nil && ok && rec.Run.AccountID != "" {
			p.Prefer = append([]string{rec.Run.AccountID}, p.Prefer...)
		}
		return p
	case shared:
		return p
	case acct != "":
		return contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: acct, Strict: true}
	case def:
		return contracts.AccountPolicy{Mode: contracts.AccountAmbient}
	}
	return p
}

func claimHash(secret string) string {
	s := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(s[:])
}

func sameHash(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// execHandlers are the local-socket endpoints (LocalHandler mounts them).
func (d *Daemon) execHandlers() map[string]http.Handler {
	return map[string]http.Handler{
		"GET " + PathLocalExec: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var hs []string
			for _, a := range d.cfg.ExecAdapters {
				hs = append(hs, string(a.Harness()))
			}
			writeLocalJSON(w, http.StatusOK, ExecInfo{NodeID: d.nodeID, Version: buildinfo.Version, Harnesses: hs, Start: d.canStartExec()})
		}),
		"POST " + PathLocalExecStart:   http.HandlerFunc(d.serveExecStart), // execstart.go
		"POST " + PathLocalExecClaim:   http.HandlerFunc(d.serveExecClaim),
		"POST " + PathLocalExecStarted: http.HandlerFunc(d.serveExecProgress),
		"POST " + PathLocalExecExited:  http.HandlerFunc(d.serveExecProgress),
	}
}

func writeLocalJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeLocalErr(w http.ResponseWriter, err error) {
	ce := contracts.AsError(err)
	status := http.StatusBadRequest
	switch ce.Code {
	case contracts.ErrNotFound:
		status = http.StatusNotFound
	case contracts.ErrForbidden, contracts.ErrUnauthorized:
		status = http.StatusForbidden
	case contracts.ErrConflict:
		status = http.StatusConflict
	case contracts.ErrUnavailable, contracts.ErrCapacityExhausted:
		status = http.StatusServiceUnavailable
	}
	writeLocalJSON(w, status, map[string]any{"error": ce})
}

// execClaimWait bounds how long a claim waits for the launch to finish.
var execClaimWait = 60 * time.Second

func (d *Daemon) serveExecClaim(w http.ResponseWriter, r *http.Request) {
	var req ExecClaimRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeLocalErr(w, contracts.Errorf(contracts.ErrInvalid, "claim: %v", err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execClaimWait)
	defer cancel()
	// The CLI claims as soon as the hub recorded the start; the start
	// command may still be on its way to this node.
	c := d.execClaim(req.RunID)
	for c == nil && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
		c = d.execClaim(req.RunID)
	}
	if c == nil || !sameHash(c.hash, claimHash(req.Secret)) {
		writeLocalErr(w, contracts.Errorf(contracts.ErrNotFound, "no headless run %s waiting for this claim", req.RunID))
		return
	}
	if !d.takeExecClaim(c, false) {
		writeLocalErr(w, contracts.Errorf(contracts.ErrConflict, "headless run %s was already claimed", req.RunID))
		return
	}
	select {
	case <-c.ready:
	case <-ctx.Done():
		writeLocalErr(w, contracts.Errorf(contracts.ErrUnavailable, "headless run %s is still launching", req.RunID))
		return
	}
	if c.err != nil {
		d.dropExecClaim(req.RunID)
		writeLocalErr(w, c.err)
		return
	}
	lr, ok := d.getLive(req.RunID)
	if !ok {
		writeLocalErr(w, contracts.Errorf(contracts.ErrNotFound, "headless run %s is not live", req.RunID))
		return
	}
	rec, _ := lr.snapshot()
	ex, ok := lr.adapter.(execRunAdapter)
	if !ok || rec.Handle == nil {
		writeLocalErr(w, contracts.Errorf(contracts.ErrInvalid, "run %s is not headless", req.RunID))
		return
	}
	if err := ex.Claimed(*rec.Handle, req.ClientPID); err != nil {
		writeLocalErr(w, err)
		return
	}
	b := c.b
	d.takeExecClaim(c, true) // the secret environment is handed out once
	writeLocalJSON(w, http.StatusOK, b)
}

func (d *Daemon) serveExecProgress(w http.ResponseWriter, r *http.Request) {
	var req ExecProgress
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeLocalErr(w, contracts.Errorf(contracts.ErrInvalid, "exec report: %v", err))
		return
	}
	lr, ok := d.getLive(req.RunID)
	if !ok {
		writeLocalErr(w, contracts.Errorf(contracts.ErrNotFound, "headless run %s is not live", req.RunID))
		return
	}
	rec, _ := lr.snapshot()
	ex, ok := lr.adapter.(execRunAdapter)
	if !ok || rec.Handle == nil || !sameHash(rec.Handle.Data[handleDataExecClaim], claimHash(req.Secret)) {
		writeLocalErr(w, contracts.Errorf(contracts.ErrNotFound, "no headless run %s for this claim", req.RunID))
		return
	}
	var err error
	if strings.HasSuffix(r.URL.Path, "/started") {
		err = ex.Started(*rec.Handle, req.PID, req.Group)
	} else {
		err = ex.Exited(*rec.Handle, req.ExitCode, req.Signal, req.NativeID)
	}
	if err != nil {
		writeLocalErr(w, err)
		return
	}
	// Answer at once (the CLI exits with its child's status right after) and
	// report the change in the background rather than at the next poll.
	writeLocalJSON(w, http.StatusOK, map[string]string{"run_id": req.RunID})
	go func() {
		pctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		d.PollNow(pctx, req.RunID)
	}()
}

// Run adapter data of a headless run.
const (
	adapterDataMode = "pyre.mode"
	modeHeadless    = "headless"
)

// execCapabilities are a headless run's: stop (signals the child), read
// the transcript, resume natively (interactively, on the tmux adapter).
var execCapabilities = []string{contracts.CapStop, contracts.CapTranscript, contracts.CapResumeNative, contracts.CapAccountProfile}

// execBindingFor is what the CLI runs a headless run's command with.
func execBindingFor(rec runRecord, profileDir string, env map[string]string, unset []string) ExecBinding {
	b := ExecBinding{RunID: rec.Run.RunID, SessionID: rec.Run.SessionID, AccountID: rec.Run.AccountID, ProfileDir: profileDir,
		Env: map[string]string{}}
	for _, k := range unset {
		// Only what changes the harness's own login: a headless command
		// keeps the caller's other keys (OPENAI_API_KEY for a claude's
		// tools, ANTHROPIC_API_KEY for a codex's, ...).
		if ownLoginVar(rec.Run.Harness, k) {
			b.Unset = append(b.Unset, k)
		}
	}
	for k, v := range env {
		b.Env[k] = v
	}
	// A profile binding sets CLAUDE_CONFIG_DIR / CODEX_HOME. The machine's
	// own login (no profile) keeps the caller's, exactly as when pyre run
	// falls back without Pyre.
	if v := homeVar[rec.Run.Harness]; profileDir != "" && v != "" && b.Env[v] == "" {
		b.Env[v] = profileDir
	}
	return b
}

// execNativeKind is the native conversation kind of a harness's headless
// runs (the headless adapter's NativeKind).
var execNativeKind = map[contracts.Harness]string{contracts.HarnessClaude: "claude_session", contracts.HarnessCodex: "codex_thread"}

// homeVar is a harness's config-home variable.
var homeVar = map[contracts.Harness]string{contracts.HarnessClaude: "CLAUDE_CONFIG_DIR", contracts.HarnessCodex: "CODEX_HOME"}

// ownLoginVar reports whether k selects harness h's own login or billing
// (what a headless binding removes from the caller's environment).
func ownLoginVar(h contracts.Harness, k string) bool {
	switch h {
	case contracts.HarnessCodex:
		return strings.HasPrefix(k, "CODEX_") || slices.Contains(contracts.CodexLoginEnv, k)
	default:
		return strings.HasPrefix(k, "CLAUDE_") || strings.HasPrefix(k, "ANTHROPIC_")
	}
}
