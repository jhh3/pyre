package contracts

import (
	"context"
	"time"
)

// Batched answers (W-web-inbox, additive, optional): the owner may hold an
// answer instead of sending it at once. Held answers wait in the hub until
// AskBatchQuiet passes with no further answer held (each new hold restarts
// the wait); then all of them are sent together through the same path as
// AnswerAsks (one combined decision message per asking session). The owner
// can send them early (FlushHeldAnswers) or take one back (UnholdAnswer).
// Held answers live in the hub, so every surface (web, TUI, Android) sees the
// same queue and a restart of any client loses nothing.
//
// An ask that is decided some other way while held (deadline, withdrawal,
// an immediate answer from another surface) drops its held answer. A held
// answer does not stop the ask's deadline: the hub flushes held answers
// before applying a default, so the owner's choice wins.
//
// Not part of API: callers type-assert HeldAnswersAPI.
const (
	OpAsksHold     = "asks.hold"
	OpAsksUnhold   = "asks.unhold"
	OpAsksHeld     = "asks.held"  // list the queue
	OpAsksFlush    = "asks.flush" // send everything held now
	FeatureAskHold = "asks.hold"
)

// AskBatchQuiet is how long the hub waits after the last held answer before
// sending the batch.
const AskBatchQuiet = 5 * time.Minute

// EvAskHeldChanged is emitted when the held queue changes (payload:
// HeldAnswers).
const EvAskHeldChanged EventKind = "ask.held_changed"

type HeldAnswer struct {
	AskID    string    `json:"ask_id"`
	OptionID string    `json:"option_id,omitempty"`
	Text     string    `json:"text,omitempty"`
	HeldAt   time.Time `json:"held_at"`
}

// HeldAnswers is the whole queue and when it will be sent (nil when empty).
type HeldAnswers struct {
	Items   []HeldAnswer `json:"items"`
	SendsAt *time.Time   `json:"sends_at,omitempty"`
}

type HoldAnswerRequest struct {
	Mutation
	AnswerAskItem
}

type UnholdAnswerRequest struct {
	Mutation
	AskID string `json:"ask_id"`
}

type FlushHeldAnswersRequest struct {
	Mutation
}

type HeldAnswersAPI interface {
	HoldAnswer(ctx context.Context, req HoldAnswerRequest) (HeldAnswers, error)
	UnholdAnswer(ctx context.Context, req UnholdAnswerRequest) (HeldAnswers, error)
	ListHeldAnswers(ctx context.Context, req struct{}) (HeldAnswers, error)
	// FlushHeldAnswers sends the queue now; the response is AnswerAsks's.
	FlushHeldAnswers(ctx context.Context, req FlushHeldAnswersRequest) (AnswerAsksResponse, error)
}

// Project pages (W-web-inbox, additive, optional): each project has one
// owner-facing page, a markdown document with slug ProjectPageSlug that the
// project's lead keeps current (goal, status, results, open decisions,
// links, next steps). SetProjectPage writes a new head revision directly
// (no proposal step); callers are the owner or a top-level session in the
// project. The page is created on first write.
const (
	OpProjectsSetPage  = "projects.set_page"
	OpProjectsGetPage  = "projects.get_page"
	FeatureProjectPage = "projects.page"
	ProjectPageSlug    = "page"
	ProjectPageMaxSize = 256 << 10
)

type SetProjectPageRequest struct {
	Mutation
	ProjectID string `json:"project_id"` // default: the calling session's project
	Content   string `json:"content"`    // markdown
	Summary   string `json:"summary,omitempty"`
	// BaseRevisionID, when set, makes the write a compare-and-swap.
	BaseRevisionID string `json:"base_revision_id,omitempty"`
}

type GetProjectPageRequest struct {
	ProjectID string `json:"project_id"`
}

// ProjectPage is the page's head (Revision nil when no page was written).
type ProjectPage struct {
	ProjectID string            `json:"project_id"`
	Document  *Document         `json:"document,omitempty"`
	Revision  *DocumentRevision `json:"revision,omitempty"`
	Content   string            `json:"content"`
}

type ProjectPageAPI interface {
	SetProjectPage(ctx context.Context, req SetProjectPageRequest) (ProjectPage, error)
	GetProjectPage(ctx context.Context, req GetProjectPageRequest) (ProjectPage, error)
}

// Ask context quality: an ask's Context must let the owner decide without
// opening the session: what is being decided and why it matters now, what
// is already known or was tried, what each option leads to, and what
// happens if nobody answers. The hub rejects shorter context and options
// without Detail (invalid_argument with this guidance).
const AskMinContext = 300

// AskContextGuide is what an ask's context must cover, in the words the
// hub returns with a rejected ask (and tool descriptions repeat).
const AskContextGuide = "Write it for someone who has not seen your session: (1) what is being decided and why it needs deciding now; " +
	"(2) what is already known or was tried, with links or paths; (3) what each option leads to; (4) what happens by default if nobody answers; " +
	"(5) when an option covers several items (\"accept all 6\"), list every item in the context, one per line, so the owner can see what they accept. " +
	"Markdown is fine"
