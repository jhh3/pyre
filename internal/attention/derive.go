package attention

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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

// SessionLookup is optionally implemented by a Lookup to name sessions in
// titles.
type SessionLookup interface {
	SessionTitle(ctx context.Context, sessionID string) (string, error)
}

// SessionParentLookup is optionally implemented by a Lookup to tell
// whether a session sits under another session (contracts.Session.Parent).
// A failed run of such a session is reported to that parent (the hub's
// child report), not raised for the owner. Without it, or on error, the
// session counts as top level (the owner hears of it).
type SessionParentLookup interface {
	SessionParent(ctx context.Context, sessionID string) (string, error)
}

// SessionTagsLookup is optionally implemented by a Lookup to read a
// session's tags. A failed run of a headless pyre run session
// (contracts.TagPyreRun) raises nothing: its caller already got the exit
// code, and pyre run never creates owner attention.
type SessionTagsLookup interface {
	SessionTags(ctx context.Context, sessionID string) ([]string, error)
}

// headlessAdapters are the adapters of pyre run runs (claude.ExecAdapterName,
// codex.ExecAdapterName).
var headlessAdapters = []string{"claude-exec", "codex-exec"}

func sessionTagged(ctx context.Context, lk Lookup, sessionID, tag string) bool {
	if tl, ok := lk.(SessionTagsLookup); ok && sessionID != "" {
		if tags, err := tl.SessionTags(ctx, sessionID); err == nil {
			return slices.Contains(tags, tag)
		}
	}
	return false
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
	contracts.EvRunObserved,
	contracts.EvRunEnded,
	contracts.EvSessionUpdated,
	contracts.EvArtifactPublished,
	contracts.EvCommentCreated,
	contracts.EvDocumentConflict,
	contracts.EvUpdateResult,
	contracts.EvAskChanged,
	contracts.EvStandingGrantChanged,
}

const (
	maxTitle   = 100
	maxSummary = 160
)

// derive maps one hub event to inbox changes. Titles come from short
// record titles and fixed phrases. The only body text it copies is the
// summary of a "message" item: a sanitized excerpt of the newest message
// to the owner (MessageExcerpt; owner request 2026-10-12: the inbox showed
// a dash). Approval, comment and transcript text is never copied, and push
// payloads carry only generic titles (contracts.PushDataKeys).
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
	case contracts.EvRunStarted, contracts.EvRunObserved:
		return deriveRunLive(ev)
	case contracts.EvRunEnded:
		return deriveRunEnded(ctx, ev, lk)
	case contracts.EvSessionUpdated:
		return deriveSessionArchived(ev)
	case contracts.EvArtifactPublished:
		return deriveArtifact(ev)
	case contracts.EvCommentCreated:
		return deriveComment(ev)
	case contracts.EvDocumentConflict:
		return deriveConflict(ev)
	case contracts.EvUpdateResult:
		return deriveUpdate(ev)
	case contracts.EvAskChanged:
		return deriveAsk(ev)
	case contracts.EvStandingGrantChanged:
		return deriveStandingGrant(ev)
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
		if contracts.ValidID(env.ThreadID, contracts.PrefixAsk) {
			return nil // the message carrying an ask: its "ask" item stands for it
		}
		if storedAcknowledged(ev) {
			return nil // an update: it asks nothing of the owner (internal/messaging/ownermail)
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
		ch.summary = MessageExcerpt(env.Body.Text)
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

// storedAcknowledged reports whether a message.stored payload says the
// message was stored already acknowledged (an update for the owner).
// Older payloads (a bare envelope) say nothing.
func storedAcknowledged(ev contracts.Event) bool {
	var v contracts.MessageView
	return unmarshal(ev, &v) && v.Envelope.MessageID != "" && v.Status.Ack != "" && v.Status.Ack != contracts.AckNone
}

// maxExcerpt bounds a message item's summary (runes).
const maxExcerpt = 300

// ansiRe matches terminal escape sequences (CSI, OSC, and two-byte ESC
// sequences) so pasted terminal output does not leave "[31m" behind.
var ansiRe = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)

// MessageExcerpt is the summary of a message item: the body with escape
// sequences and control characters removed, whitespace collapsed, at most
// maxExcerpt runes.
func MessageExcerpt(body string) string {
	return short(ansiRe.ReplaceAllString(body, ""), maxExcerpt)
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
	ch := change{op: opHandle, groupKey: ownerMessageGroup(env)}
	if env.ThreadID == "" {
		// A sender's unthreaded mail is one item, whose object is its newest
		// message: acknowledging an older one leaves a newer question open.
		ch.objectID = env.MessageID
	}
	switch v.Status.Ack {
	case contracts.AckAcknowledged:
		ch.summary = "Acknowledged."
		return []change{ch}
	case contracts.AckReplied:
		ch.summary = "Replied."
		return []change{ch}
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
	case contracts.ProviderGoogle:
		return "agy"
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
	if paused, cause := switchPaused(p.Reason); paused {
		// The node's switch circuit breaker stopped an automatic switch.
		ch.title = "Account switching paused for " + quoteTitle(sessionTitle(ctx, lk, session)) + ": " + cause
		ch.summary = "Pyre stopped switching accounts automatically; the session is still on its limited account. Check it, then resume or switch it yourself."
		return []change{ch}
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

// switchPausedPrefix matches accounts.SwitchPausedPrefix (a fallback plan
// the node's switch circuit breaker stopped).
const switchPausedPrefix = "account switching paused"

// switchPaused reports whether a plan reason comes from the switch circuit
// breaker and returns its cause as a fixed phrase (never free text).
func switchPaused(reason string) (bool, string) {
	l := strings.ToLower(reason)
	if !strings.HasPrefix(l, switchPausedPrefix) {
		return false, ""
	}
	if strings.Contains(l, "on this node") {
		return true, "too many automatic switches on its machine"
	}
	return true, "repeated limits within 30m"
}

// sessionTitle resolves a session's title when the lookup can ("" otherwise).
func sessionTitle(ctx context.Context, lk Lookup, sessionID string) string {
	if sl, ok := lk.(SessionLookup); ok && sessionID != "" {
		if t, err := sl.SessionTitle(ctx, sessionID); err == nil {
			return t
		}
	}
	return ""
}

func quoteTitle(t string) string {
	if t = short(t, 40); t == "" {
		return "this session"
	}
	return "'" + t + "'"
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

// LiveRun reports whether a run state means the session is running again
// (working or idle at its prompt); starting, unknown and exiting do not.
func LiveRun(st contracts.RunState) bool {
	switch st {
	case contracts.RunRunning, contracts.RunWaitingInput, contracts.RunWaitingApproval, contracts.RunWaitingBackground:
		return true
	}
	return false
}

// resolveSession is what resolves a session's failure and account items:
// its failure item, the account_exhausted items it raised, and its place
// in grouped capacity items.
func resolveSession(session, summary string) []change {
	return []change{
		{op: opWithdraw, sessionID: session, kinds: []contracts.AttentionKind{contracts.AttnFailure, contracts.AttnAccountPool}, summary: summary},
		{op: opLeave, sessionID: session},
	}
}

// deriveRunLive: a later run of the session reached a live state (a
// retry, a resume, a fallback), so its failure and "no eligible account" /
// "needs intervention" items are stale. A run that is only starting
// resolves nothing: a retry that fails again keeps the one item.
func deriveRunLive(ev contracts.Event) []change {
	var r contracts.Run
	if !unmarshal(ev, &r) || !LiveRun(r.State) {
		return nil
	}
	session := firstNonEmpty(r.SessionID, ev.SessionID)
	if session == "" {
		return nil
	}
	out := resolveSession(session, "A later run of this session is running.")
	if r.State == contracts.RunRunning {
		// Its next turn started: whatever it was waiting on was answered
		// or dropped (an item raised after this event is kept).
		out = append(out, withdrawWaiting(session, "The session started its next turn.", ev.OccurredAt))
	}
	return out
}

// withdrawWaiting withdraws the session's "waiting on you" item last
// updated at or before before (waiting.go).
func withdrawWaiting(session, summary string, before time.Time) change {
	return change{op: opWithdraw, groupKey: WaitingGroup(session), summary: summary, before: before}
}

// deriveSessionArchived: an archived session's failure and account items,
// its "waiting on you" and the messages to the owner he already read are
// no longer his concern (owner report 2026-10-19: message items from long
// finished sessions stayed open for days). An unread message stays: most
// archives are automatic (archive when done, the nightly sweep), and a
// session's final report must not vanish with it.
func deriveSessionArchived(ev contracts.Event) []change {
	var s contracts.Session
	if !unmarshal(ev, &s) || !s.Archived {
		return nil
	}
	session := firstNonEmpty(s.SessionID, ev.SessionID)
	if session == "" {
		return nil
	}
	return append(resolveSession(session, "The session was archived."), withdrawWaiting(session, "The session was archived.", time.Time{}),
		change{op: opWithdraw, sessionID: session, kinds: []contracts.AttentionKind{contracts.AttnMessage}, readOnly: true, summary: "The session was archived."})
}

// sessionParent resolves a session's parent when the lookup can ("" otherwise).
func sessionParent(ctx context.Context, lk Lookup, sessionID string) string {
	if pl, ok := lk.(SessionParentLookup); ok && sessionID != "" {
		if p, err := pl.SessionParent(ctx, sessionID); err == nil {
			return p
		}
	}
	return ""
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
		// Account limits the session hit are moot once it ended (events are
		// applied in order, so only items raised before the end go).
		out = append(out, change{op: opWithdraw, sessionID: session, kinds: []contracts.AttentionKind{contracts.AttnAccountPool}, summary: "The session's run ended."})
		out = append(out, withdrawWaiting(session, "The session's run ended.", ev.OccurredAt))
	}
	// Only an explicit failure; "unknown" (lost contact) is never failure.
	if r.State != contracts.RunFailed {
		return out
	}
	if slices.Contains(headlessAdapters, r.Adapter) || r.Adapter == "" && sessionTagged(ctx, lk, session, contracts.TagPyreRun) {
		return out // pyre run: the caller has the exit code (a later interactive resume still raises)
	}
	if session != "" && sessionParent(ctx, lk, session) != "" {
		// A session started by another session: the hub reports the
		// failure to that parent (child report); the owner hears of it
		// only if the parent asks.
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
	if prov, ok := runfail.Capacity(r.StateReason); ok && session != "" {
		// Launches waiting on one provider's reset are one item, listing
		// the sessions (not one item each).
		if prov == "" {
			prov = contracts.ProviderFor(r.Harness)
		}
		ch.groupKey = capacityGroup(prov)
		ch.object = contracts.ObjectRef{Kind: contracts.KindSession, ID: session}
		ch.sessionID, ch.projectID = "", nil // many sessions, maybe many projects
		ch.member, ch.memberLabel = session, sessionTitle(ctx, lk, session)
		return append(out, ch)
	}
	if r.ExitCode != nil {
		ch.summary = fmt.Sprintf("Exit code %d.", *r.ExitCode)
	}
	if plain := runfail.Summary(r.StateReason, r.ExitCode != nil, runNames(ctx, r, lk)); plain != "" {
		ch.summary = short(plain, maxSummary)
	}
	switch {
	case session != "":
		// Sessions that failed for the same cause (in the same project) are
		// one item listing them (members.go).
		ch.groupKey = failedGroup(ev.ProjectID, runfail.CauseOf(r.StateReason, r.ExitCode).Key)
		ch.object = contracts.ObjectRef{Kind: contracts.KindSession, ID: session}
		ch.sessionID = "" // many sessions; the project stays
		ch.member, ch.memberLabel = session, sessionTitle(ctx, lk, session)
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

// deriveAsk (W-asks): one "ask" item per ask, raised when it is created
// (a merged repeat is not new attention), handled when it is decided and
// withdrawn when the asker withdraws it or it is superseded.
func deriveAsk(ev contracts.Event) []change {
	var a contracts.Ask
	if !unmarshal(ev, &a) || a.AskID == "" {
		return nil
	}
	group := "ask:" + a.AskID
	switch a.State {
	case contracts.AskOpen:
		// The session filed (or repeated) an ask: its question in chat is
		// in the inbox as this ask now.
		var waiting []change
		if a.SessionID != "" {
			waiting = append(waiting, withdrawWaiting(a.SessionID, "The session filed an ask.", ev.OccurredAt))
		}
		if a.Count > 1 {
			return waiting
		}
		ch := change{
			op:        opRaise,
			groupKey:  group,
			kind:      contracts.AttnAsk,
			severity:  "normal",
			object:    contracts.ObjectRef{Kind: contracts.KindAsk, ID: a.AskID},
			sessionID: a.SessionID,
			projectID: a.ProjectID,
			title:     short("Ask: "+a.Title, maxTitle),
			oneShot:   true,
		}
		if a.Urgent {
			ch.severity = "urgent"
		}
		if a.DefaultOption != "" && a.DeadlineAt != nil {
			ch.summary = short("Default "+a.DefaultOption+" applies "+askTime(*a.DeadlineAt), maxSummary)
		} else if a.DeadlineAt != nil {
			ch.summary = "No default; expires " + askTime(*a.DeadlineAt)
		}
		return append(waiting, ch)
	case contracts.AskAnswered, contracts.AskDefaulted, contracts.AskExpired:
		summary := "Answered."
		switch a.State {
		case contracts.AskDefaulted:
			summary = "Default applied at the deadline."
		case contracts.AskExpired:
			summary = "Expired with no answer."
		}
		out := []change{{op: opHandle, groupKey: group, summary: summary}}
		if a.GrantID != "" && a.State == contracts.AskAnswered {
			// A standing grant answered it: the owner hears of it as an
			// FYI collected on the grant's item.
			out = append(out, grantFYI(a.GrantID, a.ProjectID, "Standing go-ahead used: "+a.Action, "Answered \""+a.Title+"\""))
		}
		return out
	case contracts.AskWithdrawn, contracts.AskSuperseded:
		return []change{{op: opWithdraw, groupKey: group, summary: "Withdrawn.", oneShot: true}}
	}
	return nil
}

// askTime shows an ask deadline in the owner's Pacific time.
func askTime(t time.Time) string {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return t.In(loc).Format("Mon 3:04 PM") + " PT"
	}
	return t.UTC().Format("Mon 15:04") + " UTC"
}

// grantFYI raises the informational item of a standing grant (one per
// grant; later raises collapse into it with Count).
func grantFYI(grantID string, projectID *string, title, summary string) change {
	return change{
		op:        opRaise,
		groupKey:  "grant:" + grantID,
		kind:      contracts.AttnGrantFYI,
		severity:  "info",
		object:    contracts.ObjectRef{Kind: contracts.KindStandingGrant, ID: grantID},
		projectID: projectID,
		title:     short(title, maxTitle),
		summary:   short(summary, maxSummary),
	}
}

// deriveStandingGrant: an FYI when a grant is created (it has never been
// used), handled when it is revoked. Its uses come from ask events.
func deriveStandingGrant(ev contracts.Event) []change {
	var g contracts.StandingGrant
	if !unmarshal(ev, &g) || g.GrantID == "" {
		return nil
	}
	if g.RevokedAt != nil {
		return []change{{op: opHandle, groupKey: "grant:" + g.GrantID, summary: "Revoked."}}
	}
	if g.UsedCount > 0 {
		return nil
	}
	summary := "Answers asks naming " + g.Action + " that offer \"" + g.Option + "\"; expires " + askTime(g.ExpiresAt) + ". Revoke on the project page."
	return []change{grantFYI(g.GrantID, g.ProjectID, "Standing go-ahead: "+g.Action+" → "+g.Option, summary)}
}
