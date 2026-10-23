package mock

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Held answers and project pages (W-web-inbox) on the mock: the contract
// semantics in memory. Like deadlines, the quiet window is checked lazily
// on every asks call against the backend clock (expireAsksLocked ->
// sendHeldLocked), so a FakeClock.Advance past it sends the batch on the
// next read.

var (
	_ c.HeldAnswersAPI = (*Backend)(nil)
	_ c.ProjectPageAPI = (*Backend)(nil)
)

type heldEntry struct {
	c.HeldAnswer
	by c.Principal
}

// askQuality is the hub's rule for a new ask: enough context, and a
// detail on every option.
func askQuality(context string, options []c.AskOption) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(context)); n < c.AskMinContext {
		return c.Errorf(c.ErrInvalid, "context is %d characters; a new ask needs at least %d so the owner can decide without opening your session. %s (up to %d characters)",
			n, c.AskMinContext, c.AskContextGuide, c.AskMaxContext)
	}
	for _, o := range options {
		if strings.TrimSpace(o.Detail) == "" {
			return c.Errorf(c.ErrInvalid, "option %q has no detail: say in one line what happens if the owner picks it (every option needs one)", o.ID)
		}
	}
	return nil
}

func (b *Backend) heldLocked() c.HeldAnswers {
	out := c.HeldAnswers{Items: []c.HeldAnswer{}}
	var newest time.Time
	for _, e := range b.held {
		out.Items = append(out.Items, e.HeldAnswer)
		if e.HeldAt.After(newest) {
			newest = e.HeldAt
		}
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if !out.Items[i].HeldAt.Equal(out.Items[j].HeldAt) {
			return out.Items[i].HeldAt.Before(out.Items[j].HeldAt)
		}
		return out.Items[i].AskID < out.Items[j].AskID
	})
	if len(out.Items) > 0 {
		at := newest.Add(c.AskBatchQuiet)
		out.SendsAt = &at
	}
	return out
}

func (b *Backend) emitHeldLocked(askID string) c.HeldAnswers {
	h := b.heldLocked()
	b.emit(c.EvAskHeldChanged, ref(c.KindAsk, askID), "", nil, h)
	return h
}

// dropHeldLocked forgets a decided ask's held answer.
func (b *Backend) dropHeldLocked(askID string) {
	if _, ok := b.held[askID]; ok {
		delete(b.held, askID)
		b.emitHeldLocked(askID)
	}
}

// flushHeldLocked sends held answers (all, or only those named), grouped
// by the principal that held them.
func (b *Backend) flushHeldLocked(only map[string]bool) c.AnswerAsksResponse {
	out := c.AnswerAsksResponse{Results: []c.AnswerAskResult{}}
	q := b.heldLocked()
	var order []string
	groups := map[string][]heldEntry{}
	for _, it := range q.Items {
		if only != nil && !only[it.AskID] {
			continue
		}
		e := b.held[it.AskID]
		k := string(e.by.Kind) + ":" + e.by.ID
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}
	for _, k := range order {
		g := groups[k]
		items := make([]c.AnswerAskItem, len(g))
		for i, e := range g {
			items[i] = c.AnswerAskItem{AskID: e.AskID, OptionID: e.OptionID, Text: e.Text}
			delete(b.held, e.AskID) // decided below, or no longer answerable
		}
		res := b.answerAsksLocked(g[0].by, "op_askheld-"+g[0].AskID+"-"+strconv.FormatInt(g[0].HeldAt.UnixNano(), 36), items)
		out.Results = append(out.Results, res.Results...)
		b.emitHeldLocked(g[0].AskID)
	}
	return out
}

// sendHeldLocked sends the queue once AskBatchQuiet passed since the
// newest hold, else the held answers of asks past their deadline.
func (b *Backend) sendHeldLocked() {
	if len(b.held) == 0 {
		return
	}
	now := b.now()
	if q := b.heldLocked(); q.SendsAt != nil && !now.Before(*q.SendsAt) {
		b.flushHeldLocked(nil)
		return
	}
	due := map[string]bool{}
	for id := range b.held {
		if a, ok := b.askMap()[id]; ok && a.State == c.AskOpen && a.DeadlineAt != nil && !now.Before(*a.DeadlineAt) {
			due[id] = true
		}
	}
	if len(due) > 0 {
		b.flushHeldLocked(due)
	}
}

func (b *Backend) HoldAnswer(ctx context.Context, req c.HoldAnswerRequest) (c.HeldAnswers, error) {
	return mutate(b, ctx, c.OpAsksHold, req.Mutation, req, func(p c.Principal) (c.HeldAnswers, error) {
		if err := requireOwner(p); err != nil {
			return c.HeldAnswers{}, err
		}
		b.expireAsksLocked()
		a, ok := b.askMap()[req.AskID]
		switch {
		case !ok:
			return c.HeldAnswers{}, notFound(c.KindAsk, req.AskID)
		case a.State != c.AskOpen:
			return c.HeldAnswers{}, c.Errorf(c.ErrConflict, "ask is already %s", a.State).WithRef(c.KindAsk, a.AskID)
		case req.OptionID == "" && strings.TrimSpace(req.Text) == "":
			return c.HeldAnswers{}, c.Errorf(c.ErrInvalid, "an answer needs an option or text")
		}
		if _, ok := a.Option(req.OptionID); req.OptionID != "" && !ok {
			return c.HeldAnswers{}, c.Errorf(c.ErrInvalid, "option %q is not one of the ask's options", req.OptionID)
		}
		if b.held == nil {
			b.held = map[string]heldEntry{}
		}
		b.held[a.AskID] = heldEntry{HeldAnswer: c.HeldAnswer{AskID: a.AskID, OptionID: req.OptionID, Text: strings.TrimSpace(req.Text), HeldAt: b.now()}, by: p}
		return b.emitHeldLocked(a.AskID), nil
	})
}

func (b *Backend) UnholdAnswer(ctx context.Context, req c.UnholdAnswerRequest) (c.HeldAnswers, error) {
	return mutate(b, ctx, c.OpAsksUnhold, req.Mutation, req, func(p c.Principal) (c.HeldAnswers, error) {
		if err := requireOwner(p); err != nil {
			return c.HeldAnswers{}, err
		}
		b.expireAsksLocked()
		if _, ok := b.held[req.AskID]; !ok {
			return c.HeldAnswers{}, c.Errorf(c.ErrNotFound, "no held answer for %s: it was already sent, or the ask was decided another way", req.AskID).WithRef(c.KindAsk, req.AskID)
		}
		delete(b.held, req.AskID)
		return b.emitHeldLocked(req.AskID), nil
	})
}

func (b *Backend) ListHeldAnswers(ctx context.Context, _ struct{}) (c.HeldAnswers, error) {
	return query(b, ctx, func(p c.Principal) (c.HeldAnswers, error) {
		if err := requireOwner(p); err != nil {
			return c.HeldAnswers{}, err
		}
		b.expireAsksLocked()
		return b.heldLocked(), nil
	})
}

func (b *Backend) FlushHeldAnswers(ctx context.Context, req c.FlushHeldAnswersRequest) (c.AnswerAsksResponse, error) {
	return mutate(b, ctx, c.OpAsksFlush, req.Mutation, req, func(p c.Principal) (c.AnswerAsksResponse, error) {
		if err := requireOwner(p); err != nil {
			return c.AnswerAsksResponse{}, err
		}
		b.expireAsksLocked()
		return b.flushHeldLocked(nil), nil
	})
}

// ---- project pages ----

func (b *Backend) pageDocLocked(projectID string) *c.Document {
	return b.slugDocLocked(projectID, c.ProjectPageSlug)
}

func (b *Backend) slugDocLocked(projectID, slug string) *c.Document {
	for _, d := range b.documents {
		if d.ProjectID != nil && *d.ProjectID == projectID && d.Slug == slug {
			return d
		}
	}
	return nil
}

func (b *Backend) pageProjectLocked(p c.Principal, projectID string) (string, *c.Session, error) {
	var s *c.Session
	if p.Kind == c.PrincipalSession {
		got, ok := b.sessions[p.ID]
		if !ok {
			return "", nil, notFound(c.KindSession, p.ID)
		}
		s = got
		if projectID == "" && got.ProjectID != nil {
			projectID = *got.ProjectID
		}
	}
	if projectID == "" {
		return "", s, c.Errorf(c.ErrInvalid, "project_id is required (this caller is not in a project)")
	}
	if _, ok := b.projects[projectID]; !ok {
		return "", s, notFound(c.KindProject, projectID)
	}
	return projectID, s, nil
}

func (b *Backend) pageLocked(projectID string) c.ProjectPage {
	pg := c.ProjectPage{ProjectID: projectID}
	if d := b.pageDocLocked(projectID); d != nil {
		dc, rc := *d, *b.revisions[d.HeadRevisionID]
		pg.Document, pg.Revision, pg.Content = &dc, &rc, rc.Content
	}
	if d := b.slugDocLocked(projectID, c.ProjectStatusSlug); d != nil {
		r := b.revisions[d.HeadRevisionID]
		var st c.ProjectStatus
		if json.Unmarshal([]byte(r.Content), &st) == nil {
			at := r.CreatedAt
			pg.Status, pg.StatusUpdatedAt = &st, &at
		}
	}
	return pg
}

// writeDocLocked creates the project's document with slug, or adds a head
// revision to it.
func (b *Backend) writeDocLocked(p c.Principal, projectID, slug, title, mediaType, content, summary string) {
	d := b.slugDocLocked(projectID, slug)
	if d == nil {
		pid := projectID
		b.createDocLocked(p, &pid, title, slug, mediaType, content, summary)
		return
	}
	now := b.now()
	r := &c.DocumentRevision{RevisionID: c.NewID(c.PrefixRevision), DocumentID: d.DocumentID, Seq: d.HeadSeq + 1, ParentRevisionID: d.HeadRevisionID,
		Digest: sha256Digest([]byte(content)), Size: int64(len(content)), Author: p, CreatedAt: now, Summary: summary, Content: content}
	b.revisions[r.RevisionID] = r
	d.HeadRevisionID, d.HeadSeq, d.UpdatedAt = r.RevisionID, r.Seq, now
	b.emit(c.EvDocumentRevised, ref(c.KindDocumentRevision, r.RevisionID), "", d.ProjectID, withoutContent(*r))
}

func (b *Backend) SetProjectPage(ctx context.Context, req c.SetProjectPageRequest) (c.ProjectPage, error) {
	return mutate(b, ctx, c.OpProjectsSetPage, req.Mutation, req, func(p c.Principal) (c.ProjectPage, error) {
		hasContent := strings.TrimSpace(req.Content) != ""
		if !hasContent && req.Status == nil {
			return c.ProjectPage{}, c.Errorf(c.ErrInvalid, "content or status is required")
		}
		if len(req.Content) > c.ProjectPageMaxSize {
			return c.ProjectPage{}, c.Errorf(c.ErrPayloadTooLarge, "the page is %d bytes; limit %d", len(req.Content), c.ProjectPageMaxSize)
		}
		var statusJSON []byte
		if req.Status != nil {
			var err error
			if statusJSON, err = c.ValidateProjectStatus(req.Status); err != nil {
				return c.ProjectPage{}, err
			}
		}
		projectID, s, err := b.pageProjectLocked(p, strings.TrimSpace(req.ProjectID))
		if err != nil {
			return c.ProjectPage{}, err
		}
		switch {
		case p.Kind == c.PrincipalOwner:
		case s != nil && s.Parent() == "" && s.ProjectID != nil && *s.ProjectID == projectID:
		default:
			return c.ProjectPage{}, c.Errorf(c.ErrForbidden, "only the owner or a top-level session in project %s writes its page", projectID)
		}
		summary := strings.TrimSpace(req.Summary)
		if hasContent {
			d := b.pageDocLocked(projectID)
			if req.BaseRevisionID != "" && (d == nil || d.HeadRevisionID != req.BaseRevisionID) {
				head := ""
				if d != nil {
					head = d.HeadRevisionID
				}
				return c.ProjectPage{}, c.Errorf(c.ErrConflict, "the page head moved").WithDetail("current_head", head)
			}
			b.writeDocLocked(p, projectID, c.ProjectPageSlug, "Project page", "text/markdown", req.Content, summary)
		}
		if statusJSON != nil {
			b.writeDocLocked(p, projectID, c.ProjectStatusSlug, "Project status", "application/json", string(statusJSON), summary)
		}
		return b.pageLocked(projectID), nil
	})
}

func (b *Backend) GetProjectPage(ctx context.Context, req c.GetProjectPageRequest) (c.ProjectPage, error) {
	return query(b, ctx, func(p c.Principal) (c.ProjectPage, error) {
		projectID, _, err := b.pageProjectLocked(p, strings.TrimSpace(req.ProjectID))
		if err != nil {
			return c.ProjectPage{}, err
		}
		return b.pageLocked(projectID), nil
	})
}
