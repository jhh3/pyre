package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/doing"
)

// evidence of one Pyre message: a userMessage item whose clientId is the
// message id (we pass clientUserMessageId=<message_id>).
type evidence struct {
	MessageID, ItemID, TurnID string
	At                        time.Time
	Seq                       int64
}

type pendingApproval struct {
	rpcID     json.RawMessage
	method    string
	params    approvalParams
	approval  contracts.Approval
	decisions map[string]json.RawMessage // option id -> decision value
	submitted time.Time
	option    string
}

type subagent struct {
	ThreadID, Kind string
	Start, End     time.Time
}

// threadState is the live view of one run's thread, fed by notifications.
type threadState struct {
	mu         sync.Mutex
	runID      string
	sessionID  string
	threadID   string
	status     threadStatus
	statusAt   time.Time
	activeTurn string
	lastTurn   string
	// turnsStarted counts turn/started notifications of the thread;
	// turnsObserved is that count when Observe last reported the run
	// working (Observe reports a turn it never saw active once).
	turnsStarted  int64
	turnsObserved int64
	activity      string
	inflight      doing.Inflight      // items between item/started and item/completed
	evidence      map[string]evidence // by message id
	seq           int64
	approvals     map[string]*pendingApproval // by RequestID
	order         []string
	subagents     map[string]*subagent
	subOrder      []string
	limit         *contracts.LimitEvent
	// lastErr is the newest failed turn's error (cleared by a completed
	// turn). Codex reports such a thread as systemError although it is idle
	// and takes input.
	lastErr   *turnError
	lastErrAt time.Time
	// Capacity retry: a turn that failed because the model was at capacity
	// is continued at retryAt (capRetries so far; reset by a completed turn).
	capRetries int
	retryAt    time.Time
	errProbed  bool // the last turn was read once to learn a systemError's cause
	known      bool // status learned from the server at least once
	turnDone   chan struct{}
	// config is the thread's Pyre identity overrides (threadConfig), re-sent
	// on every thread/resume so a restarted app-server keeps them.
	config map[string]any
	// Background-command wake (bgwake.go): completions waiting for an
	// idle thread, item ids already queued, a wake timer armed, the time
	// before which completions are Pyre's own kills, and Stop begun.
	wakes       []bgDone
	wakeSeen    map[string]bool
	wakeArmed   bool
	wakeMute    time.Time
	closing     bool
	runningCmds map[string]bool // commandExecution items started, not yet completed
	wakeIgnore  map[string]bool // their exits are Pyre's own kills (interrupt)
	// changes counts app-server notifications and requests for this thread
	// (and its subagents); Adapter.ChangeStamp exposes it lock-free.
	changes atomic.Int64
}

func newThreadState(runID, sessionID, threadID string) *threadState {
	return &threadState{runID: runID, sessionID: sessionID, threadID: threadID,
		status:   threadStatus{Type: "unknown"},
		evidence: map[string]evidence{}, approvals: map[string]*pendingApproval{}, subagents: map[string]*subagent{},
		turnDone: make(chan struct{})}
}

func (ts *threadState) setStatus(s threadStatus, at time.Time) {
	ts.status, ts.statusAt, ts.known = s, at, true
	if s.Type != "active" && ts.activeTurn != "" {
		ts.lastTurn, ts.activeTurn = ts.activeTurn, ""
	}
}

func (ts *threadState) addEvidence(it item, turnID string, at time.Time) {
	if it.Type != "userMessage" || it.ClientID == "" {
		return
	}
	if _, ok := ts.evidence[it.ClientID]; ok {
		return
	}
	ts.seq++
	ts.evidence[it.ClientID] = evidence{MessageID: it.ClientID, ItemID: it.ID, TurnID: turnID, At: at, Seq: ts.seq}
}

// itemPhrase is the "doing now" phrase of an in-flight item ("" for items
// that are not work, e.g. messages).
func itemPhrase(it item) string {
	switch it.Type {
	case "commandExecution":
		return doing.Command(it.Command)
	case "fileChange":
		paths := make([]string, 0, len(it.Changes))
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		return doing.Files("editing", paths)
	case "mcpToolCall":
		return doing.MCP(it.Server)
	case "reasoning":
		return doing.Thinking
	case "webSearch":
		return doing.Browsing
	case "collabAgentToolCall", "subAgentActivity":
		return doing.Subagent
	case "contextCompaction":
		return doing.Compacting
	case "imageView":
		return "viewing an image"
	}
	return ""
}

// doingNow is the "doing now" phrase while the turn is active.
func (ts *threadState) doingNow() string {
	if p := ts.inflight.Phrase(); p != "" {
		return p
	}
	for _, s := range ts.subagents {
		if s.End.IsZero() {
			return doing.WaitingOnAgent
		}
	}
	return doing.Thinking
}

func (ts *threadState) onItem(it item, turnID string, started bool, at time.Time) {
	ts.addEvidence(it, turnID, at)
	if it.Type != "subAgentActivity" {
		if ph := itemPhrase(it); started && ph != "" {
			ts.inflight.Start(it.ID, ph)
		} else if !started {
			ts.inflight.Done(it.ID)
		}
	}
	if it.Type == "userMessage" {
		ts.inflight.Clear()
	}
	switch it.Type {
	case "commandExecution":
		if started {
			ts.activity = "running: " + oneLine(it.Command, 140)
		} else {
			ts.activity = "command finished: " + oneLine(it.Command, 120)
		}
	case "agentMessage":
		if !started && it.Text != "" {
			ts.activity = oneLine(it.Text, 160)
		}
	case "fileChange":
		ts.activity = "editing files"
	case "userMessage":
		ts.activity = "prompt: " + oneLine(markerStrip(it.userText()), 120)
	case "subAgentActivity":
		if it.AgentThreadID == "" {
			break
		}
		s := ts.subagents[it.AgentThreadID]
		if s == nil {
			s = &subagent{ThreadID: it.AgentThreadID, Kind: it.AgentPath, Start: at}
			ts.subagents[it.AgentThreadID] = s
			ts.subOrder = append(ts.subOrder, it.AgentThreadID)
		}
		if it.Kind == "completed" || it.Kind == "failed" || it.Kind == "interrupted" {
			if s.End.IsZero() {
				s.End = at
			}
		}
	}
}

func (ts *threadState) onTurnCompleted(t turn, at time.Time) {
	if t.ID == ts.activeTurn {
		ts.lastTurn, ts.activeTurn = t.ID, ""
	}
	ts.inflight.Clear()
	ts.retryAt = time.Time{}
	if t.Status == "failed" && ts.turnsObserved == ts.turnsStarted-1 {
		// A failed turn no observation saw (an automatic capacity
		// continue failing at once) is not reported as a turn: it would
		// end in an idle report repeating the previous message.
		ts.turnsObserved = ts.turnsStarted
	}
	if t.Status == "failed" && t.Error != nil {
		ts.noteFailure(*t.Error, at)
	} else if t.Status == "completed" {
		ts.limit = nil
		ts.lastErr, ts.capRetries = nil, 0
	}
	close(ts.turnDone)
	ts.turnDone = make(chan struct{})
}

// capBackoff spaces automatic continues after capacity failures; after the
// last one the run stays idle with the error shown.
var capBackoff = []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute}

// noteFailure records a failed turn: usage/auth limits feed account
// fallback; a capacity failure schedules the next automatic continue.
func (ts *threadState) noteFailure(e turnError, at time.Time) {
	ts.lastErr, ts.lastErrAt = &e, at
	if le := limitFromError(e, at); le != nil {
		ts.limit = le
		return
	}
	if isCapacityError(e) && ts.capRetries < len(capBackoff) {
		ts.retryAt = at.Add(capBackoff[ts.capRetries])
	}
}

// isCapacityError: the provider was overloaded ("Selected model is at
// capacity"); retrying later with the same account and model can succeed.
func isCapacityError(e turnError) bool {
	var code string
	_ = json.Unmarshal(e.CodexErrorInfo, &code)
	if code == "serverOverloaded" {
		return true
	}
	l := strings.ToLower(e.Message)
	return strings.Contains(l, "at capacity") || strings.Contains(l, "overloaded")
}

// limitFromError maps codexErrorInfo to a LimitEvent (input to fallback).
func limitFromError(e turnError, at time.Time) *contracts.LimitEvent {
	var code string
	_ = json.Unmarshal(e.CodexErrorInfo, &code)
	kind := ""
	switch code {
	case "usageLimitExceeded", "sessionBudgetExceeded":
		kind = "usage_limit"
	case "rateLimitExceeded":
		kind = "rate_limited"
	case "unauthorized":
		kind = "auth_invalid"
	}
	if kind == "" {
		l := strings.ToLower(e.Message)
		switch {
		case strings.Contains(l, "usage limit") || strings.Contains(l, "hit your limit"):
			kind = "usage_limit"
		case strings.Contains(l, "sign in again") || strings.Contains(l, "401") || strings.Contains(l, "unauthorized"):
			kind = "auth_invalid"
		default:
			return nil
		}
	}
	return &contracts.LimitEvent{Kind: kind, Detail: oneLine(e.Message, 200), At: at}
}

type approvalResolution struct {
	source, effect, option string
	at                     time.Time
}

func (r *approvalResolution) apply(ap *contracts.Approval) {
	ap.State = contracts.ApprovalResolved
	ap.Resolution = &contracts.ApprovalResolution{OptionID: r.option, ResolvedAt: r.at, Source: r.source, Effect: r.effect}
}

var secretRe = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_\-]{8,}|(token|secret|password|api[_-]?key)\s*[=:]\s*\S+|bearer\s+[A-Za-z0-9._\-]+)`)

func redact(s string) string { return secretRe.ReplaceAllString(s, "[redacted]") }

func approvalID(runID, requestID string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + requestID))
	return "apr_" + strings.ToUpper(hex.EncodeToString(sum[:13]))
}

// newPending maps an app-server approval/question request to a contract
// approval. requestID = <app-server instance>:<rpc id>:<itemId>.
func newPending(ts *threadState, instance string, rpcID json.RawMessage, method string, p approvalParams, at time.Time) *pendingApproval {
	rid := instance + ":" + string(rpcID) + ":" + p.ItemID
	pa := &pendingApproval{rpcID: rpcID, method: method, params: p, decisions: map[string]json.RawMessage{}}
	ap := contracts.Approval{
		ApprovalID: approvalID(ts.runID, rid), SessionID: ts.sessionID, RunID: ts.runID, RequestID: rid,
		Kind: contracts.ApprovalPermission, CreatedAt: at, State: contracts.ApprovalPending,
	}
	addDecision := func(id, label, kind string, v any) {
		b, _ := json.Marshal(v)
		pa.decisions[id] = b
		ap.Options = append(ap.Options, contracts.ApprovalOption{ID: id, Label: label, Kind: kind})
	}
	stdDecisions := func(list []json.RawMessage) {
		if len(list) == 0 {
			for _, s := range []string{"accept", "acceptForSession", "decline", "cancel"} {
				b, _ := json.Marshal(s)
				list = append(list, b)
			}
		}
		for _, d := range list {
			var s string
			if json.Unmarshal(d, &s) == nil {
				switch s {
				case "accept":
					addDecision("accept", "Allow", "allow", s)
				case "acceptForSession":
					addDecision("acceptForSession", "Allow for this session", "allow_always", s)
				case "decline":
					addDecision("decline", "Deny", "deny", s)
				case "cancel":
					addDecision("cancel", "Deny and stop the turn", "deny", s)
				}
				continue
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal(d, &obj) == nil {
				for k := range obj {
					label := "Always allow"
					if k == "acceptWithExecpolicyAmendment" {
						label = "Always allow this command"
					} else if k == "applyNetworkPolicyAmendment" {
						label = "Allow this network access from now on"
					}
					pa.decisions[k] = d
					ap.Options = append(ap.Options, contracts.ApprovalOption{ID: k, Label: label, Kind: "allow_always"})
				}
			}
		}
	}
	wrap := func(d json.RawMessage) json.RawMessage {
		b, _ := json.Marshal(map[string]json.RawMessage{"decision": d})
		return b
	}
	switch method {
	case "item/commandExecution/requestApproval":
		ap.Title = "Run command?"
		ap.Body = redact(oneLine(p.Command, 500))
		stdDecisions(p.AvailableDecisions)
	case "item/fileChange/requestApproval":
		ap.Title = "Apply file changes?"
		ap.Body = redact(oneLine(p.Reason+" "+p.GrantRoot, 500))
		stdDecisions(nil)
	case "item/permissions/requestApproval":
		ap.Title = "Grant additional permissions?"
		ap.Body = redact(oneLine(p.Reason+" "+string(p.Permissions), 500))
		grant, _ := json.Marshal(map[string]json.RawMessage{"permissions": p.Permissions})
		pa.decisions["accept"] = grant
		pa.decisions["decline"] = json.RawMessage(`{"permissions":{}}`)
		ap.Options = []contracts.ApprovalOption{{ID: "accept", Label: "Grant", Kind: "allow"}, {ID: "decline", Label: "Deny", Kind: "deny"}}
		ap.State = contracts.ApprovalPending
		pa.approval = ap
		return pa
	case "item/tool/requestUserInput":
		ap.Kind = contracts.ApprovalQuestion
		ap.AllowText = true
		var qs []string
		for _, q := range p.Questions {
			qs = append(qs, q.Header+": "+q.Question)
		}
		ap.Title = "Question from the agent"
		ap.Body = redact(oneLine(strings.Join(qs, " | "), 500))
		if len(p.Questions) == 1 {
			for _, o := range p.Questions[0].Options {
				ap.Options = append(ap.Options, contracts.ApprovalOption{ID: o.Label, Label: o.Label, Kind: "choice"})
			}
		}
		pa.approval = ap
		return pa
	}
	// Decision-style responses are wrapped as {"decision": …}.
	for k, v := range pa.decisions {
		pa.decisions[k] = wrap(v)
	}
	pa.approval = ap
	return pa
}

// questionAnswer builds item/tool/requestUserInput's response.
func questionAnswer(p approvalParams, optionID, text string) json.RawMessage {
	ans := text
	if ans == "" {
		ans = optionID
	}
	m := map[string]any{}
	for _, q := range p.Questions {
		m[q.ID] = map[string]any{"answers": []string{ans}}
	}
	b, _ := json.Marshal(map[string]any{"answers": m})
	return b
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func markerStrip(s string) string {
	if i := strings.Index(s, "[pyre-msg:"); i == 0 {
		if j := strings.Index(s, "]"); j > 0 {
			return strings.TrimSpace(s[j+1:])
		}
	}
	return s
}
