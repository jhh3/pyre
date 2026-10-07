package projects

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
)

const (
	maxNameLen        = 200
	maxDescriptionLen = 8 << 10
	maxListItems      = 100
)

const projectCols = `project_id, name, description, brief_document_id, repos, checkouts, links, env_profile, connections, archived, version, created_by, created_at, updated_at`

func scanProject(sc interface{ Scan(...any) error }) (contracts.Project, error) {
	var p contracts.Project
	var repos, checkouts, links, conns, createdBy, createdAt, updatedAt string
	var archived int
	if err := sc.Scan(&p.ProjectID, &p.Name, &p.Description, &p.BriefDocumentID, &repos, &checkouts, &links, &p.EnvProfile, &conns, &archived, &p.Version, &createdBy, &createdAt, &updatedAt); err != nil {
		return p, err
	}
	p.Repos = []contracts.RepoBinding{}
	p.Checkouts = []contracts.CheckoutBinding{}
	_ = svcutil.FromJSON(repos, &p.Repos)
	_ = svcutil.FromJSON(checkouts, &p.Checkouts)
	_ = svcutil.FromJSON(links, &p.Links)
	_ = svcutil.FromJSON(conns, &p.Connections)
	_ = svcutil.FromJSON(createdBy, &p.CreatedBy)
	p.Archived = archived != 0
	p.CreatedAt = svcutil.ParseTime(createdAt)
	p.UpdatedAt = svcutil.ParseTime(updatedAt)
	return p, nil
}

func getProjectQ(ctx context.Context, q queryer, id string) (contracts.Project, error) {
	p, err := scanProject(q.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE project_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, contracts.Errorf(contracts.ErrNotFound, "project not found").WithRef(contracts.KindProject, id)
	}
	return p, err
}

// CreateProject creates a project. Only Name is required. A non-empty Brief
// creates the "brief" document at revision 1 and sets BriefDocumentID.
func (s *Store) CreateProject(ctx context.Context, req contracts.CreateProjectRequest) (contracts.Project, error) {
	var out contracts.Project
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpCreateProject, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		name := strings.TrimSpace(req.Name)
		if err := validName(name); err != nil {
			return nil, err
		}
		if len(req.Description) > maxDescriptionLen {
			return nil, contracts.Errorf(contracts.ErrInvalid, "description exceeds %d bytes", maxDescriptionLen)
		}
		repos, err := normalizeRepos(req.Repos)
		if err != nil {
			return nil, err
		}
		links, err := normalizeLinks(req.Links)
		if err != nil {
			return nil, err
		}
		now := s.clock.Now()
		p := contracts.Project{
			ProjectID:   contracts.NewID(contracts.PrefixProject),
			Name:        name,
			Description: req.Description,
			Repos:       repos,
			Checkouts:   []contracts.CheckoutBinding{},
			Links:       links,
			Version:     1,
			CreatedBy:   who,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := s.insertProject(ctx, tx, p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(req.Brief) != "" {
			pid := p.ProjectID
			dwr, err := s.insertDocument(ctx, tx, who, now, &pid, "Brief", "brief", "", req.Brief, "initial brief", nil)
			if err != nil {
				return nil, err
			}
			p.BriefDocumentID = dwr.Document.DocumentID
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET brief_document_id=? WHERE project_id=?`, p.BriefDocumentID, p.ProjectID); err != nil {
				return nil, err
			}
		}
		pid := p.ProjectID
		if err := svcutil.Emit(ctx, tx, s.sink, now, contracts.EvProjectChanged, contracts.ObjectRef{Kind: contracts.KindProject, ID: p.ProjectID}, &pid, sessionOf(who), p); err != nil {
			return nil, err
		}
		return p, nil
	})
	return out, err
}

func (s *Store) insertProject(ctx context.Context, tx *sql.Tx, p contracts.Project) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO projects(`+projectCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ProjectID, p.Name, p.Description, p.BriefDocumentID, svcutil.JSON(p.Repos), svcutil.JSON(p.Checkouts), svcutil.JSON(nonNilLinks(p.Links)),
		p.EnvProfile, svcutil.JSON(nonNilStrings(p.Connections)), boolInt(p.Archived), p.Version, svcutil.JSON(p.CreatedBy), svcutil.Time(p.CreatedAt), svcutil.Time(p.UpdatedAt))
	return err
}

// GetProject returns a project by ID.
func (s *Store) GetProject(ctx context.Context, req contracts.GetRequest) (contracts.Project, error) {
	return getProjectQ(ctx, s.db, req.ID)
}

// ListProjects lists projects newest first. Archived projects are excluded
// unless IncludeArchived. Query is a case-insensitive substring match on
// name and description.
func (s *Store) ListProjects(ctx context.Context, req contracts.ListProjectsRequest) (contracts.Page[contracts.Project], error) {
	page := contracts.Page[contracts.Project]{Items: []contracts.Project{}, SnapshotAt: s.clock.Now()}
	after, err := svcutil.DecodeCursor(req.Cursor)
	if err != nil {
		return page, err
	}
	where := []string{"1=1"}
	var args []any
	if !req.IncludeArchived {
		where = append(where, "archived=0")
	}
	if q := strings.TrimSpace(req.Query); q != "" {
		where = append(where, `(name LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\')`)
		args = append(args, svcutil.LikeEscape(q), svcutil.LikeEscape(q))
	}
	if after != "" {
		where = append(where, "project_id < ?")
		args = append(args, after)
	}
	limit := svcutil.Limit(req.Limit)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects WHERE `+strings.Join(where, " AND ")+` ORDER BY project_id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, p)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = svcutil.EncodeCursor(page.Items[limit-1].ProjectID)
	}
	return page, nil
}

// UpdateProject replaces the provided mutable fields if req.Version equals
// the stored version (compare-and-swap); otherwise ErrConflict with details
// {"current_version": n}. Archiving is Archived=true.
func (s *Store) UpdateProject(ctx context.Context, req contracts.UpdateProjectRequest) (contracts.Project, error) {
	var out contracts.Project
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpUpdateProject, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		p, err := getProjectQ(ctx, tx, req.ProjectID)
		if err != nil {
			return nil, err
		}
		if req.Version != p.Version {
			return nil, contracts.Errorf(contracts.ErrConflict, "project version is %d, not %d", p.Version, req.Version).
				WithRef(contracts.KindProject, p.ProjectID).WithDetail("current_version", p.Version)
		}
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if err := validName(name); err != nil {
				return nil, err
			}
			p.Name = name
		}
		if req.Description != nil {
			if len(*req.Description) > maxDescriptionLen {
				return nil, contracts.Errorf(contracts.ErrInvalid, "description exceeds %d bytes", maxDescriptionLen)
			}
			p.Description = *req.Description
		}
		if req.Repos != nil {
			repos, err := normalizeRepos(*req.Repos)
			if err != nil {
				return nil, err
			}
			p.Repos = repos
		}
		if req.Checkouts != nil {
			p.Checkouts = *req.Checkouts
		}
		// Checkouts are validated against the final repo set (either may change).
		cos, err := normalizeCheckouts(p.Checkouts, p.Repos)
		if err != nil {
			return nil, err
		}
		p.Checkouts = cos
		if req.Links != nil {
			links, err := normalizeLinks(*req.Links)
			if err != nil {
				return nil, err
			}
			p.Links = links
		}
		if req.EnvProfile != nil {
			if len(*req.EnvProfile) > maxNameLen {
				return nil, contracts.Errorf(contracts.ErrInvalid, "env_profile too long")
			}
			p.EnvProfile = strings.TrimSpace(*req.EnvProfile)
		}
		if req.Connections != nil {
			conns, err := normalizeConnections(*req.Connections)
			if err != nil {
				return nil, err
			}
			p.Connections = conns
		}
		if req.Archived != nil {
			p.Archived = *req.Archived
		}
		now := s.clock.Now()
		p.Version++
		p.UpdatedAt = now
		res, err := tx.ExecContext(ctx, `UPDATE projects SET name=?, description=?, repos=?, checkouts=?, links=?, env_profile=?, connections=?, archived=?, version=?, updated_at=? WHERE project_id=? AND version=?`,
			p.Name, p.Description, svcutil.JSON(p.Repos), svcutil.JSON(p.Checkouts), svcutil.JSON(nonNilLinks(p.Links)), p.EnvProfile, svcutil.JSON(nonNilStrings(p.Connections)),
			boolInt(p.Archived), p.Version, svcutil.Time(now), p.ProjectID, req.Version)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, contracts.Errorf(contracts.ErrConflict, "project changed concurrently").WithRef(contracts.KindProject, p.ProjectID)
		}
		pid := p.ProjectID
		if err := svcutil.Emit(ctx, tx, s.sink, now, contracts.EvProjectChanged, contracts.ObjectRef{Kind: contracts.KindProject, ID: p.ProjectID}, &pid, sessionOf(who), p); err != nil {
			return nil, err
		}
		return p, nil
	})
	return out, err
}

// ---- validation ----

func validName(name string) error {
	if name == "" {
		return contracts.Errorf(contracts.ErrInvalid, "name is required")
	}
	if len(name) > maxNameLen {
		return contracts.Errorf(contracts.ErrInvalid, "name exceeds %d bytes", maxNameLen)
	}
	if strings.ContainsAny(name, "\n\r\x00") {
		return contracts.Errorf(contracts.ErrInvalid, "name must be a single line")
	}
	return nil
}

func validKey(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func normalizeRepos(in []contracts.RepoBinding) ([]contracts.RepoBinding, error) {
	if len(in) > maxListItems {
		return nil, contracts.Errorf(contracts.ErrInvalid, "too many repos (max %d)", maxListItems)
	}
	out := make([]contracts.RepoBinding, 0, len(in))
	seen := map[string]bool{}
	for _, r := range in {
		r.RepoID = strings.TrimSpace(r.RepoID)
		if !validKey(r.RepoID) {
			return nil, contracts.Errorf(contracts.ErrInvalid, "repo_id %q must be 1-64 chars of [A-Za-z0-9._-]", r.RepoID)
		}
		if seen[r.RepoID] {
			return nil, contracts.Errorf(contracts.ErrInvalid, "duplicate repo_id %q", r.RepoID)
		}
		seen[r.RepoID] = true
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" {
			r.Name = r.RepoID
		}
		if len(r.Name) > maxNameLen || len(r.URL) > 2048 {
			return nil, contracts.Errorf(contracts.ErrInvalid, "repo %q name/url too long", r.RepoID)
		}
		out = append(out, r)
	}
	return out, nil
}

func normalizeCheckouts(in []contracts.CheckoutBinding, repos []contracts.RepoBinding) ([]contracts.CheckoutBinding, error) {
	if len(in) > maxListItems {
		return nil, contracts.Errorf(contracts.ErrInvalid, "too many checkouts (max %d)", maxListItems)
	}
	known := map[string]bool{}
	for _, r := range repos {
		known[r.RepoID] = true
	}
	out := make([]contracts.CheckoutBinding, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c.NodeID = strings.TrimSpace(c.NodeID)
		c.Path = strings.TrimSpace(c.Path)
		if c.NodeID == "" || c.Path == "" {
			return nil, contracts.Errorf(contracts.ErrInvalid, "checkout requires node_id and path")
		}
		if len(c.Path) > 4096 || strings.ContainsRune(c.Path, 0) {
			return nil, contracts.Errorf(contracts.ErrInvalid, "checkout path is invalid")
		}
		switch c.Mode {
		case contracts.CheckoutShared, contracts.CheckoutWorktree, contracts.CheckoutPlain:
		default:
			return nil, contracts.Errorf(contracts.ErrInvalid, "checkout mode must be shared, worktree or plain (got %q)", c.Mode)
		}
		if c.RepoID != "" && !known[c.RepoID] {
			return nil, contracts.Errorf(contracts.ErrInvalid, "checkout references unknown repo_id %q", c.RepoID)
		}
		key := c.NodeID + "\x00" + c.Path
		if seen[key] {
			return nil, contracts.Errorf(contracts.ErrInvalid, "duplicate checkout %s:%s", c.NodeID, c.Path)
		}
		seen[key] = true
		out = append(out, c)
	}
	return out, nil
}

func normalizeLinks(in []contracts.Link) ([]contracts.Link, error) {
	if len(in) > maxListItems {
		return nil, contracts.Errorf(contracts.ErrInvalid, "too many links (max %d)", maxListItems)
	}
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]contracts.Link, 0, len(in))
	for _, l := range in {
		l.URL = strings.TrimSpace(l.URL)
		l.Title = strings.TrimSpace(l.Title)
		u, err := url.Parse(l.URL)
		if l.URL == "" || err != nil || u.Scheme == "" || len(l.URL) > 2048 {
			return nil, contracts.Errorf(contracts.ErrInvalid, "link url %q must be an absolute URL", l.URL)
		}
		if strings.EqualFold(u.Scheme, "javascript") || strings.EqualFold(u.Scheme, "data") {
			return nil, contracts.Errorf(contracts.ErrInvalid, "link scheme %q not allowed", u.Scheme)
		}
		if l.Title == "" {
			l.Title = l.URL
		}
		out = append(out, l)
	}
	return out, nil
}

func normalizeConnections(in []string) ([]string, error) {
	if len(in) > maxListItems {
		return nil, contracts.Errorf(contracts.ErrInvalid, "too many connections (max %d)", maxListItems)
	}
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c = strings.TrimSpace(c)
		// Connection references are names only; refuse anything that looks
		// like an inline credential assignment.
		if !validKey(strings.ReplaceAll(c, "/", "-")) {
			return nil, contracts.Errorf(contracts.ErrInvalid, "connection reference %q must be a name, never a secret", c)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

func nonNilLinks(l []contracts.Link) []contracts.Link {
	if l == nil {
		return []contracts.Link{}
	}
	return l
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sessionOf returns the session ID of a session principal (for event
// attribution), else "".
func sessionOf(p contracts.Principal) string {
	if p.Kind == contracts.PrincipalSession {
		return p.ID
	}
	return ""
}
