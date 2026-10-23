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

	conversationID string
	transcriptPath string // from hooks (authoritative), else guessed
	model          string

	started   bool // a hook fired: agy began a conversation turn
	turnOpen  bool // PreInvocation/PostToolUse/PostInvocation since the last Stop
	lastEvent time.Time
	lastStop  hookRecord
	lastTool  string
	toolAt    time.Time

	// accepted maps message id -> the USER_INPUT step that carried its
	// marker (acceptance evidence).
	accepted map[string]evidence
}

type evidence struct {
	Ref string // "<conversation>#<step_index>"
	At  time.Time
	Off int64 // transcript byte offset of the step (receipt cursors)
}

func newRunState() *runState { return &runState{accepted: map[string]evidence{}} }

func (rs *runState) applyHook(r hookRecord) {
	rs.started = true
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
	if t := rs.lastStop.Termination; t != "" && t != "NO_TOOL_CALL" {
		reason += " (" + t + ")"
	}
	return contracts.RunWaitingInput, reason
}
