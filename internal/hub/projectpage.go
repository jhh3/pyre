package hub

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
)

// Project pages (W-web-inbox, contracts/webinbox.go): the owner-facing page
// of a project is its document with slug contracts.ProjectPageSlug. The
// owner or a lead of the project (projectLeadOf) writes it directly: the
// first write creates the document, each later write is a new head
// revision (compare-and-swap on BaseRevisionID when given; otherwise on
// the head just read, retried once if another write won the race).
//
// Clients refresh on the documents domain's document.revised event, which
// carries the project ID (Event.ProjectID) and the document ref; no
// separate page event is emitted.

var _ contracts.ProjectPageAPI = (*Hub)(nil)

// pageProject resolves the request's project: explicit, else the calling
// session's.
func (h *Hub) pageProject(ctx context.Context, p contracts.Principal, projectID string) (string, *contracts.Session, error) {
	projectID = strings.TrimSpace(projectID)
	var s *contracts.Session
	if p.Kind == contracts.PrincipalSession {
		got, err := h.getSession(ctx, h.db, p.ID)
		if err != nil {
			return "", nil, err
		}
		s = &got
		if projectID == "" && got.ProjectID != nil {
			projectID = *got.ProjectID
		}
	}
	if projectID == "" {
		return "", s, contracts.Errorf(contracts.ErrInvalid, "project_id is required (this caller is not in a project)")
	}
	if h.cfg.Projects != nil {
		pr, err := h.cfg.Projects.GetProject(ctx, contracts.GetRequest{ID: projectID})
		if err != nil {
			return "", s, err
		}
		projectID = pr.ProjectID
	}
	return projectID, s, nil
}

// maxLeadDepth bounds projectLeadOf's walk: at most this many parent
// lookups.
const maxLeadDepth = 32

// projectLeadOf is the lead of projectID that s works under: walking up
// from s while the parent is in the project, the first session with no
// parent or whose parent is outside the project (its root of delegation:
// the owner started it, or a coordinator elsewhere appointed it). s leads
// the project when the result is s itself; a child started in the same
// project by its lead does not. A missing parent counts as outside. ""
// when s is not in the project, the walk loops or passes maxLeadDepth, or
// the session it ends at is a bounded worker (TagRoleWorker, I63: never a
// lead).
func (h *Hub) projectLeadOf(ctx context.Context, q queryer, s contracts.Session, projectID string) (string, error) {
	in := func(x contracts.Session) bool { return x.ProjectID != nil && *x.ProjectID == projectID }
	if projectID == "" || !in(s) {
		return "", nil
	}
	root := func(x contracts.Session) string {
		if x.NoLead() {
			return "" // a bounded worker leads nothing
		}
		return x.SessionID
	}
	seen := map[string]bool{s.SessionID: true}
	for depth := 0; ; depth++ {
		pid := s.Parent()
		if pid == "" {
			return root(s), nil
		}
		if seen[pid] || depth == maxLeadDepth {
			return "", nil // a lineage loop, or too deep: no lead
		}
		parent, err := h.getSession(ctx, q, pid)
		if contracts.IsCode(err, contracts.ErrNotFound) {
			return root(s), nil
		}
		if err != nil {
			return "", err
		}
		if !in(parent) {
			return root(s), nil
		}
		seen[pid] = true
		s = parent
	}
}

// leadsOwnProject reports whether s leads the project it is in
// (StartRunCommand.ProjectLead). A lookup error reads as not leading: the
// flag only shapes the run's guide and page reminders; the hub still
// checks every page write.
func (h *Hub) leadsOwnProject(ctx context.Context, q queryer, s contracts.Session) bool {
	if s.ProjectID == nil || *s.ProjectID == "" {
		return false
	}
	lead, err := h.projectLeadOf(ctx, q, s, *s.ProjectID)
	return err == nil && lead == s.SessionID
}

// sessionRole is session sessionID's live role in projectID (nil: its own
// project), by projectLeadOf: the one rule SetProjectPage enforces, read
// when asked (LaunchContext.Role), so a lead flag fixed at launch never
// outlives a parent that joined the project later. For a session in the
// project it also names the lead that maintains the page and when the page
// was last written (pageOwnership, I63); if that cannot be read, the role
// comes without it (a lead then reads as the page owner, as before I63).
func (h *Hub) sessionRole(ctx context.Context, sessionID string, projectID *string) (contracts.ProjectRole, error) {
	s, err := h.getSession(ctx, h.db, sessionID)
	if err != nil {
		return contracts.ProjectRole{}, err
	}
	var role contracts.ProjectRole
	switch {
	case projectID != nil && *projectID != "":
		role.ProjectID = *projectID
	case s.ProjectID != nil:
		role.ProjectID = *s.ProjectID
	}
	if role.ProjectID == "" {
		return role, nil
	}
	lead, err := h.projectLeadOf(ctx, h.db, s, role.ProjectID)
	if err != nil {
		return contracts.ProjectRole{}, err
	}
	role.LeadSessionID, role.Lead = lead, lead != "" && lead == s.SessionID
	if s.ProjectID != nil && *s.ProjectID == role.ProjectID {
		if own, err := h.pageOwnership(ctx, s, role); err == nil {
			role = own
		}
	}
	return role, nil
}

// maxPageAuthorScan bounds how many revisions of the page (and of its
// status card) pageOwnership reads back looking for a live lead's write.
const maxPageAuthorScan = 100

// pageOwnership fills role's page fields for caller s, a session in
// role.ProjectID (I63, iss_…). Several sessions
// lead one project when a coordinator outside it starts more than one
// session in it; only one of them should keep the page, so get_context,
// the guide and the Stop-hook reminder name one: of the live leads
// (liveProjectLeads; the caller counts when it leads), the one that wrote
// the page or its status card last, else the longest-running (earliest
// created, then lowest ID). PageUpdatedAt is the last write by anyone.
func (h *Hub) pageOwnership(ctx context.Context, s contracts.Session, role contracts.ProjectRole) (contracts.ProjectRole, error) {
	leads, err := h.liveProjectLeads(ctx, role.ProjectID)
	if err != nil {
		return role, err
	}
	if role.Lead && !slices.ContainsFunc(leads, func(x contracts.Session) bool { return x.SessionID == s.SessionID }) {
		leads = append(leads, s) // it is asking, so it is live
	}
	var docs []string // the page's and the status card's document IDs
	if h.cfg.Documents != nil {
		for _, slug := range []string{contracts.ProjectPageSlug, contracts.ProjectStatusSlug} {
			dr, err := h.cfg.Documents.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: role.ProjectID, Slug: slug})
			if contracts.IsCode(err, contracts.ErrNotFound) {
				continue
			}
			if err != nil {
				return role, err
			}
			docs = append(docs, dr.Document.DocumentID)
			if at := dr.Revision.CreatedAt; role.PageUpdatedAt == nil || at.After(*role.PageUpdatedAt) {
				role.PageUpdatedAt = &at
			}
		}
	}
	if len(leads) == 0 {
		return role, nil
	}
	owner := ""
	if len(leads) > 1 {
		isLead := map[string]bool{}
		for _, l := range leads {
			isLead[l.SessionID] = true
		}
		var best time.Time
		for _, doc := range docs {
			id, at, err := h.lastPageWriter(ctx, doc, isLead)
			if err != nil {
				return role, err
			}
			if id != "" && (owner == "" || at.After(best)) {
				owner, best = id, at
			}
		}
	}
	if owner == "" {
		owner = slices.MinFunc(leads, func(a, b contracts.Session) int {
			if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(a.SessionID, b.SessionID)
		}).SessionID
	}
	role.PageOwnerSessionID, role.PageOwner = owner, owner == s.SessionID
	if role.Lead {
		for _, l := range leads {
			if l.SessionID != s.SessionID {
				role.CoLeads = append(role.CoLeads, l.SessionID)
			}
		}
		slices.Sort(role.CoLeads)
	}
	return role, nil
}

// lastPageWriter is the newest of documentID's last maxPageAuthorScan
// revisions written by a session in leads ("" when none).
func (h *Hub) lastPageWriter(ctx context.Context, documentID string, leads map[string]bool) (string, time.Time, error) {
	revs, err := h.cfg.Documents.ListRevisions(ctx, contracts.ListRevisionsRequest{PageRequest: contracts.PageRequest{Limit: maxPageAuthorScan}, DocumentID: documentID})
	if err != nil {
		return "", time.Time{}, err
	}
	for _, r := range revs.Items { // newest first
		if r.Author.Kind == contracts.PrincipalSession && leads[r.Author.ID] {
			return r.Author.ID, r.CreatedAt, nil
		}
	}
	return "", time.Time{}, nil
}

// liveProjectLeads are the sessions that lead projectID (projectLeadOf is
// the session itself) and are live: not archived, with a current run that
// has not ended.
func (h *Hub) liveProjectLeads(ctx context.Context, projectID string) ([]contracts.Session, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT `+sesCols+` FROM sessions WHERE project_id=? AND archived=0 AND EXISTS
		(SELECT 1 FROM runs WHERE runs.run_id=sessions.current_run_id AND runs.state NOT IN (?,?))`,
		projectID, string(contracts.RunExited), string(contracts.RunFailed))
	if err != nil {
		return nil, err
	}
	var in []contracts.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		in = append(in, s)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var leads []contracts.Session
	for _, s := range in {
		lead, err := h.projectLeadOf(ctx, h.db, s, projectID)
		if err != nil {
			return nil, err
		}
		if lead == s.SessionID {
			leads = append(leads, s)
		}
	}
	return leads, nil
}

// roleContext is the launch context's line on the caller's role ("" when it
// is in no project, or nobody leads it).
func roleContext(role contracts.ProjectRole) string {
	owner := role.PageOwnerSessionID
	switch {
	case role.ProjectID == "":
		return ""
	case role.Lead && len(role.CoLeads) == 0:
		return "## Your role\n\nYou lead project " + role.ProjectID + " (as of now): you own its project page and status card."
	case role.Lead && (role.PageOwner || owner == ""):
		return "## Your role\n\nYou lead project " + role.ProjectID + " (as of now) together with " + strings.Join(role.CoLeads, ", ") +
			" (each was started in it from outside it).\nYou maintain its project page and status card; the other leads leave them to you."
	case role.Lead:
		return "## Your role\n\nYou lead project " + role.ProjectID + " (as of now) together with " + strings.Join(role.CoLeads, ", ") +
			" (each was started in it from outside it).\nSession " + owner + " maintains its project page and status card: leave them to it (do not rewrite them)\n" +
			"and report your results to your parent."
	case role.LeadSessionID != "":
		text := "## Your role\n\nSession " + role.LeadSessionID + " leads project " + role.ProjectID + " above you (as of now): it keeps the project page and status card;\n" +
			"report to your parent session instead of writing them."
		if owner != "" && owner != role.LeadSessionID {
			text += "\nAmong the project's leads, session " + owner + " maintains the page."
		}
		return text
	case owner != "":
		return "## Your role\n\nYou work in project " + role.ProjectID + " without leading it (as of now): session " + owner + " maintains its project page and status card;\n" +
			"report to your parent session instead of writing them."
	default:
		return ""
	}
}

// SetProjectPage implements projects.set_page.
func (h *Hub) SetProjectPage(ctx context.Context, req contracts.SetProjectPageRequest) (contracts.ProjectPage, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.ProjectPage{}, err
	}
	if h.cfg.Documents == nil {
		return contracts.ProjectPage{}, notConfigured("documents")
	}
	hasContent := strings.TrimSpace(req.Content) != ""
	if !hasContent && req.Status == nil {
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrInvalid, "content or status is required: the page in markdown (goal, current status, results with links, open decisions, what's next) and/or the status card")
	}
	if !hasContent && req.BaseRevisionID != "" {
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrInvalid, "base_revision_id applies to content; a status-only write has no base")
	}
	var statusJSON []byte
	if req.Status != nil {
		var err error
		if statusJSON, err = contracts.ValidateProjectStatus(req.Status); err != nil {
			return contracts.ProjectPage{}, err
		}
	}
	if len(req.Content) > contracts.ProjectPageMaxSize {
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrPayloadTooLarge, "the page is %d bytes; limit %d. Link to longer material (pyre publish) instead of inlining it", len(req.Content), contracts.ProjectPageMaxSize).
			WithDetail("max_bytes", contracts.ProjectPageMaxSize)
	}
	if !utf8.ValidString(req.Content) {
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrInvalid, "content must be UTF-8 text")
	}
	projectID, s, err := h.pageProject(ctx, p, req.ProjectID)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	if !isOwner(p) {
		lead := ""
		if s != nil {
			if lead, err = h.projectLeadOf(ctx, h.db, *s, projectID); err != nil {
				return contracts.ProjectPage{}, err
			}
		}
		switch {
		case s != nil && lead == s.SessionID:
		case s != nil && s.NoLead():
			return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrForbidden, "this session was started as a bounded worker (--no-lead, tag %s) and never leads project %s, so it does not write its page: report to your parent session instead", contracts.TagRoleWorker, projectID).
				WithRef(contracts.KindProject, projectID)
		case lead != "":
			return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrForbidden, "only a lead of project %s (a session in it with no parent, or whose parent is outside it) or the owner writes the project page; %s leads it above you: report to your parent session instead", projectID, lead).
				WithRef(contracts.KindSession, lead)
		default:
			return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrForbidden, "only the owner or a lead of project %s (a session in it with no parent, or whose parent is outside it) writes its page", projectID).WithRef(contracts.KindProject, projectID)
		}
	}
	summary := strings.TrimSpace(req.Summary)
	var out contracts.ProjectPage
	if hasContent {
		dr, err := h.writePageDoc(ctx, projectID, contracts.ProjectPageSlug, "Project page", "", req.Content, summary, req.BaseRevisionID, req.OperationID+":page")
		if err != nil {
			return contracts.ProjectPage{}, err
		}
		out = pageOf(projectID, dr)
	} else if out, err = h.readPage(ctx, projectID); err != nil {
		return contracts.ProjectPage{}, err
	}
	if req.Status != nil {
		dr, err := h.writePageDoc(ctx, projectID, contracts.ProjectStatusSlug, "Project status", "application/json", string(statusJSON), summary, "", req.OperationID+":status")
		if err != nil {
			return contracts.ProjectPage{}, err
		}
		st := *req.Status
		at := dr.Revision.CreatedAt
		out.Status, out.StatusUpdatedAt = &st, &at
	} else if err := h.readStatus(ctx, &out); err != nil {
		return contracts.ProjectPage{}, err
	}
	return out, nil
}

// writePageDoc writes a project document by slug: the first write creates
// it, later ones are head revisions (compare-and-swap on base when given;
// otherwise on the head just read, retried once if another write won).
func (h *Hub) writePageDoc(ctx context.Context, projectID, slug, title, mediaType, content, summary, base, opID string) (contracts.DocumentWithRevision, error) {
	docs := h.cfg.Documents
	cur, err := docs.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: projectID, Slug: slug})
	switch {
	case contracts.IsCode(err, contracts.ErrNotFound):
		if base != "" {
			return contracts.DocumentWithRevision{}, contracts.Errorf(contracts.ErrConflict, "the project has no page yet, so base_revision_id %s cannot be its head; write without it to create the page", base).
				WithDetail("current_head", "")
		}
		pid := projectID
		dr, err := docs.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: contracts.Mutation{OperationID: opID + "-create"},
			ProjectID: &pid, Title: title, Slug: slug, MediaType: mediaType, Content: content, Summary: summary})
		if err == nil {
			return dr, nil
		}
		if !contracts.IsCode(err, contracts.ErrConflict) {
			return contracts.DocumentWithRevision{}, err
		}
		// Created concurrently: write a revision on top of it.
		if cur, err = docs.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: projectID, Slug: slug}); err != nil {
			return contracts.DocumentWithRevision{}, err
		}
	case err != nil:
		return contracts.DocumentWithRevision{}, err
	}
	base = strings.TrimSpace(base)
	cas := base != ""
	if !cas {
		base = cur.Document.HeadRevisionID
	}
	dr, err := docs.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: opID + "-rev"},
		DocumentID: cur.Document.DocumentID, BaseRevisionID: base, Content: content, Summary: summary})
	if contracts.IsCode(err, contracts.ErrConflict) && !cas {
		// Another write landed between our read and write: last writer wins.
		if cur, err = docs.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: cur.Document.DocumentID}); err != nil {
			return contracts.DocumentWithRevision{}, err
		}
		dr, err = docs.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: opID + "-rev2"},
			DocumentID: cur.Document.DocumentID, BaseRevisionID: cur.Document.HeadRevisionID, Content: content, Summary: summary})
	}
	return dr, err
}

// GetProjectPage implements projects.get_page; a project without a page
// returns an empty ProjectPage (Revision nil); Status is the status card.
func (h *Hub) GetProjectPage(ctx context.Context, req contracts.GetProjectPageRequest) (contracts.ProjectPage, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	if h.cfg.Documents == nil {
		return contracts.ProjectPage{}, notConfigured("documents")
	}
	projectID, _, err := h.pageProject(ctx, p, req.ProjectID)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	pg, err := h.readPage(ctx, projectID)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	if err := h.readStatus(ctx, &pg); err != nil {
		return contracts.ProjectPage{}, err
	}
	return pg, nil
}

// readPage is the page head (Revision nil when none was written).
func (h *Hub) readPage(ctx context.Context, projectID string) (contracts.ProjectPage, error) {
	dr, err := h.cfg.Documents.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: projectID, Slug: contracts.ProjectPageSlug})
	if contracts.IsCode(err, contracts.ErrNotFound) {
		return contracts.ProjectPage{ProjectID: projectID}, nil
	}
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	return pageOf(projectID, dr), nil
}

// readStatus fills pg's status card from its document (none: unchanged).
func (h *Hub) readStatus(ctx context.Context, pg *contracts.ProjectPage) error {
	dr, err := h.cfg.Documents.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: pg.ProjectID, Slug: contracts.ProjectStatusSlug})
	if contracts.IsCode(err, contracts.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var st contracts.ProjectStatus
	if json.Unmarshal([]byte(dr.Revision.Content), &st) != nil {
		return nil // hand-edited into something else: no card rather than a broken page
	}
	at := dr.Revision.CreatedAt
	pg.Status, pg.StatusUpdatedAt = &st, &at
	return nil
}

func pageOf(projectID string, dr contracts.DocumentWithRevision) contracts.ProjectPage {
	d, r := dr.Document, dr.Revision
	return contracts.ProjectPage{ProjectID: projectID, Document: &d, Revision: &r, Content: r.Content}
}
