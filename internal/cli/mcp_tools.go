package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"

	c "github.com/jhh3/pyre/internal/contracts"
)

// MCP protocol versions this server accepts, newest first. A client asking
// for any other version (including 2026-07-28, which Claude Code does not
// register for channel servers) is answered with the newest listed here.
// The stateless 2026-07-28 probe `server/discover` gets -32601, the
// correct "legacy handshake only" signal.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// ToolAnnotations are MCP behaviour hints (advisory, not security). All
// four hints are always emitted so clients never fall back to the spec
// defaults (which assume destructive, non-idempotent, open-world).
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// MCPTool is one tool as listed by tools/list (and agents/tools/pyre-mcp-tools.json).
type MCPTool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description"`
	InputSchema  map[string]any   `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`

	run func(ctx context.Context, ops *Ops, args json.RawMessage) (any, error)
	// note optionally returns a short line sent as an extra text block
	// after a successful result (e.g. send_message's delivery status).
	note func(result any) string
}

// withNote attaches a result note to a tool.
func withNote(t MCPTool, note func(result any) string) MCPTool {
	t.note = note
	return t
}

// behaviour classes. Pyre tools act only on the owner's own fleet, hub and
// memory, so none is open-world. Mutations that create new records (a
// message, session, comment, artifact version) are not idempotent unless
// the caller passes operation_id; the hint describes a call without it.
type behaviour int

const (
	readOnly    behaviour = iota // no side effects
	additive                     // creates new records; nothing is lost
	idempotent                   // repeating it has no further effect
	destructive                  // may end work, authorize actions, or duplicate effects
)

func hints(title string, b behaviour, idem bool) *ToolAnnotations {
	a := &ToolAnnotations{Title: title}
	switch b {
	case readOnly:
		a.ReadOnlyHint, a.IdempotentHint = true, true
	case idempotent:
		a.IdempotentHint = true
	case destructive:
		a.DestructiveHint, a.IdempotentHint = true, idem
	}
	return a
}

// tool builds an MCPTool whose arguments decode strictly into P — the same
// parameter struct the CLI command fills — and are passed to fn. The
// output schema is derived from R, the contract type fn returns, and the
// result is sent as structuredContent conforming to it.
func tool[P, R any](name, title, desc string, in map[string]any, ann *ToolAnnotations, fn func(ctx context.Context, ops *Ops, p P) (R, error)) MCPTool {
	t := MCPTool{Name: name, Title: title, Description: desc, InputSchema: in, Annotations: ann,
		OutputSchema: OutputSchemaFor(reflect.TypeFor[R]())}
	t.run = func(ctx context.Context, ops *Ops, args json.RawMessage) (any, error) {
		var p P
		if trimmed := bytes.TrimSpace(args); len(trimmed) > 0 && string(trimmed) != "null" {
			dec := json.NewDecoder(bytes.NewReader(args))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&p); err != nil {
				return nil, usagef("invalid arguments for %s: %v", name, err)
			}
		}
		r, err := fn(ctx, ops, p)
		if err != nil {
			if reflect.ValueOf(&r).Elem().IsZero() {
				return nil, err
			}
			return r, err
		}
		return r, nil
	}
	return t
}

type props map[string]any

func obj(p props, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": map[string]any(p), "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func boo(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

var opIDProp = str("optional stable ID; repeating a call with the same ID returns the original result instead of acting twice")

type emptyParams struct{}

// MCPTools returns the tool set. Tools call the same Ops methods as the CLI.
func MCPTools() []MCPTool {
	sess := str("session ID (ses_...), exact title, or \"self\"")
	node := str("node ID (node_...) or name; default: this session's node")
	project := str("project ID (prj_...) or name")
	ts := []MCPTool{
		tool("whoami", "Who am I", "Show the identity Pyre authenticates you as (an agent session or the owner).", obj(props{}),
			hints("Who am I", readOnly, true),
			func(ctx context.Context, o *Ops, _ emptyParams) (c.Principal, error) { return o.API.WhoAmI(ctx) }),
		compactly(tool("list_nodes", "List machines", "List machines with connectivity, harnesses and load. Offline does not mean idle.",
			obj(props{"include_revoked": boo("include revoked nodes")}), hints("List machines", readOnly, true),
			func(ctx context.Context, o *Ops, p struct {
				IncludeRevoked bool `json:"include_revoked"`
			}) (c.Page[c.Node], error) {
				return o.ListNodes(ctx, p.IncludeRevoked)
			}), renderNodes),
		tool("list_models", "List models", "List the models and reasoning efforts each machine's agent harnesses offer, for start_session model and effort (any name the harness accepts also works).",
			obj(props{"node": str("only this node (ID or name)"), "harness": str("only this harness: claude, codex or agy")}), hints("List models", readOnly, true),
			func(ctx context.Context, o *Ops, p struct {
				Node    string `json:"node"`
				Harness string `json:"harness"`
			}) (ModelsResult, error) {
				l, err := o.Models(ctx, p.Node, p.Harness)
				return ModelsResult{Harnesses: l}, err
			}),
		compactly(tool("list_sessions", "List sessions", "List sessions across the fleet: one line each with id, title, run state, machine (\"this machine\" shares your filesystem), age and project.",
			obj(props{"node": node, "project": str("project ID/name, or \"none\" for standalone sessions"), "harness": str("claude, codex, agy or shell"),
				"query": str("text search"), "states": strs("run states, e.g. running, waiting_input"), "include_archived": boo("include archived"),
				"include_monitors": boo("include monitor sessions (pyre monitor add); by default each session shows only how many monitors watch for it")}),
			hints("List sessions", readOnly, true),
			func(ctx context.Context, o *Ops, p SessionFilter) (c.Page[c.SessionView], error) {
				return o.ListSessions(ctx, p)
			}), renderSessions),
		compactly(tool("get_session", "Show session", "Get one session with its current run, capabilities, lease and pending approvals.",
			obj(props{"session": sess}, "session"), hints("Show session", readOnly, true),
			func(ctx context.Context, o *Ops, p struct {
				Session string `json:"session"`
			}) (c.SessionView, error) {
				return o.ResolveSession(ctx, p.Session)
			}), renderSession),
		tool("start_session", "Start session", "Start a peer session on a machine (no project needed). Waits up to 20 s; a failed launch is an error. It messages you automatically when it finishes a turn (with its last message), needs input, or exits; no need to ask it to.",
			obj(props{"node": str("node ID (node_...) or name; default: the least loaded connected node with the harness (with project+repo: among nodes holding that checkout; with cwd: this session's node); the result's placement says which and why"), "harness": str("claude, codex, agy or shell (agy: Google Antigravity, on machines that have it)"), "cwd": str("absolute working directory on the node"),
				"project": project, "worktree": str("branch for an isolated git worktree"), "repo": str("with project: project repo ID (its recorded checkout, or the worktree source); without project: a repository name from the repo index, fuzzy-matched (the machine defaults to where it exists, the harness to claude, the directory to its main checkout)"),
				"repos": strs("several repository names from the repo index: fresh worktrees of each on one branch in ~/pyre-work/<slug>/"), "fresh": boo("start in a new worktree and branch of repo instead of its main checkout"),
				"branch":           str("branch for the fresh worktree (default pyre/<slug of the prompt or title>)"),
				"account":          str("explicit account: ID, label or rota name, or a unique prefix; at its limit the best eligible account is used instead (with a note) unless strict_account"),
				"strict_account":   boo("keep account even when it is at its limit or ineligible (the launch fails)"),
				"fallback_harness": str("claude: when no Codex (OpenAI) account is free, run on Claude instead; use for reviews and second opinions. Default: a Codex launch with no free account fails at once with the reason"),
				"ambient":          boo("use the node's existing login"), "prompt": str("initial prompt"),
				"permissions": str("ask (approval prompts) or bypass (no approval prompts, no sandbox); default: the node's setting"),
				"model":       str("model (default: the harness's own); claude takes opus/sonnet/haiku/fable or a full name; choices per machine: list_models"),
				"effort":      str("reasoning effort: low, medium, high, xhigh or max (codex also ultra); default: the harness's own"),
				"title":       str("session title"), "include_context": boo("include the bounded project brief"), "tags": strs("tags"),
				"report_to_parent": boo("report back to the starting session automatically (default true when a session starts it; false turns it off)"),
				"harness_args":     strs("extra harness flags from an allowlist, e.g. claude --append-system-prompt-file=F, --plugin-dir=D, --dangerously-load-development-channels=plugin:N@M; kept across resumes; Pyre-controlled flags are refused"),
				"native_resume":    str("claude: continue this existing conversation (session id) started outside Pyre; stop its process first"),
				"env_file":         str("absolute path on the node to KEY=VALUE lines added to the harness environment at every launch (a private file: chmod 600; Pyre and login keys refused unless empty); only the path is recorded"),
				"operation_id":     opIDProp}, "harness"),
			hints("Start session", additive, false),
			func(ctx context.Context, o *Ops, p StartParams) (StartResult, error) {
				var pl Placement
				res, err := o.StartSession(withPlacement(ctx, &pl), p)
				if err == nil {
					err = o.awaitLaunch(ctx, &res, o.LaunchWait) // a failed launch is an error now, not a report later
				}
				out := StartResult{StartSessionResponse: res}
				if pl.NodeID != "" {
					out.Placement = &pl
				}
				return out, err
			}),
		compactly(tool("read_session", "Read transcript", "Read a session's bounded, redacted transcript. Use next_cursor to continue.",
			obj(props{"session": sess, "run": str("run ID (default current)"), "tail": num("latest N entries"),
				"cursor": str("continue after a previous next_cursor"), "limit": num("max entries")}, "session"),
			hints("Read transcript", readOnly, true),
			func(ctx context.Context, o *Ops, p ReadParams) (c.ReadSessionResponse, error) {
				if p.Tail == 0 && p.Cursor == "" {
					p.Tail = 40
				}
				return o.ReadSession(ctx, p)
			}), renderTranscript),
		tool("interrupt_session", "Interrupt session", "Interrupt a session's current turn (for an idle Codex session, also stop the background commands it left running). The receipt reports acceptance; the outcome is observed separately.",
			obj(props{"session": sess, "reason": str("reason"), "operation_id": opIDProp}, "session"),
			hints("Interrupt session", destructive, false),
			func(ctx context.Context, o *Ops, p SessionCmdParams) (c.CommandReceipt, error) {
				return receiptResult(o.Interrupt(ctx, p))
			}),
		tool("stop_session", "Stop session", "Stop a session's current run (the session stays resumable).",
			obj(props{"session": sess, "reason": str("reason"), "operation_id": opIDProp}, "session"),
			hints("Stop session", destructive, true),
			func(ctx context.Context, o *Ops, p SessionCmdParams) (c.CommandReceipt, error) {
				return receiptResult(o.Stop(ctx, p))
			}),
		withNote(tool("send_message", "Send message", "Send a durable message to a session, \"owner\", channel:<name> or project; reply_to alone replies to its sender. delivery_status.summary says if it gets through. A report to the owner that asks nothing is an update, not in his inbox.",
			obj(props{"to": str("session ID/title, \"owner\", channel:<name>, or project:<id>"), "text": str("message text"),
				"thread": str("thread ID"), "reply_to": str("message ID this replies to"), "steer": boo("explicit immediate steering, where supported"),
				"info": boo("store only; never inject into a runtime"), "attach": strs("published IDs: ver_..., rev_..., note_... (not file paths)"),
				"request_ack": boo("ask the recipient to acknowledge (to the owner: keeps a report in his inbox)"), "project": project, "ttl_seconds": num("retention for undelivered mail"),
				"message_id": str("stable message ID for retries"), "operation_id": opIDProp}, "text"),
			hints("Send message", additive, false),
			func(ctx context.Context, o *Ops, p SendParams) (c.MessageView, error) { return o.Send(ctx, p) }),
			sendNote),
		compactly(tool("inbox", "Inbox", "List unacked messages addressed to this session (or a given mailbox), one line each; get_message <id> shows a full body. wait_seconds long-polls for new mail.",
			obj(props{"session": str("mailbox: session ref or \"owner\" (default: this session)"), "box": str("inbox (default), outbox or all"),
				"thread": str("list a whole thread instead"), "unacked": boo("only messages without ack/reply (default true for an inbox; false lists handled mail too)"),
				"states": strs("runtime delivery states, e.g. indeterminate"), "wait_seconds": num("long-poll up to N seconds (max 60)"),
				"after_cursor": num("only messages after this inbox cursor"), "limit": num("max messages")}),
			hints("Inbox", readOnly, true),
			func(ctx context.Context, o *Ops, mp mcpInboxParams) (c.Page[c.MessageView], error) {
				p := mp.effective()
				p.WaitSeconds = min(p.WaitSeconds, 60)
				return o.Inbox(ctx, p)
			}), renderInbox),
		tool("wait_reply", "Wait for reply", "Wait for a reply to a message you sent. Times out with an error saying whether your message is still queued and why; returns at once if the recipient ended or was archived.",
			obj(props{"message_id": str("message ID"), "timeout_seconds": num("default 60, max 300")}, "message_id"),
			hints("Wait for reply", readOnly, true),
			func(ctx context.Context, o *Ops, p WaitReplyParams) (WaitReplyResult, error) {
				p.TimeoutSeconds = min(p.TimeoutSeconds, 300)
				return o.WaitReply(ctx, p)
			}),
		compactly(tool("get_message", "Show message", "Show a message: sender, thread, state and full body (format json adds every delivery fact and receipt). Indeterminate means unknown, not failed.",
			obj(props{"message_id": str("message ID")}, "message_id"), hints("Show message", readOnly, true),
			func(ctx context.Context, o *Ops, p MessageRef) (c.MessageView, error) { return o.GetMessage(ctx, p) }), renderMessage),
		tool("ack_message", "Acknowledge message", "Acknowledge a message without replying.",
			obj(props{"message_id": str("message ID"), "note": str("optional note"), "operation_id": opIDProp}, "message_id"),
			hints("Acknowledge message", idempotent, true),
			func(ctx context.Context, o *Ops, p MessageRef) (c.MessageView, error) { return o.Ack(ctx, p) }),
		tool("reissue_message", "Reissue message", "Re-send uncertain or rejected work as a new message referencing the original. Requires accept_duplicate_risk=true; never use as a blind retry.",
			obj(props{"message_id": str("original message ID"), "accept_duplicate_risk": boo("must be true: the recipient may act twice"),
				"text": str("replacement body (default original)"), "operation_id": opIDProp}, "message_id", "accept_duplicate_risk"),
			hints("Reissue message", destructive, false),
			func(ctx context.Context, o *Ops, p ReissueParams) (c.MessageView, error) { return o.Reissue(ctx, p) }),
		tool("resolve_delivery", "Resolve delivery", "Record a decision (accepted or rejected) about an indeterminate delivery after inspecting evidence.",
			obj(props{"message_id": str("message ID"), "resolution": str("accepted or rejected"), "note": str("evidence inspected"), "operation_id": opIDProp}, "message_id", "resolution"),
			hints("Resolve delivery", idempotent, true),
			func(ctx context.Context, o *Ops, p ResolveDeliveryParams) (c.MessageView, error) {
				return o.ResolveDelivery(ctx, p)
			}),
		compactly(tool("list_approvals", "List approvals", "List pending approvals/questions (all states with all=true).",
			obj(props{"session": sess, "all": boo("include resolved/expired/withdrawn")}), hints("List approvals", readOnly, true),
			func(ctx context.Context, o *Ops, p ApprovalFilter) (c.Page[c.Approval], error) {
				return o.ListApprovals(ctx, p)
			}), renderApprovals),
		tool("answer_approval", "Answer approval", "Answer an approval by ID; the answer is bound to the approval's own run and refused if stale.",
			obj(props{"approval_id": str("approval ID"), "option": str("option ID"), "text": str("free-text answer where allowed"), "operation_id": opIDProp}, "approval_id"),
			hints("Answer approval", destructive, true),
			func(ctx context.Context, o *Ops, p ApproveParams) (c.Approval, error) { return o.Approve(ctx, p) }),
		compactly(tool("list_projects", "List projects", "List projects (optional shared workspaces).",
			obj(props{"query": str("text search"), "include_archived": boo("include archived")}), hints("List projects", readOnly, true),
			func(ctx context.Context, o *Ops, p struct {
				Query           string `json:"query"`
				IncludeArchived bool   `json:"include_archived"`
			}) (c.Page[c.Project], error) {
				return listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Project], error) {
					return o.API.ListProjects(ctx, c.ListProjectsRequest{PageRequest: c.PageRequest{Cursor: cur}, Query: p.Query, IncludeArchived: p.IncludeArchived})
				})
			}), renderProjects),
		tool("create_project", "Create project", "Create a project (an optional shared workspace). Only name is required; repos and checkouts are optional.",
			obj(props{"name": str("project name"), "description": str("short description"), "brief": str("brief text; becomes the brief document"),
				"repos": strs("repo bindings as id=url"), "links": strs("links as title=url"),
				"checkouts":    strs("checkouts as node:/abs/path:repo:mode[:branch] (mode shared|worktree|plain; repo is one of repos)"),
				"operation_id": opIDProp}, "name"),
			hints("Create project", additive, false),
			func(ctx context.Context, o *Ops, p CreateProjectParams) (c.Project, error) {
				return o.CreateProject(ctx, p)
			}),
		tool("archive_session", "Archive session", "When your work is finished and reported, archive yourself with archive_session (no arguments); a live one archives when its turn ends; sessions under it go with it. Sessions outside your tree go to the owner for approval (approval_ask).",
			obj(props{"session": str("session ID or exact title (default: yourself)"), "operation_id": opIDProp}),
			hints("Archive session", idempotent, true),
			func(ctx context.Context, o *Ops, p ArchiveParams) (ArchiveResult, error) {
				p.WhenDone = true
				sv, err := o.Archive(ctx, p)
				if r, ok := approvalResult(sv, err); ok {
					return r, nil
				}
				if err != nil {
					return ArchiveResult{}, err
				}
				return archiveResult(sv, sv.Session.SessionID == o.Self.SessionID), nil
			}),
		tool("archive_project", "Archive project", "Archive a project. Live sessions in it refuse unless with_sessions: true, which archives its ended sessions now and its live ones when their turns end.",
			obj(props{"project": project, "with_sessions": boo("also archive the project's sessions (live ones when their turns end)"), "operation_id": opIDProp}, "project"),
			hints("Archive project", idempotent, true),
			func(ctx context.Context, o *Ops, p ArchiveProjectParams) (ArchiveProjectResult, error) {
				return o.ArchiveProject(ctx, p)
			}),
		tool("associate_session_project", "Associate session with project", "Attach an existing session to a project, or detach it with none=true.",
			obj(props{"session": sess, "project": project, "none": boo("detach from any project (standalone)"), "operation_id": opIDProp}, "session"),
			hints("Associate session with project", idempotent, true),
			func(ctx context.Context, o *Ops, p AssociateParams) (c.SessionView, error) {
				return o.Associate(ctx, p)
			}),
		tool("list_documents", "List documents", "List versioned documents (optionally in a project).",
			obj(props{"project": project}), hints("List documents", readOnly, true),
			func(ctx context.Context, o *Ops, p DocFilter) (c.Page[c.Document], error) {
				return o.ListDocuments(ctx, p)
			}),
		tool("get_document", "Read document", "Read a document (head or a revision). Note revision_id: pass it as base_revision when proposing an edit.",
			obj(props{"document": str("document ID, or slug with project"), "project": project, "revision": str("revision ID (default head)")}, "document"),
			hints("Read document", readOnly, true),
			func(ctx context.Context, o *Ops, p DocRef) (c.DocumentWithRevision, error) {
				return o.GetDocument(ctx, p)
			}),
		tool("propose_document", "Propose document revision", "Propose a successor revision. Commits only if the head is still base_revision; otherwise returns a conflict and preserves your text as a proposal.",
			obj(props{"document": str("document ID, or slug with project"), "project": project, "base_revision": str("revision your edit started from"),
				"content": str("full new content"), "summary": str("revision summary"), "operation_id": opIDProp}, "document", "base_revision", "content"),
			hints("Propose document revision", additive, false),
			func(ctx context.Context, o *Ops, p ProposeParams) (c.DocumentWithRevision, error) {
				return o.Propose(ctx, p)
			}),
		tool("publish", "Publish artifact", "Publish a local file as an immutable, durable artifact version. Returns the receipt; a local path alone is not shared.",
			obj(props{"path": str("local file path"), "title": str("artifact title"), "description": str("description"), "project": project,
				"artifact_id": str("publish a new version of this artifact"), "review": boo("ask the owner to review"), "note": str("version note"),
				"media_type": str("media type (default detected)"), "operation_id": opIDProp}, "path"),
			hints("Publish artifact", additive, false),
			func(ctx context.Context, o *Ops, p PublishParams) (c.PublishReceipt, error) { return o.Publish(ctx, p) }),
		tool("list_artifacts", "List artifacts", "List published artifacts.",
			obj(props{"project": project, "session": sess}), hints("List artifacts", readOnly, true),
			func(ctx context.Context, o *Ops, p ArtifactFilter) (c.Page[c.Artifact], error) {
				return o.ListArtifacts(ctx, p)
			}),
		tool("comment", "Comment", "Comment on an exact artifact version (ver_...) or document revision (rev_...).",
			obj(props{"target": str("ver_... or rev_..."), "text": str("comment"), "anchor": str("line:12-18, quote:TEXT, page:3"),
				"reply_to": str("comment ID"), "mentions": strs("session IDs or \"owner\" to notify"), "operation_id": opIDProp}, "target", "text"),
			hints("Comment", additive, false),
			func(ctx context.Context, o *Ops, p CommentParams) (c.ReviewComment, error) { return o.Comment(ctx, p) }),
		tool("list_comments", "List comments", "List review comments for a version, revision, artifact (art_...) or document (doc_...).",
			obj(props{"target": str("ver_/rev_/art_/doc_ ID"), "project": project, "unresolved": boo("only unresolved")}),
			hints("List comments", readOnly, true),
			func(ctx context.Context, o *Ops, p CommentFilter) (c.Page[c.ReviewComment], error) {
				return o.ListComments(ctx, p)
			}),
		tool("create_note", "Record note", "Record a note, handoff or decision in shared memory. Put pointers (version/note IDs, branches) in handoffs.",
			obj(props{"title": str("title"), "body": str("body"), "kind": str("note (default), handoff or decision"), "project": project, "operation_id": opIDProp}, "title", "body"),
			hints("Record note", additive, false),
			func(ctx context.Context, o *Ops, p NoteParams) (c.Note, error) { return o.CreateNote(ctx, p) }),
		tool("get_context", "Launch context", "Get the bounded launch context: current project brief plus retrieval pointers.",
			obj(props{"project": project, "max_bytes": num("size bound")}), hints("Launch context", readOnly, true),
			func(ctx context.Context, o *Ops, p ContextParams) (c.LaunchContext, error) { return o.Context(ctx, p) }),
		tool("memory_search", "Search memory", "Search shared memory (transcripts, notes, decisions) with provenance and freshness.",
			obj(props{"query": str("search text"), "node": node, "project": project, "limit": num("max hits")}, "query"),
			hints("Search memory", readOnly, true),
			func(ctx context.Context, o *Ops, p MemorySearchParams) (c.MemorySearchResult, error) {
				return o.MemorySearch(ctx, p)
			}),
	}
	for _, p := range mcpToolProviders {
		ts = append(ts, p()...)
	}
	return ts
}

// receiptResult turns a non-success command outcome into a tool error
// while still returning the receipt.
func receiptResult(rc c.CommandReceipt, err error) (c.CommandReceipt, error) {
	if err != nil {
		return c.CommandReceipt{}, err
	}
	if st := outcomeStatus(rc.Outcome, rc.Detail); st != nil {
		return rc, st
	}
	return rc, nil
}
