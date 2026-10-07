package attention

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runfail"
)

// Lookup resolves facts that some event payloads do not carry. All methods
// are optional in effect: an error or empty result falls back to "unknown".
type Lookup interface {
	// AccountProvider returns the provider of an account ID.
	AccountProvider(ctx context.Context, accountID string) (contracts.Provider, error)
}

// LabelLookup is optionally implemented by a Lookup to resolve friendly
// names for titles. Without it (or on error) IDs are shown.
type LabelLookup interface {
	// AccountLabel returns the owner-chosen label of an account.
	AccountLabel(ctx context.Context, accountID string) (string, error)
	// NodeName returns a node's display name.
	NodeName(ctx context.Context, nodeID string) (string, error)
}

// derivedKinds are the event kinds that can produce attention. The deriver
// asks the hub for only these; attention.changed is deliberately absent.
var derivedKinds = []contracts.EventKind{
	contracts.EvApprovalRequested,
	contracts.EvApprovalResolved,
	contracts.EvApprovalWithdrawn,
	contracts.EvMessageStored,
	contracts.EvMessageDelivery,
	contracts.EvMessageAck,
	contracts.EvGrantChanged,
	contracts.EvAccountExhausted,
	contracts.EvFallbackPlanned,
	contracts.EvRunStarted,
	contracts.EvRunEnded,
	contracts.EvArtifactPublished,
	contracts.EvCommentCreated,
	contracts.EvDocumentConflict,
	contracts.EvUpdateResult,
}

const (
	maxTitle   = 100
	maxSummary = 160
)

// derive maps one hub event to inbox changes. It never reads message
// bodies, approval bodies, comment bodies, or transcript text into titles or
// summaries; titles come from short record titles and fixed phrases.
func derive(ctx context.Context, ev contracts.Event, lk Lookup) []change {
	switch ev.Kind {
	case contracts.EvAttentionChanged:
		return nil // never derive attention from attention (no loops)
	case contracts.EvApprovalRequested:
		a, ok := decodeApproval(ev)
		if !ok {
			return nil
		}
		return []change{approvalRaise(a, ev)}
	case contracts.EvApprovalResolved, contracts.EvApprovalWithdrawn:
		a, ok := decodeApproval(ev)
		if !ok {
			return nil
		}
		ch := approvalRaise(a, ev)
		ch.op = opWithdraw
		ch.summary = approvalEndSummary(ev.Kind, a)
		if ev.Kind == contracts.EvApprovalWithdrawn && a.State == contracts.ApprovalExpired {
			return []change{ch, terminalPromptRaise(a, ev)}
		}
		// The prompt ended (answered here or in the terminal, or the run
		// moved on): drop any "waiting in its terminal" item too.
		return []change{ch, {op: opWithdraw, groupKey: terminalGroup(a.ApprovalID), summary: ch.summary}}
	case contracts.EvMessageStored:
		return deriveMessageStored(ev)
	case contracts.EvMessageDelivery:
		return deriveDelivery(ev)
	case contracts.EvMessageAck:
		return deriveMessageAck(ev)
	case contracts.EvGrantChanged:
		return deriveGrant(ctx, ev, lk)
	case contracts.EvAccountExhausted:
		return deriveExhausted(ctx, ev, lk)
	case contracts.EvFallbackPlanned:
		return deriveFallback(ctx, ev, lk)
	case contracts.EvRunStarted:
		return deriveRunStarted(ev)
	case contracts.EvRunEnded:
		return deriveRunEnded(ctx, ev, lk)
	case contracts.EvArtifactPublished:
		return deriveArtifact(ev)
	case contracts.EvCommentCreated:
		return deriveComment(ev)
	case contracts.EvDocumentConflict:
		return deriveConflict(ev)
	case contracts.EvUpdateResult:
		return deriveUpdate(ev)
	}
	return nil
}

func unmarshal(ev contracts.Event, v any) bool {
	if len(ev.Payload) == 0 {
		return false
	}
	return json.Unmarshal(ev.Payload, v) == nil
}

func objectID(ev contracts.Event, kind contracts.ObjectKind) string {
	if ev.Object.Kind == kind {
		return ev.Object.ID
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// short sanitizes s for a title/summary: control characters (including
// newlines) become spaces, runs of spaces collapse, and the result is
// truncated to n runes.
func short(s string, n int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) <= n {
		return out
	}
	rs := []rune(out)
	return strings.TrimSpace(string(rs[:n-1])) + "…"
}

// ---- approvals ----

func decodeApproval(ev contracts.Event) (contracts.Approval, bool) {
	var a contracts.Approval
	unmarshal(ev, &a)
	if a.ApprovalID == "" {
		var w struct {
			Approval contracts.Approval `json:"approval"`
		}
		if unmarshal(ev, &w) && w.Approval.ApprovalID != "" {
			a = w.Approval
		}
	}
	if a.ApprovalID == "" {
		a.ApprovalID = objectID(ev, contracts.KindApproval)
	}
	if a.SessionID == "" {
		a.SessionID = ev.SessionID
	}
	return a, a.ApprovalID != ""
}

func approvalRaise(a contracts.Approval, ev contracts.Event) change {
	kind := contracts.AttnApproval
	title := short(a.Title, maxTitle-15)
	switch {
	case a.Kind == contracts.ApprovalQuestion:
		kind = contracts.AttnQuestion
		if title == "" {
			title = "Question waiting for an answer"
		} else {
			title = "Question: " + title
		}
	case a.Kind == contracts.ApprovalPlan:
		if title == "" {
			title = "Plan waiting for approval"
		} else {
			title = "Approve plan: " + title
		}
	default:
		if title == "" {
			title = "Approval requested"
		} else {
			title = "Approve: " + title
		}
	}
	ref := contracts.ObjectRef{Kind: contracts.KindApproval, ID: a.ApprovalID}
	return change{
		op:        opRaise,
		groupKey:  "approval:" + a.ApprovalID,
		kind:      kind,
		severity:  "urgent",
		object:    ref,
		sessionID: a.SessionID,
		projectID: ev.ProjectID,
		title:     title,
		oneShot:   true,
	}
}

func terminalGroup(approvalID string) string { return "terminal:" + approvalID }

// terminalPromptRaise: the remote answer window of an approval expired
// while the harness's own prompt is still on screen (Claude falls back to
// its dialog). It can no longer be answered through Pyre, but the run still
// waits for the owner, so the inbox keeps an urgent item that says how to
// resolve it. The object is the session (attach / interrupt act on it), so
// no client offers approval controls for it. Never answered silently.
func terminalPromptRaise(a contracts.Approval, ev contracts.Event) change {
	title := short(a.Title, maxTitle-30)
	if title == "" {
		title = "a prompt"
	}
	obj := contracts.ObjectRef{Kind: contracts.KindApproval, ID: a.ApprovalID}
	if a.SessionID != "" {
		obj = contracts.ObjectRef{Kind: contracts.KindSession, ID: a.SessionID}
	}
	return change{
		op:        opRaise,
		groupKey:  terminalGroup(a.ApprovalID),
		kind:      contracts.AttnApproval,
		severity:  "urgent",
		object:    obj,
		sessionID: a.SessionID,
		projectID: ev.ProjectID,
		title:     "Answer in its terminal: " + title,
		summary:   "The remote answer window closed; the prompt is still open in the session's terminal. Attach to answer it there, or interrupt the session to cancel it.",
		oneShot:   true,
	}
}

// approvalEndSummary never invents which client answered: a native
// resolution is reported only as resolved in the terminal.
func approvalEndSummary(kind contracts.EventKind, a contracts.Approval) string {
	if kind == contracts.EvApprovalWithdrawn {
		if a.State == contracts.ApprovalExpired {
			return "Remote answer window closed; waiting in its terminal."
		}
		return "Withdrawn by the runtime."
	}
	if a.Resolution != nil {
		switch a.Resolution.Source {
		case "native":
			return "Resolved in the terminal."
		case "pyre":
			return "Answered through Pyre."
		}
	}
	return "Resolved."
}

// ---- messages ----

func decodeEnvelope(ev contracts.Event) (contracts.MessageEnvelope, bool) {
	var v contracts.MessageView
	if unmarshal(ev, &v) && v.Envelope.MessageID != "" {
		return v.Envelope, true
	}
	var e contracts.MessageEnvelope
	if unmarshal(ev, &e) && e.MessageID != "" {
		return e, true
	}
	return e, false
}

func principalLabel(p contracts.Principal) string {
	return short(firstNonEmpty(p.Name, p.ID, string(p.Kind)), 48)
}

func deriveMessageStored(ev contracts.Event) []change {
	env, ok := decodeEnvelope(ev)
	if !ok {
		return nil
	}
	project := env.ProjectID
	if project == nil {
		project = ev.ProjectID
	}
	switch {
	case env.Recipient.Kind == contracts.AddrOwner:
		if env.Sender.Kind == contracts.PrincipalOwner {
			return nil // a note to self is not attention
		}
		ch := change{
			op:        opRaise,
			kind:      contracts.AttnMessage,
			severity:  "normal",
			projectID: project,
			title:     "Message from " + principalLabel(env.Sender),
		}
		if env.Mode == contracts.ModeInfo {
			ch.severity = "info"
		}
		if env.Sender.Kind == contracts.PrincipalSession {
			ch.sessionID = env.Sender.ID
		}
		ch.groupKey = ownerMessageGroup(env)
		if env.ThreadID != "" {
			ch.object = contracts.ObjectRef{Kind: contracts.KindThread, ID: env.ThreadID}
		} else {
			ch.object = contracts.ObjectRef{Kind: contracts.KindMessage, ID: env.MessageID}
		}
		return []change{ch}
	case env.Sender.Kind == contracts.PrincipalOwner:
		// The owner replying is the owner acting on that conversation, and
		// the owner messaging a session engages with it: its messages and
		// its own flag raised before this message are answered.
		var out []change
		if env.ThreadID != "" {
			out = append(out, change{op: opHandle, groupKey: "message:" + env.ThreadID, summary: "Replied."})
		}
		if env.Recipient.Kind == contracts.AddrSession {
			out = append(out,
				change{op: opHandle, groupKey: "message:from:session:" + env.Recipient.ID, summary: "Replied."},
				change{op: opHandle, sessionID: env.Recipient.ID, kinds: EngageKinds, summary: "You messaged the session.", before: ev.OccurredAt})
		}
		return out
	}
	// Agent-to-agent (and channel/project) traffic, including progress
	// updates, is not owner attention.
	return nil
}

// ownerMessageGroup is the group of the "message" item raised for a
// message addressed to the owner.
func ownerMessageGroup(env contracts.MessageEnvelope) string {
	if env.ThreadID != "" {
		return "message:" + env.ThreadID
	}
	return "message:from:" + string(env.Sender.Kind) + ":" + env.Sender.ID
}

// deriveMessageAck: an acknowledgement or reply recorded on a message
// addressed to the owner (owner ack, or reply linking) is the owner acting
// on it, so the item that message raised is handled.
func deriveMessageAck(ev contracts.Event) []change {
	var v contracts.MessageView
	if !unmarshal(ev, &v) || v.Envelope.MessageID == "" {
		return nil
	}
	env := v.Envelope
	if env.Recipient.Kind != contracts.AddrOwner || env.Sender.Kind == contracts.PrincipalOwner {
		return nil
	}
	switch v.Status.Ack {
	case contracts.AckAcknowledged:
		return []change{{op: opHandle, groupKey: ownerMessageGroup(env), summary: "Acknowledged."}}
	case contracts.AckReplied:
		return []change{{op: opHandle, groupKey: ownerMessageGroup(env), summary: "Replied."}}
	}
	return nil
}

type deliveryPayload struct {
	Envelope *struct {
		MessageID string            `json:"message_id"`
		Recipient contracts.Address `json:"recipient"`
		ProjectID *string           `json:"project_id"`
	} `json:"envelope"`
	Status *struct {
		Runtime contracts.RuntimeDeliveryState `json:"runtime"`
	} `json:"status"`
	Runtime   contracts.RuntimeDeliveryState `json:"runtime"`
	State     contracts.RuntimeDeliveryState `json:"state"`
	MessageID string                         `json:"message_id"`
	SessionID string                         `json:"session_id"`
}

func deriveDelivery(ev contracts.Event) []change {
	var p deliveryPayload
	unmarshal(ev, &p)
	msgID := objectID(ev, contracts.KindMessage)
	state := p.Runtime
	sessionID := p.SessionID
	project := ev.ProjectID
	if p.Envelope != nil {
		msgID = firstNonEmpty(msgID, p.Envelope.MessageID)
		if p.Envelope.Recipient.Kind == contracts.AddrSession {
			sessionID = firstNonEmpty(sessionID, p.Envelope.Recipient.ID)
		}
		if p.Envelope.ProjectID != nil {
			project = p.Envelope.ProjectID
		}
	}
	if p.Status != nil && p.Status.Runtime != "" {
		state = p.Status.Runtime
	}
	if state == "" {
		state = p.State
	}
	msgID = firstNonEmpty(msgID, p.MessageID)
	sessionID = firstNonEmpty(sessionID, ev.SessionID)
	if msgID == "" {
		return nil
	}
	group := "indeterminate:" + msgID
	switch state {
	case contracts.DeliveryIndeterminate:
		return []change{{
			op:        opRaise,
			groupKey:  group,
			kind:      contracts.AttnIndeterminate,
			severity:  "urgent",
			object:    contracts.ObjectRef{Kind: contracts.KindMessage, ID: msgID},
			sessionID: sessionID,
			projectID: project,
			title:     "Delivery outcome unknown",
			summary:   "Inspect evidence before reissuing.",
			oneShot:   true,
		}}
	case contracts.DeliveryAccepted, contracts.DeliveryRejected, contracts.DeliveryExpired, contracts.DeliveryCancelled:
		return []change{{op: opWithdraw, groupKey: group, summary: "Delivery outcome resolved: " + string(state) + "."}}
	}
	return nil
}

// ---- accounts ----

func providerLabel(p contracts.Provider) string {
	switch p {
	case contracts.ProviderAnthropic:
		return "Claude"
	case contracts.ProviderOpenAI:
		return "Codex"
	case "":
		return "Unknown-provider"
	}
	return short(string(p), 24)
}

func resolveProvider(ctx context.Context, lk Lookup, given contracts.Provider, accountIDs ...string) contracts.Provider {
	if given != "" {
		return given
	}
	if lk == nil {
		return ""
	}
	for _, id := range accountIDs {
		if id == "" {
			continue
		}
		if p, err := lk.AccountProvider(ctx, id); err == nil && p != "" {
			return p
		}
	}
	return ""
}

func poolGroup(p contracts.Provider) string {
	if p == "" {
		return "pool:unknown"
	}
	return "pool:" + string(p)
}

func deriveExhausted(ctx context.Context, ev contracts.Event, lk Lookup) []change {
	var p struct {
		AccountID string                    `json:"account_id"`
		Provider  contracts.Provider        `json:"provider"`
		ResetAt   *time.Time                `json:"reset_at"`
		FromRunID string                    `json:"from_run_id"`
		Outcome   contracts.FallbackOutcome `json:"outcome"`
		Account   *struct {
			AccountID string             `json:"account_id"`
			Provider  contracts.Provider `json:"provider"`
		} `json:"account"`
	}
	unmarshal(ev, &p)
	if p.FromRunID != "" && (p.Outcome == contracts.FallbackWaiting || p.Outcome == contracts.FallbackIntervention) {
		// Exhaustion that belongs to a fallback plan for one run: the
		// account.fallback event of that same plan raises the item, so one
		// limit is one occurrence (seen live: count x3 before the hub kept a
		// single account.exhausted per run fallback).
		return nil
	}
	if p.Account != nil {
		p.AccountID = firstNonEmpty(p.AccountID, p.Account.AccountID)
		if p.Provider == "" {
			p.Provider = p.Account.Provider
		}
	}
	acct := firstNonEmpty(p.AccountID, objectID(ev, contracts.KindAccount))
	prov := resolveProvider(ctx, lk, p.Provider, acct)
	ch := change{
		op:        opRaise,
		groupKey:  poolGroup(prov),
		kind:      contracts.AttnAccountPool,
		severity:  "normal",
		projectID: ev.ProjectID,
		title:     providerLabel(prov) + " account limit reached",
	}
	if p.ResetAt != nil {
		ch.summary = "Earliest reset " + p.ResetAt.UTC().Format("2006-01-02 15:04Z") + "."
	}
	if ev.SessionID != "" {
		ch.sessionID = ev.SessionID
		ch.object = contracts.ObjectRef{Kind: contracts.KindSession, ID: ev.SessionID}
	} else if acct != "" {
		ch.object = contracts.ObjectRef{Kind: contracts.KindAccount, ID: acct}
	} else {
		return nil
	}
	return []change{ch}
}

func deriveFallback(ctx context.Context, ev contracts.Event, lk Lookup) []change {
	var p struct {
		contracts.FallbackPlan
		Provider contracts.Provider `json:"provider"`
	}
	if !unmarshal(ev, &p) {
		return nil
	}
	if p.Outcome != contracts.FallbackWaiting && p.Outcome != contracts.FallbackIntervention {
		return nil
	}
	prov := resolveProvider(ctx, lk, p.Provider, p.FromAccountID, p.ToAccountID)
	session := firstNonEmpty(p.SessionID, ev.SessionID)
	if session == "" {
		return nil
	}
	ch := change{
		op:        opRaise,
		groupKey:  poolGroup(prov),
		kind:      contracts.AttnAccountPool,
		severity:  "urgent",
		object:    contracts.ObjectRef{Kind: contracts.KindSession, ID: session},
		sessionID: session,
		projectID: ev.ProjectID,
	}
	if p.Outcome == contracts.FallbackWaiting {
		ch.title = "No eligible " + providerLabel(prov) + " account"
		ch.summary = "Session paused and resumable; waiting for an eligible account or reset."
	} else {
		ch.title = providerLabel(prov) + " session needs intervention"
		ch.summary = "Human action required to continue."
	}
	if p.UncertainWork {
		ch.summary += " In-flight work is uncertain; the last instruction was not replayed."
	}
	return []change{ch}
}

// deriveGrant raises a "node" item when a device login needs the owner to
// log in again (state dead or expired) and withdraws it when a later event
// for the same grant reports it valid (re-login) or revoked (the owner
// removed it). Kind "node": the problem is one node's login for one
// account; a single unusable grant does not by itself block fallback (pool
// exhaustion is reported by account.exhausted / account.fallback).
func deriveGrant(ctx context.Context, ev contracts.Event, lk Lookup) []change {
	var g contracts.DeviceGrant
	if !unmarshal(ev, &g) {
		return nil
	}
	id := firstNonEmpty(g.GrantID, objectID(ev, contracts.KindGrant))
	if id == "" {
		return nil
	}
	group := "grant:" + id
	switch g.State {
	case contracts.GrantValid:
		return []change{{op: opWithdraw, groupKey: group, summary: "Logged in again."}}
	case contracts.GrantRevoked:
		return []change{{op: opWithdraw, groupKey: group, summary: "Login removed."}}
	case contracts.GrantDead, contracts.GrantExpired:
	default:
		return nil
	}
	label, node := g.AccountID, g.NodeID
	if ll, ok := lk.(LabelLookup); ok {
		if g.AccountID != "" {
			if l, err := ll.AccountLabel(ctx, g.AccountID); err == nil && l != "" {
				label = l
			}
		}
		if g.NodeID != "" {
			if n, err := ll.NodeName(ctx, g.NodeID); err == nil && n != "" {
				node = n
			}
		}
	}
	title := "Re-login needed for " + short(firstNonEmpty(label, "an account"), 40)
	if node != "" {
		title += " on " + short(node, 40)
	}
	summary := "The login expired and could not be refreshed."
	if g.State == contracts.GrantDead {
		summary = "The provider rejected the login."
	}
	return []change{{
		op:        opRaise,
		groupKey:  group,
		kind:      contracts.AttnNode,
		severity:  "normal",
		object:    contracts.ObjectRef{Kind: contracts.KindGrant, ID: id},
		projectID: ev.ProjectID,
		title:     title,
		summary:   summary,
	}}
}

// ---- runs ----

// deriveRunStarted: a later run of the session started (a retry, a resume,
// a fallback), so an open failure for the session resolves itself.
func deriveRunStarted(ev contracts.Event) []change {
	var r contracts.Run
	if !unmarshal(ev, &r) {
		return nil
	}
	session := firstNonEmpty(r.SessionID, ev.SessionID)
	if session == "" {
		return nil
	}
	return []change{{op: opWithdraw, groupKey: "failure:" + session, summary: "A later run of this session started."}}
}

func deriveRunEnded(ctx context.Context, ev contracts.Event, lk Lookup) []change {
	var r contracts.Run
	if !unmarshal(ev, &r) {
		return nil
	}
	session := firstNonEmpty(r.SessionID, ev.SessionID)
	var out []change
	if session != "" {
		// The run that flagged its session is gone: its own flag ends with
		// it (a newer flag, from a later run, is kept).
		out = append(out, change{op: opWithdraw, groupKey: flagGroup(session), summary: "The run ended.", before: ev.OccurredAt})
	}
	// Only an explicit failure; "unknown" (lost contact) is never failure.
	if r.State != contracts.RunFailed {
		return out
	}
	ch := change{
		op:        opRaise,
		kind:      contracts.AttnFailure,
		severity:  "normal",
		projectID: ev.ProjectID,
		sessionID: session,
		title:     "Session run failed",
	}
	if r.ExitCode != nil {
		ch.summary = fmt.Sprintf("Exit code %d.", *r.ExitCode)
	}
	if plain := runfail.Summary(r.StateReason, r.ExitCode != nil, runNames(ctx, r, lk)); plain != "" {
		ch.summary = short(plain, maxSummary)
	}
	switch {
	case session != "":
		ch.groupKey = "failure:" + session
		ch.object = contracts.ObjectRef{Kind: contracts.KindSession, ID: session}
	case r.RunID != "":
		ch.groupKey = "failure:" + r.RunID
		ch.object = contracts.ObjectRef{Kind: contracts.KindRun, ID: r.RunID}
	default:
		return out
	}
	return append(out, ch)
}

// ---- artifacts, comments, documents ----

func deriveArtifact(ev contracts.Event) []change {
	var p struct {
		Artifact        *contracts.Artifact `json:"artifact"`
		Version         *json.RawMessage    `json:"version"`
		ReviewRequested bool                `json:"review_requested"`
		RequestReview   bool                `json:"request_review"`
		// Bare ArtifactVersion payload fields.
		VersionID  string               `json:"version_id"`
		ArtifactID string               `json:"artifact_id"`
		Seq        int64                `json:"seq"`
		Filename   string               `json:"filename"`
		Provenance contracts.Provenance `json:"provenance"`
	}
	if !unmarshal(ev, &p) {
		return nil
	}
	var v struct {
		contracts.ArtifactVersion
		ReviewRequested bool `json:"review_requested"`
	}
	if p.Version != nil {
		_ = json.Unmarshal(*p.Version, &v)
	} else {
		v.VersionID, v.ArtifactID, v.Seq, v.Filename, v.Provenance = p.VersionID, p.ArtifactID, p.Seq, p.Filename, p.Provenance
	}
	if !(p.ReviewRequested || p.RequestReview || v.ReviewRequested) {
		return nil
	}
	versionID := firstNonEmpty(v.VersionID, objectID(ev, contracts.KindArtifactVersion))
	artifactID := v.ArtifactID
	title := v.Filename
	project := ev.ProjectID
	if p.Artifact != nil {
		artifactID = firstNonEmpty(artifactID, p.Artifact.ArtifactID)
		title = firstNonEmpty(p.Artifact.Title, title)
		if p.Artifact.ProjectID != nil {
			project = p.Artifact.ProjectID
		}
	}
	artifactID = firstNonEmpty(artifactID, objectID(ev, contracts.KindArtifact))
	if versionID == "" && artifactID == "" {
		return nil
	}
	if project == nil {
		project = v.Provenance.ProjectID
	}
	title = short(firstNonEmpty(title, "Artifact"), maxTitle-20)
	if v.Seq > 1 {
		title = fmt.Sprintf("%s v%d", title, v.Seq)
	}
	obj := contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: versionID}
	if versionID == "" {
		obj = contracts.ObjectRef{Kind: contracts.KindArtifact, ID: artifactID}
	}
	return []change{{
		op:        opRaise,
		groupKey:  "review:" + firstNonEmpty(artifactID, versionID),
		kind:      contracts.AttnReviewReady,
		severity:  "normal",
		object:    obj,
		sessionID: firstNonEmpty(v.Provenance.SessionID, ev.SessionID),
		projectID: project,
		title:     title + " ready for review",
	}}
}

func mentionsOwner(ms []string) bool {
	for _, m := range ms {
		m = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(m), "@"))
		if m == "owner" {
			return true
		}
	}
	return false
}

func deriveComment(ev contracts.Event) []change {
	var c contracts.ReviewComment
	if !unmarshal(ev, &c) || c.CommentID == "" {
		return nil
	}
	if c.Author.Kind == contracts.PrincipalOwner || !mentionsOwner(c.Mentions) {
		return nil
	}
	project := c.ProjectID
	if project == nil {
		project = ev.ProjectID
	}
	session := ev.SessionID
	if c.Author.Kind == contracts.PrincipalSession {
		session = firstNonEmpty(c.Author.ID, session)
	}
	return []change{{
		op:        opRaise,
		groupKey:  "comment:" + firstNonEmpty(c.Target.ID, c.CommentID),
		kind:      contracts.AttnComment,
		severity:  "normal",
		object:    contracts.ObjectRef{Kind: contracts.KindComment, ID: c.CommentID},
		sessionID: session,
		projectID: project,
		title:     "Comment from " + principalLabel(c.Author),
	}}
}

func deriveConflict(ev contracts.Event) []change {
	var p struct {
		DocumentID string              `json:"document_id"`
		ProposalID string              `json:"proposal_id"`
		Author     contracts.Principal `json:"author"`
	}
	unmarshal(ev, &p)
	docID := firstNonEmpty(p.DocumentID, objectID(ev, contracts.KindDocument))
	if docID == "" {
		return nil
	}
	if p.Author.Kind == contracts.PrincipalOwner {
		return nil // the owner's own client received the conflict synchronously
	}
	ch := change{
		op:        opRaise,
		groupKey:  "conflict:" + docID,
		kind:      contracts.AttnConflict,
		severity:  "normal",
		object:    contracts.ObjectRef{Kind: contracts.KindDocument, ID: docID},
		sessionID: ev.SessionID,
		projectID: ev.ProjectID,
		title:     "Document edit conflict",
		summary:   "A conflicting edit was preserved as a proposal.",
	}
	if p.Author.Kind == contracts.PrincipalSession {
		ch.sessionID = firstNonEmpty(ch.sessionID, p.Author.ID)
	}
	return []change{ch}
}

// ---- updates ----

func deriveUpdate(ev contracts.Event) []change {
	var p struct {
		Tool       string `json:"tool"`
		Version    string `json:"version"`
		State      string `json:"state"`
		Health     string `json:"health"`
		Outcome    string `json:"outcome"`
		RolledBack bool   `json:"rolled_back"`
		NodeID     string `json:"node_id"`
	}
	if !unmarshal(ev, &p) {
		return nil
	}
	node := firstNonEmpty(p.NodeID, objectID(ev, contracts.KindNode))
	if node == "" && strings.HasPrefix(ev.Origin, "node:") {
		node = strings.TrimPrefix(ev.Origin, "node:")
	}
	if node == "" {
		return nil
	}
	tool := short(firstNonEmpty(p.Tool, "update"), 32)
	group := "update:" + tool + ":" + node
	failed := p.State == "failed" || strings.HasPrefix(p.Health, "failed") ||
		p.Outcome == "failed" || p.Outcome == "rolled_back" || p.RolledBack
	if !failed {
		if p.State == "active" && (p.Health == "" || p.Health == "ok") || p.Outcome == "succeeded" {
			return []change{{op: opWithdraw, groupKey: group, summary: "A later update succeeded."}}
		}
		return nil
	}
	title := tool
	if p.Version != "" {
		title += " " + short(p.Version, 24)
	}
	title += " update failed"
	summary := ""
	if p.RolledBack || p.Outcome == "rolled_back" {
		summary = "Rolled back to the previous version."
	}
	return []change{{
		op:        opRaise,
		groupKey:  group,
		kind:      contracts.AttnUpdate,
		severity:  "normal",
		object:    contracts.ObjectRef{Kind: contracts.KindNode, ID: node},
		projectID: ev.ProjectID,
		title:     title,
		summary:   summary,
	}}
}

// runNames resolves account labels and the node name for a failure summary.
func runNames(ctx context.Context, r contracts.Run, lk Lookup) runfail.Names {
	var n runfail.Names
	ll, ok := lk.(LabelLookup)
	if !ok {
		return n
	}
	if r.NodeID != "" {
		if name, err := ll.NodeName(ctx, r.NodeID); err == nil {
			n.Node = name
		}
	}
	n.Account = func(id string) string {
		lb, err := ll.AccountLabel(ctx, id)
		if err != nil {
			return ""
		}
		return lb
	}
	return n
}
