package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

const (
	tabOverview = iota
	tabDocs
	tabArtifacts
	tabDiscussion
	tabSessions
	numTabs
)

var tabNames = [numTabs]string{"Overview", "Docs", "Artifacts", "Discussion", "Sessions"}

// ---- projects list ----

func (m *Model) visibleProjects() []contracts.Project {
	var out []contracts.Project
	for _, p := range m.projects() {
		if p.Archived {
			continue
		}
		var repos []string
		for _, r := range p.Repos {
			repos = append(repos, r.Name)
		}
		if m.filter != "" && !filterOK(m.filter, p.Name, p.Description, strings.Join(repos, " "), p.ProjectID) {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ProjectID < out[j].ProjectID
	})
	return out
}

func (m *Model) syncProjects() {
	vis := m.visibleProjects()
	ids := make([]string, len(vis))
	for i, p := range vis {
		ids[i] = p.ProjectID
	}
	m.projList.setIDs(ids)
}

func (m *Model) projectSessions(pid string) []contracts.SessionView {
	var out []contracts.SessionView
	for _, s := range m.sessions() {
		if s.Session.ProjectID != nil && *s.Session.ProjectID == pid && !s.Session.Archived {
			out = append(out, s)
		}
	}
	sortSessions(out)
	return out
}

func (m *Model) loadProjectPreview(force bool) tea.Cmd {
	p := m.project(m.projList.sel)
	if p == nil || p.BriefDocumentID == "" {
		return nil
	}
	return m.loadDoc(p.BriefDocumentID, "", force)
}

func (m *Model) keyProjects(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if navKey(&m.projList, key, m.halfPage()) {
		return m.loadProjectPreview(false)
	}
	switch key {
	case "enter":
		if m.projList.sel != "" {
			return m.openProject(m.projList.sel)
		}
	case "X":
		if m.projList.sel != "" && !k.IsRepeat {
			return m.confirmArchiveProject(m.projList.sel) // archive.go (W-archive2)
		}
	case "esc":
		if m.filter != "" {
			m.applyFilter("")
		}
	}
	return nil
}

func (m *Model) viewProjects(w, h int) []string {
	list := func(w, h int) []string {
		vis := m.visibleProjects()
		nameW := w - 2 - 20 - 5
		out := []string{m.colHeader("  "+pad("PROJECT", nameW)+" "+pad("CONTENTS", 19)+" "+padLeft("AGE", 4), w)}
		_, st := get[[]contracts.Project](m, "projects")
		if len(vis) == 0 {
			switch {
			case st == nil || (!st.have && st.inflight):
				out = append(out, m.th.dim.Render("  loading…"))
			case !st.have && st.err != nil:
				out = append(out, m.th.tone(toneBad, "  ✗ "+errText(st.err)))
			case m.filter != "":
				out = append(out, m.th.dim.Render("  no projects match"))
			default:
				out = append(out, m.th.dim.Render("  no projects — sessions work fine without one"))
			}
			return out
		}
		byID := map[string]contracts.Project{}
		for _, p := range vis {
			byID[p.ProjectID] = p
		}
		now := m.now()
		start, end := m.projList.window(h - 1)
		for i := start; i < end; i++ {
			p := byID[m.projList.ids[i]]
			sel := i == m.projList.idx
			cur := m.cursor(sel)
			meta := plural(len(p.Repos), "repo", "repos") + " · " + plural(len(m.projectSessions(p.ProjectID)), "session", "sessions")
			row := cur + m.titleCell(sel, p.Name, nameW) + " " + m.th.dim.Render(pad(meta, 19)) + " " + m.th.dim.Render(padLeft(age(now, p.UpdatedAt), 4))
			out = append(out, m.markRow(&m.projList, i, sel, row, w))
		}
		return out
	}
	detail := func(w, h int) []string {
		p := m.project(m.projList.sel)
		if p == nil {
			return []string{m.th.dim.Render("No project selected")}
		}
		return m.projectOverview(*p, w, h, false)
	}
	return m.listDetail(w, h, list, detail)
}

// ---- project home ----

func (m *Model) openProject(pid string) tea.Cmd {
	m.curProject = pid
	m.projTab = tabOverview
	m.docScroll = 0
	m.projList.selectID(pid)
	return m.push(viewProject)
}

func (m *Model) projectHints() []hint {
	h := []hint{{"←→", "section"}}
	switch m.projTab {
	case tabDocs:
		h = append(h, hint{"enter", "read"}, hint{"e", "edit"})
	case tabArtifacts:
		h = append(h, hint{"enter", "versions & review"})
	case tabDiscussion:
		h = append(h, hint{"c", "post to project thread"})
	case tabSessions:
		h = append(h, hint{"enter", "go to session"})
	}
	return h
}

func (m *Model) loadProjectView(force bool) []tea.Cmd {
	pid := m.curProject
	if pid == "" {
		return nil
	}
	api := m.api
	pidp := &pid
	ld := func(key string, fn loaderFn) tea.Cmd {
		if force {
			return m.reload(key, fn)
		}
		return m.ensure(key, 0, fn)
	}
	var cmds []tea.Cmd
	switch m.view {
	case viewProject:
		switch m.projTab {
		case tabOverview:
			if p := m.project(pid); p != nil && p.BriefDocumentID != "" {
				cmds = append(cmds, m.loadDoc(p.BriefDocumentID, "", force))
			}
			fallthrough
		case tabArtifacts:
			cmds = append(cmds, ld("proj:"+pid+":arts", func(ctx context.Context) (any, error) {
				return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.Artifact], error) {
					return api.ListArtifacts(ctx, contracts.ListArtifactsRequest{ProjectID: pidp, PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
				})
			}))
			if m.projTab == tabOverview {
				cmds = append(cmds, m.loadProjectDocs(force))
			}
		case tabDocs:
			cmds = append(cmds, m.loadProjectDocs(force))
			if d := m.listFor("proj:" + pid + ":docs").sel; d != "" {
				cmds = append(cmds, m.loadDoc(d, "", force))
			}
		case tabDiscussion:
			cmds = append(cmds, ld("proj:"+pid+":comments", func(ctx context.Context) (any, error) {
				return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.ReviewComment], error) {
					return api.ListComments(ctx, contracts.ListCommentsRequest{ProjectID: pidp, PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
				})
			}), ld("proj:"+pid+":thread", func(ctx context.Context) (any, error) {
				p, err := api.ListMessages(ctx, contracts.ListMessagesRequest{Box: "inbox", Address: &contracts.Address{Kind: contracts.AddrProject, ID: pid}, PageRequest: contracts.PageRequest{Limit: 100}})
				return p.Items, err
			}))
		}
	case viewArtifact:
		aid := m.curArtifact
		cmds = append(cmds, ld("artifact:"+aid, func(ctx context.Context) (any, error) {
			return api.GetArtifact(ctx, contracts.GetRequest{ID: aid})
		}), ld("art:"+aid+":versions", func(ctx context.Context) (any, error) {
			return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.ArtifactVersion], error) {
				return api.ListVersions(ctx, contracts.ListVersionsRequest{ArtifactID: aid, PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
			})
		}), ld("art:"+aid+":comments", func(ctx context.Context) (any, error) {
			return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.ReviewComment], error) {
				return api.ListComments(ctx, contracts.ListCommentsRequest{ArtifactID: aid, PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
			})
		}))
		if v, ok := m.selectedVersion(); ok && previewable(v) {
			cmds = append(cmds, m.loadContent(v, force))
		}
	case viewDoc:
		did := m.curDoc
		cmds = append(cmds, m.loadDoc(did, m.curDocRev, force), ld("doc:"+did+":revs", func(ctx context.Context) (any, error) {
			return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.DocumentRevision], error) {
				return api.ListRevisions(ctx, contracts.ListRevisionsRequest{DocumentID: did, PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
			})
		}))
	}
	return cmds
}

func (m *Model) loadProjectDocs(force bool) tea.Cmd {
	pid, api := m.curProject, m.api
	pidp := &pid
	fn := func(ctx context.Context) (any, error) {
		return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.Document], error) {
			return api.ListDocuments(ctx, contracts.ListDocumentsRequest{ProjectID: pidp, PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
		})
	}
	if force {
		return m.reload("proj:"+pid+":docs", fn)
	}
	return m.ensure("proj:"+pid+":docs", 0, fn)
}

func (m *Model) loadDoc(docID, rev string, force bool) tea.Cmd {
	api := m.api
	fn := func(ctx context.Context) (any, error) {
		return api.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: docID, RevisionID: rev})
	}
	key := "doc:" + docID + "@" + rev
	if force || rev == "" && m.loads[key] != nil && m.loads[key].stale {
		return m.reload(key, fn)
	}
	return m.ensure(key, 0, fn)
}

type contentPreview struct {
	text      string
	truncated bool
}

const maxInlineBytes = 256 << 10

func previewable(v contracts.ArtifactVersion) bool {
	mt := strings.ToLower(v.MediaType)
	if strings.Contains(mt, "html") {
		return false // active content: only in an isolated viewer
	}
	return strings.HasPrefix(mt, "text/") || strings.Contains(mt, "markdown") || strings.Contains(mt, "json") ||
		strings.Contains(mt, "yaml") || strings.HasSuffix(strings.ToLower(v.Filename), ".md")
}

func (m *Model) loadContent(v contracts.ArtifactVersion, force bool) tea.Cmd {
	api, vid := m.api, v.VersionID
	fn := func(ctx context.Context) (any, error) {
		rc, _, err := api.OpenContent(ctx, vid)
		if err != nil {
			return contentPreview{}, err
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxInlineBytes+1))
		if err != nil {
			return contentPreview{}, err
		}
		cp := contentPreview{text: string(b)}
		if len(b) > maxInlineBytes {
			cp.text, cp.truncated = string(b[:maxInlineBytes]), true
		}
		return cp, nil
	}
	if force {
		return m.reload("content:"+vid, fn)
	}
	return m.ensure("content:"+vid, 0, fn)
}

func (m *Model) keyProject(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	pid := m.curProject
	switch key {
	case "esc":
		return m.goBack()
	case "right", "l", "tab":
		m.projTab = (m.projTab + 1) % numTabs
		return tea.Batch(m.loadProjectView(false)...)
	case "left", "h", "shift+tab":
		m.projTab = (m.projTab + numTabs - 1) % numTabs
		return tea.Batch(m.loadProjectView(false)...)
	case "r":
		return tea.Batch(m.loadProjectView(true)...)
	}
	switch m.projTab {
	case tabDocs:
		l := m.listFor("proj:" + pid + ":docs")
		if navKey(l, key, m.halfPage()) {
			if l.sel != "" {
				return m.loadDoc(l.sel, "", false)
			}
			return nil
		}
		switch key {
		case "enter":
			if l.sel != "" {
				return m.openDoc(l.sel)
			}
		case "e":
			if l.sel != "" {
				return m.editDoc(l.sel)
			}
		}
	case tabArtifacts:
		l := m.listFor("proj:" + pid + ":arts")
		if navKey(l, key, m.halfPage()) {
			return nil
		}
		if key == "enter" && l.sel != "" {
			m.curArtifact = l.sel
			m.docScroll = 0
			return m.push(viewArtifact)
		}
	case tabDiscussion:
		l := m.listFor("proj:" + pid + ":disc")
		if navKey(l, key, m.halfPage()) {
			return nil
		}
		if key == "c" {
			api := m.api
			return m.openTextPrompt("Post to project thread", "message (stored for the project; not injected into any runtime)", func(m *Model, text string) tea.Cmd {
				req := contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
					Recipient: contracts.Address{Kind: contracts.AddrProject, ID: pid}, ProjectID: &pid, Mode: contracts.ModeInfo, Body: contracts.MessageBody{Text: text}}
				return m.do(func(ctx context.Context) (any, error) { return api.SendMessage(ctx, req) }, func(m *Model, v any, err error) tea.Cmd {
					if err != nil {
						return m.errToast("post failed", err)
					}
					return tea.Batch(m.setToast("posted to project thread", toneGood), tea.Batch(m.loadProjectView(true)...))
				})
			})
		}
	case tabSessions:
		l := m.listFor("proj:" + pid + ":sess")
		if navKey(l, key, m.halfPage()) {
			return nil
		}
		if key == "enter" && l.sel != "" {
			return m.focusSession(l.sel)
		}
	}
	return nil
}

func (m *Model) projectDocs(p contracts.Project, w, h int) []string {
	docs, st := get[[]contracts.Document](m, "proj:"+p.ProjectID+":docs")
	sort.SliceStable(docs, func(i, j int) bool { return strings.ToLower(docs[i].Title) < strings.ToLower(docs[j].Title) })
	l := m.listFor("proj:" + p.ProjectID + ":docs")
	ids := make([]string, len(docs))
	byID := map[string]contracts.Document{}
	for i, d := range docs {
		ids[i] = d.DocumentID
		byID[d.DocumentID] = d
	}
	l.setIDs(ids)
	now := m.now()
	list := func(w, h int) []string {
		out := []string{m.colHeader("  DOCUMENTS", w)}
		if len(docs) == 0 {
			out = append(out, m.loadingOr(st, "no documents"))
			return out
		}
		start, end := l.window(h - 1)
		for i := start; i < end; i++ {
			d := byID[ids[i]]
			sel := i == l.idx
			cur := m.cursor(sel)
			row := cur + pad(d.Title, w-2-14) + m.rowTone(sel, toneMuted, padLeft(fmt.Sprintf("r%d · %s", d.HeadSeq, age(now, d.UpdatedAt)), 13))
			out = append(out, m.markRow(l, i, sel, row, w))
		}
		return out
	}
	detail := func(w, h int) []string {
		d, ok := byID[l.sel]
		if !ok {
			return nil
		}
		out := []string{m.th.bold.Render(trunc(d.Title, w)), m.th.dim.Render(trunc(fmt.Sprintf("%s · %s · head r%d", d.Slug, d.MediaType, d.HeadSeq), w)), ""}
		dr, st := get[contracts.DocumentWithRevision](m, "doc:"+d.DocumentID+"@")
		if !st.ok() {
			return append(out, m.loadingOr(st, ""))
		}
		return append(out, wrap(dr.Revision.Content, w)...)
	}
	return m.listDetail(w, h, list, detail)
}

func (st *loadState) ok() bool { return st != nil && st.have }

func (m *Model) loadingOr(st *loadState, empty string) string {
	switch {
	case st == nil || (!st.have && st.err == nil):
		return m.th.dim.Render("  loading…")
	case !st.have || st.err != nil:
		return m.th.tone(toneBad, "  ✗ "+errText(st.err))
	}
	return m.th.dim.Render("  " + empty)
}

func (m *Model) projectArtifacts(p contracts.Project, w, h int) []string {
	arts, st := get[[]contracts.Artifact](m, "proj:"+p.ProjectID+":arts")
	sort.SliceStable(arts, func(i, j int) bool {
		if !arts[i].UpdatedAt.Equal(arts[j].UpdatedAt) {
			return arts[i].UpdatedAt.After(arts[j].UpdatedAt)
		}
		return arts[i].ArtifactID < arts[j].ArtifactID
	})
	l := m.listFor("proj:" + p.ProjectID + ":arts")
	ids := make([]string, len(arts))
	byID := map[string]contracts.Artifact{}
	for i, a := range arts {
		ids[i] = a.ArtifactID
		byID[a.ArtifactID] = a
	}
	l.setIDs(ids)
	now := m.now()
	list := func(w, h int) []string {
		out := []string{m.colHeader("  ARTIFACTS", w)}
		if len(arts) == 0 {
			return append(out, m.loadingOr(st, "nothing published yet"))
		}
		start, end := l.window(h - 1)
		for i := start; i < end; i++ {
			a := byID[ids[i]]
			sel := i == l.idx
			cur := m.cursor(sel)
			row := cur + pad(a.Title, w-2-12) + m.rowTone(sel, toneMuted, padLeft(fmt.Sprintf("v%d · %s", a.VersionCount, age(now, a.UpdatedAt)), 11))
			out = append(out, m.markRow(l, i, sel, row, w))
		}
		return out
	}
	detail := func(w, h int) []string {
		a, ok := byID[l.sel]
		if !ok {
			return nil
		}
		lw := 10
		out := []string{m.th.bold.Render(trunc(a.Title, w))}
		if a.Description != "" {
			out = append(out, wrap(a.Description, w)...)
		}
		out = append(out, "", m.kvl("Versions", itoa(a.VersionCount), lw), m.kvl("Latest", shortID(a.LatestVersionID), lw),
			m.kvl("By", trunc(m.whoFull(a.CreatedBy), w-lw), lw))
		if a.SessionID != "" {
			out = append(out, m.kvl("Session", trunc(m.sessionTitle(a.SessionID), w-lw), lw))
		}
		out = append(out, m.kvl("Updated", ago(now, a.UpdatedAt), lw), "", m.th.dim.Render("enter: versions, provenance, content and comments"))
		return out
	}
	return m.listDetail(w, h, list, detail)
}

type discItem struct {
	id   string
	at   int64
	line string
	body string
	meta string
}

func (m *Model) projectDiscussion(p contracts.Project, w, h int) []string {
	cs, cst := get[[]contracts.ReviewComment](m, "proj:"+p.ProjectID+":comments")
	ms, _ := get[[]contracts.MessageView](m, "proj:"+p.ProjectID+":thread")
	now := m.now()
	var items []discItem
	for _, c := range cs {
		target := string(c.Target.Kind) + " " + shortID(c.Target.ID)
		meta := "comment on " + target
		if c.Anchor != nil {
			meta += " @" + c.Anchor.Kind + " " + c.Anchor.Value
		}
		if c.Resolved {
			meta += " · resolved"
		}
		items = append(items, discItem{id: c.CommentID, at: c.CreatedAt.UnixNano(),
			line: "✎ " + m.who(c.Author) + ": " + firstLine(c.Body), body: c.Body, meta: meta + " · " + ago(now, c.CreatedAt)})
	}
	for _, mv := range ms {
		items = append(items, discItem{id: mv.Envelope.MessageID, at: mv.Envelope.CreatedAt.UnixNano(),
			line: "✉ " + m.who(mv.Envelope.Sender) + ": " + firstLine(mv.Envelope.Body.Text), body: mv.Envelope.Body.Text,
			meta: "project thread · " + ago(now, mv.Envelope.CreatedAt)})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].at != items[j].at {
			return items[i].at > items[j].at
		}
		return items[i].id < items[j].id
	})
	l := m.listFor("proj:" + p.ProjectID + ":disc")
	ids := make([]string, len(items))
	byID := map[string]discItem{}
	for i, it := range items {
		ids[i] = it.id
		byID[it.id] = it
	}
	l.setIDs(ids)
	list := func(w, h int) []string {
		out := []string{m.colHeader("  DISCUSSION · newest first", w)}
		if len(items) == 0 {
			return append(out, m.loadingOr(cst, "no comments or thread messages yet — c to post"))
		}
		start, end := l.window(h - 1)
		for i := start; i < end; i++ {
			sel := i == l.idx
			cur := m.cursor(sel)
			out = append(out, m.markRow(l, i, sel, trunc(cur+byID[ids[i]].line, w), w))
		}
		return out
	}
	detail := func(w, h int) []string {
		it, ok := byID[l.sel]
		if !ok {
			return nil
		}
		return append([]string{m.th.dim.Render(trunc(it.meta, w)), ""}, wrap(it.body, w)...)
	}
	return m.listDetail(w, h, list, detail)
}

func (m *Model) projectSessionsView(p contracts.Project, w, h int) []string {
	ss := m.projectSessions(p.ProjectID)
	l := m.listFor("proj:" + p.ProjectID + ":sess")
	ids := make([]string, len(ss))
	for i, s := range ss {
		ids[i] = s.Session.SessionID
	}
	l.setIDs(ids)
	out := []string{m.colHeader("  SESSIONS IN THIS PROJECT", w)}
	if len(ss) == 0 {
		return append(out, m.th.dim.Render("  none — launch one with n from Sessions, or associate a standalone session with p"))
	}
	now := m.now()
	start, end := l.window(h - 1)
	for i := start; i < end; i++ {
		s := ss[i]
		sel := i == l.idx
		st := sessionStatus(s, m.nodeOf(s))
		cur := m.cursor(sel)
		branch := ""
		if s.Run != nil && s.Run.Checkout != nil {
			branch = string(s.Run.Checkout.Mode) + " " + s.Run.Checkout.Branch
		}
		row := cur + m.statusCell(st, 10) + " " + pad(iconTitle(s), w-60) + " " + pad(s.NodeName, 12) + " " + pad(branch, 20) + " " + padLeft(age(now, observedAt(s)), 4)
		out = append(out, m.markRow(l, i, sel, trunc(row, w), w))
	}
	return out
}

// ---- artifact view ----

func (m *Model) artifact() (contracts.Artifact, bool) {
	if a, st := get[contracts.Artifact](m, "artifact:"+m.curArtifact); st.ok() {
		return a, true
	}
	arts, _ := get[[]contracts.Artifact](m, "proj:"+m.curProject+":arts")
	for _, a := range arts {
		if a.ArtifactID == m.curArtifact {
			return a, true
		}
	}
	return contracts.Artifact{ArtifactID: m.curArtifact, Title: m.curArtifact}, false
}

func (m *Model) artifactVersions() []contracts.ArtifactVersion {
	vs, _ := get[[]contracts.ArtifactVersion](m, "art:"+m.curArtifact+":versions")
	out := append([]contracts.ArtifactVersion(nil), vs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out
}

func (m *Model) selectedVersion() (contracts.ArtifactVersion, bool) {
	l := m.listFor("art:" + m.curArtifact)
	vs := m.artifactVersions()
	ids := make([]string, len(vs))
	for i, v := range vs {
		ids[i] = v.VersionID
	}
	l.setIDs(ids)
	for _, v := range vs {
		if v.VersionID == l.sel {
			return v, true
		}
	}
	return contracts.ArtifactVersion{}, false
}

func (m *Model) openArtifactByRef(pid string, ref contracts.ObjectRef) tea.Cmd {
	m.curProject = pid
	m.projTab = tabArtifacts
	if ref.Kind == contracts.KindArtifact {
		m.curArtifact = ref.ID
		m.docScroll = 0
		m.view, m.back = viewProject, []viewID{viewSessions}
		return m.push(viewArtifact)
	}
	api, vid := m.api, ref.ID
	return m.do(func(ctx context.Context) (any, error) { return api.GetVersion(ctx, contracts.GetRequest{ID: vid}) },
		func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				return m.errToast("open review failed", err)
			}
			ver := v.(contracts.ArtifactVersion)
			m.curArtifact = ver.ArtifactID
			m.listFor("art:" + ver.ArtifactID).sel = ver.VersionID
			m.docScroll = 0
			m.view, m.back = viewProject, []viewID{viewSessions}
			return tea.Batch(m.push(viewArtifact), tea.Batch(m.loadProjectView(false)...))
		})
}

func (m *Model) keyArtifact(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	l := m.listFor("art:" + m.curArtifact)
	m.selectedVersion()
	if navKey(l, key, m.halfPage()) {
		m.docScroll = 0
		if v, ok := m.selectedVersion(); ok && previewable(v) {
			return m.loadContent(v, false)
		}
		return nil
	}
	switch key {
	case "esc":
		return m.goBack()
	case "J", "pgdown":
		m.docScroll += 5
	case "K", "pgup":
		m.docScroll = max(0, m.docScroll-5)
	case "r":
		return tea.Batch(m.loadProjectView(true)...)
	case "c":
		v, ok := m.selectedVersion()
		if !ok {
			return nil
		}
		api := m.api
		vid, aid := v.VersionID, m.curArtifact
		return m.openTextPrompt(fmt.Sprintf("Comment on v%d (%s)", v.Seq, v.Filename), "comment on this exact version", func(m *Model, text string) tea.Cmd {
			req := contracts.CreateCommentRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
				Target: contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: vid}, Body: text}
			return m.do(func(ctx context.Context) (any, error) { return api.CreateComment(ctx, req) }, func(m *Model, v any, err error) tea.Cmd {
				if err != nil {
					return m.errToast("comment failed", err)
				}
				return tea.Batch(m.setToast("comment added to this version", toneGood), m.reload("art:"+aid+":comments", nil))
			})
		})
	}
	return nil
}

func (m *Model) viewArtifact(w, h int) []string {
	a, _ := m.artifact()
	vs := m.artifactVersions()
	m.selectedVersion()
	l := m.listFor("art:" + m.curArtifact)
	_, vst := get[[]contracts.ArtifactVersion](m, "art:"+m.curArtifact+":versions")
	comments, _ := get[[]contracts.ReviewComment](m, "art:"+m.curArtifact+":comments")
	now := m.now()
	list := func(w, h int) []string {
		out := []string{m.th.heading.Render(trunc(a.Title, w)), m.colHeader("  VERSIONS · newest first", w)}
		if len(vs) == 0 {
			return append(out, m.loadingOr(vst, "no versions"))
		}
		start, end := l.window(h - 2)
		for i := start; i < end; i++ {
			v := vs[i]
			sel := i == l.idx
			cur := m.cursor(sel)
			n := 0
			for _, c := range comments {
				if c.Target.ID == v.VersionID {
					n++
				}
			}
			flag := ""
			if v.ReviewRequested {
				flag = " ◇review"
			}
			if n > 0 {
				flag += " ✎" + itoa(n)
			}
			row := cur + pad(fmt.Sprintf("v%d %s", v.Seq, v.Filename), w-2-16) + m.rowTone(sel, toneMuted, padLeft(age(now, v.CreatedAt)+flag, 15))
			out = append(out, m.markRow(l, i, sel, row, w))
		}
		return out
	}
	detail := func(w, h int) []string {
		v, ok := m.selectedVersion()
		if !ok {
			return nil
		}
		lw := 11
		out := []string{m.th.bold.Render(fmt.Sprintf("v%d · %s", v.Seq, trunc(v.Filename, w-8)))}
		out = append(out, m.kvl("Type", v.MediaType+" · "+humanBytes(v.Size), lw))
		out = append(out, m.kvl("Digest", trunc(string(v.Digest), w-lw), lw))
		out = append(out, m.kvl("Author", trunc(m.whoFull(v.Author), w-lw), lw))
		prov := v.Provenance
		var pv []string
		if prov.SessionID != "" {
			pv = append(pv, "session "+m.sessionTitle(prov.SessionID))
		}
		if prov.RunID != "" {
			pv = append(pv, "run "+shortID(prov.RunID))
		}
		if prov.NodeID != "" {
			pv = append(pv, "on "+m.nodeName(prov.NodeID))
		}
		if prov.Commit != "" {
			pv = append(pv, "commit "+prov.Commit)
		}
		if len(pv) == 0 {
			pv = []string{"—"}
		}
		out = append(out, m.kvl("Provenance", trunc(strings.Join(pv, " · "), w-lw), lw))
		if v.PreviousVersionID != "" {
			out = append(out, m.kvl("Follows", shortID(v.PreviousVersionID), lw))
		}
		out = append(out, m.kvl("Published", ago(now, v.CreatedAt), lw))
		if v.Note != "" {
			out = append(out, m.kvl("Note", trunc(v.Note, w-lw), lw))
		}
		if v.ReviewRequested {
			out = append(out, m.kvl("Review", m.th.tone(toneAccent, "◇ review requested"), lw))
		}
		// comments: this version first, then earlier versions (preserved)
		var mine, earlier []contracts.ReviewComment
		for _, c := range comments {
			if c.Target.ID == v.VersionID {
				mine = append(mine, c)
			} else {
				earlier = append(earlier, c)
			}
		}
		out = append(out, "", m.sectionTitle(fmt.Sprintf("Comments on v%d (%d)", v.Seq, len(mine)), w))
		for _, c := range mine {
			out = append(out, m.commentLines(c, w)...)
		}
		if len(earlier) > 0 {
			out = append(out, m.th.dim.Render(fmt.Sprintf("%d comment(s) on other versions are preserved there", len(earlier))))
		}
		out = append(out, "", m.sectionTitle("Content", w))
		switch {
		case strings.Contains(strings.ToLower(v.MediaType), "html"):
			out = append(out, wrap("HTML is active content and is never rendered here. View it in the isolated preview or download it: pyre artifact get "+v.VersionID, w)...)
		case !previewable(v):
			out = append(out, wrap("Not shown inline ("+v.MediaType+"). Download the exact bytes with: pyre artifact get "+v.VersionID, w)...)
		default:
			cp, st := get[contentPreview](m, "content:"+v.VersionID)
			if !st.ok() {
				out = append(out, m.loadingOr(st, ""))
				break
			}
			body := wrap(cp.text, min(w, textMax))
			if strings.Contains(strings.ToLower(v.MediaType), "markdown") || strings.HasSuffix(strings.ToLower(v.Filename), ".md") {
				body = m.markdownLines(cp.text, min(w, textMax))
			}
			if m.docScroll > len(body)-1 {
				m.docScroll = max(0, len(body)-1)
			}
			body = body[m.docScroll:]
			if cp.truncated {
				body = append(body, m.th.dim.Render("… truncated; pyre artifact get "+v.VersionID))
			}
			out = append(out, body...)
		}
		return out
	}
	return m.listDetail(w, h, list, detail)
}

func (m *Model) commentLines(c contracts.ReviewComment, w int) []string {
	head := m.who(c.Author) + " · " + ago(m.now(), c.CreatedAt)
	if c.Anchor != nil {
		head += " · " + c.Anchor.Kind + " " + c.Anchor.Value
	}
	if c.Resolved {
		head += " · resolved"
	}
	out := []string{m.th.dim.Render(trunc(head, w))}
	for _, l := range wrap(c.Body, w-2) {
		out = append(out, "  "+l)
	}
	return out
}

// ---- document reader ----

func (m *Model) openDoc(docID string) tea.Cmd {
	m.curDoc, m.curDocRev, m.docScroll = docID, "", 0
	m.docRevs = false
	return m.push(viewDoc)
}

func (m *Model) keyDoc(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if m.docRevs {
		l := m.listFor("doc:" + m.curDoc + ":revs")
		if navKey(l, key, m.halfPage()) {
			m.curDocRev = l.sel
			m.docScroll = 0
			return m.loadDoc(m.curDoc, m.curDocRev, false)
		}
	}
	switch key {
	case "esc":
		if m.docRevs {
			m.docRevs = false
			return nil
		}
		return m.goBack()
	case "down", "j":
		m.docScroll++
	case "up", "k":
		m.docScroll = max(0, m.docScroll-1)
	case "pgdown", " ", "J":
		m.docScroll += 10
	case "pgup", "K":
		m.docScroll = max(0, m.docScroll-10)
	case "v":
		m.docRevs = !m.docRevs
	case "e":
		return m.editDoc(m.curDoc)
	case "r":
		return tea.Batch(m.loadProjectView(true)...)
	}
	return nil
}

func (m *Model) viewDoc(w, h int) []string {
	dr, st := get[contracts.DocumentWithRevision](m, "doc:"+m.curDoc+"@"+m.curDocRev)
	revs, _ := get[[]contracts.DocumentRevision](m, "doc:"+m.curDoc+":revs")
	now := m.now()
	reader := func(w, h int) []string {
		if !st.ok() {
			return []string{m.loadingOr(st, "")}
		}
		head := dr.Document.HeadRevisionID == dr.Revision.RevisionID
		tag := "head"
		if !head {
			tag = m.th.tone(toneWarn, "older revision (head is r"+itoa(int(dr.Document.HeadSeq))+")")
		}
		out := []string{
			m.th.title.Render(trunc(dr.Document.Title, w)),
			m.th.dim.Render(trunc(fmt.Sprintf("r%d · %s · %s · by %s", dr.Revision.Seq, dr.Document.MediaType, ago(now, dr.Revision.CreatedAt), m.who(dr.Revision.Author)), w)) + "  " + tag,
		}
		if dr.Revision.Summary != "" {
			out = append(out, m.th.dim.Render(trunc("summary: "+dr.Revision.Summary, w)))
		}
		out = append(out, "")
		body := wrap(dr.Revision.Content, w)
		if m.docScroll > max(0, len(body)-(h-len(out))) {
			m.docScroll = max(0, len(body)-(h-len(out)))
		}
		return append(out, body[min(m.docScroll, len(body)):]...)
	}
	if !m.docRevs {
		return reader(w, h)
	}
	sorted := append([]contracts.DocumentRevision(nil), revs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq > sorted[j].Seq })
	l := m.listFor("doc:" + m.curDoc + ":revs")
	ids := make([]string, len(sorted))
	for i, r := range sorted {
		ids[i] = r.RevisionID
	}
	if l.sel == "" && m.curDocRev == "" && len(ids) > 0 {
		l.sel = ids[0]
	}
	l.setIDs(ids)
	list := func(w, h int) []string {
		out := []string{m.colHeader("  REVISIONS", w)}
		start, end := l.window(h - 1)
		for i := start; i < end; i++ {
			r := sorted[i]
			sel := i == l.idx
			cur := m.cursor(sel)
			row := fmt.Sprintf("%sr%d %s %s", cur, r.Seq, padLeft(age(now, r.CreatedAt), 4), m.who(r.Author))
			if r.Summary != "" {
				row += " — " + r.Summary
			}
			out = append(out, m.markRow(l, i, sel, trunc(row, w), w))
		}
		return out
	}
	return m.listDetail(w, h, list, reader)
}

// ---- document editing via $EDITOR ----

type editDoneMsg struct {
	docID, baseRev, path, original string
	err                            error
}

type editExec struct {
	fn   EditFunc
	path string
}

func (e *editExec) Run() error          { return minDuration(func() error { return e.fn(e.path) }) }
func (e *editExec) SetStdin(io.Reader)  {}
func (e *editExec) SetStdout(io.Writer) {}
func (e *editExec) SetStderr(io.Writer) {}

// editDoc checks out the head revision into a local draft file, opens the
// editor, and proposes a successor based on that exact revision.
func (m *Model) editDoc(docID string) tea.Cmd {
	api := m.api
	return m.do(func(ctx context.Context) (any, error) {
		return api.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: docID})
	}, func(m *Model, v any, err error) tea.Cmd {
		if err != nil {
			return m.errToast("cannot open document", err)
		}
		dr := v.(contracts.DocumentWithRevision)
		f, err := os.CreateTemp("", "pyre-draft-*.md")
		if err != nil {
			return m.errToast("cannot create draft", err)
		}
		_, werr := f.WriteString(dr.Revision.Content)
		f.Close()
		if werr != nil {
			return m.errToast("cannot write draft", werr)
		}
		path, base, orig := f.Name(), dr.Revision.RevisionID, dr.Revision.Content
		return m.opts.ExecFunc(&editExec{fn: m.opts.Edit, path: path}, func(err error) tea.Msg {
			return editDoneMsg{docID: docID, baseRev: base, path: path, original: orig, err: err}
		})
	})
}

func (m *Model) onEditDone(msg editDoneMsg) tea.Cmd {
	if msg.err != nil {
		return m.setToast("editor exited with an error; local draft kept at "+msg.path, toneWarn)
	}
	b, err := os.ReadFile(msg.path)
	if err != nil {
		return m.errToast("cannot read draft", err)
	}
	content := string(b)
	if content == msg.original {
		os.Remove(msg.path)
		return m.setToast("no changes; nothing published", toneMuted)
	}
	api := m.api
	req := contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: m.opID("propose:" + msg.docID + ":" + msg.baseRev + ":" + msg.path)},
		DocumentID: msg.docID, BaseRevisionID: msg.baseRev, Content: content, Summary: "edited in pyre tui"}
	path := msg.path
	return tea.Batch(m.setToast("local draft saved; publishing revision based on "+shortID(msg.baseRev)+"…", toneMuted),
		m.do(func(ctx context.Context) (any, error) { return api.ProposeRevision(ctx, req) }, func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				if contracts.IsCode(err, contracts.ErrConflict) {
					ce := contracts.AsError(err)
					head, prop := ce.Details["current_head"], ce.Details["proposal_id"]
					return m.setToast(fmt.Sprintf("Conflict: someone published %v first. Your edit is preserved as proposal %v and in local draft %s — nothing was lost.", head, prop, path), toneWarn)
				}
				return m.setToast("not published ("+errText(err)+"); local draft kept at "+path, toneBad)
			}
			dr := v.(contracts.DocumentWithRevision)
			os.Remove(path)
			if st := m.loads["doc:"+msg.docID+"@"]; st != nil {
				st.stale = true
			}
			return tea.Batch(m.setToast(fmt.Sprintf("published r%d of %s", dr.Revision.Seq, dr.Document.Title), toneGood), tea.Batch(m.loadProjectView(true)...))
		}))
}
