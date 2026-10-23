package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

func (m *Model) viewProjectHome(w, h int) []string {
	p := m.project(m.curProject)
	if p == nil {
		return []string{m.th.dim.Render("project " + m.curProject + " not loaded")}
	}
	var tabs []string
	for i, n := range tabNames {
		tabs = append(tabs, m.mark(zoneDef{kind: zoneSection, idx: i}, m.plainTab(n, i == m.projTab)))
	}
	title := m.th.title.Render(trunc(p.Name, w/2))
	tabLine := strings.Join(tabs, gap1)
	var out []string
	if width(title)+4+width(tabLine) <= w {
		out = []string{title + gap2 + m.th.faint.Render("·") + gap2 + tabLine, ""}
	} else {
		out = []string{title, tabLine, ""}
	}
	bh := h - len(out)
	var body []string
	switch m.projTab {
	case tabOverview:
		body = m.projectOverview(*p, w, bh, true)
	case tabDocs:
		body = m.projectDocs(*p, w, bh)
	case tabArtifacts:
		body = m.projectArtifacts(*p, w, bh)
	case tabDiscussion:
		body = m.projectDiscussion(*p, w, bh)
	case tabSessions:
		body = m.projectSessionsView(*p, w, bh)
	case tabGrants:
		body = m.projectGrantsView(*p, w, bh)
	}
	return append(out, body...)
}

// projectOverview answers: what is this, where is the work, what changed.
// Wide terminals put the facts and the brief side by side.
func (m *Model) projectOverview(p contracts.Project, w, h int, full bool) []string {
	var head []string
	if !full {
		head = append(head, m.th.title.Render(trunc(p.Name, w)))
	}
	if full && w >= splitMin {
		lw := min(84, w*45/100)
		rw := w - lw - 3
		facts := m.projectFacts(p, lw, true)
		brief := m.projectBrief(p, min(rw, textMax))
		return hjoin(fit(facts, lw, h), lw, " "+m.th.faint.Render("│")+" ", fit(brief, rw, h), rw)
	}
	out := append(head, m.projectFacts(p, w, full)...)
	out = append(out, "")
	return append(out, m.projectBrief(p, min(w, textMax))...)
}

func (m *Model) projectFacts(p contracts.Project, w int, full bool) []string {
	lw := 13
	vw := w - lw
	now := m.now()
	var out []string
	if p.Description != "" {
		out = append(out, wrap(p.Description, min(w, textMax))...)
		out = append(out, "")
	}
	if len(p.Repos) == 0 {
		out = append(out, m.kvl("Repos", m.th.dim.Render("none (a project needs no repository)"), lw))
	}
	for i, r := range p.Repos {
		label := ""
		if i == 0 {
			label = "Repos"
		}
		v := r.Name
		if r.URL != "" {
			v += m.th.dim.Render("  " + r.URL)
		}
		out = append(out, m.kvl(label, trunc(v, vw), lw))
	}
	for i, c := range p.Checkouts {
		label := ""
		if i == 0 {
			label = "Checkouts"
		}
		d := " (" + string(c.Mode)
		if c.Branch != "" {
			d += " " + c.Branch
		}
		out = append(out, m.kvl(label, trunc(m.nodeName(c.NodeID)+":"+c.Path+m.th.dim.Render(d+")"), vw), lw))
	}
	for i, l := range p.Links {
		label := ""
		if i == 0 {
			label = "Links"
		}
		out = append(out, m.kvl(label, trunc(l.Title+"  "+m.th.dim.Render(l.URL), vw), lw))
	}
	if p.EnvProfile != "" {
		out = append(out, m.kvl("Environment", p.EnvProfile, lw))
	}
	if len(p.Connections) > 0 {
		out = append(out, m.kvl("Connections", trunc(strings.Join(p.Connections, ", "), vw), lw))
	}
	out = append(out, m.kvl("Updated", ago(now, p.UpdatedAt), lw))
	ss := m.projectSessions(p.ProjectID)
	out = append(out, "", m.sectionTitle(fmt.Sprintf("Sessions · %d", len(ss)), w))
	if len(ss) == 0 {
		out = append(out, m.th.dim.Render("None yet — n launches one into this project."))
	}
	for i, s := range ss {
		if i >= 6 {
			out = append(out, m.th.dim.Render(fmt.Sprintf("… %d more (Sessions section)", len(ss)-6)))
			break
		}
		st := sessionStatus(s, m.nodeOf(s))
		out = append(out, trunc(m.th.tone(st.tone, st.sym+" "+pad(st.label, 10))+" "+iconTitle(s)+m.th.dim.Render("  "+s.NodeName), w))
	}
	if full {
		if arts, st := get[[]contracts.Artifact](m, "proj:"+p.ProjectID+":arts"); st != nil && st.have {
			out = append(out, "", m.sectionTitle(fmt.Sprintf("Artifacts · %d", len(arts)), w))
			sort.SliceStable(arts, func(i, j int) bool { return arts[i].UpdatedAt.After(arts[j].UpdatedAt) })
			for i, a := range arts {
				if i >= 4 {
					break
				}
				meta := fmt.Sprintf("v%d · %s", a.VersionCount, age(now, a.UpdatedAt))
				out = append(out, pad(trunc(a.Title, w-width(meta)-2), w-width(meta))+m.th.dim.Render(meta))
			}
		}
	}
	return out
}

func (m *Model) projectBrief(p contracts.Project, w int) []string {
	out := []string{m.sectionTitle("Brief", w)}
	if p.BriefDocumentID == "" {
		return append(out, m.th.dim.Render("No brief yet."))
	}
	dr, st := get[contracts.DocumentWithRevision](m, "doc:"+p.BriefDocumentID+"@")
	switch {
	case st == nil || (!st.have && st.err == nil):
		out = append(out, m.th.dim.Render("loading brief…"))
	case !st.have:
		out = append(out, m.th.tone(toneBad, "✗ "+errText(st.err)))
	default:
		out = append(out, m.th.dim.Render(trunc(fmt.Sprintf("revision %d · %s · by %s", dr.Revision.Seq, ago(m.now(), dr.Revision.CreatedAt), m.who(dr.Revision.Author)), w)), "")
		out = append(out, m.markdownLines(dr.Revision.Content, w)...)
	}
	return out
}

// markdownLines lightly styles Markdown for the terminal reader: headings
// bold, list bullets and code spans kept readable, text wrapped.
func (m *Model) markdownLines(md string, w int) []string {
	var out []string
	inCode := false
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "```"):
			inCode = !inCode
			out = append(out, m.th.faint.Render(trunc(t, w)))
		case inCode:
			out = append(out, m.th.accent.Render(trunc("  "+line, w)))
		case strings.HasPrefix(t, "#"):
			out = append(out, m.th.bold.Render(trunc(strings.TrimSpace(strings.TrimLeft(t, "#")), w)))
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			for i, l := range wrap(t[2:], w-2) {
				lead := m.th.accent.Render("•") + " "
				if i > 0 {
					lead = "  "
				}
				out = append(out, lead+l)
			}
		default:
			out = append(out, wrap(line, w)...)
		}
	}
	return out
}
