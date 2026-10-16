package claude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
	"github.com/jhh3/pyre/internal/runtime/doing"
)

// evidence is acceptance (or consumption) evidence for one Pyre message.
type evidence struct {
	MessageID string
	Kind      contracts.EvidenceCategory
	Ref       string
	At        time.Time
	Source    byte // 'e' events.jsonl, 't' transcript
	Offset    int64
}

type subagent struct {
	ID, Kind   string
	Start, End time.Time
}

type approvalTrack struct {
	RequestID, ToolUseID, ToolName, EventID string
	CreatedAt, ExpiresAt                    time.Time
	HookPID                                 int
	AnsweredAt                              time.Time // broker returned Pyre's decision
	Behavior                                string
	TimedOut, Withdrawn                     bool
	Summary                                 string
	// DialogSeenAt: an observation showed a permission dialog while this
	// request was open. ScreenAnsweredAt: a later observation showed the
	// dialog gone and the turn working (answered in the terminal; Claude
	// fires no hook for a TUI "Yes" until the tool finishes).
	DialogSeenAt, ScreenAnsweredAt time.Time
}

// runState is the incremental fold of a run's hook events and transcript.
type runState struct {
	eventsOff int64
	tPath     string
	tOff      int64

	hasStart       bool
	sessionID      string
	transcriptPath string
	state          contracts.RunState
	reason         string
	lastStateAt    time.Time
	lastEventAt    time.Time
	ended          bool
	activity       string
	inflight       doing.Inflight // tool calls between PreToolUse and their Post/denial
	doingHint      string         // e.g. compacting, when no tool is in flight

	approvals     map[string]*approvalTrack
	approvalOrder []string
	toolPostAt    map[string]time.Time // tool ran: PostToolUse or PostToolUseFailure
	toolDeniedAt  map[string]time.Time // auto mode denied it: PermissionDenied
	humanTool     string               // tool_use_id of a pending human-only dialog tool
	permPrompt    bool                 // waiting_approval came from a permission request/prompt

	subagents map[string]*subagent
	subOrder  []string

	limit *contracts.LimitEvent

	accepted map[string][]evidence // msg id -> acceptance evidence
	consumed map[string]evidence   // msg id -> consumption evidence (prompt user record)
	acked    map[string]time.Time  // msg id -> ack_message tool call (doorbell path)

	doorbells      int       // doorbell notifications seen in the transcript
	lastDoorbell   time.Time // latest one
	doorbellFailed int       // Claude reported the monitor script failed (it is not restarted)

	lastTurnDurationAt time.Time

	pluginVersion string                   // Claude version the delivery module reported at load
	pluginClaimed map[string]time.Time     // msg id -> the module claimed it from the spool
	pluginFailed  map[string]pluginFailure // msg id -> dropped / submit failed
}

func newRunState() *runState {
	return &runState{
		state: contracts.RunStarting, reason: "launching; no SessionStart hook yet",
		approvals: map[string]*approvalTrack{}, toolPostAt: map[string]time.Time{}, toolDeniedAt: map[string]time.Time{},
		subagents: map[string]*subagent{}, accepted: map[string][]evidence{}, consumed: map[string]evidence{}, acked: map[string]time.Time{},
		pluginClaimed: map[string]time.Time{}, pluginFailed: map[string]pluginFailure{},
	}
}

func (rs *runState) set(s contracts.RunState, reason string, at time.Time) {
	rs.state, rs.reason, rs.lastStateAt = s, reason, at
	rs.permPrompt = false
}

func (rs *runState) addEvidence(e evidence) {
	for _, x := range rs.accepted[e.MessageID] {
		if x.Kind == e.Kind && x.Ref == e.Ref {
			return
		}
	}
	rs.accepted[e.MessageID] = append(rs.accepted[e.MessageID], e)
}

// humanOnlyTools show their own dialogs that Pyre cannot answer yet.
var humanOnlyTools = map[string]bool{"AskUserQuestion": true, "ExitPlanMode": true}

func (rs *runState) applyHook(r hookRecord) {
	p := r.payload()
	rs.lastEventAt = r.At
	switch r.Event {
	case "SessionStart":
		rs.hasStart, rs.ended = true, false
		if r.SessionID != "" {
			rs.sessionID = r.SessionID
		}
		if r.TranscriptPath != "" {
			rs.transcriptPath = r.TranscriptPath
		}
		rs.set(contracts.RunWaitingInput, "session started ("+str(p, "source")+")", r.At)
		rs.activity = "session started"
		rs.inflight.Clear()
		rs.doingHint = ""
	case "UserPromptSubmit":
		rs.set(contracts.RunRunning, "prompt submitted", r.At)
		for _, m := range r.Markers {
			rs.addEvidence(evidence{MessageID: m, Kind: contracts.EvidenceClaudePromptHook, Ref: r.ID, At: r.At, Source: 'e', Offset: r.offset})
		}
		rs.activity = "prompt: " + oneLine(markerRe.ReplaceAllString(str(p, "prompt"), ""), 120)
		rs.inflight.Clear()
		rs.doingHint = ""
	case "PreToolUse":
		if rs.hiddenAgentEvent(p) {
			break
		}
		if humanOnlyTools[r.ToolName] {
			rs.humanTool = r.ToolUseID
			rs.set(contracts.RunWaitingApproval, "needs human: "+r.ToolName+" dialog", r.At)
		} else if rs.state != contracts.RunExiting {
			rs.set(contracts.RunRunning, "tool: "+r.ToolName, r.At)
		}
		rs.activity = r.ToolName + ": " + oneLine(toolSummary(p), 120)
		ti, _ := p["tool_input"].(map[string]any)
		rs.inflight.Start(r.ToolUseID, doing.ClaudeTool(r.ToolName, ti))
	case "PostToolUse", "PostToolUseFailure":
		// Both mean the tool executed (a failure is still execution: an
		// approved call that then fails was effectively allowed). Verified
		// on Claude Code 2.1.289: a call denied by the PermissionRequest hook
		// or in the TUI fires neither.
		if r.ToolUseID != "" {
			rs.toolPostAt[r.ToolUseID] = r.At
			rs.inflight.Done(r.ToolUseID)
		}
		if r.ToolUseID != "" && r.ToolUseID == rs.humanTool {
			rs.humanTool = ""
		}
		if rs.hiddenAgentEvent(p) {
			break
		}
		switch r.ToolName {
		case toolPyreInbox, toolPyreGetMessage:
			for _, m := range r.Markers {
				rs.addEvidence(evidence{MessageID: m, Kind: contracts.EvidenceClaudeInboxRead, Ref: r.ID, At: r.At, Source: 'e', Offset: r.offset})
			}
		case toolPyreAck:
			for _, m := range r.Markers {
				if _, ok := rs.acked[m]; !ok {
					rs.acked[m] = r.At
				}
			}
		}
		if rs.state != contracts.RunExiting {
			verb := "tool finished: "
			if r.Event == "PostToolUseFailure" {
				verb = "tool failed: "
			}
			rs.set(contracts.RunRunning, verb+r.ToolName, r.At)
		}
	case "PermissionDenied":
		// Auto mode's classifier denied the call; the turn continues.
		if r.ToolUseID != "" {
			rs.toolDeniedAt[r.ToolUseID] = r.At
			rs.inflight.Done(r.ToolUseID)
		}
		if rs.state != contracts.RunExiting {
			rs.set(contracts.RunRunning, "permission denied (auto mode): "+r.ToolName, r.At)
		}
		rs.activity = "denied: " + r.ToolName + ": " + oneLine(str(p, "reason"), 120)
	case "PostCompact":
		rs.activity = "context compacted (" + str(p, "compaction_type") + ")"
		rs.doingHint = ""
	case "PermissionRequest":
		rs.set(contracts.RunWaitingApproval, "permission request ("+r.ToolName+")", r.At)
		rs.permPrompt = true
		if r.RequestID != "" {
			if _, ok := rs.approvals[r.RequestID]; !ok {
				rs.approvalOrder = append(rs.approvalOrder, r.RequestID)
			}
			rs.approvals[r.RequestID] = &approvalTrack{
				RequestID: r.RequestID, ToolUseID: r.ToolUseID, ToolName: r.ToolName, EventID: r.ID,
				CreatedAt: r.At, HookPID: r.HookPID, Summary: toolSummary(p),
			}
		}
		rs.activity = "waiting for approval: " + r.ToolName
	case "Notification":
		switch str(p, "notification_type") {
		case "permission_prompt":
			rs.set(contracts.RunWaitingApproval, "permission prompt shown", r.At)
			rs.permPrompt = true
		case "idle_prompt":
			if rs.state != contracts.RunWaitingApproval {
				rs.set(contracts.RunWaitingInput, "idle prompt", r.At)
			}
		case "elicitation_dialog":
			rs.set(contracts.RunWaitingApproval, "needs human: elicitation dialog", r.At)
		}
		if m := str(p, "message"); m != "" {
			rs.activity = oneLine(m, 160)
		}
	case "Stop":
		if bg := backgroundTasks(p); bg > 0 {
			rs.set(contracts.RunWaitingBackground, fmt.Sprintf("turn ended; %d background task(s) may start a new turn", bg), r.At)
		} else {
			rs.set(contracts.RunWaitingInput, "turn ended (Stop)", r.At)
		}
		rs.humanTool = ""
		rs.limit = nil
		rs.inflight.Clear()
		rs.doingHint = ""
		if m := str(p, "last_assistant_message"); m != "" {
			rs.activity = oneLine(m, 160)
		}
	case "StopFailure":
		rs.set(contracts.RunWaitingInput, "turn failed (StopFailure)", r.At)
		rs.humanTool = ""
		rs.inflight.Clear()
		rs.doingHint = ""
		if le := detectLimit(string(r.Payload), r.At); le != nil {
			rs.limit = le
		}
		rs.activity = "turn failed"
	case "SubagentStart":
		id := str(p, "agent_id")
		if id != "" {
			if _, ok := rs.subagents[id]; !ok {
				rs.subOrder = append(rs.subOrder, id)
			}
			rs.subagents[id] = &subagent{ID: id, Kind: str(p, "agent_type"), Start: r.At}
		}
		if rs.state != contracts.RunExiting {
			rs.set(contracts.RunRunning, "subagent started", r.At)
		}
	case "SubagentStop":
		if s, ok := rs.subagents[str(p, "agent_id")]; ok {
			s.End = r.At
		}
	case "SessionEnd":
		rs.ended = true
		rs.set(contracts.RunExiting, "SessionEnd ("+str(p, "reason")+")", r.At)
		rs.inflight.Clear()
	case "PreCompact":
		rs.activity = "compacting context"
		rs.doingHint = doing.Compacting
	case evApprovalAnswered:
		if t := rs.approvals[r.RequestID]; t != nil {
			t.AnsweredAt, t.Behavior = r.At, r.Behavior
		}
		if r.Behavior == "allow" {
			rs.set(contracts.RunRunning, "approval answered through Pyre", r.At)
		}
	case evApprovalTimeout:
		if t := rs.approvals[r.RequestID]; t != nil {
			t.TimedOut = true
		}
	case evApprovalWithdrawn:
		if t := rs.approvals[r.RequestID]; t != nil {
			t.Withdrawn = true
		}
	case evStopInjected:
		if r.MessageID != "" {
			rs.addEvidence(evidence{MessageID: r.MessageID, Kind: contracts.EvidenceClaudeStopHook, Ref: r.ID, At: r.At, Source: 'e', Offset: r.offset})
		}
		rs.set(contracts.RunRunning, "continuing after Stop-hook injection", r.At)
	case evAskNudged:
		rs.set(contracts.RunRunning, "continuing: asked to file its owner question as an ask", r.At)
	case evPageNudged:
		rs.set(contracts.RunRunning, "continuing: asked to update its project page", r.At)
	case claudeplugin.EvPluginReady:
		rs.pluginVersion = str(p, "version")
	case claudeplugin.EvPluginClaim:
		if r.MessageID != "" {
			rs.pluginClaimed[r.MessageID] = r.At
		}
	case claudeplugin.EvPluginSubmitted:
		if r.MessageID != "" {
			rs.addEvidence(evidence{MessageID: r.MessageID, Kind: contracts.EvidenceClaudePluginPrompt, Ref: r.ID, At: r.At, Source: 'e', Offset: r.offset})
		}
	case claudeplugin.EvPluginDropped, claudeplugin.EvPluginFailed:
		if r.MessageID != "" {
			why := str(p, "reason")
			if why == "" {
				why = str(p, "error")
			}
			rs.pluginFailed[r.MessageID] = pluginFailure{Event: r.Event, Reason: oneLine(why, 300), At: r.At}
		}
	}
}

// pluginFailure is the plugin module's report that a claimed message did
// not enter the session (dropped by a prompt hook) or that its submit call
// failed (outcome unknown).
type pluginFailure struct {
	Event, Reason string
	At            time.Time
}

// hiddenAgentEvent reports whether a tool hook event comes from one of
// Claude Code's own background agents rather than the conversation: it
// carries an agent_id that no SubagentStart announced, and the turn is not
// open. Observed on 2.1.292: after Stop, internal agents run tools (e.g.
// ScheduleWakeup {"stop":true}) with only a SubagentStop and often no
// PostToolUse. Folding them in showed an idle session as running ("using
// ScheduleWakeup") until the next idle_prompt, and that false
// waiting_input -> running -> waiting_input cycle opens a new child turn,
// so the parent got a second automatic report.
func (rs *runState) hiddenAgentEvent(p map[string]any) bool {
	id := str(p, "agent_id")
	if id == "" {
		return false
	}
	if _, ok := rs.subagents[id]; ok {
		return false
	}
	return rs.state != contracts.RunRunning && rs.state != contracts.RunWaitingApproval
}

// doingNow is the "doing now" phrase while the run works: the in-flight
// tool calls, else a hint (compacting), else waiting on a running subagent,
// else thinking (the model is generating between tool calls).
func (rs *runState) doingNow() string {
	if p := rs.inflight.Phrase(); p != "" {
		return p
	}
	if rs.doingHint != "" {
		return rs.doingHint
	}
	for _, s := range rs.subagents {
		if s.End.IsZero() {
			return doing.WaitingOnAgent
		}
	}
	return doing.Thinking
}

// backgroundTasks counts the harness's background tasks at Stop, leaving
// out Pyre's own doorbell monitor: Claude lists the plugin monitor as a
// running shell task, but it never starts work on its own.
func backgroundTasks(p map[string]any) int {
	switch v := p["background_tasks"].(type) {
	case []any:
		n := 0
		for _, t := range v {
			if !pyreMonitorTask(t) {
				n++
			}
		}
		return n
	case map[string]any:
		n := 0
		for _, t := range v {
			if !pyreMonitorTask(t) {
				n++
			}
		}
		return n
	case float64:
		return int(v)
	}
	return 0
}

// pyreMonitorTask reports whether a Stop background_tasks entry is the
// doorbell monitor (verified shape on 2.1.289: {type: shell, description:
// <monitor description>, command: "<pyred> inbox-watch --run-dir …"}).
func pyreMonitorTask(t any) bool {
	m, ok := t.(map[string]any)
	if !ok {
		return false
	}
	return str(m, "description") == claudeplugin.MonitorDescription || strings.Contains(str(m, "command"), " inbox-watch")
}

func (rs *runState) applyTranscript(t transcriptRecord, off int64) {
	at := t.at()
	switch {
	case t.Type == "system" && t.Subtype == "turn_duration":
		if at.After(rs.lastTurnDurationAt) {
			rs.lastTurnDurationAt = at
		}
	case t.Type == "queue-operation" && t.Operation == "enqueue":
		for _, m := range findMarkers(t.queueContent()) {
			rs.addEvidence(evidence{MessageID: m, Kind: contracts.EvidenceClaudeQueue, Ref: "queue-enqueue@" + t.Timestamp, At: at, Source: 't', Offset: off})
		}
	case t.isPromptRecord() && strings.Contains(t.text(), "<task-notification>") && strings.Contains(t.text(), "New Pyre messages for this session"):
		if strings.Contains(t.text(), "<status>failed</status>") || strings.Contains(t.text(), "script failed") {
			rs.doorbellFailed++
			rs.activity = "doorbell monitor failed (Claude does not restart it)"
			break
		}
		rs.doorbells++
		rs.lastDoorbell = at
		rs.activity = "doorbell: new Pyre mail notified"
	case t.isPromptRecord():
		for _, m := range findMarkers(t.text()) {
			kind := contracts.EvidenceClaudeUserRecord
			if t.IsMeta {
				kind = contracts.EvidenceClaudeStopHook
			}
			e := evidence{MessageID: m, Kind: kind, Ref: t.UUID, At: at, Source: 't', Offset: off}
			rs.addEvidence(e)
			if _, ok := rs.consumed[m]; !ok {
				rs.consumed[m] = e
			}
		}
	case t.Type == "assistant" && t.IsAPIError:
		if le := detectLimit(t.text(), at); le != nil {
			rs.limit = le
		}
	}
}

// derive returns the observed state, reconciling hook state with the
// transcript: an interrupt or a TUI denial ends the turn without a Stop hook
// but writes system/turn_duration.
func (rs *runState) derive() (contracts.RunState, string) {
	if !rs.hasStart && !rs.ended {
		return rs.state, rs.reason
	}
	s, reason := rs.state, rs.reason
	if (s == contracts.RunRunning || s == contracts.RunWaitingApproval) && rs.lastTurnDurationAt.After(rs.lastStateAt) {
		return contracts.RunWaitingInput, "turn ended without Stop hook (transcript turn_duration: interrupted or denied in the TUI)"
	}
	return s, reason
}

// bestEvidence returns the strongest acceptance evidence for a message.
func (rs *runState) bestEvidence(msgID string) (evidence, bool) {
	evs := rs.accepted[msgID]
	if len(evs) == 0 {
		return evidence{}, false
	}
	rank := map[contracts.EvidenceCategory]int{
		contracts.EvidenceClaudePluginPrompt: 6, contracts.EvidenceClaudeInboxRead: 5, contracts.EvidenceClaudePromptHook: 4, contracts.EvidenceClaudeStopHook: 3,
		contracts.EvidenceClaudeQueue: 2, contracts.EvidenceClaudeUserRecord: 1,
	}
	best := evs[0]
	for _, e := range evs[1:] {
		if rank[e.Kind] > rank[best.Kind] {
			best = e
		}
	}
	return best, true
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncStr(s, n)
}

// Limit detection (input to account fallback, WP13). Patterns observed in
// Claude Code error text; matching is deliberately narrow.
//
// Claude Code 2.1.x on a subscription limit (server, 2026-10-08): the
// StopFailure hook carries {"error":"rate_limit","last_assistant_message":
// "You've hit your session limit · resets 3pm (America/Los_Angeles)"} and
// the same text is the transcript's API-error record and the screen line.
// "session"/"weekly"/"Opus" limits, curly apostrophes and the StopFailure
// error codes are all recognised.
var (
	limitUsageRe = regexp.MustCompile(`(?i)(usage limit reached|you(?:'|’)?ve hit your (?:[\w-]+ )?limit|out of extra usage|limit will reset|5-hour limit reached|weekly limit reached|session limit reached|"error"\s*:\s*"billing_error")`)
	limitRateRe  = regexp.MustCompile(`(?i)(rate_limit_error|rate limit exceeded|too many requests|"error"\s*:\s*"rate_limit")`)
	limitAuthRe  = regexp.MustCompile(`(?i)(invalid api key|oauth token (has )?expired|please run /login|authentication_error|not logged in|invalid authentication credentials|token has been revoked|"error"\s*:\s*"authentication_failed")`)
	limitEpochRe = regexp.MustCompile(`limit reached\|(\d{9,11})`)
	// "resets 3pm (America/Los_Angeles)", "resets 3:30pm", "resets Oct 10,
	// 9am (Europe/Berlin)".
	limitResetRe = regexp.MustCompile(`(?i)resets\s+(?:([a-z]{3})[a-z]*\.?\s+(\d{1,2}),?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap]m)(?:\s*\(([^)]+)\))?`)
)

func detectLimit(text string, at time.Time) *contracts.LimitEvent {
	if text == "" {
		return nil
	}
	var kind string
	switch {
	case limitUsageRe.MatchString(text) || limitEpochRe.MatchString(text):
		kind = "usage_limit"
	case limitAuthRe.MatchString(text):
		kind = "auth_invalid"
	case limitRateRe.MatchString(text):
		kind = "rate_limited"
	default:
		return nil
	}
	le := &contracts.LimitEvent{Kind: kind, At: at, Detail: limitDetail(text)}
	if m := limitEpochRe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			t := time.Unix(n, 0).UTC()
			le.ResetAt = &t
		}
	}
	if le.ResetAt == nil && kind != "auth_invalid" {
		le.ResetAt = parseResetClock(text, at)
	}
	return le
}

// parseResetClock reads Claude's "resets 3pm (America/Los_Angeles)": the
// next such wall-clock time after at in that zone (local when unnamed or
// unknown). nil when absent.
func parseResetClock(text string, at time.Time) *time.Time {
	m := limitResetRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	loc := time.Local
	if m[6] != "" {
		if l, err := time.LoadLocation(strings.TrimSpace(m[6])); err == nil {
			loc = l
		}
	}
	h, _ := strconv.Atoi(m[3])
	mi := 0
	if m[4] != "" {
		mi, _ = strconv.Atoi(m[4])
	}
	if h < 1 || h > 12 || mi > 59 {
		return nil
	}
	h %= 12
	if strings.EqualFold(m[5], "pm") {
		h += 12
	}
	if at.IsZero() {
		at = time.Now()
	}
	now := at.In(loc)
	var t time.Time
	if m[1] != "" {
		mon, err := time.Parse("Jan", strings.ToUpper(m[1][:1])+strings.ToLower(m[1][1:3]))
		day, _ := strconv.Atoi(m[2])
		if err != nil || day < 1 || day > 31 {
			return nil
		}
		t = time.Date(now.Year(), mon.Month(), day, h, mi, 0, 0, loc)
		if t.Before(now.Add(-24 * time.Hour)) {
			t = t.AddDate(1, 0, 0)
		}
	} else {
		t = time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, loc)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
	}
	u := t.UTC()
	return &u
}

// limitDetail keeps a short, credential-free excerpt around the match
// (StopFailure: "<error code>: <message>").
func limitDetail(text string) string {
	var s string
	if json.Valid([]byte(text)) {
		var m map[string]any
		if json.Unmarshal([]byte(text), &m) == nil {
			for _, k := range []string{"error", "error_message", "message", "reason", "stop_reason"} {
				if v := str(m, k); v != "" {
					s = v
					break
				}
			}
			if msg := str(m, "last_assistant_message"); msg != "" && msg != s {
				s = strings.TrimPrefix(s+": "+msg, ": ")
			}
		}
	}
	if s == "" {
		s = text
	}
	return redact(oneLine(s, 200))
}

var secretRe = regexp.MustCompile(`(?i)(sk-ant-[A-Za-z0-9_\-]+|(token|secret|password|api[_-]?key)\s*[=:]\s*\S+|bearer\s+[A-Za-z0-9._\-]+)`)

// redact masks obvious credentials in short display strings.
func redact(s string) string { return secretRe.ReplaceAllString(s, "[redacted]") }
