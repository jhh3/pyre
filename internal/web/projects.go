package web

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// pageTime is when a project page was last written (nil: no page).
func pageTime(p c.ProjectPage) *time.Time {
	if p.Revision != nil && !p.Revision.CreatedAt.IsZero() {
		t := p.Revision.CreatedAt
		return &t
	}
	if p.Document != nil && !p.Document.UpdatedAt.IsZero() {
		t := p.Document.UpdatedAt
		return &t
	}
	return nil
}

var (
	mdLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdMarker = regexp.MustCompile("(?m)^\\s{0,3}(#{1,6}\\s+|>\\s?|[-*+]\\s+|\\d+[.)]\\s+|```.*$)")
	mdInline = regexp.MustCompile("[*_`~]{1,3}")
)

// pageSummary is the page's opening as plain text (~200 characters).
func pageSummary(md string) string {
	s := mdLink.ReplaceAllString(md, "$1")
	s = mdMarker.ReplaceAllString(s, "")
	s = mdInline.ReplaceAllString(s, "")
	return excerpt(s, 200)
}

// pages loads the pages of projects in parallel (each cached until a
// document event names its project).
func (s *Server) pagesFor(ctx context.Context, ids []string) map[string]c.ProjectPage {
	out := make(map[string]c.ProjectPage, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			p, err := s.getPage(ctx, id)
			if err != nil {
				s.logf("pyre web: page of %s: %v", id, err)
				return // the card shows no page rather than failing the list
			}
			mu.Lock()
			out[id] = p
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := hubCtx(r)
	defer cancel()
	sn, err := s.loadSnap(ctx, need{asks: true, projects: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	var ids []string
	for _, p := range sn.projList {
		if !p.Archived {
			ids = append(ids, p.ProjectID)
		}
	}
	pages := s.pagesFor(ctx, ids)
	type agg struct {
		leads    []SessionCard
		at       map[string]time.Time
		waiting  bool
		openAsks int
		live     int
		last     time.Time
	}
	by := map[string]*agg{}
	for _, id := range ids {
		by[id] = &agg{at: map[string]time.Time{}, last: sn.projects[id].UpdatedAt}
	}
	later := func(g *agg, t time.Time) {
		if t.After(g.last) {
			g.last = t
		}
	}
	for _, v := range sn.active {
		if v.Session.ProjectID == nil {
			continue
		}
		g := by[*v.Session.ProjectID]
		if g == nil {
			continue
		}
		at := activeAt(v)
		later(g, at)
		switch sessionState(v) {
		case StateNeedsYou:
			g.waiting = true
			g.live++
		case StateWorking, StateIdle, StateStarting:
			g.live++
		}
		if isLead(v) {
			card := sessionCard(v)
			g.leads = append(g.leads, card)
			g.at[card.ID] = at
		}
	}
	for _, a := range sn.asks {
		if a.ProjectID == nil || by[*a.ProjectID] == nil {
			continue
		}
		g := by[*a.ProjectID]
		later(g, a.UpdatedAt)
		if a.State == c.AskOpen {
			g.openAsks++
			g.waiting = true
		}
	}
	cards := make([]ProjectCard, 0, len(ids))
	for _, id := range ids {
		p, g := sn.projects[id], by[id]
		sortCards(g.leads, g.at)
		if g.leads == nil {
			g.leads = []SessionCard{}
		}
		card := ProjectCard{ProjectID: id, Name: p.Name, Description: p.Description, OpenAsks: g.openAsks, Waiting: g.waiting, Leads: g.leads}
		if pg, ok := pages[id]; ok {
			card.PageUpdatedAt = pageTime(pg)
			card.PageSummary = pageSummary(pg.Content)
			if card.PageUpdatedAt != nil {
				later(g, *card.PageUpdatedAt)
			}
		}
		card.LastActivityAt = g.last
		card.LiveSessions = g.live
		card.Active = g.live > 0 || g.openAsks > 0
		cards = append(cards, card)
	}
	// A stable order (by name), so the list never reshuffles as projects
	// get busy; the dots and counts say which ones want you.
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := strings.ToLower(cards[i].Name), strings.ToLower(cards[j].Name)
		if a != b {
			return a < b
		}
		return cards[i].ProjectID < cards[j].ProjectID
	})
	writeJSON(w, http.StatusOK, map[string]any{"projects": cards})
}

// decidedOnProject is how many decided asks the project page lists.
const decidedOnProject = 10

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project_id")
	ctx, cancel := hubCtx(r)
	defer cancel()
	var (
		wg               sync.WaitGroup
		proj             c.Project
		page             c.ProjectPage
		asks             []c.Ask
		arts             []c.Artifact
		sn               *snap
		pErr, sErr, aErr error
		artErr, pageErr  error
	)
	wg.Add(5)
	go func() { defer wg.Done(); proj, pErr = s.api.GetProject(ctx, c.GetRequest{ID: id}) }()
	go func() { defer wg.Done(); page, pageErr = s.getPage(ctx, id) }()
	go func() { defer wg.Done(); sn, sErr = s.loadSnap(ctx, need{projects: true}) }()
	go func() {
		defer wg.Done()
		api, ok := s.asksAPI()
		if !ok {
			return
		}
		pid := id
		asks, aErr = all(ctx, func(ctx context.Context, cur string) (c.Page[c.Ask], error) {
			return api.ListAsks(ctx, c.ListAsksRequest{PageRequest: c.PageRequest{Cursor: cur, Limit: pageLimit}, ProjectID: &pid})
		})
		if c.IsCode(aErr, c.ErrUnsupported) {
			asks, aErr = nil, nil
		}
	}()
	go func() {
		defer wg.Done()
		pid := id
		arts, artErr = all(ctx, func(ctx context.Context, cur string) (c.Page[c.Artifact], error) {
			return s.api.ListArtifacts(ctx, c.ListArtifactsRequest{PageRequest: c.PageRequest{Cursor: cur, Limit: pageLimit}, ProjectID: &pid})
		})
	}()
	wg.Wait()
	for _, err := range []error{pErr, sErr, aErr, artErr, pageErr} {
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	if page.ProjectID == "" {
		page.ProjectID = id
	}

	at := map[string]time.Time{}
	sessions := []SessionCard{}
	for _, v := range sn.active {
		if v.Session.ProjectID != nil && *v.Session.ProjectID == id {
			card := sessionCard(v)
			at[card.ID] = activeAt(v)
			sessions = append(sessions, card)
		}
	}
	sortCards(sessions, at)

	var open, decided []c.Ask
	for _, a := range asks {
		if a.State == c.AskOpen {
			open = append(open, a)
		} else {
			decided = append(decided, a)
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return open[i].CreatedAt.After(open[j].CreatedAt) })
	sort.SliceStable(decided, func(i, j int) bool { return decided[i].UpdatedAt.After(decided[j].UpdatedAt) })
	if len(decided) > decidedOnProject {
		decided = decided[:decidedOnProject]
	}
	outAsks := append(append([]c.Ask{}, open...), decided...)

	sort.SliceStable(arts, func(i, j int) bool { return arts[i].UpdatedAt.After(arts[j].UpdatedAt) })
	if len(arts) > 20 {
		arts = arts[:20]
	}
	artifacts := make([]ArtifactRef, 0, len(arts))
	for _, a := range arts {
		artifacts = append(artifacts, ArtifactRef{ID: a.ArtifactID, Title: a.Title, URL: "/api/artifacts/" + url.PathEscape(a.ArtifactID) + "/content", UpdatedAt: a.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": proj, "page": page, "sessions": sessions, "asks": outAsks, "artifacts": artifacts})
}

// artifactCSP: published files are served inert (no scripts, no
// same-origin access), whatever their type.
const artifactCSP = "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; media-src 'self'"

// handleArtifactContent streams an artifact's latest version.
func (s *Server) handleArtifactContent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := hubCtx(r)
	defer cancel()
	a, err := s.api.GetArtifact(ctx, c.GetRequest{ID: r.PathValue("artifact_id")})
	if err != nil {
		writeErr(w, err)
		return
	}
	if a.LatestVersionID == "" {
		writeErr(w, c.Errorf(c.ErrNotFound, "%s has no published version yet.", a.Title))
		return
	}
	rc, v, err := s.api.OpenContent(ctx, a.LatestVersionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rc.Close()
	h := w.Header()
	h.Set("Content-Security-Policy", artifactCSP)
	h.Del("Cache-Control")
	h.Set("Cache-Control", "private, max-age=300")
	ct := v.MediaType
	if _, _, err := mime.ParseMediaType(ct); err != nil || ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	if name := strings.TrimSpace(v.Filename); name != "" {
		h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	}
	_, _ = io.Copy(w, rc)
}
