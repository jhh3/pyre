package agy

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// runState is what the hook records and the transcript say about a run.
type runState struct {
	eventsOff int64
	tPath     string
	tOff      int64
	tSeq      int64

	conversationID string // the main conversation (never a subagent's)
	transcriptPath string // from hooks (authoritative), else guessed
	model          string

	started   bool      // a hook fired: agy began a conversation turn
	turnOpen  bool      // main conversation: PreInvocation/PostToolUse/PostInvocation since its last Stop
	lastEvent time.Time // newest hook of any conversation (subagents included)
	lastStop  hookRecord
	lastTool  string
	toolAt    time.Time

	// subs are the run's subagent conversations. agy runs each subagent
	// (invoke_subagent) as its own conversation whose hooks go to the same
	// hooks.json, so they land in the run's event file between the main
	// conversation's (seen on 1.4.7: two subagents' SessionStart,
	// Pre/PostInvocation and Stop interleaved with the parent's). A
	// subagent's Stop is not the run's turn end, and its conversation and
	// transcript are not the run's.
	subs map[string]*subState

	// limit is the provider limit a failed Stop named (terminationReason
	// ERROR with RESOURCE_EXHAUSTED), until a main turn ends normally.
	limit *contracts.LimitEvent

	// idleSince is when the screen was first seen at the idle prompt
	// since it last showed a running turn (zero: not idle); derive holds a
	// turn end back until that has lasted idleSettle.
	idleSince time.Time

	// accepted maps message id -> the USER_INPUT step that carried its
	// marker (acceptance evidence).
	accepted map[string]evidence
}

// subState is one subagent conversation.
type subState struct {
	open     bool // a hook since its last Stop
	lastStop time.Time
}

type evidence struct {
	Ref string // "<conversation>#<step_index>"
	At  time.Time
	Off int64 // transcript byte offset of the step (receipt cursors)
}

func newRunState() *runState {
	return &runState{accepted: map[string]evidence{}, subs: map[string]*subState{}}
}

// background reports whether the main conversation's last Stop left work
// that may start a new turn by itself: agy said it was not fully idle (a
// background command or a subagent still running; every such Stop seen on
// 1.4.7 was followed by a wake of the same conversation), or a subagent
// conversation is still open.
func (rs *runState) background() bool {
	if rs.turnOpen || !rs.started {
		return false
	}
	if f := rs.lastStop.FullyIdle; f != nil && !*f {
		return true
	}
	return rs.openSubs() > 0
}

func (rs *runState) openSubs() int {
	n := 0
	for _, s := range rs.subs {
		if s.open {
			n++
		}
	}
	return n
}

// isSub reports whether hook r belongs to a subagent conversation,
// recording a new one. The main conversation is the first one seen (or the
// one the run resumed); another conversation that starts while the main
// one runs a turn (or while other subagents run) is a subagent, and one
// that starts otherwise replaces the main one (agy's /new).
func (rs *runState) isSub(r hookRecord) bool {
	c := r.ConversationID
	if c == "" || c == rs.conversationID {
		return false
	}
	if _, ok := rs.subs[c]; ok {
		return true
	}
	if rs.conversationID != "" && (rs.turnOpen || rs.openSubs() > 0) {
		rs.subs[c] = &subState{}
		return true
	}
	return false
}

// applySub folds a subagent conversation's hook: activity of the run, not
// its turn state.
func (rs *runState) applySub(r hookRecord) {
	s := rs.subs[r.ConversationID]
	if r.Event != "Stop" && !s.lastStop.IsZero() && !r.At.After(s.lastStop) {
		return // a late append of the subagent's own last hook
	}
	if r.Event != "Stop" && rs.lastStop.FullyIdle != nil && *rs.lastStop.FullyIdle && !r.At.After(rs.lastStop.At) {
		return // from before the main Stop that said nothing else runs
	}
	if r.At.After(rs.lastEvent) {
		rs.lastEvent = r.At
	}
	switch r.Event {
	case "Stop":
		s.open, s.lastStop = false, r.At
	case "PostToolUse":
		s.open = true
		rs.lastTool, rs.toolAt = r.Tool, r.At
	default:
		s.open = true
	}
}

func (rs *runState) applyHook(r hookRecord) {
	rs.started = true
	if rs.isSub(r) {
		if le := stopLimit(r); le != nil && le.Kind == "usage_limit" {
			rs.limit = le // the subagent runs on the run's login
		}
		rs.applySub(r)
		return
	}
	if r.Event != "Stop" && r.Event != "SessionStart" && !rs.lastStop.At.IsZero() && !r.At.After(rs.lastStop.At) {
		// Each hook is its own process: a late append of the turn's last
		// PostInvocation after its Stop must not reopen the turn.
		return
	}
	if r.At.After(rs.lastEvent) {
		rs.lastEvent = r.At
	}
	if r.ConversationID != "" {
		rs.conversationID = r.ConversationID
	}
	if r.TranscriptPath != "" {
		rs.transcriptPath = r.TranscriptPath
	}
	if r.Model != "" {
		rs.model = r.Model
	}
	switch r.Event {
	case "SessionStart", "PreInvocation", "PostInvocation":
		rs.turnOpen = true
	case "PostToolUse":
		rs.turnOpen = true
		rs.lastTool, rs.toolAt = r.Tool, r.At
	case "Stop":
		rs.turnOpen = false
		rs.lastStop = r
		// Each main Stop says what limit, if any, the turn ended on: a
		// later failure that names none must not read as the old limit.
		rs.limit = stopLimit(r)
		if r.FullyIdle != nil && *r.FullyIdle {
			// agy says nothing else runs: a subagent whose Stop was lost
			// does not keep the run busy.
			for _, s := range rs.subs {
				s.open = false
			}
		}
	}
}

func (rs *runState) applyTranscript(line []byte, off int64) {
	var r transcriptRecord
	if json.Unmarshal(line, &r) != nil {
		return
	}
	rs.tSeq++
	if r.Type != "USER_INPUT" {
		return
	}
	for _, m := range markerRe.FindAllStringSubmatch(userRequest(r.Content), -1) {
		if _, dup := rs.accepted[m[1]]; dup {
			continue
		}
		at := r.CreatedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		rs.accepted[m[1]] = evidence{Ref: rs.conversationID + "#" + strconv.FormatInt(r.StepIndex, 10), At: at, Off: off}
	}
}

// hookState is the run state the hook records alone imply.
func (rs *runState) hookState() (contracts.RunState, string) {
	switch {
	case !rs.started:
		return contracts.RunWaitingInput, "agy ready (no turn yet)"
	case rs.turnOpen:
		return contracts.RunRunning, "turn in progress"
	}
	reason := "turn ended"
	switch t := rs.lastStop.Termination; {
	case t == "ERROR":
		// "turn failed": the hub words the report of a failed turn from
		// the node's limit plan (hub/limitreport.go), never "finished".
		reason = "turn failed (ERROR)"
		if rs.limit != nil {
			reason = "turn failed (ERROR: " + rs.limit.Kind + ")"
		}
	case t != "" && t != "NO_TOOL_CALL":
		reason += " (" + t + ")"
	}
	if rs.background() {
		what := "background work"
		if n := rs.openSubs(); n > 0 {
			what = strconv.Itoa(n) + " subagent(s)"
		}
		return contracts.RunWaitingBackground, reason + "; " + what + " may start a new turn"
	}
	return contracts.RunWaitingInput, reason
}
