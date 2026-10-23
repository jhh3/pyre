package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/harnessargs"
	"github.com/jhh3/pyre/internal/refs"
)

// Identity is the calling agent session, from PYRE_SESSION_ID/RUN_ID/NODE_ID.
// It is empty for a human at a terminal.
type Identity struct {
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
}

// IdentityFromEnv reads the agent-session environment.
func IdentityFromEnv(getenv func(string) string) Identity {
	return Identity{SessionID: getenv(EnvSessionID), RunID: getenv(EnvRunID), NodeID: getenv(EnvNodeID)}
}

// Ops is the single operation layer beneath both the CLI commands and the
// MCP tools: each method takes the same parameter struct (whose JSON form
// is the MCP tool input) and returns contract types. Nothing here formats
// output.
type Ops struct {
	API  c.API
	Self Identity
	// ClientCredential: the CLI authenticates with a client credential
	// (PYRE_TOKEN or the client config: the owner's), not as a session.
	// Self then still names the machine, but a PYRE_SESSION_ID/RUN_ID left
	// in the environment by an enclosing agent session is not the caller:
	// a start does not become that session's child (nor join its project)
	// unless --parent says so.
	ClientCredential bool
	// PollInterval paces WaitReply (default 1s).
	PollInterval time.Duration
	// Stderr receives progress notes (a start replaying a lost answer); nil
	// discards them.
	Stderr io.Writer
	// StartRecover bounds the replays of a start whose answer was lost
	// (default startRecoverDefault: delays 2/5/10/15 s within 75 s).
	StartRecover *StartRecovery
	// LaunchWait bounds how long start_session waits for the run to leave
	// "starting" (awaitLaunch); 0 returns once the launch is queued.
	LaunchWait time.Duration
}

// StartRecovery paces StartSession's replays: one single-attempt replay
// after each delay, all within Total.
type StartRecovery struct {
	Delays []time.Duration
	Total  time.Duration
}

var startRecoverDefault = StartRecovery{Delays: []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second}, Total: 75 * time.Second}

// mut returns a Mutation, minting the operation ID once per logical
// operation when the caller gave none (transport retries then reuse it).
func mut(opID string) c.Mutation {
	if opID == "" {
		opID = c.NewID(c.PrefixOperation)
	}
	return c.Mutation{OperationID: opID}
}

func listAll[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) (c.Page[T], error)) (c.Page[T], error) {
	var snap time.Time
	items, err := client.All(ctx, func(ctx context.Context, cur string) (c.Page[T], error) {
		p, err := fetch(ctx, cur)
		if snap.IsZero() {
			snap = p.SnapshotAt
		}
		return p, err
	})
	if items == nil {
		items = []T{}
	}
	return c.Page[T]{Items: items, SnapshotAt: snap}, err
}

func notFoundf(kind c.ObjectKind, ref string) error {
	return c.Errorf(c.ErrNotFound, "no %s matches %q", kind, ref)
}

func ambiguous(kind c.ObjectKind, ref string, ids []string) error {
	return c.Errorf(c.ErrConflict, "%q matches several %ss (%s); use an ID", ref, kind, strings.Join(ids, ", "))
}

// ---- reference resolution (IDs are discoverable; names are aliases) ----

// ResolveNode accepts a node ID or an exact name / ssh alias / hostname.
func (o *Ops) ResolveNode(ctx context.Context, ref string) (c.Node, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return c.Node{}, usagef("a node is required (see `pyre nodes`)")
	}
	if c.ValidID(ref, c.PrefixNode) {
		return o.API.GetNode(ctx, c.GetRequest{ID: ref})
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return c.Node{}, err
	}
	var hits []c.Node
	for _, pass := range []func(a, b string) bool{func(a, b string) bool { return a == b }, strings.EqualFold} {
		for _, n := range page.Items {
			if pass(n.Name, ref) || (n.SSHAlias != "" && pass(n.SSHAlias, ref)) || (n.Hostname != "" && pass(n.Hostname, ref)) {
				hits = append(hits, n)
			}
		}
		if len(hits) > 0 {
			break
		}
	}
	switch len(hits) {
	case 0:
		return c.Node{}, notFoundf(c.KindNode, ref)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, n := range hits {
		ids[i] = n.NodeID
	}
	return c.Node{}, ambiguous(c.KindNode, ref, ids)
}

// NodeID resolves ref, defaulting to the calling session's node, then to
// the only connected node.
func (o *Ops) NodeID(ctx context.Context, ref string) (string, error) {
	if ref != "" {
		n, err := o.ResolveNode(ctx, ref)
		return n.NodeID, err
	}
	if o.Self.NodeID != "" {
		return o.Self.NodeID, nil
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return "", err
	}
	var connected, names []string
	for _, n := range page.Items {
		names = append(names, n.Name)
		if n.Connection.State == c.ConnConnected {
			connected = append(connected, n.NodeID)
		}
	}
	if len(connected) == 1 {
		return connected[0], nil
	}
	return "", usagef("--node is required (nodes: %s)", strings.Join(names, ", "))
}

// ResolveSession accepts a session ID, "self" (the calling session), or an
// exact title.
func (o *Ops) ResolveSession(ctx context.Context, ref string) (c.SessionView, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "self" {
		if o.Self.SessionID == "" {
			return c.SessionView{}, usagef("a session is required (see `pyre sessions`)")
		}
		ref = o.Self.SessionID
	}
	if c.ValidID(ref, c.PrefixSession) {
		return o.API.GetSession(ctx, c.GetRequest{ID: ref})
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
		return o.API.ListSessions(ctx, c.ListSessionsRequest{PageRequest: c.PageRequest{Cursor: cur}, Query: ref, IncludeMonitors: true})
	})
	if err != nil {
		return c.SessionView{}, err
	}
	var hits []c.SessionView
	for _, pass := range []func(a, b string) bool{func(a, b string) bool { return a == b }, strings.EqualFold} {
		for _, s := range page.Items {
			if pass(s.Session.Title, ref) {
				hits = append(hits, s)
			}
		}
		if len(hits) > 0 {
			break
		}
	}
	switch len(hits) {
	case 0:
		if err := o.subagentRef(ctx, ref); err != nil {
			return c.SessionView{}, err
		}
		return c.SessionView{}, notFoundf(c.KindSession, ref)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, s := range hits {
		ids[i] = s.Session.SessionID
	}
	return c.SessionView{}, ambiguous(c.KindSession, ref, ids)
}

// subagentRef explains a reference that names a native subagent observed on
// some run: Pyre reports native subagents but never makes them attachable or
// addressable fleet sessions (A03), so say that instead of "not found".
func (o *Ops) subagentRef(ctx context.Context, ref string) error {
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
		return o.API.ListSessions(ctx, c.ListSessionsRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return nil
	}
	for _, s := range page.Items {
		if s.Run == nil {
			continue
		}
		for _, sa := range s.Run.Subagents {
			if sa.ID == ref {
				return c.Errorf(c.ErrUnsupported, "%s is a native %s subagent of session %s (%s, %s); Pyre observes native subagents but they are not independently attachable or addressable: use the parent session",
					ref, s.Session.Harness, s.Session.SessionID, orDash(sa.Kind), sa.State).WithRef(c.KindSession, s.Session.SessionID)
			}
		}
	}
	return nil
}

func (o *Ops) sessionID(ctx context.Context, ref string) (string, error) {
	if c.ValidID(ref, c.PrefixSession) {
		return ref, nil
	}
	s, err := o.ResolveSession(ctx, ref)
	return s.Session.SessionID, err
}

// ResolveProject accepts a project ID or exact name.
func (o *Ops) ResolveProject(ctx context.Context, ref string) (c.Project, error) {
	if strings.TrimSpace(ref) == "" {
		return c.Project{}, usagef("a project is required (see `pyre project ls`)")
	}
	return refs.Project(ctx, o.API, ref)
}

// projectPtr resolves an optional project reference ("" = none given;
// "none" = explicitly no project: a child otherwise joins its parent's).
func (o *Ops) projectPtr(ctx context.Context, ref string) (*string, error) {
	return refs.ProjectPtr(ctx, o.API, ref)
}

// ---- fleet ----

// ListNodes returns every node.
func (o *Ops) ListNodes(ctx context.Context, includeRevoked bool) (c.Page[c.Node], error) {
	return listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}, IncludeRevoked: includeRevoked})
	})
}

// ---- sessions ----

// SessionFilter selects sessions (MCP list_sessions input).
type SessionFilter struct {
	Node            string   `json:"node,omitempty"`
	Project         string   `json:"project,omitempty"` // ID/name, or "none" for standalone only
	Harness         string   `json:"harness,omitempty"`
	Query           string   `json:"query,omitempty"`
	States          []string `json:"states,omitempty"`
	IncludeArchived bool     `json:"include_archived,omitempty"`
	// IncludeMonitors also lists monitor sessions (pyre monitor add); by
	// default they are left out and shown on the sessions they wake.
	IncludeMonitors bool `json:"include_monitors,omitempty"`
}

func (o *Ops) ListSessions(ctx context.Context, f SessionFilter) (c.Page[c.SessionView], error) {
	req := c.ListSessionsRequest{Harness: c.Harness(f.Harness), Query: f.Query, IncludeArchived: f.IncludeArchived, IncludeMonitors: f.IncludeMonitors}
	for _, s := range f.States {
		req.States = append(req.States, c.RunState(s))
	}
	if f.Node != "" {
		n, err := o.ResolveNode(ctx, f.Node)
		if err != nil {
			return c.Page[c.SessionView]{}, err
		}
		req.NodeID = n.NodeID
	}
	if f.Project == "none" {
		empty := ""
		req.ProjectID = &empty
	} else if f.Project != "" {
		p, err := o.projectPtr(ctx, f.Project)
		if err != nil {
			return c.Page[c.SessionView]{}, err
		}
		req.ProjectID = p
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
		r := req
		r.Cursor = cur
		return o.API.ListSessions(ctx, r)
	})
	if err == nil && !f.IncludeMonitors {
		page.Items = withoutMonitors(page.Items) // a hub older than include_monitors lists them
	}
	return page, err
}

// withoutMonitors drops monitor sessions from a list.
func withoutMonitors(ss []c.SessionView) []c.SessionView {
	kept := ss[:0]
	for _, s := range ss {
		if !s.Session.IsMonitor() {
			kept = append(kept, s)
		}
	}
	return kept
}

// StartParams launches a session (MCP start_session input).
type StartParams struct {
	Node          string `json:"node"`
	Harness       string `json:"harness"`
	Cwd           string `json:"cwd,omitempty"`
	Project       string `json:"project,omitempty"`
	Worktree      string `json:"worktree,omitempty"`       // branch for an isolated git worktree
	Repo          string `json:"repo,omitempty"`           // project repo id: its recorded checkout on the node (shared), or the worktree source
	Account       string `json:"account,omitempty"`        // explicit account: ID, unique label or rota name, or a unique prefix of either
	StrictAccount bool   `json:"strict_account,omitempty"` // keep --account even when it is at its limit (default: fall back)
	Ambient       bool   `json:"ambient,omitempty"`        // node's existing login, no Pyre assignment
	// FallbackHarness: when the harness has no free account on the node
	// (Codex), start on this harness instead (only "claude"; for reviews and
	// second opinions). Empty: the launch fails at once with the reason.
	FallbackHarness string `json:"fallback_harness,omitempty"`
	Permissions     string `json:"permissions,omitempty"` // ask | bypass; "" = the node's --permissions
	// Model and Effort pick a non-default model and reasoning effort ("" =
	// the harness default). The node lists choices per harness (pyre models);
	// any name the harness accepts may be given.
	Model          string   `json:"model,omitempty"`
	Effort         string   `json:"effort,omitempty"`
	Prompt         string   `json:"prompt,omitempty"`
	Title          string   `json:"title,omitempty"`
	IncludeContext bool     `json:"include_context,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	OperationID    string   `json:"operation_id,omitempty"`
	// Without a project, repo/repos name repositories from the repo index
	// (fuzzy) and fresh asks for new worktrees (quicklaunch.go).
	Repos  []string `json:"repos,omitempty"`
	Fresh  bool     `json:"fresh,omitempty"`
	Branch string   `json:"branch,omitempty"`
	// ReportToParent: a session started by a session reports back to it
	// (turn finished, needs input, exited) unless this is false.
	ReportToParent *bool `json:"report_to_parent,omitempty"`
	// Parent makes the new session a child of this session (ID or title).
	// Empty: the calling session when a session credential is used, else
	// none (see Ops.ClientCredential).
	Parent string `json:"parent,omitempty"`
	placed bool   // launchNode placed it after checking accounts on every candidate (placement.go)
	// HarnessArgs are extra harness arguments from the harness's allowlist
	// (internal/harnessargs; Claude: --append-system-prompt-file,
	// --dangerously-load-development-channels, --plugin-dir, ...). The run
	// records them; resumes and account switches relaunch with them.
	HarnessArgs []string `json:"harness_args,omitempty"`
	// NativeResume continues an existing Claude conversation started
	// outside Pyre (its session id) as the new session's first run.
	NativeResume string `json:"native_resume,omitempty"`
	// EnvFile is an absolute path on the node to dotenv KEY=VALUE lines the
	// node applies to the harness process at every launch of the session
	// (internal/envfile). Only the path travels and is recorded.
	EnvFile string `json:"env_file,omitempty"`
}

func validHarness(h string) bool {
	switch c.Harness(h) {
	case c.HarnessClaude, c.HarnessCodex, c.HarnessAgy, c.HarnessShell, c.HarnessMock:
		return true
	}
	return false
}

// modelArgs renders a launch's model and effort as the node's start args
// (daemon.ModelArg/EffortArg), validated here for a clear usage error.
func modelArgs(harness, model, effort string) ([]string, error) {
	model, effort = strings.TrimSpace(model), strings.ToLower(strings.TrimSpace(effort))
	if model == "" && effort == "" {
		return nil, nil
	}
	if c.Harness(harness) == c.HarnessShell {
		return nil, usagef("--model/--effort apply to agent harnesses, not shell")
	}
	var args []string
	if model != "" {
		if !c.ValidModelName(model) {
			return nil, usagef("--model %q is not a model name", model)
		}
		args = append(args, modelArg+"="+model)
	}
	if effort != "" {
		if !c.ValidEffort(effort) {
			return nil, usagef("--effort %q is not an effort level (low, medium, high, xhigh, max)", effort)
		}
		args = append(args, effortArg+"="+effort)
	}
	return args, nil
}

// Per-launch model and effort, removed by the node before the harness sees
// them (daemon.ModelArg, daemon.EffortArg).
const (
	modelArg  = "--pyre-model"
	effortArg = "--pyre-effort"
)

func (o *Ops) StartSession(ctx context.Context, p StartParams) (c.StartSessionResponse, error) {
	if p.ReportToParent != nil && !*p.ReportToParent && !slices.Contains(p.Tags, c.TagReportNone) {
		// The reserved tag carries the opt-out through every launch path
		// (quick launch included); the hub records the decision.
		p.Tags = append(slices.Clone(p.Tags), c.TagReportNone)
	}
	if p.Project == "" && p.Cwd == "" && (p.Repo != "" || len(p.Repos) > 0) {
		return o.startFromRepos(ctx, p) // W-quicklaunch
	}
	if !validHarness(p.Harness) {
		return c.StartSessionResponse{}, usagef("--harness must be claude, codex, agy or shell (got %q)", p.Harness)
	}
	if p.Account != "" && p.Ambient {
		return c.StartSessionResponse{}, usagef("--account and --ambient are mutually exclusive")
	}
	var args []string
	switch p.Permissions {
	case "":
	case "ask", "bypass":
		args = []string{permissionsArg + "=" + p.Permissions}
	default:
		return c.StartSessionResponse{}, usagef("--permissions must be ask or bypass (got %q)", p.Permissions)
	}
	if _, err := modelArgs(p.Harness, p.Model, p.Effort); err != nil { // syntax first, before any placement
		return c.StartSessionResponse{}, err
	}
	hargs, err := startHarnessArgs(p)
	if err != nil {
		return c.StartSessionResponse{}, err
	}
	args = append(args, hargs...)
	if p.NativeResume != "" {
		if c.Harness(p.Harness) != c.HarnessClaude {
			return c.StartSessionResponse{}, usagef("--native-resume continues a Claude conversation (--harness claude)")
		}
		if !nativeSessionRe.MatchString(strings.TrimSpace(p.NativeResume)) {
			return c.StartSessionResponse{}, usagef("--native-resume %q is not a Claude session id (a UUID)", p.NativeResume)
		}
		args = append(args, nativeResumeArg+"="+strings.TrimSpace(p.NativeResume))
	}
	envArgs, err := envFileArgs(p)
	if err != nil {
		return c.StartSessionResponse{}, err
	}
	args = append(args, envArgs...)
	nodeID, err := o.launchNode(ctx, &p) // placement.go
	if err != nil {
		return c.StartSessionResponse{}, err
	}
	requested := p.Harness
	if err := o.harnessPreflight(ctx, &nodeID, &p); err != nil { // ops_harnessfallback.go
		return c.StartSessionResponse{}, err
	}
	// Model and effort are built for the harness that actually starts: a
	// Codex model never reaches the Claude fallback.
	margs, err := o.nodeModelArgs(ctx, nodeID, requested, &p)
	if err != nil {
		return c.StartSessionResponse{}, err
	}
	args = append(args, margs...)
	if err := o.checkNodeLaunchArgs(ctx, nodeID, requested, p); err != nil {
		return c.StartSessionResponse{}, err
	}
	if p.Account, err = o.accountRef(ctx, p.Account, c.ProviderFor(c.Harness(p.Harness))); err != nil {
		return c.StartSessionResponse{}, err
	}
	parent, err := o.startParent(ctx, p.Parent)
	if err != nil {
		return c.StartSessionResponse{}, err
	}
	req := c.StartSessionRequest{
		Mutation: mut(p.OperationID), NodeID: nodeID, Harness: c.Harness(p.Harness), Title: p.Title,
		Cwd: p.Cwd, InitialPrompt: p.Prompt, Tags: p.Tags, IncludeContext: p.IncludeContext,
		ParentSessionID: parent, AccountPolicy: accountPolicy(p.Account, p.Ambient, p.StrictAccount), Args: args,
		ReportToParent: p.ReportToParent,
	}
	if req.ProjectID, err = o.projectPtr(ctx, p.Project); err != nil {
		return c.StartSessionResponse{}, err
	}
	if req.Checkout, req.Cwd, err = o.launchCheckout(ctx, req.ProjectID, nodeID, p); err != nil {
		return c.StartSessionResponse{}, err
	}
	return o.startRecovering(ctx, req)
}

// startParent is the new session's parent: --parent, else the calling
// session, which a client credential never is (an owner command run from
// inside an agent session's shell still sees that session's PYRE_SESSION_ID).
func (o *Ops) startParent(ctx context.Context, ref string) (string, error) {
	if ref = strings.TrimSpace(ref); ref != "" {
		if !o.ClientCredential && o.Self.SessionID != "" {
			// The hub sets a session's own ID as the parent of what it starts.
			return "", usagef("--parent applies to the owner's credential only: a session's starts are always its own children (the hub sets the parent)")
		}
		return o.sessionID(ctx, ref)
	}
	if o.ClientCredential {
		return "", nil
	}
	return o.Self.SessionID, nil
}

// startRecovering sends the start and, when its answer was lost after the
// hub may have stored it (transport failure, hub unavailable or restarting,
// internal error), replays the identical request with the same operation
// ID: the hub then returns the session it created, or creates it once. If
// no answer comes back, the error is indeterminate (exit 7) and names the
// operation ID to retry with, never a blind new start (a duplicate).
//
// The replays are single attempts (the client's own retries would multiply
// the wait) and all fit in StartRecovery.Total, so a hub that hangs costs
// at most about that much more before exit 7.
func (o *Ops) startRecovering(ctx context.Context, req c.StartSessionRequest) (c.StartSessionResponse, error) {
	res, err := o.API.StartSession(ctx, req)
	if err == nil || !startMayHaveApplied(err) {
		return res, err
	}
	rec := startRecoverDefault
	if o.StartRecover != nil {
		rec = *o.StartRecover
	}
	first := err
	if o.Stderr != nil {
		fmt.Fprintf(o.Stderr, "pyre: start: the hub's answer was lost (%s); retrying with operation %s for up to %s\n", oneLine(first), req.OperationID, rec.Total)
	}
	rctx, cancel := context.WithTimeout(client.WithMaxAttempts(ctx, 1), rec.Total)
	defer cancel()
	for _, d := range rec.Delays {
		select {
		case <-rctx.Done():
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			return res, startIndeterminate(req.OperationID, first)
		case <-time.After(d):
		}
		res, err = o.API.StartSession(rctx, req)
		if err == nil {
			return res, nil
		}
		if !startMayHaveApplied(err) {
			return res, err // a definite answer (e.g. the node went away before it was stored)
		}
	}
	return res, startIndeterminate(req.OperationID, first)
}

// startMayHaveApplied: the request may have reached the hub and been
// stored although no answer came back: the hub could not be reached or timed
// out (possibly after committing), a gateway answered for it, or the outcome
// is indeterminate (an internal error is the hub's answer: rolled back). An answer from the hub itself (node disconnected,
// invalid, not found, ...) is definite and not replayed, and neither is a
// failure to reach the local daemon's socket (nothing was sent).
func startMayHaveApplied(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ce *c.Error
	if !errors.As(err, &ce) {
		return true // a deadline or a malformed answer
	}
	switch ce.Code {
	case c.ErrOutcomeIndeterminate:
		return true
	case c.ErrUnavailable:
		switch ce.Details[client.DetailUnreachable] {
		case "hub":
			return true
		case "local_daemon":
			return false
		}
		return strings.HasPrefix(ce.Message, "hub returned HTTP 50") // a gateway's answer, not the hub's
	}
	return false
}

func startIndeterminate(opID string, cause error) error {
	return c.Errorf(c.ErrOutcomeIndeterminate,
		"start: no answer from the hub (%s); the session may have been created. Re-run the same command with --operation-id %s: it returns that session if it exists and never starts a second one (or look for it with `pyre sessions`)",
		oneLine(cause), opID).WithRef(c.KindOperation, opID)
}

// checkNodeLaunchArgs refuses extra harness arguments or a native resume
// the chosen node cannot apply: an older pyred passes start args to the
// harness unchecked and unrecorded (resumes would drop them), and would hand
// --pyre-native-resume to Claude as an unknown option.
func (o *Ops) checkNodeLaunchArgs(ctx context.Context, nodeID, requested string, p StartParams) error {
	if len(p.HarnessArgs) == 0 && p.NativeResume == "" && p.EnvFile == "" {
		return nil
	}
	if p.Harness != requested && (len(p.HarnessArgs) > 0 || p.NativeResume != "") {
		return usagef("no %s account was free and the launch fell back to %s: the extra harness arguments and --native-resume were given for %s; start again with --harness %s", requested, p.Harness, requested, p.Harness)
	}
	n, err := o.ResolveNode(ctx, nodeID)
	if err != nil {
		return err
	}
	h, _ := nodeHarness(n, p.Harness)
	name := n.Name
	if name == "" {
		name = n.NodeID
	}
	if len(p.HarnessArgs) > 0 && c.Harness(p.Harness) != c.HarnessShell && !slices.Contains(h.Capabilities, c.CapLaunchHarnessArgs) {
		return usagef("%s's pyred cannot apply extra harness arguments for %s (an older version: it would pass them unchecked and drop them on resume); upgrade it or pick another machine", name, p.Harness)
	}
	if p.NativeResume != "" && !slices.Contains(h.Capabilities, c.CapResumeOutside) {
		return usagef("%s's pyred cannot continue a conversation started outside Pyre (an older version); upgrade it or pick another machine", name)
	}
	if p.EnvFile != "" && !slices.Contains(h.Capabilities, c.CapLaunchEnvFile) {
		return usagef("%s's pyred cannot apply an environment file for %s (an older version: it would hand --env-file to the harness); upgrade it or pick another machine", name, p.Harness)
	}
	return nil
}

// envFileArgs renders --env-file as the node's start arg (daemon.EnvFileArg).
// The file is read on the node, never here: only its path is sent.
func envFileArgs(p StartParams) ([]string, error) {
	path := strings.TrimSpace(p.EnvFile)
	if path == "" {
		return nil, nil
	}
	if !nodeAbsPath(path) {
		return nil, usagef("--env-file %q must be an absolute path on the node", path)
	}
	return []string{envFileArg + "=" + path}, nil
}

// nodeAbsPath reports whether p is an absolute local path on a Unix or
// Windows node (network and device paths are refused).
func nodeAbsPath(p string) bool {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return false
	}
	return strings.HasPrefix(p, "/") || windowsAbs(p)
}

// windowsAbs reports whether p is an absolute Windows drive path (C:\x or
// C:/x): the node may run another OS than this CLI.
func windowsAbs(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		ch := p[0] | 0x20
		return ch >= 'a' && ch <= 'z'
	}
	return false
}

// envFileArg is the node's env-file start arg (daemon.EnvFileArg);
// envFileNone on a resume drops the session's file.
const (
	envFileArg  = c.StartArgEnvFile
	envFileNone = "none"
)

// startHarnessArgs checks a start's extra harness arguments (the node
// checks again) and returns them in canonical form.
func startHarnessArgs(p StartParams) ([]string, error) {
	if len(p.HarnessArgs) == 0 {
		return nil, nil
	}
	if c.Harness(p.Harness) == c.HarnessShell {
		// A shell's arguments are its command line (`sh -lc 'cmd'`); every
		// node passes them unchanged, as monitors rely on.
		return p.HarnessArgs, nil
	}
	if harnessargs.Passthrough(c.Harness(p.Harness)) && c.Harness(p.Harness) != c.HarnessMock {
		return nil, usagef("--harness-arg applies to agent harnesses (claude) and shell, not %s", p.Harness)
	}
	args, err := harnessargs.Normalize(c.Harness(p.Harness), p.HarnessArgs)
	if err != nil {
		return nil, usagef("%s", c.AsError(err).Message)
	}
	return args, nil
}

// nativeResumeArg mirrors daemon.NativeResumeArg (pyre start --native-resume).
const nativeResumeArg = "--pyre-native-resume"

var nativeSessionRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// permissionsArg is the node's per-launch permission override, carried in
// the start request's args and removed by the node before the harness sees
// them (daemon.PermissionsArg).
const permissionsArg = "--pyre-permissions"

func accountPolicy(account string, ambient, strict bool) c.AccountPolicy {
	switch {
	case account != "":
		return c.AccountPolicy{Mode: c.AccountExplicit, AccountID: account, Strict: strict}
	case ambient:
		return c.AccountPolicy{Mode: c.AccountAmbient}
	}
	return c.AccountPolicy{Mode: c.AccountEligible}
}

// ReadParams reads a transcript (MCP read_session input).
type ReadParams struct {
	Session string `json:"session"`
	Run     string `json:"run,omitempty"`
	Tail    int    `json:"tail,omitempty"`   // latest N entries
	Cursor  string `json:"cursor,omitempty"` // continue after a previous next_cursor
	Limit   int    `json:"limit,omitempty"`
}

func (o *Ops) ReadSession(ctx context.Context, p ReadParams) (c.ReadSessionResponse, error) {
	id, err := o.sessionID(ctx, p.Session)
	if err != nil {
		return c.ReadSessionResponse{}, err
	}
	req := c.ReadSessionRequest{SessionID: id, RunID: p.Run, Cursor: p.Cursor, Limit: p.Limit}
	if p.Tail > 0 && p.Cursor == "" {
		req.Tail, req.Limit = true, p.Tail
	}
	return o.API.ReadSession(ctx, req)
}

// SessionCmdParams drives interrupt/stop.
type SessionCmdParams struct {
	Session     string `json:"session"`
	Reason      string `json:"reason,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

func (o *Ops) Interrupt(ctx context.Context, p SessionCmdParams) (c.CommandReceipt, error) {
	id, err := o.sessionID(ctx, p.Session)
	if err != nil {
		return c.CommandReceipt{}, err
	}
	return o.API.InterruptSession(ctx, c.SessionCommandRequest{Mutation: mut(p.OperationID), SessionID: id, Reason: p.Reason})
}

func (o *Ops) Stop(ctx context.Context, p SessionCmdParams) (c.CommandReceipt, error) {
	id, err := o.sessionID(ctx, p.Session)
	if err != nil {
		return c.CommandReceipt{}, err
	}
	return o.API.StopSession(ctx, c.SessionCommandRequest{Mutation: mut(p.OperationID), SessionID: id, Reason: p.Reason})
}

// ResumeParams creates a new run for a session.
type ResumeParams struct {
	Session       string `json:"session"`
	Node          string `json:"node,omitempty"`
	Account       string `json:"account,omitempty"`
	StrictAccount bool   `json:"strict_account,omitempty"`
	Ambient       bool   `json:"ambient,omitempty"`
	AllowHandoff  bool   `json:"allow_handoff,omitempty"`
	// EnvFile replaces the session's environment file (an absolute path on
	// the node) or, "none", drops it; "" keeps the previous run's.
	EnvFile     string `json:"env_file,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

func (o *Ops) Resume(ctx context.Context, p ResumeParams) (c.StartSessionResponse, error) {
	id, err := o.sessionID(ctx, p.Session)
	if err != nil {
		return c.StartSessionResponse{}, err
	}
	if p.Account, err = o.accountRef(ctx, p.Account, ""); err != nil {
		return c.StartSessionResponse{}, err
	}
	req := c.ResumeRequest{Mutation: mut(p.OperationID), SessionID: id, AllowHandoff: p.AllowHandoff, AccountPolicy: accountPolicy(p.Account, p.Ambient, p.StrictAccount)}
	if req.EnvFile = strings.TrimSpace(p.EnvFile); req.EnvFile != "" && req.EnvFile != envFileNone && !nodeAbsPath(req.EnvFile) {
		return c.StartSessionResponse{}, usagef("--env-file %q must be an absolute path on the node, or none", req.EnvFile)
	}
	if p.Node != "" {
		n, err := o.ResolveNode(ctx, p.Node)
		if err != nil {
			return c.StartSessionResponse{}, err
		}
		req.NodeID = n.NodeID
	}
	return o.API.ResumeSession(ctx, req)
}

// AssociateParams attaches a session to a project, or detaches it.
type AssociateParams struct {
	Session     string `json:"session"`
	Project     string `json:"project,omitempty"`
	None        bool   `json:"none,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

func (o *Ops) Associate(ctx context.Context, p AssociateParams) (c.SessionView, error) {
	if (p.Project == "") == !p.None {
		return c.SessionView{}, usagef("exactly one of --project or --none is required")
	}
	id, err := o.sessionID(ctx, p.Session)
	if err != nil {
		return c.SessionView{}, err
	}
	pid, err := o.projectPtr(ctx, p.Project)
	if err != nil {
		return c.SessionView{}, err
	}
	return o.API.AssociateSession(ctx, c.AssociateRequest{Mutation: mut(p.OperationID), SessionID: id, ProjectID: pid})
}

// ---- messages ----

// SendParams sends a message (MCP send_message input). To may be a session
// ID or title, "owner", "channel:<name>", or a project ID/"project:<name>".
// With ReplyTo and no To, the reply goes to the original sender in the
// original thread.
type SendParams struct {
	To          string   `json:"to,omitempty"`
	Text        string   `json:"text"`
	Thread      string   `json:"thread,omitempty"`
	ReplyTo     string   `json:"reply_to,omitempty"`
	Steer       bool     `json:"steer,omitempty"`
	Info        bool     `json:"info,omitempty"` // stored and visible, never injected into a runtime
	Attach      []string `json:"attach,omitempty"`
	RequestAck  bool     `json:"request_ack,omitempty"`
	Project     string   `json:"project,omitempty"`
	TTLSeconds  int      `json:"ttl_seconds,omitempty"`
	MessageID   string   `json:"message_id,omitempty"` // stable ID for retries across processes
	OperationID string   `json:"operation_id,omitempty"`
}

func (o *Ops) Send(ctx context.Context, p SendParams) (c.MessageView, error) {
	if p.Steer && p.Info {
		return c.MessageView{}, usagef("--steer and --info are mutually exclusive")
	}
	req := c.SendMessageRequest{Mutation: mut(p.OperationID), MessageID: p.MessageID, ThreadID: p.Thread, InReplyTo: p.ReplyTo,
		Body: c.MessageBody{Text: p.Text}, RequestAck: p.RequestAck, TTLSeconds: p.TTLSeconds, Mode: c.ModeQueue}
	if p.Steer {
		req.Mode = c.ModeSteer
	}
	if p.Info {
		req.Mode = c.ModeInfo
	}
	for _, a := range p.Attach {
		att, err := attachmentFor(a)
		if err != nil {
			return c.MessageView{}, err
		}
		req.Attachments = append(req.Attachments, att)
	}
	var err error
	if req.ProjectID, err = o.projectPtr(ctx, p.Project); err != nil {
		return c.MessageView{}, err
	}
	if p.To == "" {
		if p.ReplyTo == "" {
			return c.MessageView{}, usagef("a recipient is required (session, \"owner\", channel:<name>, project:<id>)")
		}
		orig, err := o.API.GetMessage(ctx, c.GetRequest{ID: p.ReplyTo})
		if err != nil {
			return c.MessageView{}, err
		}
		switch orig.Envelope.Sender.Kind {
		case c.PrincipalSession:
			req.Recipient = c.Address{Kind: c.AddrSession, ID: orig.Envelope.Sender.ID}
		case c.PrincipalOwner:
			req.Recipient = c.Address{Kind: c.AddrOwner, ID: "owner"}
		default:
			return c.MessageView{}, usagef("message %s was sent by %s %s; give an explicit recipient", p.ReplyTo, orig.Envelope.Sender.Kind, orig.Envelope.Sender.ID)
		}
		if req.ThreadID == "" {
			req.ThreadID = orig.Envelope.ThreadID
		}
		if req.ProjectID == nil {
			req.ProjectID = orig.Envelope.ProjectID
		}
	} else {
		if req.Recipient, err = o.address(ctx, p.To); err != nil {
			return c.MessageView{}, err
		}
	}
	mv, err := o.API.SendMessage(ctx, req)
	if err == nil {
		o.withDelivery(ctx, &mv, nil) // immediate snapshot (W-stuckmail)
	}
	return mv, err
}

func attachmentFor(id string) (c.Attachment, error) {
	switch c.IDPrefix(id) {
	case c.PrefixVersion:
		return c.Attachment{ArtifactVersionID: id}, nil
	case c.PrefixRevision:
		return c.Attachment{DocumentRevisionID: id}, nil
	case c.PrefixNote:
		return c.Attachment{NoteID: id}, nil
	}
	return c.Attachment{}, usagef("--attach takes a published artifact version (ver_...), document revision (rev_...) or note (note_...) ID; a file path is not transferable — `pyre publish` it first")
}

// address parses a recipient reference.
func (o *Ops) address(ctx context.Context, to string) (c.Address, error) {
	switch {
	case to == "owner":
		return c.Address{Kind: c.AddrOwner, ID: "owner"}, nil
	case strings.HasPrefix(to, "channel:"):
		return c.Address{Kind: c.AddrChannel, ID: strings.TrimPrefix(to, "channel:")}, nil
	case strings.HasPrefix(to, "project:"):
		p, err := o.ResolveProject(ctx, strings.TrimPrefix(to, "project:"))
		return c.Address{Kind: c.AddrProject, ID: p.ProjectID}, err
	case c.ValidID(to, c.PrefixProject):
		return c.Address{Kind: c.AddrProject, ID: to}, nil
	}
	id, err := o.sessionID(ctx, to)
	return c.Address{Kind: c.AddrSession, ID: id}, err
}

// InboxParams lists messages (MCP inbox input). The default mailbox is the
// calling session's inbox, or the owner's for a human.
type InboxParams struct {
	Session     string   `json:"session,omitempty"` // session ref or "owner"
	Box         string   `json:"box,omitempty"`     // inbox (default) | outbox | all
	Thread      string   `json:"thread,omitempty"`
	Unacked     bool     `json:"unacked,omitempty"`
	States      []string `json:"states,omitempty"` // runtime delivery states, e.g. indeterminate
	WaitSeconds int      `json:"wait_seconds,omitempty"`
	AfterCursor int64    `json:"after_cursor,omitempty"`
	Limit       int      `json:"limit,omitempty"`
}

func (o *Ops) Inbox(ctx context.Context, p InboxParams) (c.Page[c.MessageView], error) {
	req := c.ListMessagesRequest{Box: p.Box, ThreadID: p.Thread, Unacked: p.Unacked, AfterCursor: p.AfterCursor, WaitSeconds: p.WaitSeconds}
	if req.Box == "" {
		req.Box = "inbox"
	}
	for _, s := range p.States {
		req.RuntimeStates = append(req.RuntimeStates, c.RuntimeDeliveryState(s))
	}
	if p.Thread == "" {
		switch {
		case p.Session == "owner":
			req.Address = &c.Address{Kind: c.AddrOwner, ID: "owner"}
		case p.Session != "":
			id, err := o.sessionID(ctx, p.Session)
			if err != nil {
				return c.Page[c.MessageView]{}, err
			}
			req.Address = &c.Address{Kind: c.AddrSession, ID: id}
		case o.Self.SessionID != "":
			req.Address = &c.Address{Kind: c.AddrSession, ID: o.Self.SessionID}
		default:
			req.Address = &c.Address{Kind: c.AddrOwner, ID: "owner"}
		}
	}
	if p.WaitSeconds > 0 || p.Limit > 0 {
		// A long-poll or bounded read is one page.
		req.Limit = p.Limit
		pg, err := o.API.ListMessages(ctx, req)
		if pg.Items == nil {
			pg.Items = []c.MessageView{}
		}
		if err == nil {
			o.markRead(ctx, "inbox", pg.Items...)
		}
		return pg, err
	}
	pg, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.MessageView], error) {
		r := req
		r.Cursor = cur
		return o.API.ListMessages(ctx, r)
	})
	if err == nil {
		o.markRead(ctx, "inbox", pg.Items...)
	}
	return pg, err
}

// markRead tells the hub that this session read these messages (their
// bodies were just returned to it), so their pending paste deliveries are
// withdrawn: read = delivered (W-delivery). Only the session's own pending
// mail is reported; the hub verifies the principal. Best effort: a hub
// without messages.mark_read leaves the paste path as before.
func (o *Ops) markRead(ctx context.Context, via string, ms ...c.MessageView) {
	if o.Self.SessionID == "" {
		return
	}
	ra, ok := o.API.(c.MessageReadAPI)
	if !ok {
		return
	}
	var ids []string
	for _, m := range ms {
		e := m.Envelope
		if e.Recipient.Kind != c.AddrSession || e.Recipient.ID != o.Self.SessionID || e.Mode == c.ModeInfo {
			continue
		}
		if m.Status.Runtime == c.DeliveryQueued || m.Status.Runtime == c.DeliveryNodeReceived {
			ids = append(ids, e.MessageID)
		}
	}
	for len(ids) > 0 {
		n := min(len(ids), 200)
		if _, err := ra.MarkMessagesRead(ctx, c.MarkMessagesReadRequest{MessageIDs: ids[:n], Via: via}); err != nil {
			return
		}
		ids = ids[n:]
	}
}

// WaitReplyParams awaits a reply (MCP wait_reply input).
type WaitReplyParams struct {
	MessageID      string `json:"message_id"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // default 60
}

// WaitReplyResult is the original message plus its reply, if one arrived.
// Delivery is the original message's derived delivery status line
// (W-stuckmail, additive), e.g. "queued: recipient's machine server is
// disconnected".
type WaitReplyResult struct {
	Message  c.MessageView  `json:"message"`
	Reply    *c.MessageView `json:"reply,omitempty"`
	Delivery string         `json:"delivery,omitempty"`
}

// WaitReply polls the message until its acknowledgement state says it was
// replied to, then returns the reply. A timeout is ErrUnavailable (retry
// later) with the latest state of the original message in the result and
// its delivery status in the error text. When no reply can arrive without
// someone acting (the recipient ended or was archived, or delivery was
// refused, expired or cancelled) it returns at once with ErrConflict.
func (o *Ops) WaitReply(ctx context.Context, p WaitReplyParams) (WaitReplyResult, error) {
	if p.MessageID == "" {
		return WaitReplyResult{}, usagef("a message ID is required")
	}
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	interval := o.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(timeout)
	var rc recipientCache
	for {
		m, err := o.API.GetMessage(ctx, c.GetRequest{ID: p.MessageID})
		if err != nil {
			return WaitReplyResult{}, err
		}
		res := WaitReplyResult{Message: m}
		if m.Status.Ack == c.AckReplied && m.Status.AckRef != "" {
			r, err := o.API.GetMessage(ctx, c.GetRequest{ID: m.Status.AckRef})
			if err != nil {
				return res, err
			}
			o.markRead(ctx, "wait_reply", r)
			res.Reply = &r
			o.withDelivery(ctx, &res.Message, &rc)
			res.Delivery = deliveryLine(res.Message)
			return res, nil
		}
		o.withDelivery(ctx, &m, &rc)
		res.Message, res.Delivery = m, deliveryLine(m)
		if m.Status.Runtime == c.DeliveryRejected || m.Status.Runtime == c.DeliveryExpired || m.Status.Runtime == c.DeliveryCancelled || m.Status.Storage == c.StorageRejected {
			return res, c.Errorf(c.ErrConflict, "message %s will not be answered: delivery %s (%s)", p.MessageID, m.Status.Runtime, m.Status.RuntimeReason).WithRef(c.KindMessage, p.MessageID)
		}
		if m.Delivery != nil && m.Delivery.Final {
			// The recipient ended or was archived: waiting longer cannot help.
			return res, c.Errorf(c.ErrConflict, "message %s will not be answered unless someone acts: %s", p.MessageID, m.Delivery.Summary).WithRef(c.KindMessage, p.MessageID)
		}
		left := time.Until(deadline)
		if left <= 0 {
			if res.Delivery != "" {
				return res, c.Errorf(c.ErrUnavailable, "no reply to %s within %s; your message is %s", p.MessageID, timeout, stillPhrase(res.Delivery)).WithRef(c.KindMessage, p.MessageID)
			}
			return res, c.Errorf(c.ErrUnavailable, "no reply to %s within %s (delivery: %s, ack: %s)", p.MessageID, timeout, m.Status.Runtime, m.Status.Ack).WithRef(c.KindMessage, p.MessageID)
		}
		t := time.NewTimer(min(interval, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return res, ctx.Err()
		case <-t.C:
		}
	}
}

// MessageRef identifies a message (MCP get/ack input).
type MessageRef struct {
	MessageID   string `json:"message_id"`
	Note        string `json:"note,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

func (o *Ops) GetMessage(ctx context.Context, p MessageRef) (c.MessageView, error) {
	m, err := o.API.GetMessage(ctx, c.GetRequest{ID: p.MessageID})
	if err == nil {
		o.markRead(ctx, "get_message", m)
	}
	return m, err
}

func (o *Ops) Ack(ctx context.Context, p MessageRef) (c.MessageView, error) {
	return o.API.AckMessage(ctx, c.AckMessageRequest{Mutation: mut(p.OperationID), MessageID: p.MessageID, Note: p.Note})
}

func (o *Ops) Cancel(ctx context.Context, p MessageRef) (c.MessageView, error) {
	return o.API.CancelMessage(ctx, c.CancelMessageRequest{Mutation: mut(p.OperationID), MessageID: p.MessageID})
}

// ReissueParams explicitly re-sends uncertain work as a new message.
type ReissueParams struct {
	MessageID           string `json:"message_id"`
	AcceptDuplicateRisk bool   `json:"accept_duplicate_risk"`
	Text                string `json:"text,omitempty"`
	OperationID         string `json:"operation_id,omitempty"`
}

func (o *Ops) Reissue(ctx context.Context, p ReissueParams) (c.MessageView, error) {
	if !p.AcceptDuplicateRisk {
		return c.MessageView{}, usagef("reissue may duplicate work the recipient already started; pass --accept-duplicate-risk to confirm")
	}
	req := c.ReissueMessageRequest{Mutation: mut(p.OperationID), MessageID: p.MessageID, AcceptDuplicateRisk: true}
	if p.Text != "" {
		req.Body = &c.MessageBody{Text: p.Text}
	}
	return o.API.ReissueMessage(ctx, req)
}

// ResolveDeliveryParams records a decision about an indeterminate delivery.
type ResolveDeliveryParams struct {
	MessageID   string `json:"message_id"`
	Resolution  string `json:"resolution"` // accepted | rejected
	Note        string `json:"note,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

func (o *Ops) ResolveDelivery(ctx context.Context, p ResolveDeliveryParams) (c.MessageView, error) {
	r := c.RuntimeDeliveryState(p.Resolution)
	if r != c.DeliveryAccepted && r != c.DeliveryRejected {
		return c.MessageView{}, usagef("resolution must be accepted or rejected")
	}
	return o.API.ResolveDelivery(ctx, c.ResolveDeliveryRequest{Mutation: mut(p.OperationID), MessageID: p.MessageID, Resolution: r, Note: p.Note})
}

// ---- approvals ----

// ApprovalFilter lists approvals.
type ApprovalFilter struct {
	Session string `json:"session,omitempty"`
	All     bool   `json:"all,omitempty"` // include resolved/expired
}

func (o *Ops) ListApprovals(ctx context.Context, f ApprovalFilter) (c.Page[c.Approval], error) {
	req := c.ListApprovalsRequest{}
	if f.Session != "" {
		id, err := o.sessionID(ctx, f.Session)
		if err != nil {
			return c.Page[c.Approval]{}, err
		}
		req.SessionID = id
	}
	if f.All {
		req.States = []c.ApprovalState{c.ApprovalPending, c.ApprovalResolved, c.ApprovalExpired, c.ApprovalWithdrawn}
	} else {
		req.States = []c.ApprovalState{c.ApprovalPending, c.ApprovalExpired}
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Approval], error) {
		r := req
		r.Cursor = cur
		return o.API.ListApprovals(ctx, r)
	})
	if err != nil || f.All {
		return page, err
	}
	// Default list: pending approvals, plus expired ones whose run still
	// waits on the harness's own prompt (the remote answer window closed
	// but the prompt is on screen). Those are resolved in the terminal.
	waiting := map[string]bool{}
	asked := map[string]bool{}
	items := page.Items[:0]
	for _, a := range page.Items {
		if a.State == c.ApprovalExpired {
			if !asked[a.SessionID] {
				asked[a.SessionID] = true
				if v, err := o.API.GetSession(ctx, c.GetRequest{ID: a.SessionID}); err == nil && v.Run != nil && v.Run.State == c.RunWaitingApproval {
					waiting[v.Run.RunID] = true
				}
			}
			if !waiting[a.RunID] {
				continue
			}
		}
		items = append(items, a)
	}
	page.Items = items
	return page, nil
}

// ApproveParams answers one approval for its own run.
type ApproveParams struct {
	ApprovalID  string `json:"approval_id"`
	Option      string `json:"option,omitempty"`
	Text        string `json:"text,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// Approve binds the answer to the approval's originating run, so it can
// never land in a different pending prompt.
func (o *Ops) Approve(ctx context.Context, p ApproveParams) (c.Approval, error) {
	a, err := o.API.GetApproval(ctx, c.GetRequest{ID: p.ApprovalID})
	if err != nil {
		return c.Approval{}, err
	}
	if p.Option == "" && p.Text == "" {
		return a, usagef("give --option (one of %s) or --text", optionIDs(a))
	}
	if p.Option != "" && len(a.Options) > 0 {
		ok := false
		for _, op := range a.Options {
			ok = ok || op.ID == p.Option
		}
		if !ok {
			return a, usagef("option %q is not offered (options: %s)", p.Option, optionIDs(a))
		}
	}
	return o.API.AnswerApproval(ctx, c.AnswerApprovalRequest{Mutation: mut(p.OperationID), ApprovalID: a.ApprovalID, RunID: a.RunID, OptionID: p.Option, Text: p.Text})
}

func optionIDs(a c.Approval) string {
	ids := make([]string, len(a.Options))
	for i, op := range a.Options {
		ids[i] = op.ID
	}
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// ---- documents ----

// DocRef addresses a document by ID or by project + slug.
type DocRef struct {
	Document string `json:"document"`
	Project  string `json:"project,omitempty"`
	Revision string `json:"revision,omitempty"`
}

func (o *Ops) GetDocument(ctx context.Context, p DocRef) (c.DocumentWithRevision, error) {
	if p.Document == "" {
		return c.DocumentWithRevision{}, usagef("a document ID or slug is required")
	}
	if c.ValidID(p.Document, c.PrefixDocument) {
		return o.API.GetDocument(ctx, c.GetDocumentRequest{DocumentID: p.Document, RevisionID: p.Revision})
	}
	if p.Project == "" {
		return c.DocumentWithRevision{}, usagef("addressing a document by slug (%q) requires --project", p.Document)
	}
	pr, err := o.ResolveProject(ctx, p.Project)
	if err != nil {
		return c.DocumentWithRevision{}, err
	}
	return o.API.GetDocument(ctx, c.GetDocumentRequest{ProjectID: pr.ProjectID, Slug: p.Document, RevisionID: p.Revision})
}

// DocFilter lists documents.
type DocFilter struct {
	Project string `json:"project,omitempty"`
}

func (o *Ops) ListDocuments(ctx context.Context, f DocFilter) (c.Page[c.Document], error) {
	pid, err := o.projectPtr(ctx, f.Project)
	if err != nil {
		return c.Page[c.Document]{}, err
	}
	return listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Document], error) {
		return o.API.ListDocuments(ctx, c.ListDocumentsRequest{PageRequest: c.PageRequest{Cursor: cur}, ProjectID: pid})
	})
}

// ProposeParams proposes a successor revision against a known base.
type ProposeParams struct {
	DocRef
	Base        string `json:"base_revision"`
	Content     string `json:"content"`
	Summary     string `json:"summary,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

func (o *Ops) Propose(ctx context.Context, p ProposeParams) (c.DocumentWithRevision, error) {
	docID := p.Document
	if !c.ValidID(docID, c.PrefixDocument) {
		d, err := o.GetDocument(ctx, DocRef{Document: p.Document, Project: p.Project})
		if err != nil {
			return c.DocumentWithRevision{}, err
		}
		docID = d.Document.DocumentID
	}
	if p.Base == "" {
		return c.DocumentWithRevision{}, usagef("a base revision is required (the revision your edit started from)")
	}
	return o.API.ProposeRevision(ctx, c.ProposeRevisionRequest{Mutation: mut(p.OperationID), DocumentID: docID, BaseRevisionID: p.Base, Content: p.Content, Summary: p.Summary})
}

// ---- artifacts ----

// PublishParams publishes a local file (MCP publish input).
type PublishParams struct {
	Path        string `json:"path"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Project     string `json:"project,omitempty"`
	ArtifactID  string `json:"artifact_id,omitempty"` // publish a new version of this artifact (ID or title)
	Review      bool   `json:"review,omitempty"`      // ask the owner to review this version
	Note        string `json:"note,omitempty"`
	MediaType   string `json:"media_type,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// Publish uploads a file and returns the durable receipt. It reports
// success only when the hub's receipt records exactly the bytes hashed
// locally; any mismatch is an error and nothing is reported as published.
func (o *Ops) Publish(ctx context.Context, p PublishParams) (c.PublishReceipt, error) {
	if p.Path == "" {
		return c.PublishReceipt{}, usagef("a file to publish is required")
	}
	abs, err := filepath.Abs(p.Path)
	if err != nil {
		return c.PublishReceipt{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return c.PublishReceipt{}, c.Errorf(c.ErrInvalid, "publish: %v", err)
	}
	if !st.Mode().IsRegular() {
		return c.PublishReceipt{}, c.Errorf(c.ErrInvalid, "publish: %s is not a regular file", p.Path)
	}
	digest, size, err := fileDigest(abs)
	if err != nil {
		return c.PublishReceipt{}, err
	}
	pid, err := o.projectPtr(ctx, p.Project)
	if err != nil {
		return c.PublishReceipt{}, err
	}
	if p.ArtifactID, err = o.artifactID(ctx, p.ArtifactID); err != nil {
		return c.PublishReceipt{}, err
	}
	title := p.Title
	if title == "" && p.ArtifactID == "" {
		title = filepath.Base(abs)
	}
	// Without an explicit --media-type the hub decides (its pinned
	// extension table, then content sniffing): the CLI's own lookup reads
	// platform mime tables and gave the same file different types on macOS
	// and Linux.
	mt := p.MediaType
	meta := client.PublishMeta{
		ArtifactID: p.ArtifactID, ProjectID: pid, Title: title, Description: p.Description,
		Filename: filepath.Base(abs), MediaType: mt, Note: p.Note, OperationID: p.OperationID, ReviewRequested: p.Review,
		Provenance: c.Provenance{SessionID: o.Self.SessionID, RunID: o.Self.RunID, NodeID: o.Self.NodeID, ProjectID: pid, SourcePath: abs},
	}
	rec, err := client.PublishFile(ctx, o.API, abs, meta)
	if err != nil {
		return c.PublishReceipt{}, err
	}
	if !strings.EqualFold(string(rec.Version.Digest), string(digest)) || rec.Version.Size != size {
		return c.PublishReceipt{}, c.Errorf(c.ErrConflict,
			"publish: hub receipt records %s (%d bytes) but the local file is %s (%d bytes); not treating this as published",
			rec.Version.Digest, rec.Version.Size, digest, size).WithRef(c.KindArtifactVersion, rec.Version.VersionID)
	}
	return rec, nil
}

func fileDigest(path string) (c.Digest, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, c.Errorf(c.ErrInvalid, "publish: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, c.Errorf(c.ErrInvalid, "publish: read %s: %v", path, err)
	}
	return c.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), n, nil
}

func detectMediaType(path string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		return t
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown"
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	return http.DetectContentType(buf[:n])
}

// ArtifactFilter lists artifacts.
type ArtifactFilter struct {
	Project string `json:"project,omitempty"`
	Session string `json:"session,omitempty"`
}

func (o *Ops) ListArtifacts(ctx context.Context, f ArtifactFilter) (c.Page[c.Artifact], error) {
	pid, err := o.projectPtr(ctx, f.Project)
	if err != nil {
		return c.Page[c.Artifact]{}, err
	}
	sid := ""
	if f.Session != "" {
		if sid, err = o.sessionID(ctx, f.Session); err != nil {
			return c.Page[c.Artifact]{}, err
		}
	}
	return listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Artifact], error) {
		return o.API.ListArtifacts(ctx, c.ListArtifactsRequest{PageRequest: c.PageRequest{Cursor: cur}, ProjectID: pid, SessionID: sid})
	})
}

// ---- review ----

// CommentParams comments on an exact version/revision (MCP comment input).
type CommentParams struct {
	Target      string   `json:"target"` // ver_... or rev_...
	Text        string   `json:"text"`
	Anchor      string   `json:"anchor,omitempty"` // "line:12-18", "quote:...", "page:3" or a bare value
	ReplyTo     string   `json:"reply_to,omitempty"`
	Mentions    []string `json:"mentions,omitempty"` // session IDs or "owner"
	OperationID string   `json:"operation_id,omitempty"`
}

func targetRef(id string) (c.ObjectRef, error) {
	switch c.IDPrefix(id) {
	case c.PrefixVersion:
		return c.ObjectRef{Kind: c.KindArtifactVersion, ID: id}, nil
	case c.PrefixRevision:
		return c.ObjectRef{Kind: c.KindDocumentRevision, ID: id}, nil
	}
	return c.ObjectRef{}, usagef("comments target an exact artifact version (ver_...) or document revision (rev_...), got %q", id)
}

func parseAnchor(s string) *c.Anchor {
	if s == "" {
		return nil
	}
	if k, v, ok := strings.Cut(s, ":"); ok {
		switch k {
		case "line", "range", "quote", "page", "region":
			return &c.Anchor{Kind: k, Value: v}
		}
	}
	return &c.Anchor{Kind: "quote", Value: s}
}

func (o *Ops) Comment(ctx context.Context, p CommentParams) (c.ReviewComment, error) {
	t, err := targetRef(p.Target)
	if err != nil {
		return c.ReviewComment{}, err
	}
	if strings.TrimSpace(p.Text) == "" {
		return c.ReviewComment{}, usagef("comment text is required")
	}
	return o.API.CreateComment(ctx, c.CreateCommentRequest{Mutation: mut(p.OperationID), Target: t, Anchor: parseAnchor(p.Anchor), Body: p.Text, InReplyTo: p.ReplyTo, Mentions: p.Mentions})
}

// CommentFilter lists comments (MCP list_comments input). Target may be a
// version, revision, artifact (art_...) or document (doc_...) ID.
type CommentFilter struct {
	Target     string `json:"target,omitempty"`
	Project    string `json:"project,omitempty"`
	Unresolved bool   `json:"unresolved,omitempty"`
}

func (o *Ops) ListComments(ctx context.Context, f CommentFilter) (c.Page[c.ReviewComment], error) {
	req := c.ListCommentsRequest{Unresolved: f.Unresolved}
	switch c.IDPrefix(f.Target) {
	case "":
	case c.PrefixArtifact:
		req.ArtifactID = f.Target
	case c.PrefixDocument:
		req.DocumentID = f.Target
	default:
		t, err := targetRef(f.Target)
		if err != nil {
			return c.Page[c.ReviewComment]{}, err
		}
		req.Target = &t
	}
	var err error
	if req.ProjectID, err = o.projectPtr(ctx, f.Project); err != nil {
		return c.Page[c.ReviewComment]{}, err
	}
	return listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.ReviewComment], error) {
		r := req
		r.Cursor = cur
		return o.API.ListComments(ctx, r)
	})
}

// ---- notes and context ----

// NoteParams records a note, handoff or decision (MCP create_note input).
type NoteParams struct {
	Title       string `json:"title"`
	Body        string `json:"body"`
	Kind        string `json:"kind,omitempty"` // note (default) | handoff | decision
	Project     string `json:"project,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

func (o *Ops) CreateNote(ctx context.Context, p NoteParams) (c.Note, error) {
	kind := c.NoteKind(p.Kind)
	if kind == "" {
		kind = c.NoteNote
	}
	if kind != c.NoteNote && kind != c.NoteHandoff && kind != c.NoteDecision {
		return c.Note{}, usagef("note kind must be note, handoff or decision")
	}
	if strings.TrimSpace(p.Title) == "" {
		return c.Note{}, usagef("a note title is required")
	}
	pid, err := o.projectPtr(ctx, p.Project)
	if err != nil {
		return c.Note{}, err
	}
	return o.API.CreateNote(ctx, c.CreateNoteRequest{Mutation: mut(p.OperationID), Kind: kind, ProjectID: pid, SessionID: o.Self.SessionID, Title: p.Title, Body: p.Body})
}

// ContextParams asks for a bounded launch brief (MCP get_context input).
type ContextParams struct {
	Project  string `json:"project,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

func (o *Ops) Context(ctx context.Context, p ContextParams) (c.LaunchContext, error) {
	pid, err := o.projectPtr(ctx, p.Project)
	if err != nil {
		return c.LaunchContext{}, err
	}
	req := c.ContextRequest{ProjectID: pid, MaxBytes: p.MaxBytes}
	if pid == nil {
		req.SessionID = o.Self.SessionID
	}
	return o.API.GetLaunchContext(ctx, req)
}

// ---- node actions ----

// NodeAction invokes a typed action and decodes a succeeded result into
// out. A non-succeeded outcome is an error whose exit code reflects it
// (indeterminate stays indeterminate).
func (o *Ops) NodeAction(ctx context.Context, node, action string, params any, out any, opID string) (c.NodeActionResult, error) {
	nodeID, err := o.NodeID(ctx, node)
	if err != nil {
		return c.NodeActionResult{}, err
	}
	var raw c.RawJSON
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return c.NodeActionResult{}, err
		}
		raw = b
	}
	res, err := o.API.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: mut(opID), NodeID: nodeID, Action: action, Params: raw, TimeoutSeconds: 30})
	if err != nil {
		return res, err
	}
	if res.Error != nil {
		return res, res.Error
	}
	if res.Outcome != c.OutcomeSucceeded {
		switch res.Outcome {
		case c.OutcomeIndeterminate, c.OutcomePending, c.OutcomeDelivered:
			return res, c.Errorf(c.ErrOutcomeIndeterminate, "%s on %s: outcome %s (command %s); check `pyre nodes command %s` before retrying", action, nodeID, res.Outcome, res.CommandID, res.CommandID).WithRef(c.KindCommand, res.CommandID)
		case c.OutcomeUnsupported:
			return res, c.Errorf(c.ErrUnsupported, "%s is not supported on %s", action, nodeID)
		}
		return res, c.Errorf(c.ErrInternal, "%s on %s failed (command %s)", action, nodeID, res.CommandID)
	}
	if out != nil && len(res.Result) > 0 {
		if err := json.Unmarshal(res.Result, out); err != nil {
			return res, c.Errorf(c.ErrInternal, "decode %s result: %v", action, err)
		}
	}
	return res, nil
}

// MemorySearchParams searches shared memory (MCP memory_search input).
type MemorySearchParams struct {
	Query   string `json:"query"`
	Node    string `json:"node,omitempty"`
	Project string `json:"project,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func (o *Ops) MemorySearch(ctx context.Context, p MemorySearchParams) (c.MemorySearchResult, error) {
	if strings.TrimSpace(p.Query) == "" {
		return c.MemorySearchResult{}, usagef("a search query is required")
	}
	params := c.MemorySearchParams{Query: p.Query, Limit: p.Limit}
	if p.Project != "" {
		pid, err := o.projectPtr(ctx, p.Project)
		if err != nil {
			return c.MemorySearchResult{}, err
		}
		params.ProjectID = *pid
	}
	var out c.MemorySearchResult
	_, err := o.NodeAction(ctx, p.Node, c.ActionMemorySearch, params, &out, "")
	if out.Hits == nil {
		out.Hits = []c.MemoryHit{}
	}
	return out, err
}

// ---- small helpers ----

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
