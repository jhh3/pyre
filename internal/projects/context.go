package projects

import (
	"context"
	"fmt"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
)

// Launch context bounds.
const (
	DefaultContextBytes = 8 << 10
	MinContextBytes     = 512
	MaxContextBytes     = 64 << 10
	contextListItems    = 8
	truncationReserve   = 160
)

// ctxBuilder appends whole lines within a byte budget and records the
// references for everything it included.
type ctxBuilder struct {
	b         strings.Builder
	budget    int
	refs      []contracts.ObjectRef
	truncated bool
	omitted   int
}

func (c *ctxBuilder) room() int { return c.budget - c.b.Len() }

// line adds s+"\n" if it fits; otherwise marks truncation.
func (c *ctxBuilder) line(s string, refs ...contracts.ObjectRef) bool {
	if len(s)+1 > c.room() {
		c.truncated = true
		c.omitted++
		return false
	}
	c.b.WriteString(s)
	c.b.WriteByte('\n')
	c.refs = append(c.refs, refs...)
	return true
}

// section adds items under a heading; the heading is only written if at
// least one item fits.
func (c *ctxBuilder) section(heading string, items []ctxItem) {
	if len(items) == 0 {
		return
	}
	if len(heading)+len(items[0].text)+3 > c.room() {
		c.truncated = true
		c.omitted += len(items)
		return
	}
	c.line("")
	c.line(heading)
	for i, it := range items {
		if !c.line(it.text, it.ref) {
			c.omitted += len(items) - i - 1
			return
		}
	}
}

type ctxItem struct {
	text string
	ref  contracts.ObjectRef
}

// GetLaunchContext builds a bounded launch brief (default 8 KiB): project
// name and short description, the current brief revision text (truncated
// with a pointer if needed), recent decision and handoff titles, other
// project document titles and artifact pointers, each with a pyre://
// reference. It records the brief revision and every included reference,
// marks truncation, and never includes transcripts or note bodies.
//
// With only SessionID it summarizes that session's decisions and handoffs.
// With neither, it returns an empty context.
func (s *Store) GetLaunchContext(ctx context.Context, req contracts.ContextRequest) (contracts.LaunchContext, error) {
	max := req.MaxBytes
	switch {
	case max <= 0:
		max = DefaultContextBytes
	case max < MinContextBytes:
		max = MinContextBytes
	case max > MaxContextBytes:
		max = MaxContextBytes
	}
	now := s.clock.Now()
	out := contracts.LaunchContext{
		References:      []contracts.ObjectRef{},
		MemoryFreshness: &contracts.Freshness{ObservedAt: now, Source: "hub", Reason: "hub-published notes only; Hive ranked memory is retrieved separately"},
	}
	projectID := svcutil.CopyProject(req.ProjectID)
	if projectID == nil && req.SessionID == "" {
		return out, nil
	}
	c := &ctxBuilder{budget: max - truncationReserve}

	var project contracts.Project
	if projectID != nil {
		var err error
		project, err = getProjectQ(ctx, s.db, *projectID)
		if err != nil {
			return out, err
		}
		head := "# Project: " + project.Name
		if project.Archived {
			head += " (archived)"
		}
		c.line(svcutil.TruncateUTF8(head, 300), contracts.ObjectRef{Kind: contracts.KindProject, ID: project.ProjectID})
		c.line("Reference: " + contracts.DeepLink(contracts.ObjectRef{Kind: contracts.KindProject, ID: project.ProjectID}))
		if d := firstLine(project.Description); d != "" {
			c.line(svcutil.TruncateUTF8(d, 300))
		}
		if len(project.Repos) > 0 {
			names := make([]string, 0, len(project.Repos))
			for _, r := range project.Repos {
				names = append(names, r.RepoID)
			}
			c.line(svcutil.TruncateUTF8("Repos: "+strings.Join(names, ", "), 300))
		}
		if project.BriefDocumentID != "" {
			if err := s.addBrief(ctx, c, &out, project.BriefDocumentID, max); err != nil {
				return out, err
			}
		}
	} else {
		c.line("# Session context: " + req.SessionID)
	}

	decisions, err := s.recentNotes(ctx, projectID, req.SessionID, contracts.NoteDecision)
	if err != nil {
		return out, err
	}
	c.section("## Recent decisions", decisions)
	handoffs, err := s.recentNotes(ctx, projectID, req.SessionID, contracts.NoteHandoff)
	if err != nil {
		return out, err
	}
	c.section("## Recent handoffs", handoffs)

	if projectID != nil {
		docs, err := s.ListDocuments(ctx, contracts.ListDocumentsRequest{ProjectID: projectID, PageRequest: contracts.PageRequest{Limit: contextListItems + 1}})
		if err != nil {
			return out, err
		}
		var items []ctxItem
		for _, d := range docs.Items {
			if d.DocumentID == project.BriefDocumentID || len(items) >= contextListItems {
				continue
			}
			ref := contracts.ObjectRef{Kind: contracts.KindDocument, ID: d.DocumentID}
			items = append(items, ctxItem{text: fmt.Sprintf("- %s [%s] rev %d %s", d.Title, d.Slug, d.HeadSeq, contracts.DeepLink(ref)), ref: ref})
		}
		if docs.NextCursor != "" {
			c.truncated = true
		}
		c.section("## Documents", items)

		if s.artifacts != nil {
			arts, err := s.artifacts.ListArtifacts(ctx, contracts.ListArtifactsRequest{ProjectID: projectID, PageRequest: contracts.PageRequest{Limit: contextListItems}})
			if err != nil {
				return out, err
			}
			items = nil
			for _, a := range arts.Items {
				ref := contracts.ObjectRef{Kind: contracts.KindArtifact, ID: a.ArtifactID}
				text := fmt.Sprintf("- %s (%d version(s)) %s", a.Title, a.VersionCount, contracts.DeepLink(ref))
				if a.LatestVersionID != "" {
					ref = contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: a.LatestVersionID}
					text = fmt.Sprintf("- %s (v%d) %s", a.Title, a.VersionCount, contracts.DeepLink(ref))
				}
				items = append(items, ctxItem{text: text, ref: ref})
			}
			c.section("## Artifacts", items)
		}
	}

	if c.truncated {
		c.budget = max
		msg := "\n[context truncated to fit " + fmt.Sprint(max) + " bytes"
		if c.omitted > 0 {
			msg += fmt.Sprintf("; %d item(s) omitted", c.omitted)
		}
		msg += "; retrieve more by reference]"
		c.line(svcutil.TruncateUTF8(msg, c.room()-1))
	}
	out.Text = c.b.String()
	out.Bytes = len(out.Text)
	out.Truncated = c.truncated
	out.References = dedupeRefs(c.refs)
	return out, nil
}

func (s *Store) addBrief(ctx context.Context, c *ctxBuilder, out *contracts.LaunchContext, docID string, max int) error {
	dwr, err := s.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: docID})
	if contracts.IsCode(err, contracts.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	rev := dwr.Revision
	ref := contracts.ObjectRef{Kind: contracts.KindDocumentRevision, ID: rev.RevisionID}
	heading := fmt.Sprintf("## Brief (revision %d, %s)", rev.Seq, contracts.DeepLink(ref))
	// The brief gets at most ~55% of the budget so pointers still fit.
	allow := max*55/100 - len(heading) - 2
	if r := c.room() - len(heading) - 3; allow > r {
		allow = r
	}
	if allow < 64 {
		c.truncated = true
		c.omitted++
		return nil
	}
	text := strings.TrimRight(rev.Content, "\n")
	if len(text) > allow {
		note := "\n[brief truncated; full text: " + contracts.DeepLink(ref) + "]"
		cut := svcutil.TruncateUTF8(text, allow-len(note))
		if i := strings.LastIndexByte(cut, '\n'); i > len(cut)/2 {
			cut = cut[:i]
		}
		text = cut + note
		c.truncated = true
	}
	c.line("")
	c.line(heading)
	c.line(text, ref)
	out.BriefRevision = rev.RevisionID
	return nil
}

func (s *Store) recentNotes(ctx context.Context, projectID *string, sessionID string, kind contracts.NoteKind) ([]ctxItem, error) {
	req := contracts.ListNotesRequest{Kind: kind, PageRequest: contracts.PageRequest{Limit: contextListItems}}
	if projectID != nil {
		req.ProjectID = projectID
	} else {
		req.SessionID = sessionID
	}
	page, err := s.ListNotes(ctx, req)
	if err != nil {
		return nil, err
	}
	items := make([]ctxItem, 0, len(page.Items))
	for _, n := range page.Items {
		ref := contracts.ObjectRef{Kind: contracts.KindNote, ID: n.NoteID}
		items = append(items, ctxItem{
			text: fmt.Sprintf("- %s (%s, %s) %s", svcutil.TruncateUTF8(n.Title, 200), n.CreatedAt.Format("2006-01-02"), authorLabel(n), contracts.DeepLink(ref)),
			ref:  ref,
		})
	}
	return items, nil
}

func authorLabel(n contracts.Note) string {
	if n.SessionID != "" {
		return n.SessionID
	}
	return string(n.Author.Kind)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func dedupeRefs(in []contracts.ObjectRef) []contracts.ObjectRef {
	out := make([]contracts.ObjectRef, 0, len(in))
	seen := map[contracts.ObjectRef]bool{}
	for _, r := range in {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}
