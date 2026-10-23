package hub

import (
	"context"
	"fmt"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Launch context every session gets (setup review F2/F8), with or without
// IncludeContext: the project's fact sheet (settled facts quoting the
// owner, contracts/factsheet.go) and the standing grants that hold for it
// (its parent's tree and its project; standing_grants.go). Children so
// start knowing what the owner already settled and approved, instead of
// re-deriving it from a paraphrased brief.

// launchGrantsMax bounds the grants listed at launch, one line each of at
// most launchGrantLineMax runes; fact sheet and grants together stay
// within contracts.StandingContextMaxBytes.
const (
	launchGrantsMax    = 20
	launchGrantLineMax = 360
)

// standingContext renders the fact sheet and held grants for a session
// about to start under parentID in projectID ("" text when there is none).
func (h *Hub) standingContext(ctx context.Context, projectID *string, parentID string) (string, []contracts.ObjectRef) {
	var b strings.Builder
	var refs []contracts.ObjectRef
	if projectID != nil && *projectID != "" && h.cfg.Documents != nil {
		d, err := h.cfg.Documents.GetDocument(h.systemCtx(ctx), contracts.GetDocumentRequest{ProjectID: *projectID, Slug: contracts.FactSheetSlug})
		// Backstop: a head revision someone else wrote is not the owner's
		// word (contracts.FactSheetWriter); it is left out.
		if text := factSheetBody(d.Revision.Content); err == nil && text != "" && contracts.FactSheetWriter(d.Revision.Author) {
			ref := contracts.ObjectRef{Kind: contracts.KindDocumentRevision, ID: d.Revision.RevisionID}
			b.WriteString("## Project facts (settled by the owner; his words quoted)\n\n")
			if len(text) > contracts.FactSheetMaxBytes {
				text = clipBytes(text, contracts.FactSheetMaxBytes) + "\n[fact sheet truncated; `pyre fact ls` shows all of it]"
			}
			b.WriteString(text)
			b.WriteString("\n\nFollow these over any brief that says otherwise; quote them, don't paraphrase. When the owner settles another fact, propose it with propose_fact (it asks him). Sheet: " + contracts.DeepLink(ref) + "\n")
			refs = append(refs, ref)
		}
	}
	if grants, err := h.grantsHeldBy(ctx, h.db, parentID, projectID); err == nil && len(grants) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## Standing go-aheads from the owner\n\nThese hold for you (children inherit them): an ask with that action and option is answered automatically. Irreversible and prod actions always go to the owner.\n")
		for i, g := range grants {
			if i == launchGrantsMax {
				fmt.Fprintf(&b, "- … %d more: `pyre grant ls`\n", len(grants)-i)
				break
			}
			line := fmt.Sprintf("- %s → %q", g.Action, g.Option)
			if g.Condition != "" {
				line += " — note: " + oneLineText(g.Condition, 120)
			}
			line += " — " + grantProvenance(g) + " (" + g.GrantID
			if !g.ExpiresAt.IsZero() {
				line += ", until " + askClock(g.ExpiresAt)
			}
			line += ")"
			line = clipRunes(line, launchGrantLineMax) + "\n"
			if b.Len()+len(line) > contracts.StandingContextMaxBytes-64 {
				fmt.Fprintf(&b, "- … %d more: `pyre grant ls`\n", len(grants)-i)
				break
			}
			b.WriteString(line)
			refs = append(refs, contracts.ObjectRef{Kind: contracts.KindStandingGrant, ID: g.GrantID})
		}
	}
	return b.String(), refs
}

// withStandingContext puts the standing context in front of lc's text.
func withStandingContext(lc *contracts.LaunchContext, text string, refs []contracts.ObjectRef) *contracts.LaunchContext {
	if text == "" {
		return lc
	}
	if lc == nil {
		lc = &contracts.LaunchContext{References: []contracts.ObjectRef{}}
	}
	lc.Text = strings.TrimRight(text, "\n") + "\n" + strings.TrimSpace("\n"+lc.Text)
	lc.Text = strings.TrimRight(lc.Text, "\n") + "\n"
	lc.Bytes = len(lc.Text)
	lc.References = append(lc.References, refs...)
	return lc
}

// factSheetBody is the sheet without its leading title line.
func factSheetBody(content string) string {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "# ") {
		if _, rest, ok := strings.Cut(content, "\n"); ok {
			content = strings.TrimSpace(rest)
		} else {
			content = ""
		}
	}
	return content
}

// clipBytes cuts s to at most n bytes on a line (else rune) boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > n/2 {
		return cut[:i]
	}
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	if len(cut) > 0 && cut[len(cut)-1] >= 0xC0 {
		cut = cut[:len(cut)-1]
	}
	return cut
}
