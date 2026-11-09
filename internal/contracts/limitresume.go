package contracts

import "time"

// Automatic resume after an account limit (decision I70). When a run
// stopped on a usage or spend limit and no account had room (a waiting
// fallback plan), or a limit paused automatic switching and left the run
// waiting on its own account, the hub schedules one resume for the
// session and, when capacity returns, asks the session's node to continue
// it with ActionSessionLimitResume. The node decides which account has
// room; the hub decides whether the session may be touched at all
// (archive state, the owner's and other sessions' input since the stop).

const (
	// ActionSessionLimitResume continues a session that stopped on an
	// account limit (LimitResumeParams -> LimitResumeResult). Hub-issued
	// only; asynchronous (a continuation launches a run).
	ActionSessionLimitResume = "session.limit_resume"
	// NodeCapLimitResume is the node capability (Node.Capabilities) of a
	// pyred that serves ActionSessionLimitResume. The hub schedules
	// automatic resumes only on nodes that advertise it; older pyreds never
	// do, and their sessions read "resumable after" as before.
	NodeCapLimitResume = "session.limit_resume"

	// AdapterDataLimitAt (Run.AdapterData, set by pyred, additive) is when
	// the run last reported a usage or rate limit (RFC 3339): a turn the
	// continue prompt started that hit the limit again shows as a newer
	// value.
	AdapterDataLimitAt = "pyre.limit_at"
)

// Limit resume modes (LimitResumeParams.Mode).
const (
	// LimitResumeContinue: the limited run ended (the fallback stopped it
	// and found no account with room); a continuation starts on the best
	// eligible account, as an account switch would.
	LimitResumeContinue = "continue"
	// LimitResumeInPlace: the limited run is still up, waiting on its own
	// account (switching was paused); the node sends it the continue
	// prompt once that account has room.
	LimitResumeInPlace = "in_place"
)

// Limit resume outcomes (LimitResumeResult.Outcome).
const (
	LimitResumed       = "resumed"
	LimitResumeNoRoom  = "no_room" // no account has room yet; ResetAt is when one may
	LimitResumeRefused = "refused" // the session must not be resumed (Detail says why)
)

// LimitResumeParams names the limited run to continue.
type LimitResumeParams struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Mode      string `json:"mode"`
}

// LimitResumeResult is what the node did.
type LimitResumeResult struct {
	Outcome   string `json:"outcome"`
	AccountID string `json:"account_id,omitempty"`
	// NewRunID is the continuation (mode continue) or the run itself
	// (in_place).
	NewRunID string `json:"new_run_id,omitempty"`
	// Kind is how the session goes on: native_resume, handoff or in_place.
	Kind    string     `json:"kind,omitempty"`
	ResetAt *time.Time `json:"reset_at,omitempty"`
	Detail  string     `json:"detail,omitempty"`
}
