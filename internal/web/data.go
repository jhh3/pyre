package web

import (
	"context"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// messageOpenWindow is how long an unanswered message from a session stays
// in the inbox before it moves to done.
const messageOpenWindow = 72 * time.Hour

// doneWindow is how far back the done box reaches.
const doneWindow = 14 * 24 * time.Hour

// pageLimit is the page size of the list calls.
const pageLimit = 500

// all pages through a list call.
func all[T any](ctx context.Context, list func(ctx context.Context, cursor string) (c.Page[T], error)) ([]T, error) {
	var out []T
	cur := ""
	for i := 0; i < 1000; i++ {
		p, err := list(ctx, cur)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.NextCursor == "" || p.NextCursor == cur {
			break
		}
		cur = p.NextCursor
	}
	return out, nil
}

func (s *Server) asksAPI() (c.AsksAPI, bool) {
	a, ok := s.api.(c.AsksAPI)
	return a, ok
}

func (s *Server) heldAPI() (c.HeldAnswersAPI, bool) {
	h, ok := s.api.(c.HeldAnswersAPI)
	return h, ok
}

func (s *Server) pageAPI() (c.ProjectPageAPI, bool) {
	p, ok := s.api.(c.ProjectPageAPI)
	return p, ok
}

func (s *Server) getSessions(ctx context.Context) ([]c.SessionView, error) {
	return s.sessions.get(ctx, s.now(), func(ctx context.Context) ([]c.SessionView, error) {
		return all(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
			return s.api.ListSessions(ctx, c.ListSessionsRequest{PageRequest: c.PageRequest{Cursor: cur, Limit: pageLimit}})
		})
	})
}

func (s *Server) getProjects(ctx context.Context) ([]c.Project, error) {
	return s.projects.get(ctx, s.now(), func(ctx context.Context) ([]c.Project, error) {
		return all(ctx, func(ctx context.Context, cur string) (c.Page[c.Project], error) {
			return s.api.ListProjects(ctx, c.ListProjectsRequest{PageRequest: c.PageRequest{Cursor: cur, Limit: pageLimit}})
		})
	})
}

// getAsks returns open asks and those decided in the done window. A hub
// without asks has none.
func (s *Server) getAsks(ctx context.Context) ([]c.Ask, error) {
	api, ok := s.asksAPI()
	if !ok {
		return nil, nil
	}
	return s.asks.get(ctx, s.now(), func(ctx context.Context) ([]c.Ask, error) {
		since := s.now().Add(-doneWindow)
		out, err := all(ctx, func(ctx context.Context, cur string) (c.Page[c.Ask], error) {
			return api.ListAsks(ctx, c.ListAsksRequest{PageRequest: c.PageRequest{Cursor: cur, Limit: pageLimit}, Since: &since})
		})
		if c.IsCode(err, c.ErrUnsupported) || c.IsCode(err, c.ErrNotFound) {
			return nil, nil
		}
		return out, err
	})
}

func (s *Server) getAttention(ctx context.Context) ([]c.AttentionItem, error) {
	return s.attention.get(ctx, s.now(), func(ctx context.Context) ([]c.AttentionItem, error) {
		return all(ctx, func(ctx context.Context, cur string) (c.Page[c.AttentionItem], error) {
			return s.api.ListAttention(ctx, c.ListAttentionRequest{PageRequest: c.PageRequest{Cursor: cur, Limit: pageLimit}})
		})
	})
}

// getHeld returns the held-answer queue (empty when the hub cannot hold).
func (s *Server) getHeld(ctx context.Context) (c.HeldAnswers, error) {
	api, ok := s.heldAPI()
	if !ok {
		return emptyHeld(), nil
	}
	h, err := s.held.get(ctx, s.now(), func(ctx context.Context) (c.HeldAnswers, error) {
		h, err := api.ListHeldAnswers(ctx, struct{}{})
		if c.IsCode(err, c.ErrUnsupported) || c.IsCode(err, c.ErrNotFound) {
			return emptyHeld(), nil
		}
		return h, err
	})
	if h.Items == nil {
		h.Items = []c.HeldAnswer{}
	}
	return h, err
}

func emptyHeld() c.HeldAnswers { return c.HeldAnswers{Items: []c.HeldAnswer{}} }

// pageInfo is a project's page head as the project list needs it.
type pageInfo struct {
	page c.ProjectPage
}

// getPage returns a project's page: ProjectPageAPI when the hub has it,
// else the project document with slug "page" (no page: empty content).
func (s *Server) getPage(ctx context.Context, projectID string) (c.ProjectPage, error) {
	s.pagesMu.Lock()
	ch := s.pages[projectID]
	if ch == nil {
		ch = &cached[pageInfo]{maxAge: 10 * time.Minute}
		s.pages[projectID] = ch
	}
	s.pagesMu.Unlock()
	pi, err := ch.get(ctx, s.now(), func(ctx context.Context) (pageInfo, error) {
		if api, ok := s.pageAPI(); ok {
			p, err := api.GetProjectPage(ctx, c.GetProjectPageRequest{ProjectID: projectID})
			if err == nil {
				return pageInfo{page: p}, nil
			}
			if !c.IsCode(err, c.ErrUnsupported) {
				if c.IsCode(err, c.ErrNotFound) {
					return pageInfo{page: c.ProjectPage{ProjectID: projectID}}, nil
				}
				return pageInfo{}, err
			}
		}
		d, err := s.api.GetDocument(ctx, c.GetDocumentRequest{ProjectID: projectID, Slug: c.ProjectPageSlug})
		if c.IsCode(err, c.ErrNotFound) {
			return pageInfo{page: c.ProjectPage{ProjectID: projectID}}, nil
		}
		if err != nil {
			return pageInfo{}, err
		}
		doc, rev := d.Document, d.Revision
		return pageInfo{page: c.ProjectPage{ProjectID: projectID, Document: &doc, Revision: &rev, Content: rev.Content}}, nil
	})
	return pi.page, err
}

func (s *Server) invalidatePage(projectID string) {
	s.pagesMu.Lock()
	ch := s.pages[projectID]
	s.pagesMu.Unlock()
	if ch != nil {
		ch.invalidate()
	}
}

// snap is everything one page load reads from the hub.
type snap struct {
	now       time.Time
	sessions  map[string]c.SessionView // active and (looked up) archived
	active    []c.SessionView
	projects  map[string]c.Project
	projList  []c.Project
	asks      []c.Ask
	attention []c.AttentionItem
	held      c.HeldAnswers
	heldBy    map[string]c.HeldAnswer
	mail      threads
	askThread map[string]int // thread ID or mail key -> index in asks
}

// need selects what loadSnap fetches.
type need struct{ mail, asks, attention, held, projects bool }

// loadSnap fetches what a page needs in parallel (each from its cache when
// current).
func (s *Server) loadSnap(ctx context.Context, n need) (*snap, error) {
	sn := &snap{now: s.now(), sessions: map[string]c.SessionView{}, projects: map[string]c.Project{}, heldBy: map[string]c.HeldAnswer{},
		askThread: map[string]int{}, held: emptyHeld()}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		mail map[string]c.MessageView
	)
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	run(func() (err error) { sn.active, err = s.getSessions(ctx); return })
	if n.projects {
		run(func() (err error) { sn.projList, err = s.getProjects(ctx); return })
	}
	if n.asks {
		run(func() (err error) { sn.asks, err = s.getAsks(ctx); return })
	}
	if n.attention {
		run(func() (err error) { sn.attention, err = s.getAttention(ctx); return })
	}
	if n.held {
		run(func() (err error) { sn.held, err = s.getHeld(ctx); return })
	}
	if n.mail {
		if !s.live.Load() {
			// Without events the index ages: reload it at most once a minute.
			s.mail.staleAfter(time.Minute)
		}
		run(func() (err error) { mail, err = s.mail.snapshot(ctx, s.api); return })
	}
	wg.Wait()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	for _, v := range sn.active {
		sn.sessions[v.Session.SessionID] = v
	}
	for _, p := range sn.projList {
		sn.projects[p.ProjectID] = p
	}
	for _, h := range sn.held.Items {
		sn.heldBy[h.AskID] = h
	}
	for i, a := range sn.asks {
		sn.askThread[askThreadID(a)] = i
		if k := askMailKey(a); k != "" {
			sn.askThread[k] = i
		}
	}
	sn.mail = groupThreads(mail)
	// Sessions the mail or asks name that are not active (archived): look
	// them up once and remember them (titles of ended work rarely change).
	var missing []string
	seen := map[string]bool{}
	want := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			if _, ok := sn.sessions[id]; !ok {
				missing = append(missing, id)
			}
		}
	}
	for _, a := range sn.asks {
		want(a.SessionID)
	}
	for _, m := range mail {
		want(fromSession(m))
		if fromOwner(m) {
			want(m.Envelope.Recipient.ID)
		}
	}
	s.fillKnown(ctx, sn, missing)
	return sn, nil
}

// fillKnown adds sessions outside the active list, from the remembered set
// or the hub (bounded parallel GetSession calls).
func (s *Server) fillKnown(ctx context.Context, sn *snap, ids []string) {
	var fetch []string
	s.knownMu.Lock()
	for _, id := range ids {
		if v, ok := s.known[id]; ok {
			sn.sessions[id] = v
		} else {
			fetch = append(fetch, id)
		}
	}
	s.knownMu.Unlock()
	if len(fetch) == 0 {
		return
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, id := range fetch {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			v, err := s.api.GetSession(ctx, c.GetRequest{ID: id})
			if err != nil {
				if !c.IsCode(err, c.ErrNotFound) {
					return // try again next time
				}
				v = c.SessionView{Session: c.Session{SessionID: id, Archived: true}}
			}
			mu.Lock()
			sn.sessions[id] = v
			mu.Unlock()
			s.knownMu.Lock()
			s.known[id] = v
			s.knownMu.Unlock()
		}(id)
	}
	wg.Wait()
}

// askThreadID is an ask's thread ID: Ask.ThreadID, else "ask:<id>".
func askThreadID(a c.Ask) string {
	if a.ThreadID != "" {
		return a.ThreadID
	}
	return "ask:" + a.AskID
}

// askMailKey is the mail thread key of an ask's conversation: its
// ThreadID, else (hubs that do not thread asks) its carrying message.
func askMailKey(a c.Ask) string {
	if a.ThreadID != "" {
		return a.ThreadID
	}
	return a.MessageID
}
