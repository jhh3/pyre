package contracts

import (
	"context"
	"time"
)

// Asks (W-asks, additive, optional): a structured question from a
// top-level session to the owner, with 2–4 options, an optional default
// and a deadline after which the hub applies the default. One open ask per
// (scope, topic key): a repeat on the same topic merges into the open ask
// instead of becoming new mail, and a topic already decided returns the
// decision instead of asking again.
//
// Rules (enforced by the hub, not only by guidance):
//   - Only top-level sessions (Session.Parent() == "") and the owner create
//     asks. A child's ask is forwarded to its parent as an ordinary message
//     (status "redirected"); nothing reaches the owner.
//   - Context is required: the owner may not be in the weeds.
//   - Deadline: DeadlineSeconds, else AskDefaultDeadline; a deadline that
//     falls inside quiet hours (owner NotificationPrefs, default
//     22:00–08:00 America/Los_Angeles) moves to the end of quiet hours
//     (DeadlineRule "morning"). Urgent asks keep their deadline.
//   - At the deadline an ask with DefaultOption becomes "defaulted"; one
//     without becomes "expired". Answer and deadline are mutually exclusive
//     (first commit wins).
//   - The decision reaches the asking session as a queue message replying
//     to Ask.MessageID (so wait_reply works), one combined message per
//     session for a batch answer.
//
// Not part of API: callers type-assert AsksAPI; a hub without it answers
// 404 (ErrUnsupported) and does not advertise FeatureAsks.
const (
	OpAsksCreate   = "asks.create"
	OpAsksList     = "asks.list"
	OpAsksGet      = "asks.get"
	OpAsksAnswer   = "asks.answer" // batch: one or more answers
	OpAsksWithdraw = "asks.withdraw"
)

// FeatureAsks is the Hello feature advertising the asks ops.
const FeatureAsks = "asks"

// PrefixAsk is the ask ID prefix.
const PrefixAsk = "ask"

// KindAsk is the ObjectRef kind of an ask.
const KindAsk ObjectKind = "ask"

// AttnAsk is the attention kind for an open ask (one item per ask; it is
// handled when the ask is decided, withdrawn or superseded).
const AttnAsk AttentionKind = "ask"

// EvAskChanged is emitted on every ask change (payload: Ask).
const EvAskChanged EventKind = "ask.changed"

// AskDefaultDeadline is the deadline when the request gives none.
const AskDefaultDeadline = time.Hour

// Limits.
const (
	AskMinOptions    = 2
	AskMaxOptions    = 4
	AskMaxTopicKey   = 120
	AskMaxTitle      = 120
	AskMaxContext    = 16000
	AskMaxOptionText = 300
)

type AskState string

const (
	AskOpen       AskState = "open"
	AskAnswered   AskState = "answered"   // the owner chose
	AskDefaulted  AskState = "defaulted"  // deadline passed; DefaultOption applied
	AskExpired    AskState = "expired"    // deadline passed with no default
	AskWithdrawn  AskState = "withdrawn"  // the asker took it back (or its session was archived)
	AskSuperseded AskState = "superseded" // replaced by SupersededBy
)

// Decided reports whether the ask has a final outcome.
func (s AskState) Decided() bool { return s != AskOpen && s != "" }

type AskOption struct {
	ID     string `json:"id"`               // short, stable ("a", "rebase")
	Label  string `json:"label"`            // shown on the button / number key
	Detail string `json:"detail,omitempty"` // one line of consequence
}

type AskAnswer struct {
	OptionID  string    `json:"option_id,omitempty"`
	Text      string    `json:"text,omitempty"` // free text instead of / alongside an option
	By        Principal `json:"by"`
	ByDefault bool      `json:"by_default,omitempty"`
	At        time.Time `json:"at"`
}

// AskUpdate is a follow-up merged into an open ask (same topic) instead of
// a new inbox item.
type AskUpdate struct {
	Seq       int       `json:"seq"`
	SessionID string    `json:"session_id,omitempty"`
	Text      string    `json:"text"`
	At        time.Time `json:"at"`
}

type Ask struct {
	AskID         string      `json:"ask_id"`
	TopicKey      string      `json:"topic_key"` // normalized (lowercase slug)
	ProjectID     *string     `json:"project_id"`
	SessionID     string      `json:"session_id,omitempty"` // asker ("" = owner-created)
	Title         string      `json:"title"`
	Context       string      `json:"context"`
	Options       []AskOption `json:"options"`
	DefaultOption string      `json:"default_option,omitempty"`
	Urgent        bool        `json:"urgent,omitempty"`
	DeadlineAt    *time.Time  `json:"deadline_at,omitempty"`
	DeadlineRule  string      `json:"deadline_rule,omitempty"` // "1h" | "custom" | "morning" | "urgent"
	State         AskState    `json:"state"`
	Answer        *AskAnswer  `json:"answer,omitempty"`
	Updates       []AskUpdate `json:"updates,omitempty"`
	Count         int         `json:"count"` // 1 + merged repeats
	SupersededBy  string      `json:"superseded_by,omitempty"`
	MessageID     string      `json:"message_id,omitempty"` // the owner message carrying the ask
	ThreadID      string      `json:"thread_id,omitempty"`
	AttentionID   string      `json:"attention_id,omitempty"`
	// DecisionMessageID is the message that told the asker the outcome.
	DecisionMessageID string    `json:"decision_message_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Option returns the option with id, if any.
func (a Ask) Option(id string) (AskOption, bool) {
	for _, o := range a.Options {
		if o.ID == id {
			return o, true
		}
	}
	return AskOption{}, false
}

type CreateAskRequest struct {
	Mutation
	TopicKey        string      `json:"topic_key"` // required; same topic = same item
	Title           string      `json:"title"`
	Context         string      `json:"context"` // required: why, what you know, what happens next
	Options         []AskOption `json:"options"`
	DefaultOption   string      `json:"default_option,omitempty"`
	DeadlineSeconds int         `json:"deadline_seconds,omitempty"`
	Urgent          bool        `json:"urgent,omitempty"`
	ProjectID       *string     `json:"project_id,omitempty"` // default: the session's project
	// Reopen asks again on a topic that was already decided (new ask; the
	// decided one stays as history). Reason goes into Context.
	Reopen bool `json:"reopen,omitempty"`
	// ConfirmNew creates the ask even when a similar open ask under another
	// topic key exists (status possible_duplicate otherwise).
	ConfirmNew bool `json:"confirm_new,omitempty"`
}

// AskCreateStatus says what the hub did with a CreateAskRequest.
type AskCreateStatus string

const (
	AskCreated           AskCreateStatus = "created"
	AskMerged            AskCreateStatus = "merged"             // same topic open: appended as an update, no new notification
	AskAlreadyDecided    AskCreateStatus = "decided"            // same topic decided: Ask carries the answer; nothing sent
	AskPossibleDuplicate AskCreateStatus = "possible_duplicate" // similar open ask(s) in Candidates; resend with ConfirmNew or use their topic
	AskRedirected        AskCreateStatus = "redirected"         // caller is a child session: forwarded to its parent as MessageID
)

type CreateAskResponse struct {
	Status     AskCreateStatus `json:"status"`
	Ask        *Ask            `json:"ask,omitempty"`
	Candidates []Ask           `json:"candidates,omitempty"`
	MessageID  string          `json:"message_id,omitempty"` // redirected: the message to the parent
	Detail     string          `json:"detail,omitempty"`
}

type ListAsksRequest struct {
	PageRequest
	States    []AskState `json:"states,omitempty"` // default: all
	ProjectID *string    `json:"project_id,omitempty"`
	SessionID string     `json:"session_id,omitempty"` // asker
	TopicKey  string     `json:"topic_key,omitempty"`
	Query     string     `json:"query,omitempty"` // full-text over title/context/options
	// Since limits decided asks to those decided after it (open ones always
	// match).
	Since *time.Time `json:"since,omitempty"`
}

type AnswerAskItem struct {
	AskID    string `json:"ask_id"`
	OptionID string `json:"option_id,omitempty"`
	Text     string `json:"text,omitempty"` // free text; required when OptionID is empty
}

// AnswerAsksRequest answers one or more asks at once (owner only). Each
// item commits independently; the decisions for one asking session go out
// as one combined message.
type AnswerAsksRequest struct {
	Mutation
	Answers []AnswerAskItem `json:"answers"`
}

type AnswerAskResult struct {
	AskID string `json:"ask_id"`
	Ask   *Ask   `json:"ask,omitempty"`
	Error *Error `json:"error,omitempty"` // e.g. conflict: already decided
}

type AnswerAsksResponse struct {
	Results []AnswerAskResult `json:"results"`
}

type WithdrawAskRequest struct {
	Mutation
	AskID        string `json:"ask_id"`
	Reason       string `json:"reason,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
}

// AsksAPI is implemented by hubs (and the client, the node-local proxy and
// the mock) that offer the asks ops.
type AsksAPI interface {
	CreateAsk(ctx context.Context, req CreateAskRequest) (CreateAskResponse, error)
	ListAsks(ctx context.Context, req ListAsksRequest) (Page[Ask], error)
	GetAsk(ctx context.Context, req GetRequest) (Ask, error)
	AnswerAsks(ctx context.Context, req AnswerAsksRequest) (AnswerAsksResponse, error)
	WithdrawAsk(ctx context.Context, req WithdrawAskRequest) (Ask, error)
}
