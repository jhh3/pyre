package contracts

import "context"

// OpInboxWait (W-localwait, additive, optional, node-local only): block on
// the local pyred until a session may have new mail, without holding a
// request against the hub. pyred wakes the waiter when a deliver command for
// that session arrives over the node link, when the hub link (re)connects,
// or when the wait times out (<= 60s). The answer carries no message
// content: the caller then lists the inbox once (non-waiting) through the
// normal messages.list, which stays the source of truth for content, cursors
// and acks. Only pyred's local API serves it; a hub (or an older pyred)
// answers 404, which clients report as ErrUnsupported and fall back to a
// messages.list long-poll.
//
// Wakeups are coarse ("may have"): a wake without new mail is normal, and
// mail the hub does not route through a deliver command (mode info, a
// session whose run the hub does not consider live) is only noticed on the
// timeout. Callers that must see such mail promptly keep long-polling the hub.
const OpInboxWait = "inbox.wait"

type InboxWaitRequest struct {
	SessionID string `json:"session_id"`
	// Epoch and AfterGeneration are the values from the previous answer
	// ("" / 0 on the first call). The wait returns at once when the epoch
	// differs (pyred restarted) or the session's generation moved past
	// AfterGeneration, so a signal between two waits is never lost.
	Epoch           string `json:"epoch,omitempty"`
	AfterGeneration int64  `json:"after_generation,omitempty"`
	// WaitSeconds bounds the wait (0 = answer at once, max 60).
	WaitSeconds int `json:"wait_seconds,omitempty"`
}

type InboxWaitResponse struct {
	Epoch      string `json:"epoch"`
	Generation int64  `json:"generation"`
	// Woken is true when the session may have new mail (signal, reconnect,
	// or epoch/generation mismatch); false on a plain timeout.
	Woken bool `json:"woken"`
}

// InboxWaitAPI is implemented by pyred's local API (and the client) that
// offer OpInboxWait.
type InboxWaitAPI interface {
	InboxWait(ctx context.Context, req InboxWaitRequest) (InboxWaitResponse, error)
}
