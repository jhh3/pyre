package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
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
	// PollInterval paces WaitReply (default 1s).
	PollInterval time.Duration
}

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
		return o.API.ListSessions(ctx, c.ListSessionsRequest{PageRequest: c.PageRequest{Cursor: cur}, Query: ref})
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
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return c.Project{}, usagef("a project is required (see `pyre project ls`)")
	}
	if c.ValidID(ref, c.PrefixProject) {
		return o.API.GetProject(ctx, c.GetRequest{ID: ref})
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Project], error) {
		return o.API.ListProjects(ctx, c.ListProjectsRequest{PageRequest: c.PageRequest{Cursor: cur}, IncludeArchived: true})
	})
	if err != nil {
		return c.Project{}, err
	}
	var hits []c.Project
	for _, pass := range []func(a, b string) bool{func(a, b string) bool { return a == b }, strings.EqualFold} {
		for _, p := range page.Items {
			if pass(p.Name, ref) {
				hits = append(hits, p)
			}
		}
		if len(hits) > 0 {
			break
		}
	}
	switch len(hits) {
	case 0:
		return c.Project{}, notFoundf(c.KindProject, ref)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, p := range hits {
		ids[i] = p.ProjectID
	}
	return c.Project{}, ambiguous(c.KindProject, ref, ids)
}

// projectPtr resolves an optional project reference ("" = none given).
func (o *Ops) projectPtr(ctx context.Context, ref string) (*string, error) {
	if ref == "" {
		return nil, nil
	}
	if ref == "none" { // explicitly no project (a child otherwise joins its parent's)
		empty := ""
		return &empty, nil
	}
	if c.ValidID(ref, c.PrefixProject) {
		return &ref, nil
	}
	p, err := o.ResolveProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &p.ProjectID, nil
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
}

func (o *Ops) ListSessions(ctx context.Context, f SessionFilter) (c.Page[c.SessionView], error) {
	req := c.ListSessionsRequest{Harness: c.Harness(f.Harness), Query: f.Query, IncludeArchived: f.IncludeArchived}
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
	return listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
		r := req
		r.Cursor = cur
		return o.API.ListSessions(ctx, r)
	})
}

// StartParams launches a session (MCP start_session input).
type StartParams struct {
	Node           string   `json:"node"`
	Harness        string   `json:"harness"`
	Cwd            string   `json:"cwd,omitempty"`
	Project        string   `json:"project,omitempty"`
	Worktree       string   `json:"worktree,omitempty"`    // branch for an isolated git worktree
	Repo           string   `json:"repo,omitempty"`        // project repo id: its recorded checkout on the node (shared), or the worktree source
	Account        string   `json:"account,omitempty"`     // explicit account: ID, unique label or rota name, or a unique prefix of either
	Ambient        bool     `json:"ambient,omitempty"`     // node's existing login, no Pyre assignment
	Permissions    string   `json:"permissions,omitempty"` // ask | bypass; "" = the node's --permissions
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
}

func validHarness(h string) bool {
	switch c.Harness(h) {
	case c.HarnessClaude, c.HarnessCodex, c.HarnessShell, c.HarnessMock:
		return true
	}
	return false
}

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
		return c.StartSessionResponse{}, usagef("--harness must be claude, codex or shell (got %q)", p.Harness)
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
	nodeID, err := o.NodeID(ctx, p.Node)
	if err != nil {
		return c.StartSessionResponse{}, err
	}
	if p.Account, err = o.accountRef(ctx, p.Account, c.ProviderFor(c.Harness(p.Harness))); err != nil {
		return c.StartSessionResponse{}, err
	}
	req := c.StartSessionRequest{
		Mutation: mut(p.OperationID), NodeID: nodeID, Harness: c.Harness(p.Harness), Title: p.Title,
		Cwd: p.Cwd, InitialPrompt: p.Prompt, Tags: p.Tags, IncludeContext: p.IncludeContext,
		ParentSessionID: o.Self.SessionID, AccountPolicy: accountPolicy(p.Account, p.Ambient), Args: args,
		ReportToParent: p.ReportToParent,
	}
	if req.ProjectID, err = o.projectPtr(ctx, p.Project); err != nil {
		return c.StartSessionResponse{}, err
	}
	if req.Checkout, req.Cwd, err = o.launchCheckout(ctx, req.ProjectID, nodeID, p); err != nil {
		return c.StartSessionResponse{}, err
	}
	return o.API.StartSession(ctx, req)
}

// permissionsArg is the node's per-launch permission override, carried in
// the start request's args and removed by the node before the harness sees
// them (daemon.PermissionsArg).
const permissionsArg = "--pyre-permissions"

func accountPolicy(account string, ambient bool) c.AccountPolicy {
	switch {
	case account != "":
		return c.AccountPolicy{Mode: c.AccountExplicit, AccountID: account}
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
	Session      string `json:"session"`
	Node         string `json:"node,omitempty"`
	Account      string `json:"account,omitempty"`
	Ambient      bool   `json:"ambient,omitempty"`
	AllowHandoff bool   `json:"allow_handoff,omitempty"`
	OperationID  string `json:"operation_id,omitempty"`
}

func (o *Ops) Resume(ctx context.Context, p ResumeParams) (c.StartSessionResponse, error) {
	id, err := o.sessionID(ctx, p.Session)
	if err != nil {
		return c.StartSessionResponse{}, err
	}
	if p.Account, err = o.accountRef(ctx, p.Account, ""); err != nil {
		return c.StartSessionResponse{}, err
	}
	req := c.ResumeRequest{Mutation: mut(p.OperationID), SessionID: id, AllowHandoff: p.AllowHandoff, AccountPolicy: accountPolicy(p.Account, p.Ambient)}
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
